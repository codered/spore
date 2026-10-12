package main

import (
	"context"
	"testing"

	"github.com/codered/spore/internal/config"
)

func TestCompanionCheckRejectsBadFlags(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Daemon.Addr = "127.0.0.1:1"
	for _, args := range [][]string{{"--force"}, {"extra"}, {"--nope"}} {
		if err := companionCheck(context.Background(), cfg, args); err == nil {
			t.Errorf("check %v: accepted", args)
		}
	}
}
