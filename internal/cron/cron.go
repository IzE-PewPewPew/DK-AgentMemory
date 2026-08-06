// Package cron parses five-field cron expressions and computes the next firing
// time.
//
// This exists rather than a dependency because the scheduler needs exactly one
// thing — "when does this next run" — and because expressions are parsed at
// config load. A typo in `fact_extraction_cron` should stop the server at boot
// with the offending field named, not silently never fire at 2am.
//
// Fields, in order: minute hour day-of-month month day-of-week.
// Supported syntax per field: * , - / and the shorthands @hourly, @daily,
// @weekly, @monthly, @yearly and @every <duration>.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression.
type Schedule struct {
	expr string

	minute [60]bool
	hour   [24]bool
	dom    [32]bool // 1..31
	month  [13]bool // 1..12
	dow    [7]bool  // 0..6, Sunday = 0

	// domRestricted and dowRestricted record whether the field was something
	// other than "*". Standard cron ORs the two when both are restricted --
	// "0 0 1 * 1" means the 1st of the month *or* any Monday -- which is
	// surprising but is what every other implementation does.
	domRestricted bool
	dowRestricted bool

	// every is set by the @every shorthand, which is an interval rather than a
	// calendar expression.
	every time.Duration
}

// String returns the original expression.
func (s *Schedule) String() string { return s.expr }

type fieldSpec struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteSpec = fieldSpec{"minute", 0, 59, nil}
	hourSpec   = fieldSpec{"hour", 0, 23, nil}
	domSpec    = fieldSpec{"day-of-month", 1, 31, nil}
	monthSpec  = fieldSpec{"month", 1, 12, map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	dowSpec = fieldSpec{"day-of-week", 0, 6, map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

// Parse compiles a cron expression.
func Parse(expr string) (*Schedule, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, fmt.Errorf("cron: empty expression")
	}

	if strings.HasPrefix(trimmed, "@") {
		return parseShorthand(trimmed)
	}

	fields := strings.Fields(trimmed)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: %q has %d fields, want 5 (minute hour day-of-month month day-of-week)", expr, len(fields))
	}

	s := &Schedule{expr: trimmed}
	if err := parseField(fields[0], minuteSpec, s.minute[:]); err != nil {
		return nil, err
	}
	if err := parseField(fields[1], hourSpec, s.hour[:]); err != nil {
		return nil, err
	}
	if err := parseField(fields[2], domSpec, s.dom[:]); err != nil {
		return nil, err
	}
	if err := parseField(fields[3], monthSpec, s.month[:]); err != nil {
		return nil, err
	}
	if err := parseField(fields[4], dowSpec, s.dow[:]); err != nil {
		return nil, err
	}

	s.domRestricted = fields[2] != "*"
	s.dowRestricted = fields[4] != "*"
	return s, nil
}

func parseShorthand(expr string) (*Schedule, error) {
	if rest, ok := strings.CutPrefix(expr, "@every "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return nil, fmt.Errorf("cron: %q: %w", expr, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("cron: %q: interval must be positive", expr)
		}
		return &Schedule{expr: expr, every: d}, nil
	}

	var equivalent string
	switch expr {
	case "@hourly":
		equivalent = "0 * * * *"
	case "@daily", "@midnight":
		equivalent = "0 0 * * *"
	case "@weekly":
		equivalent = "0 0 * * 0"
	case "@monthly":
		equivalent = "0 0 1 * *"
	case "@yearly", "@annually":
		equivalent = "0 0 1 1 *"
	default:
		return nil, fmt.Errorf("cron: unknown shorthand %q (want @hourly, @daily, @weekly, @monthly, @yearly or @every <duration>)", expr)
	}

	s, err := Parse(equivalent)
	if err != nil {
		return nil, err
	}
	s.expr = expr
	return s, nil
}

func parseField(field string, spec fieldSpec, bits []bool) error {
	for _, part := range strings.Split(field, ",") {
		if err := parseRange(part, spec, bits); err != nil {
			return err
		}
	}
	return nil
}

func parseRange(part string, spec fieldSpec, bits []bool) error {
	part = strings.TrimSpace(part)
	if part == "" {
		return fmt.Errorf("cron: %s: empty element", spec.name)
	}

	step := 1
	if base, stepStr, ok := strings.Cut(part, "/"); ok {
		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return fmt.Errorf("cron: %s: %q is not a positive step", spec.name, stepStr)
		}
		step = n
		part = base
		if part == "*" {
			part = fmt.Sprintf("%d-%d", spec.min, spec.max)
		}
	}

	lo, hi := spec.min, spec.max
	if part != "*" {
		loStr, hiStr, isRange := strings.Cut(part, "-")
		var err error
		if lo, err = parseValue(loStr, spec); err != nil {
			return err
		}
		if isRange {
			if hi, err = parseValue(hiStr, spec); err != nil {
				return err
			}
		} else {
			hi = lo
		}
		if lo > hi {
			return fmt.Errorf("cron: %s: range %q runs backwards", spec.name, part)
		}
	}

	for v := lo; v <= hi; v += step {
		bits[v] = true
	}
	return nil
}

func parseValue(s string, spec fieldSpec) (int, error) {
	s = strings.TrimSpace(s)
	if spec.names != nil {
		if v, ok := spec.names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("cron: %s: %q is not a number", spec.name, s)
	}
	// Sunday is conventionally writable as either 0 or 7.
	if spec.name == "day-of-week" && v == 7 {
		v = 0
	}
	if v < spec.min || v > spec.max {
		return 0, fmt.Errorf("cron: %s: %d is out of range %d-%d", spec.name, v, spec.min, spec.max)
	}
	return v, nil
}

// Next returns the first firing time strictly after t, in t's location.
//
// It returns the zero time if no match exists within five years, which happens
// only for impossible calendar dates such as "0 0 30 2 *".
func (s *Schedule) Next(t time.Time) time.Time {
	if s.every > 0 {
		return t.Add(s.every)
	}

	// Advance to the next whole minute; cron has minute resolution and a
	// schedule must never fire twice for the same minute.
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)

	for t.Before(limit) {
		if !s.month[int(t.Month())] {
			// Skip to the first day of the next month rather than stepping a
			// day at a time through February.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
			continue
		}
		if !s.hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(time.Hour)
			continue
		}
		if !s.minute[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom[t.Day()]
	dowOK := s.dow[int(t.Weekday())]

	switch {
	case s.domRestricted && s.dowRestricted:
		return domOK || dowOK
	case s.domRestricted:
		return domOK
	case s.dowRestricted:
		return dowOK
	default:
		return true
	}
}
