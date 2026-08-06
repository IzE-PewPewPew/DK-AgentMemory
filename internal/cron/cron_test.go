package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestNext(t *testing.T) {
	base := time.Date(2026, 8, 6, 14, 30, 0, 0, time.UTC)

	cases := []struct {
		expr string
		want time.Time
	}{
		// The two schedules that actually ship in config.example.yaml.
		{"0 2 * * *", time.Date(2026, 8, 7, 2, 0, 0, 0, time.UTC)},
		{"0 3 * * 0", time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)}, // next Sunday
		{"*/15 * * * *", time.Date(2026, 8, 6, 14, 45, 0, 0, time.UTC)},
		{"0 * * * *", time.Date(2026, 8, 6, 15, 0, 0, 0, time.UTC)},
		{"30 14 * * *", time.Date(2026, 8, 7, 14, 30, 0, 0, time.UTC)}, // strictly after
		{"0 0 1 * *", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{"0 0 1 1 *", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"@daily", time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)},
		{"@weekly", time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)},
		{"0 0 * * mon", time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)},
		{"0 0 1 feb *", time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)},
	}

	for _, tc := range cases {
		got := mustParse(t, tc.expr).Next(base)
		if !got.Equal(tc.want) {
			t.Errorf("%q: got %s, want %s", tc.expr, got, tc.want)
		}
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	// A scheduler that returns the current instant fires in a hot loop.
	s := mustParse(t, "* * * * *")
	now := time.Date(2026, 8, 6, 14, 30, 0, 0, time.UTC)
	next := s.Next(now)
	if !next.After(now) {
		t.Fatalf("Next(%s) = %s, which is not after now", now, next)
	}
	if d := next.Sub(now); d != time.Minute {
		t.Fatalf("expected the next minute, got %s later", d)
	}
}

func TestDomAndDowAreOred(t *testing.T) {
	// Standard cron semantics: with both day fields restricted, either matching
	// is enough. Getting this backwards makes a weekly job fire monthly.
	s := mustParse(t, "0 0 1 * 1")
	// 2026-08-06 is a Thursday. Next Monday is the 10th; the 1st of September
	// is sooner than that? No -- Monday the 10th comes first.
	got := s.Next(time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC))
	want := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestEvery(t *testing.T) {
	s := mustParse(t, "@every 15m")
	base := time.Date(2026, 8, 6, 14, 30, 0, 0, time.UTC)
	if got := s.Next(base); !got.Equal(base.Add(15 * time.Minute)) {
		t.Fatalf("got %s", got)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	bad := []string{
		"",
		"0 2 * *",      // four fields
		"0 2 * * * *",  // six fields
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"0 0 32 * *",   // day out of range
		"0 0 * 13 *",   // month out of range
		"0 0 * * 8",    // weekday out of range
		"*/0 * * * *",  // zero step
		"5-1 * * * *",  // backwards range
		"@fortnightly", // unknown shorthand
		"@every",       // missing duration
		"@every -5m",   // negative interval
		"abc * * * *",  // not a number
	}
	for _, expr := range bad {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) should have failed", expr)
		}
	}
}

func TestImpossibleDateReturnsZero(t *testing.T) {
	// 30 February never occurs. Returning the zero time lets the scheduler log
	// "this will never fire" instead of spinning.
	s := mustParse(t, "0 0 30 2 *")
	if got := s.Next(time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Fatalf("expected zero time, got %s", got)
	}
}
