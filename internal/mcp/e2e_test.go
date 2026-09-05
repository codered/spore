package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
)

// buildFixture compiles the envprobe fixture server.
func buildFixture(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "envprobe")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/envprobe")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building the envprobe fixture: %v", err)
	}
	return bin
}

// denyingApprover fails the test if it is ever consulted: a denied call must
// never become an approval prompt.
type denyingApprover struct{ t *testing.T }

func (a denyingApprover) Ask(context.Context, policy.Ask) (policy.Answer, error) {
	a.t.Error("the guard asked for approval on a call that policy denies")
	return policy.Answer{}, fmt.Errorf("must not be asked")
}

// allowingApprover answers yes once, for the profile that is allowed to call.
type allowingApprover struct{}

func (allowingApprover) Ask(context.Context, policy.Ask) (policy.Answer, error) {
	return policy.Answer{Allow: true, Scope: policy.ScopeOnce}, nil
}

// harness builds a guard over a registry whose only tools come from a real
// MCP server, with policy loaded from a real config file. Loading matters:
// config.Load appends the baseline deny rules that config.Default() does not,
// and an engine without them proves nothing. default_model and the matching
// [providers.test] stanza exist only to satisfy Config.Validate — this test
// never talks to a model provider.
//
// The returned session id is a real row in the store, not a literal like
// "s-local": an ask-path decision persists a pending call keyed by session
// id with a foreign key into sessions, so any session that reaches the
// approver must exist there first or the write fails before the approver is
// ever consulted.
func harness(t *testing.T, approver policy.Approver, extraPolicy string) (guard *policy.Guard, host *mcp.Host, workspace, sessionID string) {
	t.Helper()
	bin := buildFixture(t)
	workspace = t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "spore.toml")

	body := fmt.Sprintf(`
default_model = "test/model"

[providers.test]
kind = "anthropic"
api_key = "dummy"

[policy]
workspace = %q
default = "deny"
approval_timeout = "5s"
ask = ["mcp__*"]

%s

[[mcp.server]]
name = "probe"
transport = "stdio"
command = %q
`, workspace, extraPolicy, bin)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	host = mcp.New(cfg.MCP, workspace, slog.New(slog.DiscardHandler))
	t.Cleanup(host.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	host.DialAll(ctx)

	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	reg.AddSource(host)
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	st, err := store.Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// A real row in sessions, not a literal: the ask path persists a
	// pending call keyed by session id, and that insert has a foreign key
	// into sessions — a call on an unknown session id would fail before
	// the approver is ever consulted, which would falsely look like a deny.
	sessionID, err = st.CreateSession(ctx, "e2e test", "")
	if err != nil {
		t.Fatalf("store.CreateSession: %v", err)
	}

	guard = policy.NewGuard(reg, engine, approver, st, func(policy.Decision, string) error { return nil })
	return guard, host, workspace, sessionID
}

// The security claim: under the remote profile, mcp__* is denied and the call
// never reaches the server. The marker file the server would create is the
// evidence — its absence means the subprocess was never asked.
func TestRemoteProfileDeniedCallNeverReachesTheServer(t *testing.T) {
	guard, _, workspace, sessionID := harness(t, denyingApprover{t}, "[policy.profile.remote]\ndeny = [\"mcp__*\"]\n")
	marker := filepath.Join(workspace, "reached-by-remote")

	ctx := policy.WithSession(context.Background(), policy.Session{ID: sessionID, Profile: policy.ProfileRemote, Workspace: workspace})
	args, _ := json.Marshal(map[string]string{"path": marker})
	res := guard.Run(ctx, provider.Block{ID: "1", Name: "mcp__probe__touch", Input: args})

	if !res.IsError {
		t.Errorf("Run = %+v, want a denial", res)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the denied call reached the MCP server: the marker file exists")
	}
}

// The other half: a local session may call, the call reaches the server, and
// the result comes back marked as external data.
func TestLocalProfileCallReachesTheServer(t *testing.T) {
	guard, _, workspace, sessionID := harness(t, allowingApprover{}, "")
	marker := filepath.Join(workspace, "reached-by-local")

	ctx := policy.WithSession(context.Background(), policy.Session{ID: sessionID, Profile: policy.ProfileLocal, Workspace: workspace})
	args, _ := json.Marshal(map[string]string{"path": marker})
	res := guard.Run(ctx, provider.Block{ID: "1", Name: "mcp__probe__touch", Input: args})

	if res.IsError {
		t.Fatalf("Run = %+v, want success", res)
	}
	if !strings.Contains(res.Content, "external content from MCP server") {
		t.Errorf("result = %q, want the untrusted-content prefix", res.Content)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the allowed call did not reach the server: %v", err)
	}
}
