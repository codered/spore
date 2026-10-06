package kernel_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/kernel"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// stubTools stands in for the registry behind the guard: it records what
// reached it and answers every call.
type stubTools struct {
	mu  sync.Mutex
	ran []string
}

func (s *stubTools) Specs() []provider.ToolSpec { return nil }
func (s *stubTools) ReadOnly(string) bool       { return false }
func (s *stubTools) Run(_ context.Context, c provider.Block) provider.Block {
	s.mu.Lock()
	s.ran = append(s.ran, c.Name)
	s.mu.Unlock()
	return provider.Block{Type: provider.BlockToolResult, ID: c.ID, Content: "ran " + c.Name}
}

func (s *stubTools) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ran...)
}

type allowOnce struct {
	mu    sync.Mutex
	asked []string
}

func (a *allowOnce) Ask(_ context.Context, ask policy.Ask) (policy.Answer, error) {
	a.mu.Lock()
	a.asked = append(a.asked, ask.Tool)
	a.mu.Unlock()
	return policy.Answer{Allow: true, Scope: policy.ScopeOnce}, nil
}

type fixture struct {
	guard *policy.Guard
	tools *stubTools
	ap    *allowOnce
	ws    string
	sid   string
}

// newFixture builds the real guard from a config that went through
// config.Load, so the baseline deny is in force as it is in production.
func newFixture(t *testing.T) fixture {
	t.Helper()
	ws := t.TempDir()
	path := filepath.Join(ws, "config.toml")
	if err := os.WriteFile(path, []byte("default_model = \"anthropic/claude-opus-5\"\n[policy]\nworkspace = \""+ws+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(ws, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sid, err := st.CreateSession(context.Background(), "kernel policy", "")
	if err != nil {
		t.Fatal(err)
	}
	tools, ap := &stubTools{}, &allowOnce{}
	return fixture{guard: policy.NewGuard(tools, eng, ap, st), tools: tools, ap: ap, ws: ws, sid: sid}
}

func (f fixture) run(t *testing.T, profile policy.Profile, body string) string {
	t.Helper()
	src := "package main\n\nimport (\n\t\"fmt\"\n\t\"spore\"\n)\n\n" + body
	ctx := policy.WithSession(context.Background(), policy.Session{ID: f.sid, Profile: profile, Workspace: f.ws})
	res, err := kernel.Run(ctx, src, f.guard, kernel.Options{
		Timeout: 10 * time.Second, Ceiling: 30 * time.Second, MaxOutput: 1 << 20, HelperMax: 4 << 20,
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, res.Output)
	}
	return res.Output
}

func TestProgramCannotReadSSHKeys(t *testing.T) {
	f := newFixture(t)
	out := f.run(t, policy.ProfileLocal, `func main() {
	_, err := spore.ReadFile(".ssh/id_ed25519")
	fmt.Println("denied:", err != nil)
}`)
	if out != "denied: true\n" {
		t.Errorf("out = %q", out)
	}
	if len(f.tools.names()) != 0 {
		t.Errorf("a denied read reached the tool: %v", f.tools.names())
	}
}

func TestProgramCannotSudo(t *testing.T) {
	f := newFixture(t)
	out := f.run(t, policy.ProfileLocal, `func main() {
	_, err := spore.Shell("sudo ls")
	fmt.Println("denied:", err != nil)
}`)
	if out != "denied: true\n" {
		t.Errorf("out = %q", out)
	}
	if len(f.ap.asked) != 0 || len(f.tools.names()) != 0 {
		t.Errorf("a baseline deny prompted (%v) or ran (%v)", f.ap.asked, f.tools.names())
	}
}

func TestRemoteProgramCannotReachMCP(t *testing.T) {
	f := newFixture(t)
	out := f.run(t, policy.ProfileRemote, `func main() {
	_, err := spore.Call("mcp__x__y", map[string]any{})
	fmt.Println("denied:", err != nil)
}`)
	if out != "denied: true\n" {
		t.Errorf("out = %q", out)
	}
}

func TestAskedWriteContinuesOnApproval(t *testing.T) {
	f := newFixture(t)
	out := f.run(t, policy.ProfileLocal, `func main() {
	if err := spore.WriteFile("notes.md", "hi"); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("wrote")
}`)
	if out != "wrote\n" {
		t.Errorf("out = %q", out)
	}
	if len(f.ap.asked) != 1 || f.ap.asked[0] != "fs_write" {
		t.Errorf("approver asked %v, want [fs_write]", f.ap.asked)
	}
	if got := f.tools.names(); len(got) != 1 || got[0] != "fs_write" {
		t.Errorf("tools ran %v", got)
	}
}
