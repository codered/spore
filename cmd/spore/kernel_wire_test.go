package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/kernel"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/recall/sqlitefts"
	skillfiles "github.com/codered/spore/internal/skill"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/subagent"
)

// go_run re-executes the running binary as its kernel child. Under go test
// that binary is this test binary, so it must do what main does first.
func TestMain(m *testing.M) {
	if kernel.IsChild() {
		os.Exit(kernel.ChildMain())
	}
	os.Exit(m.Run())
}

// go_run must be registered AND bound to the guard, not to the raw
// registry: a program reading .env through spore.ReadFile has to meet the
// baseline deny exactly as a direct fs_read would.
func TestGoRunIsRegisteredAndBoundToTheGuard(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_model = "p/m"
data_dir = "`+dir+`"

[providers.p]
kind = "anthropic"
api_key = "x"

[policy]
workspace = "`+dir+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sid, err := st.CreateSession(context.Background(), "kernel wire", "")
	if err != nil {
		t.Fatal(err)
	}

	facts := memory.NewCache(filepath.Join(dir, "memory"))
	sup := subagent.New(st, cfg.Subagents)
	guard, host, err := buildTools(cfg, st, facts, sqlitefts.New(st.DB()), skillfiles.NewCaches(), sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != nil {
		defer host.Close()
	}

	code := `package main

import (
	"fmt"
	"spore"
)

func main() {
	_, err := spore.ReadFile(".env")
	fmt.Println("env error:", err != nil)
}`
	args, _ := json.Marshal(map[string]string{"code": code})
	ctx := policy.WithSession(context.Background(), policy.Session{ID: sid, Profile: policy.ProfileLocal, Workspace: dir})
	res := guard.Run(ctx, provider.Block{Type: provider.BlockToolUse, ID: "c1", Name: kernel.ToolName, Input: args})
	if res.IsError {
		t.Fatalf("go_run failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "env error: true") || !strings.Contains(res.Content, "fs_read error") {
		t.Errorf("reading .env from a program was not denied:\n%s", res.Content)
	}
}
