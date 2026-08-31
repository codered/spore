package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

func cmdChat(ctx context.Context, cfg *config.Config, st *store.Store, sessionID string) error {
	a, err := buildAgent(cfg, st, terminalApprover{lines: stdinLines, out: os.Stdout})
	if err != nil {
		return err
	}
	if sessionID == "" {
		sessionID, err = st.CreateSession(ctx, "chat")
		if err != nil {
			return err
		}
	}
	fmt.Printf("session %s — ctrl-d to exit\n", sessionID)

	ctx = policy.WithSession(ctx, sessionID, policy.ProfileLocal)
	sc := stdinLines
	for {
		fmt.Print("\n> ")
		if !sc.Scan() {
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ch, err := a.Run(ctx, sessionID, line)
		if err != nil {
			return err
		}
		if err := stream(ch, cfg.ShowCost); err != nil {
			fmt.Fprintln(os.Stderr, "turn failed:", err)
		}
	}
}
