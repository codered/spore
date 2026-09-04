package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newStore is openTestStore, plus the data directory the store was opened
// in: the tests below assert on paths the store derives from it.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func TestAppendAndReadMessagesInOrder(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	id, err := s.CreateSession(ctx, "first session", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, role := range []string{"user", "assistant", "user"} {
		if _, err := s.AppendMessage(ctx, Message{
			SessionID:  id,
			Role:       role,
			BlocksJSON: []byte(`[{"type":"text","text":"hi"}]`),
			Model:      "anthropic/claude-opus-5",
			CallSite:   "chat",
			TokensIn:   10,
			TokensOut:  4,
			CostUSD:    0.001,
		}); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	msgs, err := s.Messages(ctx, id)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Errorf("message %d has Seq %d, want %d", i, m.Seq, i+1)
		}
	}
	if msgs[1].Role != "assistant" || msgs[1].TokensIn != 10 {
		t.Errorf("round-trip lost fields: %+v", msgs[1])
	}
}

func TestSummaryRoundTripAndSessionListing(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	id, err := s.CreateSession(ctx, "summarised", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.SetSummary(ctx, id, "the user asked about spore", 8); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	text, through, err := s.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if text != "the user asked about spore" || through != 8 {
		t.Errorf("Summary = (%q, %d)", text, through)
	}

	// Overwriting replaces rather than accumulating.
	if err := s.SetSummary(ctx, id, "updated", 20); err != nil {
		t.Fatalf("SetSummary (update): %v", err)
	}
	text, through, _ = s.Summary(ctx, id)
	if text != "updated" || through != 20 {
		t.Errorf("after update Summary = (%q, %d)", text, through)
	}

	sessions, err := s.ListSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != id {
		t.Errorf("ListSessions = %+v", sessions)
	}
}

func TestSummaryAbsentIsNotAnError(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	id, _ := s.CreateSession(ctx, "empty", "")
	text, through, err := s.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary on session with no summary: %v", err)
	}
	if text != "" || through != 0 {
		t.Errorf("want zero values, got (%q, %d)", text, through)
	}
}

func TestTimeFormatSortsChronologically(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC) // exactly on the second
	later := base.Add(500 * time.Millisecond)

	a := base.Format(timeFormat)
	b := later.Format(timeFormat)

	if len(a) != len(b) {
		t.Fatalf("timeFormat is variable width: %q (%d) vs %q (%d)", a, len(a), b, len(b))
	}
	if !(a < b) {
		t.Errorf("later timestamp must sort after earlier one: %q should be < %q", a, b)
	}
	if _, err := time.Parse(time.RFC3339, b); err != nil {
		t.Errorf("stored timestamps must still parse as RFC3339: %v", err)
	}
}

func TestCreateSessionWritesFixedWidthTimestamp(t *testing.T) {
	s := openTestStore(t)
	id, err := s.CreateSession(context.Background(), "fixed width", "")
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT created_at FROM sessions WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != len(time.Now().UTC().Format(timeFormat)) {
		t.Errorf("stored timestamp %q is not fixed width", raw)
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Errorf("stored timestamp %q does not parse as RFC3339: %v", raw, err)
	}
}

func TestListSessionsOrdering(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Create two sessions
	id1, err := s.CreateSession(ctx, "first", "")
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	id2, err := s.CreateSession(ctx, "second", "")
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}

	// Format real time.Time values through timeFormat for the update
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	later := base.Add(500 * time.Millisecond)
	exactSecond := base.Format(timeFormat)
	withMillis := later.Format(timeFormat)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, exactSecond, id1); err != nil {
		t.Fatalf("update session 1 timestamp: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, withMillis, id2); err != nil {
		t.Fatalf("update session 2 timestamp: %v", err)
	}

	// ListSessions orders by updated_at DESC, so id2 (with later timestamp) should be first
	sessions, err := s.ListSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	if sessions[0].ID != id2 {
		t.Errorf("first session should be id2 (later timestamp), got %s", sessions[0].ID)
	}
	if sessions[1].ID != id1 {
		t.Errorf("second session should be id1 (earlier timestamp), got %s", sessions[1].ID)
	}
}

func TestPendingCallLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	sid, err := s.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AddPendingCall(ctx, PendingCall{
		SessionID: sid, ToolUseID: "call-1", Tool: "shell_exec",
		Profile: "local", Rule: "shell_exec", ArgsJSON: []byte(`{"command":"ls"}`),
	})
	if err != nil {
		t.Fatalf("AddPendingCall: %v", err)
	}
	pending, err := s.PendingCalls(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id || pending[0].ToolUseID != "call-1" {
		t.Fatalf("PendingCalls = %+v", pending)
	}
	if string(pending[0].ArgsJSON) != `{"command":"ls"}` {
		t.Errorf("args = %s", pending[0].ArgsJSON)
	}
	if pending[0].CreatedAt.IsZero() {
		t.Error("CreatedAt was not populated")
	}

	if _, err := s.ResolvePendingCall(ctx, id, "allow"); err != nil {
		t.Fatal(err)
	}
	// A resolved call is no longer pending: a restart must not re-ask.
	pending, err = s.PendingCalls(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingCalls after resolve = %+v, want empty", pending)
	}
}

func TestPendingCallsAreScopedToASession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a, _ := s.CreateSession(ctx, "a", "")
	b, _ := s.CreateSession(ctx, "b", "")
	if _, err := s.AddPendingCall(ctx, PendingCall{SessionID: a, ToolUseID: "1", Tool: "fs_write", ArgsJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PendingCalls(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("session b sees %d pending calls from session a", len(got))
	}
}

func TestSessionDecisionRemembersAllowForTheSession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	sid, _ := s.CreateSession(ctx, "t", "")

	if _, ok, err := s.SessionDecision(ctx, sid, "fs_write"); err != nil || ok {
		t.Fatalf("SessionDecision before any answer = (ok %v, err %v), want not found", ok, err)
	}
	if err := s.RecordApproval(ctx, sid, "fs_write", []byte(`{"path":"x"}`), "allow", "session"); err != nil {
		t.Fatal(err)
	}
	d, ok, err := s.SessionDecision(ctx, sid, "fs_write")
	if err != nil || !ok || d != "allow" {
		t.Fatalf("SessionDecision = (%q, %v, %v), want (allow, true, nil)", d, ok, err)
	}
	// A "once" answer is audited but never remembered.
	if err := s.RecordApproval(ctx, sid, "shell_exec", []byte(`{}`), "allow", "once"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionDecision(ctx, sid, "shell_exec"); ok {
		t.Error("a once-scoped answer was remembered for the session")
	}
	// A decision in one session must not leak into another.
	other, _ := s.CreateSession(ctx, "other", "")
	if _, ok, _ := s.SessionDecision(ctx, other, "fs_write"); ok {
		t.Error("a session-scoped decision leaked across sessions")
	}
}

func TestLatestSessionDecisionWins(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	sid, _ := s.CreateSession(ctx, "t", "")
	if err := s.RecordApproval(ctx, sid, "fs_write", []byte(`{}`), "allow", "session"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordApproval(ctx, sid, "fs_write", []byte(`{}`), "deny", "session"); err != nil {
		t.Fatal(err)
	}
	d, ok, _ := s.SessionDecision(ctx, sid, "fs_write")
	if !ok || d != "deny" {
		t.Errorf("SessionDecision = %q, want the most recent answer (deny)", d)
	}
}

func TestSessionLookupByID(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Create a session
	id, err := s.CreateSession(ctx, "test session", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Look up the existing session
	sess, found, err := s.Session(ctx, id)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if !found {
		t.Fatal("Session not found but expected to be found")
	}
	if sess.ID != id {
		t.Errorf("Session ID = %q, want %q", sess.ID, id)
	}
	if sess.Title != "test session" {
		t.Errorf("Session Title = %q, want %q", sess.Title, "test session")
	}
	if sess.CreatedAt.IsZero() {
		t.Error("CreatedAt was not populated")
	}
	if sess.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not populated")
	}

	// Look up a non-existent session
	missing, found, err := s.Session(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Session lookup for nonexistent ID: %v", err)
	}
	if found {
		t.Error("Nonexistent session was found")
	}
	if missing.ID != "" {
		t.Errorf("Nonexistent session ID should be empty, got %q", missing.ID)
	}
}

func TestPendingCallByID(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, err := st.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddPendingCall(ctx, PendingCall{
		SessionID: sid, ToolUseID: "tu1", Tool: "shell_exec",
		ArgsJSON: []byte(`{"cmd":"ls"}`), Profile: "remote", Rule: "shell_exec",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, found, err := st.PendingCallByID(ctx, id)
	if err != nil || !found {
		t.Fatalf("PendingCallByID = (_, %v, %v), want (_, true, nil)", found, err)
	}
	if got.Tool != "shell_exec" || string(got.ArgsJSON) != `{"cmd":"ls"}` {
		t.Fatalf("got %+v, want tool shell_exec with its args", got)
	}

	if _, found, err := st.PendingCallByID(ctx, id+999); err != nil || found {
		t.Fatalf("missing id: got (found=%v, err=%v), want (false, nil)", found, err)
	}
}

func TestBridgeBindings(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, err := st.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, found, err := st.SessionForExternal(ctx, "discord", "thread-1"); err != nil || found {
		t.Fatalf("unbound thread: (found=%v, err=%v), want (false, nil)", found, err)
	}
	if err := st.BindExternal(ctx, "discord", "thread-1", sid); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.SessionForExternal(ctx, "discord", "thread-1")
	if err != nil || !found || got != sid {
		t.Fatalf("got (%q, %v, %v), want (%q, true, nil)", got, found, err, sid)
	}

	// Two bridges may use the same external id without colliding.
	if _, found, _ := st.SessionForExternal(ctx, "telegram", "thread-1"); found {
		t.Fatal("bindings leaked across bridges")
	}

	// Rebinding is idempotent, not an error: the DM surface rebinds its
	// rolling session every time /new is used.
	sid2, _ := st.CreateSession(ctx, "t2", "")
	if err := st.BindExternal(ctx, "discord", "thread-1", sid2); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.SessionForExternal(ctx, "discord", "thread-1")
	if got != sid2 {
		t.Fatalf("rebind: got %q, want %q", got, sid2)
	}
}

func TestMarkSeenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	fresh, err := st.MarkSeen(ctx, "discord", "msg-1")
	if err != nil || !fresh {
		t.Fatalf("first sighting: (%v, %v), want (true, nil)", fresh, err)
	}
	// The gateway redelivers on resume. The second sighting must not run a
	// turn, so it must report false.
	fresh, err = st.MarkSeen(ctx, "discord", "msg-1")
	if err != nil || fresh {
		t.Fatalf("redelivery: (%v, %v), want (false, nil)", fresh, err)
	}
	fresh, _ = st.MarkSeen(ctx, "discord", "msg-2")
	if !fresh {
		t.Fatal("a different message id must be fresh")
	}
}

func TestPruneSeen(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if _, err := st.MarkSeen(ctx, "discord", "old"); err != nil {
		t.Fatal(err)
	}
	// Nothing is old enough to prune yet.
	if err := st.PruneSeen(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := st.MarkSeen(ctx, "discord", "old"); fresh {
		t.Fatal("PruneSeen removed a row that was inside the window")
	}
	// Everything is older than zero.
	if err := st.PruneSeen(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := st.MarkSeen(ctx, "discord", "old"); !fresh {
		t.Fatal("PruneSeen did not remove a row outside the window")
	}
}

func TestCreateSessionRecordsWorkspace(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "titled", "/projects/thing")
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Session(ctx, id)
	if err != nil || !found {
		t.Fatalf("session %s: found=%v err=%v", id, found, err)
	}
	if got.Workspace != "/projects/thing" {
		t.Fatalf("workspace = %q, want /projects/thing", got.Workspace)
	}
}

// An empty workspace is the caller saying "I have no directory of my own".
// The store allocates one under its own data directory and records it, but
// must not create it: a session that is opened and never used leaves nothing
// on disk.
func TestCreateSessionAllocatesSessionDir(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Session(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "sessions", id)
	if got.Workspace != want {
		t.Fatalf("workspace = %q, want %q", got.Workspace, want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("session directory was created at creation time: stat err = %v", err)
	}
}

func TestListSessionsCarriesWorkspace(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateSession(ctx, "a", "/ws/a"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Workspace != "/ws/a" {
		t.Fatalf("list = %+v, want one row rooted at /ws/a", list)
	}
}

func TestSetSessionWorkspace(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "", "/ws/old")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionWorkspace(ctx, id, "/ws/new"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Session(ctx, id)
	if got.Workspace != "/ws/new" {
		t.Fatalf("workspace = %q, want /ws/new", got.Workspace)
	}
	if err := s.SetSessionWorkspace(ctx, "nosuch", "/ws/new"); err == nil {
		t.Fatal("re-rooting an unknown session should error")
	}
}

// A database written before the column existed has rows with an empty
// workspace. They are backfilled with the configured ceiling so resuming one
// behaves exactly as it did.
func TestBackfillSessionWorkspaces(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "", "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-migration row.
	if _, err := s.DB().ExecContext(ctx, `UPDATE sessions SET workspace = '' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	n, err := s.BackfillSessionWorkspaces(ctx, "/home/user")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d rows, want 1", n)
	}
	got, _, _ := s.Session(ctx, id)
	if got.Workspace != "/home/user" {
		t.Fatalf("workspace = %q, want /home/user", got.Workspace)
	}
}
