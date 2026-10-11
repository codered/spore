package companion

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codered/spore/internal/scheduler"
)

// Watch is a proposal's watch: when the job runs and what it checks.
type Watch struct {
	Cron string `json:"cron"`
	Goal string `json:"goal"`
}

// cronProbe is how many firings ValidateCron inspects. Every sub-hour gap a
// five-field cron can have shows up within its first day of firings, and a
// weekly pattern's first occurrence is reached well inside fifty.
const cronProbe = 50

// ValidateCron checks a watch schedule: a recurring five-field cron the
// scheduler can parse (it runs in UTC), firing at most once an hour.
func ValidateCron(spec string, now time.Time) error {
	sched, err := scheduler.Parse(spec)
	if err != nil {
		return fmt.Errorf("watch cron %q: %w", spec, err)
	}
	if sched.Kind() != "cron" {
		return fmt.Errorf("watch cron %q must be a recurring five-field cron, not a one-off time", spec)
	}
	prev := sched.Next(now.UTC())
	if prev.IsZero() {
		return fmt.Errorf("watch cron %q never fires", spec)
	}
	for i := 0; i < cronProbe; i++ {
		next := sched.Next(prev)
		if next.IsZero() {
			break
		}
		if next.Sub(prev) < time.Hour {
			return fmt.Errorf("watch cron %q fires more than once an hour", spec)
		}
		prev = next
	}
	return nil
}

// ValidateGoal checks what a watch is told to do: 1..500 characters.
func ValidateGoal(goal string) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(goal)); n < 1 || n > 500 {
		return fmt.Errorf("watch goal must be 1..500 characters, got %d", n)
	}
	return nil
}

// ValidateWatch checks a whole watch.
func ValidateWatch(w Watch, now time.Time) error {
	if err := ValidateGoal(w.Goal); err != nil {
		return err
	}
	return ValidateCron(w.Cron, now)
}

// WatchPrompt is what a watch job runs: the goal, framed so the run knows it
// is unattended and read-only, and that "nothing to report" is an answer.
func WatchPrompt(label, goal string) string {
	return fmt.Sprintf("You are running a watch the user approved, on %s. Goal: %s\n\n"+
		"You run unattended, with read-only tools and your own notes. Report what you found in a few lines. "+
		"If nothing crosses the goal's bar, say so in one line. If you need a tool you do not have, say which and why.",
		label, strings.TrimSpace(goal))
}
