package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/store"
)

func TestSessionDeleteGoesThroughARunningDaemon(t *testing.T) {
	ws := t.TempDir()
	c := e2eDaemon(t, ws)
	ctx := context.Background()
	a, err := c.createSession(ctx, "chat", ws)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.createSession(ctx, "chat", ws)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := sessionDelete(ctx, nil, c, []string{"--yes", a}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("delete: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "deleted 1 session") {
		t.Fatalf("output = %q", out.String())
	}
	list, err := c.sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != b {
		t.Fatalf("sessions left = %+v, want only %s", list, b)
	}
}

func TestSessionDeleteAllAsksFirstAndStopsOnNo(t *testing.T) {
	st := openCLITestStore(t)
	ctx := context.Background()
	if _, err := st.CreateSession(ctx, "a", "/ws"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := sessionDelete(ctx, st, nil, []string{"--all"}, strings.NewReader("no\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want cancelled", err)
	}
	if !strings.Contains(out.String(), "type yes") {
		t.Fatalf("prompt = %q, want it to ask for yes", out.String())
	}
	if left, _ := st.ListSessions(ctx, 10, true); len(left) != 1 {
		t.Fatal("a declined delete removed sessions")
	}
}

func TestSessionDeleteWithoutADaemonUsesTheStoreAndSaysDiscordWasLeft(t *testing.T) {
	st := openCLITestStore(t)
	ctx := context.Background()
	a, _ := st.CreateSession(ctx, "a", "/ws")
	if _, err := st.CreateChildSession(ctx, "kid", "/ws", a); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := sessionDelete(ctx, st, nil, []string{"--all", "--discord"}, strings.NewReader("yes\n"), &out); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(out.String(), "deleted 2 sessions") || !strings.Contains(out.String(), "Discord was not touched") {
		t.Fatalf("output = %q, want the count and that Discord was left", out.String())
	}
	if left, _ := st.ListSessions(ctx, 10, true); len(left) != 0 {
		t.Fatalf("%d sessions left", len(left))
	}
}

func TestSessionDeleteNeedsSomethingToDelete(t *testing.T) {
	err := sessionDelete(context.Background(), openCLITestStore(t), nil, []string{"--yes"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "session ids or --all") {
		t.Fatalf("err = %v", err)
	}
}

func openCLITestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
