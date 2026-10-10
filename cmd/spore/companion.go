package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// cmdCompanion is the operator's view of what the companion has noticed.
// It reads the database directly, like spore recall, so it works with or
// without a running daemon.
func cmdCompanion(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore companion status | interests")
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	switch args[0] {
	case "status":
		return companionStatus(ctx, os.Stdout, cfg, st)
	case "interests":
		loc, err := cfg.Companion.Location()
		if err != nil {
			return err
		}
		return companionInterests(ctx, os.Stdout, st, loc)
	default:
		return fmt.Errorf("unknown companion command %q: want status or interests", args[0])
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
	_, _ = fmt.Fprintf(w, "companion: %s\n", state)
	_, _ = fmt.Fprintf(w, "timezone:  %s (habit_days %d)\n", loc, c.HabitDays)
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
