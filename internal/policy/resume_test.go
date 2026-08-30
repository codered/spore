package policy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// TestSuspensionSurvivesARestart simulates the daemon dying mid-approval:
// the first process records a pending call and then goes away; a second
// process, opening the same database file, finds the suspension and answers
// it. The turn's own goroutine is gone, so what must survive is the record.
func TestSuspensionSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "spore.db")

	// --- process 1: a turn suspends, then the process dies ---
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := st1.CreateSession(ctx, "restart test")
	if err != nil {
		t.Fatal(err)
	}
	ap := &scriptedApprover{block: make(chan struct{})} // never answers
	g1 := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{
		Ask:             []string{"fs_write"},
		ApprovalTimeout: "50ms",
	}), ap, st1, nil)
	res := g1.Run(WithSession(ctx, sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/a.go"}`))
	if !res.IsError {
		t.Fatal("the unanswered call was allowed")
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	// --- process 2: a fresh store over the same file ---
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	g2 := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{Ask: []string{"fs_write"}}), nil, st2, nil)

	// The timed-out call is resolved, not dangling: the restarted process
	// must not re-ask about a request that already expired.
	pending, err := g2.Pending(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("timed-out suspension survived as pending: %+v", pending)
	}

	// A suspension recorded but never resolved — the shape of a hard crash —
	// is still visible and answerable after the restart.
	id, err := st2.AddPendingCall(ctx, store.PendingCall{
		SessionID: sid, ToolUseID: "c2", Tool: "fs_write",
		ArgsJSON: json.RawMessage(`{"path":"/ws/b.go"}`), Profile: "local", Rule: "fs_write",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}

	st3, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	g3 := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{Ask: []string{"fs_write"}}), nil, st3, nil)

	pending, err = g3.Pending(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id || pending[0].Tool != "fs_write" {
		t.Fatalf("Pending after restart = %+v, want the recorded suspension", pending)
	}
	if string(pending[0].ArgsJSON) != `{"path":"/ws/b.go"}` {
		t.Errorf("arguments did not survive: %s", pending[0].ArgsJSON)
	}

	// Answering it clears the suspension and records the decision, so a
	// later turn in this session is not asked again.
	if err := g3.Resolve(ctx, sid, id, Answer{Allow: true, Scope: ScopeSession}); err != nil {
		t.Fatal(err)
	}
	pending, _ = g3.Pending(ctx, sid)
	if len(pending) != 0 {
		t.Errorf("Pending after Resolve = %+v, want empty", pending)
	}
	d, ok, err := st3.SessionDecision(ctx, sid, "fs_write")
	if err != nil || !ok || d != "allow" {
		t.Errorf("SessionDecision = (%q, %v, %v), want the answer remembered", d, ok, err)
	}
}

func TestResolveRejectsAForeignPendingCall(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := st.CreateSession(ctx, "a")
	b, _ := st.CreateSession(ctx, "b")
	id, _ := st.AddPendingCall(ctx, store.PendingCall{SessionID: a, ToolUseID: "c", Tool: "fs_write", ArgsJSON: []byte(`{}`)})

	g := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{}), nil, st, nil)
	err = g.Resolve(ctx, b, id, Answer{Allow: true, Scope: ScopeOnce})
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Errorf("Resolve across sessions = %v, want a session-mismatch error", err)
	}
}
