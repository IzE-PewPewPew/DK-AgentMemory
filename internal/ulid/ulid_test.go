package ulid

import (
	"sort"
	"testing"
	"time"
)

func TestNewIsValidAndSortable(t *testing.T) {
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = New()
		if err := Validate(ids[i]); err != nil {
			t.Fatalf("New() produced invalid ULID %q: %v", ids[i], err)
		}
	}

	// Monotonic within a millisecond is the property the offline write queue
	// depends on, so assert it rather than assuming it.
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("IDs are not lexicographically ordered by creation at index %d", i)
		}
	}

	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate ULID %q", id)
		}
		seen[id] = true
	}
}

func TestTimeRoundTrip(t *testing.T) {
	want := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	got, err := Time(NewAt(want))
	if err != nil {
		t.Fatalf("Time: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("round trip: got %s, want %s", got, want)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"too short":          "01J",
		"too long":           "01JQWERTYUIOPASDFGHJKLZXCVB1",
		"illegal letter I":   "01JQWERTYUIOPASDFGHJKLZXCI",
		"illegal letter U":   "01JQWERTYUIOPASDFGHJKLZXCU",
		"timestamp overflow": "81JQWERTYUIOPASDFGHJKLZXCV",
		"empty":              "",
	}
	for name, in := range cases {
		if err := Validate(in); err == nil {
			t.Errorf("%s: expected %q to be rejected", name, in)
		}
	}
}

func TestValidateAcceptsLowercase(t *testing.T) {
	// Crockford base32 is case-insensitive on decode. A ULID retyped by hand in
	// lowercase must still resolve rather than 404.
	id := New()
	lower := ""
	for _, c := range id {
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		lower += string(c)
	}
	if err := Validate(lower); err != nil {
		t.Fatalf("lowercase ULID rejected: %v", err)
	}
}
