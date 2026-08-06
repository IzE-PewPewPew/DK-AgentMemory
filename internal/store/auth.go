package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/IzE-PewPewPew/DK-AgentMemory/internal/ulid"
)

// KeyPrefix is the fixed marker every issued key starts with. It exists so a
// leaked key is recognisable in a log or a paste, and so secret scanners can be
// taught one pattern.
const KeyPrefix = "pmk"

// prefixLen is the length of the indexed, non-secret portion: "pmk_a3f2".
const prefixLen = 8

// Auth errors, distinguished so the API can return 401 with an accurate reason
// while never telling an unauthenticated caller which of the three it was.
var (
	ErrKeyMalformed = errors.New("api key is malformed")
	ErrKeyUnknown   = errors.New("api key not recognised")
	ErrKeyRevoked   = errors.New("api key has been revoked")
)

// argon2id parameters.
//
// 19 MiB / t=2 / p=1 is the OWASP minimum recommendation. Higher memory would
// be better for a password, but this runs on every authenticated request, and
// a 64 MiB allocation per request is a denial-of-service surface rather than a
// security improvement. The verification cache below removes the cost from the
// steady state; revocation is still checked against the database on every
// request, so nothing is cached that would delay a revoke.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashKey returns a PHC-format argon2id hash of the full plaintext key.
func HashKey(plaintext string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	sum := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyKey checks a plaintext key against a stored PHC hash.
//
// Parameters come from the stored string rather than from the constants above,
// so raising the cost later does not invalidate every key already issued.
func VerifyKey(plaintext, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("stored hash is not argon2id")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("stored hash has no version")
	}
	if version != argon2.Version {
		return false, fmt.Errorf("stored hash uses argon2 version %d, this build understands %d", version, argon2.Version)
	}

	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false, fmt.Errorf("stored hash has malformed parameters")
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("stored hash has a malformed salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("stored hash has a malformed digest")
	}

	got := argon2.IDKey([]byte(plaintext), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// GenerateKey returns a new plaintext key and its indexed prefix.
//
// Shape: pmk_<4 hex>_<32 base64url chars>, giving 144 bits of entropy in the
// secret portion.
func GenerateKey() (plaintext, prefix string, err error) {
	pb := make([]byte, 2)
	if _, err := rand.Read(pb); err != nil {
		return "", "", err
	}
	sb := make([]byte, 24)
	if _, err := rand.Read(sb); err != nil {
		return "", "", err
	}
	prefix = KeyPrefix + "_" + hex.EncodeToString(pb)
	plaintext = prefix + "_" + base64.RawURLEncoding.EncodeToString(sb)
	return plaintext, prefix, nil
}

// SplitKey extracts the indexed prefix from a presented key.
func SplitKey(plaintext string) (prefix string, err error) {
	if len(plaintext) <= prefixLen+1 || !strings.HasPrefix(plaintext, KeyPrefix+"_") {
		return "", ErrKeyMalformed
	}
	prefix = plaintext[:prefixLen]
	if plaintext[prefixLen] != '_' {
		return "", ErrKeyMalformed
	}
	return prefix, nil
}

// --- verification cache ----------------------------------------------------

// verifyCache remembers that a specific plaintext matched a specific key's
// hash, so argon2 runs once per key per process rather than once per request.
//
// It caches only the expensive, immutable half of authentication. Revocation
// and existence are read from the database on every single request, which is
// what makes `dkm admin key revoke` take effect on the next call rather than
// after a TTL.
type verifyCache struct {
	mu   sync.RWMutex
	seen map[string]time.Time
}

const verifyCacheMax = 4096

func newVerifyCache() *verifyCache { return &verifyCache{seen: make(map[string]time.Time)} }

func (c *verifyCache) token(keyID, plaintext string) string {
	h := sha256.Sum256([]byte(keyID + "\x00" + plaintext))
	return string(h[:])
}

func (c *verifyCache) has(keyID, plaintext string) bool {
	t := c.token(keyID, plaintext)
	c.mu.RLock()
	_, ok := c.seen[t]
	c.mu.RUnlock()
	return ok
}

func (c *verifyCache) add(keyID, plaintext string) {
	t := c.token(keyID, plaintext)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) >= verifyCacheMax {
		// Cheap bounded eviction: drop the oldest half. This is a cache of
		// successful verifications, so a miss costs one argon2 pass, not a
		// failure.
		cutoff := time.Now().Add(-time.Hour)
		for k, v := range c.seen {
			if v.Before(cutoff) {
				delete(c.seen, k)
			}
		}
		if len(c.seen) >= verifyCacheMax {
			for k := range c.seen {
				delete(c.seen, k)
				if len(c.seen) < verifyCacheMax/2 {
					break
				}
			}
		}
	}
	c.seen[t] = time.Now()
}

var (
	globalVerifyCache = newVerifyCache()

	lastUsedMu    sync.Mutex
	lastUsedFlush = map[string]time.Time{}
)

// --- authentication --------------------------------------------------------

// Authenticate resolves a presented bearer token to an Identity.
//
// This is the one authentication path in the system. HTTP, the CLI, and MCP all
// arrive here, so a key that works with curl works in the agent, and a bug
// fixed here is fixed everywhere.
func (s *Store) Authenticate(ctx context.Context, plaintext string) (*Identity, error) {
	prefix, err := SplitKey(plaintext)
	if err != nil {
		return nil, err
	}

	var (
		keyID, userID, teamID, userName, hash string
		isAdmin                               bool
		revokedAt                             *time.Time
	)
	err = s.pool.QueryRow(ctx, `
		SELECT k.id, k.hash, k.revoked_at, u.id, u.name, u.team_id, u.is_admin
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		WHERE k.prefix = $1
	`, prefix).Scan(&keyID, &hash, &revokedAt, &userID, &userName, &teamID, &isAdmin)
	if err != nil {
		if errors.Is(wrapNotFound(err), ErrNotFound) {
			return nil, ErrKeyUnknown
		}
		return nil, err
	}

	// Revocation is checked before the hash. A revoked key must not even pay
	// for a verification, and it must never be served from the cache.
	if revokedAt != nil {
		return nil, ErrKeyRevoked
	}

	if !globalVerifyCache.has(keyID, plaintext) {
		ok, err := VerifyKey(plaintext, hash)
		if err != nil {
			return nil, fmt.Errorf("verifying key %s: %w", prefix, err)
		}
		if !ok {
			return nil, ErrKeyUnknown
		}
		globalVerifyCache.add(keyID, plaintext)
	}

	s.touchKey(ctx, keyID)

	return &Identity{
		KeyID:    keyID,
		UserID:   userID,
		UserName: userName,
		TeamID:   teamID,
		IsAdmin:  isAdmin,
	}, nil
}

// touchKey records last use, at most once a minute per key.
//
// Without the throttle every read becomes a write, which turns a read-heavy
// workload into a write-heavy one and puts every request behind a row lock on
// the same key.
func (s *Store) touchKey(ctx context.Context, keyID string) {
	now := time.Now()
	lastUsedMu.Lock()
	last, ok := lastUsedFlush[keyID]
	if ok && now.Sub(last) < time.Minute {
		lastUsedMu.Unlock()
		return
	}
	lastUsedFlush[keyID] = now
	lastUsedMu.Unlock()

	// Best effort: a failure to record last-use must never fail a request.
	_, _ = s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
}

// --- teams and users -------------------------------------------------------

// CreateTeam inserts a team.
func (s *Store) CreateTeam(ctx context.Context, id, name string) (*Team, error) {
	if id = strings.TrimSpace(id); id == "" {
		return nil, fmt.Errorf("team id is required")
	}
	if name == "" {
		name = id
	}
	var t Team
	err := s.pool.QueryRow(ctx, `
		INSERT INTO teams (id, name) VALUES ($1, $2)
		RETURNING id, name, created_at
	`, id, name).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%w: team %q", ErrConflict, id)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTeams returns every team, for admin tooling.
func (s *Store) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, created_at FROM teams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateUser inserts a user into a team.
func (s *Store) CreateUser(ctx context.Context, id, teamID, name string, isAdmin bool) (*User, error) {
	if id = strings.TrimSpace(id); id == "" {
		return nil, fmt.Errorf("user id is required")
	}
	if teamID = strings.TrimSpace(teamID); teamID == "" {
		return nil, fmt.Errorf("team id is required")
	}
	if name == "" {
		name = id
	}
	var u User
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (id, team_id, name, is_admin) VALUES ($1, $2, $3, $4)
		RETURNING id, team_id, name, is_admin, created_at
	`, id, teamID, name, isAdmin).Scan(&u.ID, &u.TeamID, &u.Name, &u.IsAdmin, &u.CreatedAt)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%w: user %q", ErrConflict, id)
	}
	if isForeignKeyViolation(err) {
		return nil, fmt.Errorf("%w: team %q — create it first with `dkm admin team create`", ErrNotFound, teamID)
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUser returns one user.
func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT id, team_id, name, is_admin, created_at FROM users WHERE id = $1
	`, id).Scan(&u.ID, &u.TeamID, &u.Name, &u.IsAdmin, &u.CreatedAt)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	return &u, nil
}

// ListUsers returns the users of one team.
func (s *Store) ListUsers(ctx context.Context, teamID string) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, team_id, name, is_admin, created_at FROM users WHERE team_id = $1 ORDER BY id
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.TeamID, &u.Name, &u.IsAdmin, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// --- keys ------------------------------------------------------------------

// IssuedKey is returned once, at issue. The plaintext is not stored and cannot
// be recovered.
type IssuedKey struct {
	APIKey
	Plaintext string `json:"key"`
}

// IssueKey creates a key for a user.
func (s *Store) IssueKey(ctx context.Context, userID, label string) (*IssuedKey, error) {
	plaintext, prefix, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	hash, err := HashKey(plaintext)
	if err != nil {
		return nil, err
	}

	k := APIKey{ID: ulid.New(), UserID: userID, Prefix: prefix, Label: label}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (id, user_id, prefix, hash, label) VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at
	`, k.ID, k.UserID, k.Prefix, hash, k.Label).Scan(&k.CreatedAt)
	if isForeignKeyViolation(err) {
		return nil, fmt.Errorf("%w: user %q — create it first with `dkm admin user create`", ErrNotFound, userID)
	}
	if err != nil {
		return nil, err
	}

	return &IssuedKey{APIKey: k, Plaintext: plaintext}, nil
}

// ListKeys returns key metadata for a team. Hashes are never selected.
func (s *Store) ListKeys(ctx context.Context, teamID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT k.id, k.user_id, k.prefix, k.label, k.created_at, k.last_used_at, k.revoked_at
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		WHERE u.team_id = $1
		ORDER BY k.created_at DESC
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Prefix, &k.Label, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey marks a key revoked.
//
// The effect is immediate: Authenticate reads revoked_at from the database on
// every request, so the next call with this key fails regardless of what any
// cache holds.
func (s *Store) RevokeKey(ctx context.Context, teamID, keyID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_keys k
		SET revoked_at = now()
		FROM users u
		WHERE k.user_id = u.id AND k.id = $1 AND u.team_id = $2 AND k.revoked_at IS NULL
	`, keyID, teamID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountKeys returns the total number of keys, used to decide whether first-boot
// bootstrapping is needed.
func (s *Store) CountKeys(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&n)
	return n, err
}

// Bootstrap creates the initial team, admin user, and key when the database has
// no keys at all.
//
// It returns nil when a key already exists, which is what makes it safe to call
// on every boot: a restarted container must not mint a second admin key and
// print it to the log.
func (s *Store) Bootstrap(ctx context.Context, teamID, teamName, userID, userName string) (*IssuedKey, error) {
	n, err := s.CountKeys(ctx)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, nil
	}

	if _, err := s.CreateTeam(ctx, teamID, teamName); err != nil && !errors.Is(err, ErrConflict) {
		return nil, err
	}
	if _, err := s.CreateUser(ctx, userID, teamID, userName, true); err != nil && !errors.Is(err, ErrConflict) {
		return nil, err
	}
	return s.IssueKey(ctx, userID, "bootstrap admin key")
}
