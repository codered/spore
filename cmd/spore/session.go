package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

func cmdSession(ctx context.Context, st *store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("session needs a subcommand: list or show")
	}
	switch args[0] {
	case "list":
		sessions, err := st.ListSessions(ctx, 50)
		if err != nil {
			return err
		}
		for _, s := range sessions {
			fmt.Printf("%s  %s  %s\n", s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), s.Title)
		}
		return nil
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("session show needs a session id")
		}
		msgs, err := st.Messages(ctx, args[1])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			var blocks []provider.Block
			if err := json.Unmarshal(m.BlocksJSON, &blocks); err != nil {
				return err
			}
			fmt.Printf("\n[%d %s]\n", m.Seq, m.Role)
			for _, b := range blocks {
				switch b.Type {
				case provider.BlockText:
					fmt.Println(b.Text)
				case provider.BlockToolUse:
					fmt.Printf("→ %s %s\n", b.Name, string(b.Input))
				case provider.BlockToolResult:
					fmt.Printf("← %s\n", b.Content)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown session subcommand %q", args[0])
	}
}
