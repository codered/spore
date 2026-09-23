package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/store"
)

const sessionDeleteUsage = "usage: spore session delete <id>... | --all  [--discord] [--yes]"

// runSessionDelete is `spore session delete`: through the daemon when one is
// running (so a running turn is refused, open clients drop the session, and
// the Discord bridge can clean up), straight against the store otherwise.
func runSessionDelete(ctx context.Context, cfg *config.Config, st *store.Store, args []string, in io.Reader, out io.Writer) error {
	c := newClient(cfg.Daemon.Addr)
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := c.health(probe)
	cancel()
	if err != nil {
		c = nil
	}
	return sessionDelete(ctx, st, c, args, in, out)
}

// sessionDelete does the work; c is nil when no daemon answers.
func sessionDelete(ctx context.Context, st *store.Store, c *client, args []string, in io.Reader, out io.Writer) error {
	var ids []string
	all, discord, yes := false, false, false
	for _, a := range args {
		switch a {
		case "--all":
			all = true
		case "--discord":
			discord = true
		case "--yes", "-y":
			yes = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %s\n%s", a, sessionDeleteUsage)
			}
			ids = append(ids, a)
		}
	}
	if all == (len(ids) > 0) {
		return fmt.Errorf("name session ids or --all, not both or neither\n%s", sessionDeleteUsage)
	}

	if !yes {
		what := fmt.Sprintf("%d session(s) and any sub-agents they launched", len(ids))
		if all {
			what = "EVERY session"
		}
		if discord {
			what += ", and their copies on Discord"
		}
		fmt.Fprintf(out, "delete %s? This cannot be undone. type yes to go ahead: ", what)
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return errors.New("cancelled: nothing was deleted")
		}
	}

	var res daemon.DeleteSessionsJSON
	if c != nil {
		body := map[string]any{"ids": ids, "all": all, "discord": discord}
		if err := c.do(ctx, "POST", "/api/sessions/delete", body, &res); err != nil {
			return err
		}
	} else {
		var err error
		if all {
			res.Deleted, err = st.DeleteAllSessions(ctx)
		} else {
			res.Deleted, err = st.DeleteSessions(ctx, ids)
		}
		if err != nil {
			return err
		}
		if discord {
			res.Notes = append(res.Notes, "Discord was not touched: the daemon is not running, so the bridge is not connected")
		}
	}
	noun := "sessions"
	if len(res.Deleted) == 1 {
		noun = "session"
	}
	fmt.Fprintf(out, "deleted %d %s\n", len(res.Deleted), noun)
	for _, n := range res.Notes {
		fmt.Fprintln(out, "  "+n)
	}
	return nil
}
