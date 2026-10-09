package models

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

type fixture struct {
	svc *Service
	st  *store.Store
	sid string
	cfg string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("default_model = \"p/big\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry()
	reg.Register("p", newLister("big", "small", "tiny"), provider.ProviderPrice{})
	rt, err := router.New([]config.Route{{When: "title", Model: "p/small"}}, "p/big")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := st.CreateSession(context.Background(), "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		svc: &Service{Store: st, Router: rt, Catalog: NewCatalog(reg, nil), ConfigPath: cfgPath},
		st:  st, sid: sid, cfg: cfgPath,
	}
}

func opOf(t *testing.T, v View, name string) Op {
	t.Helper()
	for _, o := range v.Ops {
		if o.Op == name {
			return o
		}
	}
	t.Fatalf("no op %q in %+v", name, v.Ops)
	return Op{}
}

func TestViewDefaultsAndScopes(t *testing.T) {
	f := newFixture(t)
	v, err := f.svc.View(context.Background(), f.sid, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Ops) != 6 || v.Ops[0].Op != "chat" || v.Ops[5].Op != "subagent" {
		t.Fatalf("ops = %+v", v.Ops)
	}
	chat := opOf(t, v, "chat")
	if chat.Scope != ScopeSession || chat.Selected != "p/big" || chat.Default != "p/big" {
		t.Fatalf("chat = %+v", chat)
	}
	title := opOf(t, v, "title")
	if title.Scope != ScopeGlobal || title.Selected != "p/small" || title.Default != "p/small" {
		t.Fatalf("title = %+v", title)
	}
}

func TestSetChatThenSubagentInheritsIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, err := f.svc.Set(ctx, f.sid, "chat", "p/tiny")
	if err != nil {
		t.Fatal(err)
	}
	if o := opOf(t, v, "chat"); o.Selected != "p/tiny" || o.Default != "p/big" {
		t.Fatalf("chat = %+v", o)
	}
	if o := opOf(t, v, "subagent"); o.Selected != "p/tiny" || o.Default != "p/tiny" {
		t.Fatalf("subagent should inherit the chat choice: %+v", o)
	}
	got, err := f.svc.ChildModel(ctx, f.sid, "")
	if err != nil || got != "p/tiny" {
		t.Fatalf("ChildModel = %q, %v", got, err)
	}
}

func TestChoosingTheDefaultClears(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Set(ctx, f.sid, "subagent", "p/small"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Set(ctx, f.sid, "subagent", "p/big"); err != nil { // p/big is the default: the chat model
		t.Fatal(err)
	}
	sess, _, _ := f.st.Session(ctx, f.sid)
	if sess.SubagentModel != "" {
		t.Fatalf("SubagentModel = %q, want cleared so it keeps following chat", sess.SubagentModel)
	}
	if _, err := f.svc.Set(ctx, f.sid, "chat", "p/tiny"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.ChildModel(ctx, f.sid, ""); got != "p/tiny" {
		t.Fatalf("ChildModel = %q, want it to follow the new chat model", got)
	}
}

func TestSetGlobalWritesConfigAndRouter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Set(ctx, "", "title", "p/tiny"); err != nil {
		t.Fatal(err)
	}
	if got := f.svc.Router.Model("title"); got != "p/tiny" {
		t.Fatalf("router title = %q", got)
	}
	body, _ := os.ReadFile(f.cfg)
	if !strings.Contains(string(body), `title = "p/tiny"`) {
		t.Fatalf("config not written:\n%s", body)
	}
	if _, err := f.svc.Set(ctx, "", "title", "p/small"); err != nil { // the default
		t.Fatal(err)
	}
	if got := f.svc.Router.Model("title"); got != "p/small" {
		t.Fatalf("router title = %q after choosing the default", got)
	}
	body, _ = os.ReadFile(f.cfg)
	if strings.Contains(string(body), config.RoutingBegin) {
		t.Fatalf("override block left after choosing the default:\n%s", body)
	}
}

func TestSetRefusesWhatIsNotAvailable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct{ op, ref string }{
		{"chat", "p/unknown"},
		{"chat", "q/big"},
		{"nope", "p/big"},
		{"chat", "free text"},
	} {
		if _, err := f.svc.Set(ctx, f.sid, tc.op, tc.ref); !errors.Is(err, ErrInvalid) {
			t.Errorf("Set(%q, %q) err = %v, want ErrInvalid", tc.op, tc.ref, err)
		}
	}
	if _, err := f.svc.Set(ctx, "", "chat", "p/tiny"); !errors.Is(err, ErrInvalid) {
		t.Errorf("chat without a session: err = %v, want ErrInvalid", err)
	}
}

func TestChildModelPrecedence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if got, _ := f.svc.ChildModel(ctx, f.sid, ""); got != "p/big" {
		t.Fatalf("no choices: %q, want the chat default", got)
	}
	if _, err := f.svc.Set(ctx, f.sid, "subagent", "p/small"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.ChildModel(ctx, f.sid, ""); got != "p/small" {
		t.Fatalf("subagent choice: %q", got)
	}
	if got, _ := f.svc.ChildModel(ctx, f.sid, "p/tiny"); got != "p/tiny" {
		t.Fatalf("explicit: %q", got)
	}
	_, err := f.svc.ChildModel(ctx, f.sid, "p/unknown")
	if err == nil || !strings.Contains(err.Error(), "p/tiny") {
		t.Fatalf("unknown explicit model: err = %v, want one listing the available refs", err)
	}
}
