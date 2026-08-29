package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

func cmdChat(ctx context.Context, cfg *config.Config, st *store.Store, sessionID string) error {
	a, err := buildAgent(cfg, st)
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

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
		if err := stream(ch); err != nil {
			fmt.Fprintln(os.Stderr, "turn failed:", err)
		}
	}
}
