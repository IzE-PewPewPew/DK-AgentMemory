// Package ulid implements ULIDs (Universally Unique Lexicographically Sortable
// Identifiers) with no external dependencies.
//
// A ULID is 128 bits: a 48-bit big-endian millisecond timestamp followed by 80
// bits of randomness, rendered as 26 characters of Crockford base32. Two
// properties matter here:
//
//   - Lexicographic sort order matches creation order, so cursor pagination is
//     a plain `id > cursor` comparison with no secondary sort column.
//   - Clients can generate them offline. The write queue relies on this: a
//     queued memory already has its final ID before the server has ever seen
//     it, which is what makes flush idempotent (see internal/client/mirror.go).
package ulid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// encoding is Crockford base32: no I, L, O, or U, so a transcribed ID
	// cannot be confused for a different one.
	encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// Length is the character length of an encoded ULID.
	Length = 26
)

var decoding [256]byte

func init() {
	for i := range decoding {
		decoding[i] = 0xFF
	}
	for i, c := range []byte(encoding) {
		decoding[c] = byte(i)
		// Accept lowercase input on decode.
		if c >= 'A' && c <= 'Z' {
			decoding[c+32] = byte(i)
		}
	}
}

var (
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
)

// New returns a ULID for the current time.
func New() string { return NewAt(time.Now()) }

// NewAt returns a ULID for a specific instant.
//
// Within the same millisecond the random component is incremented rather than
// redrawn, so IDs generated in a tight loop remain strictly increasing. Without
// this, two memories saved in the same millisecond could sort in either order
// and a cursor could skip one.
func NewAt(t time.Time) string {
	ms := uint64(t.UnixMilli())

	mu.Lock()
	defer mu.Unlock()

	if ms == lastMS {
		if !increment(&lastRand) {
			// Overflowed 80 bits of randomness inside one millisecond, which
			// takes ~1.2e24 IDs. Step the clock forward instead of wrapping.
			ms++
			lastMS = ms
			randomise(&lastRand)
		}
	} else {
		lastMS = ms
		randomise(&lastRand)
	}

	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	copy(b[6:], lastRand[:])

	return encode(b)
}

func randomise(dst *[10]byte) {
	if _, err := rand.Read(dst[:]); err != nil {
		// crypto/rand failing means the OS entropy source is gone. Nothing
		// sensible remains to be done and silently producing predictable IDs
		// would be worse than stopping.
		panic("ulid: crypto/rand unavailable: " + err.Error())
	}
}

// increment adds one to the 80-bit randomness, returning false on overflow.
func increment(r *[10]byte) bool {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] != 0xFF {
			r[i]++
			return true
		}
		r[i] = 0
	}
	return false
}

func encode(b [16]byte) string {
	out := make([]byte, Length)

	// Timestamp: 48 bits into the first 10 characters.
	out[0] = encoding[(b[0]&224)>>5]
	out[1] = encoding[b[0]&31]
	out[2] = encoding[(b[1]&248)>>3]
	out[3] = encoding[((b[1]&7)<<2)|((b[2]&192)>>6)]
	out[4] = encoding[(b[2]&62)>>1]
	out[5] = encoding[((b[2]&1)<<4)|((b[3]&240)>>4)]
	out[6] = encoding[((b[3]&15)<<1)|((b[4]&128)>>7)]
	out[7] = encoding[(b[4]&124)>>2]
	out[8] = encoding[((b[4]&3)<<3)|((b[5]&224)>>5)]
	out[9] = encoding[b[5]&31]

	// Randomness: 80 bits into the remaining 16 characters.
	out[10] = encoding[(b[6]&248)>>3]
	out[11] = encoding[((b[6]&7)<<2)|((b[7]&192)>>6)]
	out[12] = encoding[(b[7]&62)>>1]
	out[13] = encoding[((b[7]&1)<<4)|((b[8]&240)>>4)]
	out[14] = encoding[((b[8]&15)<<1)|((b[9]&128)>>7)]
	out[15] = encoding[(b[9]&124)>>2]
	out[16] = encoding[((b[9]&3)<<3)|((b[10]&224)>>5)]
	out[17] = encoding[b[10]&31]
	out[18] = encoding[(b[11]&248)>>3]
	out[19] = encoding[((b[11]&7)<<2)|((b[12]&192)>>6)]
	out[20] = encoding[(b[12]&62)>>1]
	out[21] = encoding[((b[12]&1)<<4)|((b[13]&240)>>4)]
	out[22] = encoding[((b[13]&15)<<1)|((b[14]&128)>>7)]
	out[23] = encoding[(b[14]&124)>>2]
	out[24] = encoding[((b[14]&3)<<3)|((b[15]&224)>>5)]
	out[25] = encoding[b[15]&31]

	return string(out)
}

// ErrInvalid is returned by Validate and Time for malformed input.
var ErrInvalid = errors.New("ulid: invalid identifier")

// Validate reports whether s is a well-formed ULID.
//
// Every ID arriving from a client is checked with this before it reaches SQL.
// Client-generated IDs are the one place where an untrusted value becomes a
// primary key, so it is validated as input rather than trusted as an
// identifier.
func Validate(s string) error {
	if len(s) != Length {
		return fmt.Errorf("%w: expected %d characters, got %d", ErrInvalid, Length, len(s))
	}
	for i := 0; i < len(s); i++ {
		if decoding[s[i]] == 0xFF {
			return fmt.Errorf("%w: character %q at position %d is not Crockford base32", ErrInvalid, s[i], i)
		}
	}
	// The first character encodes the top 3 bits of a 48-bit timestamp, so
	// anything above '7' overflows and is not representable.
	if s[0] > '7' {
		return fmt.Errorf("%w: timestamp overflows 48 bits", ErrInvalid)
	}
	return nil
}

// Time extracts the creation instant encoded in a ULID.
func Time(s string) (time.Time, error) {
	if err := Validate(s); err != nil {
		return time.Time{}, err
	}
	u := strings.ToUpper(s)
	var ms uint64
	for i := 0; i < 10; i++ {
		ms = ms<<5 | uint64(decoding[u[i]])
	}
	return time.UnixMilli(int64(ms)).UTC(), nil
}
