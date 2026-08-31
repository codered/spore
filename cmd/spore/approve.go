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
// policy.Approver; the daemon and the Telegram bridge implement the same
// interface over SSE and inline keyboards in Plans 3 and 4.
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
		fmt.Fprintf(t.out, "allow? [y]es once  [n]o  [s]ession (always %s this session)  [p]attern (always %s)\n> ",
			a.Tool, a.Pattern)
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
			return policy.Answer{Allow: true, Scope: policy.ScopePattern}, nil
		default:
			fmt.Fprintln(t.out, "please answer y, n, s or p")
		}
	}
}
