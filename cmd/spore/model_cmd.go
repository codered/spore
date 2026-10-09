package main

import (
	"context"
	"fmt"
	"io"

	"github.com/codered/spore/internal/modelcmd"
)

// runModelCommand is /model in the line-oriented loop: no arguments prints
// the numbered lists, "<op> <n>" chooses from them.
func runModelCommand(ctx context.Context, c *client, sessionID string, args []string, out io.Writer) error {
	op, n, err := modelcmd.Parse(args)
	if err != nil {
		return err
	}
	v, err := c.models(ctx, sessionID, false)
	if err != nil {
		return err
	}
	if op == "" {
		_, err = fmt.Fprint(out, modelcmd.Render(v))
		return err
	}
	ref, err := modelcmd.Pick(v, op, n)
	if err != nil {
		return err
	}
	if v, err = c.setModel(ctx, sessionID, op, ref); err != nil {
		return err
	}
	_, err = fmt.Fprint(out, modelcmd.Confirm(v, op))
	return err
}
