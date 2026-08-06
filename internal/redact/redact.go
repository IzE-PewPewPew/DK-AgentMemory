// Package redact removes credential-shaped strings from text before it is
// persisted.
//
// Automatic capture means anything an agent read during a session is a
// candidate for storage: a .env file, a pasted connection string, a token in a
// curl command. Redaction therefore runs at ingest, ahead of the database, and
// the raw value never reaches a write. Storing first and scrubbing later is not
// equivalent — the value would already be in the WAL, in a backup, and in
// whatever replica exists.
//
// Findings carry a kind and a byte offset and never carry the matched text.
// Import dry-runs report "an AWS key at line 41" rather than reprinting the key
// into a terminal, a log file, and the user's scrollback.
package redact

import (
	"regexp"
	"sort"
	"strings"
)

// Kind names a class of secret. It appears in dry-run reports and in the
// redacted placeholder, so it is stable API.
type Kind string

const (
	KindAWSAccessKey  Kind = "aws_access_key"
	KindAWSSecret     Kind = "aws_secret_key"
	KindOpenAIKey     Kind = "openai_key"
	KindAnthropicKey  Kind = "anthropic_key"
	KindGoogleKey     Kind = "google_api_key"
	KindGitHubToken   Kind = "github_token"
	KindSlackToken    Kind = "slack_token"
	KindStripeKey     Kind = "stripe_key"
	KindJWT           Kind = "jwt"
	KindPrivateKey    Kind = "private_key"
	KindPassword      Kind = "password"
	KindConnString    Kind = "connection_string"
	KindBearer        Kind = "bearer_token"
	KindDKMKey        Kind = "dkm_api_key"
	KindGenericSecret Kind = "generic_secret"
)

// Finding locates one secret. It deliberately has no field carrying the matched
// text: a Finding is safe to log, to serialise into an API response, and to
// print to a terminal.
type Finding struct {
	Kind   Kind `json:"kind"`
	Offset int  `json:"offset"` // byte offset of the redacted span
	Length int  `json:"length"` // byte length of the redacted span
	Line   int  `json:"line"`   // 1-based line number
	Column int  `json:"column"` // 1-based column, in bytes
}

type rule struct {
	kind Kind
	re   *regexp.Regexp
	// group is the submatch index to redact. 0 redacts the whole match; a
	// higher index redacts only the value, keeping `password=` readable so the
	// surrounding text still makes sense to a human reading the memory.
	group int
}

// rules are ordered most-specific first. Overlapping matches resolve to the
// rule listed earlier, so `sk-ant-...` is reported as an Anthropic key rather
// than as a generic OpenAI-style token.
var rules = []rule{
	{KindPrivateKey, regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), 0},
	{KindPrivateKey, regexp.MustCompile(`(?s)-----BEGIN OPENSSH PRIVATE KEY-----.*?-----END OPENSSH PRIVATE KEY-----`), 0},

	{KindAWSAccessKey, regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|A3T[A-Z0-9])[A-Z0-9]{16}\b`), 0},
	{KindAWSSecret, regexp.MustCompile(`(?i)aws_secret_access_key\s*[=:]\s*["']?([A-Za-z0-9/+=]{40})`), 1},

	{KindAnthropicKey, regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`), 0},
	{KindOpenAIKey, regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_\-]{20,}`), 0},
	{KindStripeKey, regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`), 0},
	{KindGoogleKey, regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), 0},
	{KindGitHubToken, regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}\b`), 0},
	{KindGitHubToken, regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{60,}\b`), 0},
	{KindSlackToken, regexp.MustCompile(`\bxox[baprse]-[A-Za-z0-9\-]{10,}`), 0},
	{KindDKMKey, regexp.MustCompile(`\bpmk_[A-Za-z0-9]{4}_[A-Za-z0-9_\-]{16,}`), 0},

	// Three base64url segments. The `eyJ` prefix is a base64-encoded `{"`, which
	// is what makes this distinguishable from an arbitrary dotted identifier.
	{KindJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), 0},

	{KindConnString, regexp.MustCompile(`(?i)\b(?:postgres|postgresql|mysql|mariadb|mongodb\+srv|mongodb|redis|rediss|amqp|amqps|clickhouse)://[^\s"'<>]*:([^\s"'<>@]+)@[^\s"'<>]+`), 1},

	{KindPassword, regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|api[_\-]?key|access[_\-]?token|auth[_\-]?token|client[_\-]?secret)\s*[=:]\s*["']?([^\s"'<>,;]{6,})`), 1},
	{KindBearer, regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._\-]{20,})`), 1},

	// Long high-entropy assignments that no earlier rule named. Deliberately
	// last and deliberately conservative: it requires an explicit token-ish
	// variable name, because a rule loose enough to catch every secret is also
	// loose enough to shred ordinary prose.
	{KindGenericSecret, regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:TOKEN|SECRET|APIKEY|PRIVATE_KEY|CREDENTIAL)[A-Z0-9_]*\s*[=:]\s*["']?([A-Za-z0-9/+_\-=]{24,})`), 1},
}

