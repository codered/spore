package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/codered/spore/internal/policy"
)

// terminalApprover asks on the terminal. It is the CLI's implementation of
// policy.Approver; the daemon and the Telegram bridge implement the same
// interface over SSE and inline keyboards in Plans 3 and 4.
type terminalApprover struct {
	// lines is the shared stdin scanner (see input.go). The chat loop reads
	// from the same one: two scanners over one file descriptor would each
	// buffer ahead and swallow the other's input.
	lines *bufio.Scanner
	out   io.Writer
}

func (t terminalApprover) Ask(ctx context.Context, a policy.Ask) (policy.Answer, error) {
	args := string(a.Args)
	if pretty, err := json.MarshalIndent(json.RawMessage(a.Args), "  ", "  "); err == nil {
		args = string(pretty)
	}
	fmt.Fprintf(t.out, "\n\033[1mspore wants to run %s\033[0m  (matched policy rule %q)\n  %s\n", a.Tool, a.Rule, args)

	sc := t.lines
	for {
		fmt.Fprintf(t.out, "allow? [y]es once  [n]o  [s]ession (always %s this session)  [p]attern (always %s)\n> ",
			a.Tool, a.Pattern)
		// Honour cancellation between prompts: an approval that timed out
		// upstream should not keep a dead prompt on screen.
		select {
		case <-ctx.Done():
			return policy.Answer{}, ctx.Err()
		default:
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return policy.Answer{}, err
			}
			// No terminal to ask: deny rather than assume consent.
			fmt.Fprintln(t.out, "no input available; denying")
			return policy.Answer{}, errors.New("no input available to answer the approval request")
		}
		switch strings.ToLower(strings.TrimSpace(sc.Text())) {
		case "y", "yes":
			return policy.Answer{Allow: true, Scope: policy.ScopeOnce}, nil
		case "n", "no":
			return policy.Answer{Allow: false, Scope: policy.ScopeOnce}, nil
		case "s", "session":
			return policy.Answer{Allow: true, Scope: policy.ScopeSession}, nil
		case "p", "pattern":
			return policy.Answer{Allow: true, Scope: policy.ScopePattern}, nil
		default:
			fmt.Fprintln(t.out, "please answer y, n, s or p")
		}
	}
}
