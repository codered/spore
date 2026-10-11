package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// cmdCompanion is the operator's view of the companion. Everything but
// "answer" reads and writes the database directly, like spore recall, so it
// works with or without a running daemon; "answer" goes through the daemon
// so the answer is posted where the proposal was.
func cmdCompanion(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore companion status | interests | outreach | pause [duration] | resume | answer <id> yes|not-now|never | interests undecline <key>")
	}
	if args[0] == "answer" {
		return companionAnswer(ctx, cfg, args[1:])
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return companionCommand(ctx, os.Stdout, cfg, st, args)
}

// companionCommand runs every subcommand that needs only the store.
func companionCommand(ctx context.Context, w io.Writer, cfg *config.Config, st *store.Store, args []string) error {
	loc, err := cfg.Companion.Location()
	if err != nil {
		return err
	}
	svc := companion.NewService(st, cfg)
	switch args[0] {
	case "status":
		return companionStatus(ctx, w, cfg, st)
	case "interests":
		if len(args) == 3 && args[1] == "undecline" {
			if err := svc.Undecline(ctx, args[2]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "%s is observed again\n", args[2])
			return nil
		}
		if len(args) > 1 {
			return fmt.Errorf("usage: spore companion interests [undecline <key>]")
		}
		return companionInterests(ctx, w, st, loc)
	case "outreach":
		return companionOutreach(ctx, w, st, loc)
	case "pause":
		d := 24 * time.Hour
		if len(args) > 1 {
			if d, err = time.ParseDuration(args[1]); err != nil {
				return fmt.Errorf("pause duration %q: %w (try 2h or 72h)", args[1], err)
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
		return fmt.Errorf("unknown companion command %q: want status, interests, outreach, pause, resume or answer", args[0])
	}
}

func companionStatus(ctx context.Context, w io.Writer, cfg *config.Config, st *store.Store) error {
	c := cfg.Companion
	state := "disabled"
	if c.Enabled {
		state = "enabled"
	}
	loc, err := c.Location()
	if err != nil {
		return err
	}
	s, err := companion.NewService(st, cfg).Status(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "companion: %s\n", state)
	if !s.PausedUntil.IsZero() && time.Now().Before(s.PausedUntil) {
		_, _ = fmt.Fprintf(w, "           paused until %s\n", s.PausedUntil.In(loc).Format("Mon 2006-01-02 15:04"))
	}
	_, _ = fmt.Fprintf(w, "channel:   %s\n", s.Channel)
	_, _ = fmt.Fprintf(w, "timezone:  %s (habit_days %d, quiet %s)\n", loc, c.HabitDays, c.QuietHours)
	_, _ = fmt.Fprintf(w, "budget:    %.2f a day, %d of %d sent today\n", s.Budget, s.SpentToday, int(s.Budget))
	last := "never"
	switch {
	case !s.LastReflection.IsZero():
		last = s.LastReflection.In(loc).Format("Mon 15:04") + ": " + s.LastResult
	case s.LastResult != "":
		last = s.LastResult
	}
	_, _ = fmt.Fprintf(w, "reflected: %s\n", last)
	if s.Failures >= companion.FailureWarning {
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

// companionOutreach lists the last week's outreach, oldest first.
func companionOutreach(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error {
	rows, err := st.OutreachSince(ctx, time.Now().Add(-companion.RecentOutreach))
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

// companionAnswer presses a proposal's button from the terminal.
func companionAnswer(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: spore companion answer <outreach id> yes|not-now|never")
	}
	if _, err := strconv.ParseInt(args[0], 10, 64); err != nil {
		return fmt.Errorf("outreach id %q must be a number", args[0])
	}
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	var out struct {
		Message string `json:"message"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/companion/proposals/"+args[0], map[string]string{"choice": args[1]}, &out); err != nil {
		return err
	}
	fmt.Println(out.Message)
	return nil
}

func companionInterests(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error {
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
