package companiontool_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/tool/companiontool"
)

func fixture(t *testing.T) (*config.Config, *store.Store, *companion.Service, string) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Companion.Enabled = true
	st, err := store.Open(filepath.Join(cfg.DataDir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	if err := st.SetCompanionState(ctx, companion.StateSessionID, sid); err != nil {
		t.Fatal(err)
	}
	return cfg, st, companion.NewService(st, cfg), sid
}

func byName(tools []tool.Tool, name string) tool.Tool {
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	return nil
}

func in(sid string) context.Context {
	return policy.WithSession(context.Background(), policy.Session{ID: sid})
}

func TestToolsExistOnlyWhileTheCompanionIsOn(t *testing.T) {
	cfg, _, svc, _ := fixture(t)
	if got := companiontool.New(cfg, svc); len(got) != 2 {
		t.Fatalf("tools = %d, want 2", len(got))
	}
	cfg.Companion.Enabled = false
	if got := companiontool.New(cfg, svc); len(got) != 0 {
		t.Fatalf("tools while off = %d, want 0", len(got))
	}
	cfg.Companion.Enabled = true
	if got := companiontool.New(cfg, nil); len(got) != 0 {
		t.Fatalf("tools with no service = %d, want 0", len(got))
	}
}

func TestToolsActOnlyInTheCompanionSession(t *testing.T) {
	cfg, st, svc, _ := fixture(t)
	other, _ := st.CreateSessionFrom(context.Background(), "chat", "", store.SourceChat)
	tools := companiontool.New(cfg, svc)
	for _, name := range []string{"interest_update", "companion_feedback"} {
		args := `{"kind":"more"}`
		if name == "interest_update" {
			args = `{"key":"stock:zs","state":"retired"}`
		}
		_, err := byName(tools, name).Call(in(other), json.RawMessage(args))
		if err == nil || !strings.Contains(err.Error(), "companion conversation") {
			t.Fatalf("%s outside the companion session = %v, want refused", name, err)
		}
	}
}

func TestInterestUpdateCannotActivate(t *testing.T) {
	cfg, st, svc, sid := fixture(t)
	ctx := context.Background()
	zs, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = st.SetInterestState(ctx, zs.ID, store.InterestObserving, store.InterestProposed)
	upd := byName(companiontool.New(cfg, svc), "interest_update")
	if _, err := upd.Call(in(sid), json.RawMessage(`{"key":"stock:zs","state":"active"}`)); err == nil {
		t.Fatal("interest_update made an interest active")
	}
	if got, _, _ := st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestProposed {
		t.Fatalf("state = %s, want proposed", got.State)
	}
	out, err := upd.Call(in(sid), json.RawMessage(`{"key":"stock:zs","state":"retired"}`))
	if err != nil || !strings.Contains(out, "Retired") {
		t.Fatalf("retire = %q %v", out, err)
	}
}

func TestCompanionFeedback(t *testing.T) {
	cfg, _, svc, sid := fixture(t)
	fb := byName(companiontool.New(cfg, svc), "companion_feedback")
	if _, err := fb.Call(in(sid), json.RawMessage(`{"kind":"louder"}`)); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
	out, err := fb.Call(in(sid), json.RawMessage(`{"kind":"less"}`))
	if err != nil || !strings.Contains(out, "3") {
		t.Fatalf("less = %q %v", out, err)
	}
}