// Scan reports every secret in s without modifying it.
func Scan(s string) []Finding {
	spans := collect(s)
	out := make([]Finding, 0, len(spans))
	lines := newLineIndex(s)
	for _, sp := range spans {
		line, col := lines.at(sp.start)
		out = append(out, Finding{
			Kind:   sp.kind,
			Offset: sp.start,
			Length: sp.end - sp.start,
			Line:   line,
			Column: col,
		})
	}
	return out
}

// Apply replaces every secret in s with a `[redacted:kind]` marker and returns
// the findings.
//
// The marker is left in place rather than deleting the span, so a memory whose
// body once contained a token still reads coherently and a reader can tell that
// something was removed rather than that a sentence was truncated.
func Apply(s string) (string, []Finding) {
	spans := collect(s)
	if len(spans) == 0 {
		return s, nil
	}

	lines := newLineIndex(s)
	var b strings.Builder
	b.Grow(len(s))

	findings := make([]Finding, 0, len(spans))
	prev := 0
	for _, sp := range spans {
		b.WriteString(s[prev:sp.start])
		marker := "[redacted:" + string(sp.kind) + "]"
		line, col := lines.at(sp.start)
		findings = append(findings, Finding{
			Kind:   sp.kind,
			Offset: sp.start,
			Length: sp.end - sp.start,
			Line:   line,
			Column: col,
		})
		b.WriteString(marker)
		prev = sp.end
	}
	b.WriteString(s[prev:])
	return b.String(), findings
}

// Clean is Apply when only the text is wanted.
func Clean(s string) string {
	out, _ := Apply(s)
	return out
}

// Has reports whether s contains anything credential-shaped.
func Has(s string) bool { return len(collect(s)) > 0 }

type span struct {
	start, end int
	kind       Kind
	rank       int // index of the matching rule; lower wins an overlap
}

func collect(s string) []span {
	if s == "" {
		return nil
	}

	var all []span
	for i, r := range rules {
		for _, m := range r.re.FindAllStringSubmatchIndex(s, -1) {
			lo, hi := m[2*r.group], m[2*r.group+1]
			if lo < 0 || hi <= lo {
				continue
			}
			all = append(all, span{start: lo, end: hi, kind: r.kind, rank: i})
		}
	}
	if len(all) == 0 {
		return nil
	}

	// Resolve overlaps: earliest start wins, then the longer span, then the
	// more specific rule. Without this, a connection string containing a JWT
	// would produce two nested replacements and corrupt the offsets.
	sort.Slice(all, func(i, j int) bool {
		if all[i].start != all[j].start {
			return all[i].start < all[j].start
		}
		if all[i].end != all[j].end {
			return all[i].end > all[j].end
		}
		return all[i].rank < all[j].rank
	})

	out := all[:0:0]
	end := -1
	for _, sp := range all {
		if sp.start < end {
			continue
		}
		out = append(out, sp)
		end = sp.end
	}
	return out
}

// lineIndex converts byte offsets to 1-based line and column numbers.
type lineIndex struct{ starts []int }

func newLineIndex(s string) lineIndex {
	starts := []int{0}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return lineIndex{starts: starts}
}

func (l lineIndex) at(offset int) (line, col int) {
	i := sort.Search(len(l.starts), func(i int) bool { return l.starts[i] > offset }) - 1
	if i < 0 {
		i = 0
	}
	return i + 1, offset - l.starts[i] + 1
}

// Summary counts findings by kind, for the one-line totals in a dry-run report.
func Summary(findings []Finding) map[Kind]int {
	if len(findings) == 0 {
		return nil
	}
	out := make(map[Kind]int, len(findings))
	for _, f := range findings {
		out[f.Kind]++
	}
	return out
}
