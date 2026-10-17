package companion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codered/spore/internal/store"
)

// ErrUsage marks a companion command line Run cannot run: an unknown
// subcommand or a malformed argument. The daemon answers it with 400.
var ErrUsage = errors.New("usage")

// Usage lists what a companion command line may say, in the CLI and in chat.
const Usage = "status | interests | interests undecline <key> | outreach | pause [duration] | resume | answer <id> yes|not-now|never"

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, a...))
}

// Run runs one companion subcommand that needs only the store and writes
// its text to w. `spore companion` and the daemon's /companion both call it,
// so the terminal and every chat surface read the same. No arguments means
// status. answer and check need the daemon and are handled by the callers.
func Run(ctx context.Context, w io.Writer, svc *Service, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	st := svc.Store
	loc, err := svc.Cfg.Companion.Location()
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		return writeStatus(ctx, w, svc)
	case "interests":
		if len(args) == 3 && args[1] == "undecline" {
			if err := svc.Undecline(ctx, args[2]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "%s is observed again\n", args[2])
			return nil
		}
		if len(args) > 1 {
			return usageErr("interests [undecline <key>]")
		}
		return writeInterests(ctx, w, st, loc)
	case "outreach":
		return writeOutreach(ctx, w, st, loc)
	case "pause":
		d := 24 * time.Hour
		if len(args) > 1 {
			if d, err = time.ParseDuration(args[1]); err != nil {
				return usageErr("pause duration %q: %v (try 2h or 72h)", args[1], err)
			}
		}
		until, err := svc.Pause(ctx, d)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "paused until %s\n", until.In(loc).Format("Mon 2006-01-02 15:04"))
		return nil
	case "resume":
		if err := svc.Resume(ctx); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, "resumed")
		return nil
	default:
		return usageErr("unknown companion command %q: want %s", args[0], Usage)
	}
}

func writeStatus(ctx context.Context, w io.Writer, svc *Service) error {
	cfg, st := svc.Cfg, svc.Store
	c := cfg.Companion
	state := "disabled"
	if c.Enabled {
		state = "enabled"
	}
	loc, err := c.Location()
	if err != nil {
		return err
	}
	s, err := svc.Status(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "companion: %s\n", state)
	paused := !s.PausedUntil.IsZero() && svc.Now().Before(s.PausedUntil)
	if paused {
		_, _ = fmt.Fprintf(w, "           paused until %s\n", s.PausedUntil.In(loc).Format("Mon 2006-01-02 15:04"))
	}
	_, _ = fmt.Fprintf(w, "channel:   %s\n", s.Channel)
	_, _ = fmt.Fprintf(w, "timezone:  %s (habit_days %d, quiet %s)\n", loc, c.HabitDays, c.QuietHours)
	_, _ = fmt.Fprintf(w, "budget:    %.2f a day, %d of %d sent today\n", s.Budget, s.SpentToday, int(s.Budget))
	_, _ = fmt.Fprintf(w, "digest:    %s\n", digestState(s, paused, loc))
	research := fmt.Sprintf("%d of %d today", s.ResearchToday, ResearchPerDay)
	if s.ResearchRunning > 0 {
		research += fmt.Sprintf(", %d running", s.ResearchRunning)
	}
	_, _ = fmt.Fprintf(w, "research:  %s\n", research)
	alerts := fmt.Sprintf("%d of %d today", s.AlertsToday, s.AlertBudget)
	if s.AlertsHeld > 0 {
		alerts += fmt.Sprintf(", %d held", s.AlertsHeld)
	}
	_, _ = fmt.Fprintf(w, "alerts:    %s\n", alerts)
	last := "never"
	switch {
	case !s.LastReflection.IsZero():
		last = s.LastReflection.In(loc).Format("Mon 15:04") + ": " + s.LastResult
	case s.LastResult != "":
		last = s.LastResult
	}
	_, _ = fmt.Fprintf(w, "reflected: %s\n", last)
	if s.Failures >= FailureWarning {
		_, _ = fmt.Fprintf(w, "warning: the last %d reflections failed; see the daemon log\n", s.Failures)
	}
	size := 0
	if fi, err := os.Stat(cfg.SelfPath()); err == nil { //nolint:gosec // G703: path is the daemon's own self.md under DataDir, not user input
		size = int(fi.Size())
	}
	_, _ = fmt.Fprintf(w, "self.md:   %s, %d of %d bytes\n", cfg.SelfPath(), size, c.SelfMaxBytes)
	all, err := st.Interests(ctx)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, in := range all {
		counts[in.State]++
	}
	_, _ = fmt.Fprint(w, "interests:")
	for _, s := range []string{store.InterestObserving, store.InterestCandidate, store.InterestProposed,
		store.InterestActive, store.InterestDeclined, store.InterestRetired} {
		_, _ = fmt.Fprintf(w, " %s %d", s, counts[s])
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

// digestState says where today's digest stands. Paused wins over the rest.
func digestState(s Status, paused bool, loc *time.Location) string {
	switch {
	case paused:
		return "paused"
	case !s.DigestSentAt.IsZero():
		return "sent " + s.DigestSentAt.In(loc).Format("15:04")
	case s.DigestDue:
		return "due"
	default:
		return "none today"
	}
}

// writeOutreach lists the last week's outreach, oldest first.
func writeOutreach(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error {
	rows, err := st.OutreachSince(ctx, time.Now().Add(-RecentOutreach))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "no outreach in the last 7 days")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tOUTCOME\tCHANNEL\tSENT\tTEXT")
	for _, o := range rows {
		text := strings.Join(strings.Fields(o.Text), " ")
		if r := []rune(text); len(r) > 60 {
			text = string(r[:60]) + "…"
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", o.ID, o.Kind, o.Outcome, o.Channel,
			o.SentAt.In(loc).Format("Mon 15:04"), text)
	}
	return tw.Flush()
}

func writeInterests(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error {
	all, err := st.Interests(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		_, _ = fmt.Fprintln(w, "no interests recorded yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KEY\tSTATE\tDAYS\tLAST SEEN\tLABEL")
	for _, in := range all {
		last := "-"
		if !in.LastSeen.IsZero() {
			last = in.LastSeen.In(loc).Format("2006-01-02")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", in.Key, in.State, in.DaysSeen, last, in.Label)
	}
	return tw.Flush()
}
