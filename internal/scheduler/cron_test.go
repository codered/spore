package scheduler

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string) Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	return s
}

func TestParseCronNext(t *testing.T) {
	from := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC) // a Tuesday
	cases := []struct {
		spec string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 9, 1, 8, 31, 0, 0, time.UTC)},
		{"0 9 * * *", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)},
		{"0 8 * * *", time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 9, 1, 8, 45, 0, 0, time.UTC)},
		{"0 9 * * 1", time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)},   // next Monday
		{"30 6 1 * *", time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)}, // first of next month
		{"0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},  // next leap day
		{"0 9,17 * * *", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)},
		{"0 0 * * 0", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)},   // Sunday as 0
		{"0 0 * * 7", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)},   // Sunday as 7
		// Range forms
		{"35-40 * * * *", time.Date(2026, 9, 1, 8, 35, 0, 0, time.UTC)},       // range in current hour
		{"35-40/2 * * * *", time.Date(2026, 9, 1, 8, 35, 0, 0, time.UTC)},     // 35,37,39 -> first is 35
		{"0 10-12 * * *", time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},       // hours 10-12
		{"0 18-23 * * *", time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)},       // hours 18-23
		{"0 20-23/2 * * *", time.Date(2026, 9, 1, 20, 0, 0, 0, time.UTC)},     // 20,22
		// Day-of-month + day-of-week interactions
		{"0 0 1,15 * 1", time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)},        // next Monday (OR rule: both restricted)
		{"0 0 15 * *", time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)},         // only DOM restricted
		{"0 0 * * 1", time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)},           // only DOW restricted
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			s := mustParse(t, tc.spec)
			if s.Kind() != "cron" {
				t.Errorf("Kind() = %q, want cron", s.Kind())
			}
			if got := s.Next(from); !got.Equal(tc.want) {
				t.Errorf("Next(%v) = %v, want %v", from, got, tc.want)
			}
		})
	}
}

func TestNextIsStrictlyAfterTheGivenTime(t *testing.T) {
	// A schedule that matches "now" must return the NEXT match, or the tick
	// loop fires the same job forever.
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	s := mustParse(t, "0 9 * * *")
	if got := s.Next(at); !got.Equal(at.Add(24 * time.Hour)) {
		t.Errorf("Next(exact match) = %v, want tomorrow", got)
	}
}

func TestParseOneShot(t *testing.T) {
	s := mustParse(t, "2026-12-25T09:00:00Z")
	if s.Kind() != "once" {
		t.Errorf("Kind() = %q, want once", s.Kind())
	}
	before := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, 12, 25, 9, 0, 0, 0, time.UTC)
	if got := s.Next(before); !got.Equal(want) {
		t.Errorf("Next(before) = %v, want %v", got, want)
	}
	// After it has fired there is no next run; the zero time is how a
	// schedule says "retire me".
	if got := s.Next(want); !got.IsZero() {
		t.Errorf("Next(at or after the instant) = %v, want the zero time", got)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, spec := range []string{
		"", "  ", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 8", "a * * * *",
		"*/0 * * * *", "5-1 * * * *", "2026-12-25", "not-a-time",
	} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", spec)
		}
	}
}
