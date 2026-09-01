package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/codered/spore/internal/policy"
)

// terminalApprover asks on the terminal. It is the CLI's implementation of
// policy.Approver; the daemon implements it over SSE in Plan 3; the Discord
// bridge answers through Guard.Resolve rather than implementing this interface at all.
type terminalApprover struct {
	// lines is the source of user input (see input.go). Exactly one goroutine
	// may read from it — that is the whole point. In `once`, it is the stream
	// goroutine. In `chat`, it is the main loop.
	lines lineSource
	out   io.Writer
}

func (t terminalApprover) Ask(ctx context.Context, a policy.Ask) (policy.Answer, error) {
	args := string(a.Args)
	if pretty, err := json.MarshalIndent(json.RawMessage(a.Args), "  ", "  "); err == nil {
		args = string(pretty)
	}
	fmt.Fprintf(t.out, "\n\033[1mspore wants to run %s\033[0m  (matched policy rule %q)\n  %s\n", a.Tool, a.Rule, args)

	for {
		// An empty Pattern means the guard found no pattern to generalise to,
		// so the option is not offered. Do not print a key the user can press
		// that would silently be treated as "once".
		if a.Pattern == "" {
			fmt.Fprintf(t.out, "allow? [y]es once  [n]o  [s]ession (always %s this session)\n> ", a.Tool)
		} else {
			fmt.Fprintf(t.out, "allow? [y]es once  [n]o  [s]ession (always %s this session)  [p]attern (always %s)\n> ",
				a.Tool, a.Pattern)
		}
		// Honour cancellation between prompts: an approval that timed out
		// upstream should not keep a dead prompt on screen.
		select {
		case <-ctx.Done():
			return policy.Answer{}, ctx.Err()
		default:
		}
		line, ok := t.lines.ReadLine()
		if !ok {
			// No terminal to ask: deny rather than assume consent.
			fmt.Fprintln(t.out, "no input available; denying")
			return policy.Answer{}, errors.New("no input available to answer the approval request")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return policy.Answer{Allow: true, Scope: policy.ScopeOnce}, nil
		case "n", "no":
			return policy.Answer{Allow: false, Scope: policy.ScopeOnce}, nil
		case "s", "session":
			return policy.Answer{Allow: true, Scope: policy.ScopeSession}, nil
		case "p", "pattern":
			if a.Pattern == "" {
				fmt.Fprintln(t.out, "there is no pattern to generalise this call to; answer y, n or s")
				continue
			}
			return policy.Answer{Allow: true, Scope: policy.ScopePattern}, nil
		default:
			fmt.Fprintln(t.out, "please answer y, n, s or p")
		}
	}
}
