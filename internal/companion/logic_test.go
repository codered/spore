package companion_test

import (
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/store"
)

func TestGovernor(t *testing.T) {
	const start = 4
	for _, tc := range []struct {
		name     string
		budget   float64
		outcomes []string
		want     float64
	}{
		{"reply never exceeds start", 4, []string{store.OutcomeReplied}, 4},
		{"reply raises", 2, []string{store.OutcomeReplied, store.OutcomeReacted}, 2.5},
		{"ignored lowers", 4, []string{store.OutcomeIgnored, store.OutcomeIgnored}, 3},
		{"stop halves", 4, []string{store.OutcomeStop}, 2},
		{"floor holds", 1.2, []string{store.OutcomeIgnored, store.OutcomeStop}, 1},
		{"unanswered changes nothing", 3, []string{store.OutcomeUnanswered, store.OutcomePending}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.budget
			for _, o := range tc.outcomes {
				b = companion.ApplyOutcome(b, o, start)
			}
			if b != tc.want {
				t.Fatalf("budget = %v, want %v", b, tc.want)
			}
		})
	}
	if got := companion.ApplyFeedback(3, companion.FeedbackMore, start); got != 4 {
		t.Errorf("more from 3 = %v, want 4", got)
	}
	if got := companion.ApplyFeedback(4, companion.FeedbackMore, start); got != 4 {
		t.Errorf("more from 4 = %v, want 4 (capped)", got)
	}
	if got := companion.ApplyFeedback(1.5, companion.FeedbackLess, start); got != 1 {
		t.Errorf("less from 1.5 = %v, want 1 (floor)", got)
	}
	if got := companion.ApplyFeedback(4, companion.FeedbackStop, start); got != 2 {
		t.Errorf("stop from 4 = %v, want 2", got)
	}
}

func TestQuietHours(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 12, h, m, 0, 0, la) }
	for _, tc := range []struct {
		spec string
		now  time.Time
		want bool
	}{
		{"22:00-08:00", at(23, 30), true},
		{"22:00-08:00", at(2, 0), true},
		{"22:00-08:00", at(7, 59), true},
		{"22:00-08:00", at(8, 0), false},
		{"22:00-08:00", at(12, 0), false},
		{"22:00-08:00", at(22, 0), true},
		{"13:00-14:00", at(13, 30), true},
		{"13:00-14:00", at(14, 0), false},
	} {
		// The clock is read in la even when now arrives in UTC.
		got, err := companion.InQuietHours(tc.now.UTC(), la, tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s at %s = %v, want %v", tc.spec, tc.now.Format("15:04"), got, tc.want)
		}
	}
	if _, err := companion.InQuietHours(at(1, 0), la, "nonsense"); err == nil {
		t.Error("a bad spec must be an error")
	}
	start := companion.LocalDayStart(at(1, 30).UTC(), la)
	if !start.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, la)) {
		t.Errorf("day start = %v", start)
	}
}

func TestValidateWatch(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	ok := companion.Watch{Cron: "0 14 * * 1-5", Goal: "Check ZS; alert on a daily move over 5%."}
	if err := companion.ValidateWatch(ok, now); err != nil {
		t.Fatalf("valid watch: %v", err)
	}
	if err := companion.ValidateWatch(companion.Watch{Cron: "0 * * * *", Goal: "g"}, now); err != nil {
		t.Fatalf("hourly is allowed: %v", err)
	}
	for _, tc := range []struct {
		name string
		w    companion.Watch
		want string
	}{
		{"every 30 min", companion.Watch{Cron: "*/30 * * * *", Goal: "g"}, "more than once an hour"},
		{"twice in one hour once a week", companion.Watch{Cron: "0,30 9 * * 1", Goal: "g"}, "more than once an hour"},
		{"bad cron", companion.Watch{Cron: "every day", Goal: "g"}, "cron"},
		{"one-off", companion.Watch{Cron: "2026-11-01T10:00:00Z", Goal: "g"}, "recurring"},
		{"empty goal", companion.Watch{Cron: "0 14 * * *", Goal: "  "}, "goal"},
		{"long goal", companion.Watch{Cron: "0 14 * * *", Goal: strings.Repeat("x", 501)}, "goal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := companion.ValidateWatch(tc.w, now)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if p := companion.WatchPrompt("Zscaler (ZS)", " Check ZS. "); !strings.Contains(p, "Zscaler (ZS)") || !strings.Contains(p, "Goal: Check ZS.") {
		t.Errorf("prompt = %q", p)
	}
}
