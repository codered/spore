package main

import (
	"context"
	"encoding/json"
	"fmt"
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
// kernelGuard builds the real tool stack for a workspace at dir and returns
// the guard go_run is bound to, with a session context to call it under.
func kernelGuard(t *testing.T, dir string) (*policy.Guard, context.Context) {
	t.Helper()
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
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sid, err := st.CreateSession(context.Background(), "kernel wire", "")
	if err != nil {
		t.Fatal(err)
	}

	facts := memory.NewCache(filepath.Join(dir, "memory"))
	sup := subagent.New(st, cfg.Subagents)
	guard, host, err := buildTools(cfg, st, facts, sqlitefts.New(st.DB()), skillfiles.NewCaches(), sup, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != nil {
		t.Cleanup(host.Close)
	}
	return guard, policy.WithSession(context.Background(), policy.Session{ID: sid, Profile: policy.ProfileLocal, Workspace: dir})

}

// runProgram runs code through go_run on the guard and returns its output.
func runProgram(t *testing.T, guard *policy.Guard, ctx context.Context, code string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"code": code})
	res := guard.Run(ctx, provider.Block{Type: provider.BlockToolUse, ID: "c1", Name: kernel.ToolName, Input: args})
	if res.IsError {
		t.Fatalf("go_run failed: %s", res.Content)
	}
	return res.Content
}

func TestGoRunIsRegisteredAndBoundToTheGuard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, ctx := kernelGuard(t, dir)

	code := `package main

import (
	"fmt"
	"spore"
)

func main() {
	_, err := spore.ReadFile(".env")
	fmt.Println("env error:", err != nil)
}`
	out := runProgram(t, guard, ctx, code)
	if !strings.Contains(out, "env error: true") || !strings.Contains(out, "fs_read error") {
		t.Errorf("reading .env from a program was not denied:\n%s", out)
	}
}

// A program reading a file larger than the model's tool-output budget
// (max_output, 30 KB by default) must get all of it. fs_read used to cut the
// file at that budget with no note, so a program counting lines in a large
// file got a smaller number and reported it as fact.
func TestGoRunReadsAFileLargerThanTheToolOutputBudget(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&b, "line %04d %s\n", i, strings.Repeat("x", 40))
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, ctx := kernelGuard(t, dir)

	out := runProgram(t, guard, ctx, `package main

import (
	"fmt"
	"spore"
	"strings"
)

func main() {
	s, err := spore.ReadFile("big.txt")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("lines:", strings.Count(s, "\n"), "last:", strings.Contains(s, "line 2000"), "truncated:", strings.Contains(s, "truncated"))
}`)
	if !strings.Contains(out, "lines: 2000 last: true truncated: false") {
		t.Errorf("the program did not get the whole 100 KB file:\n%s", out)
	}
}
