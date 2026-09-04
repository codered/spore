package daemon

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/tool/fs"
)

// newFullServerWithPolicy wires the real thing: real store, real registry, real fs
// builtins, real policy guard, real agent. Only the model is scripted. Every
// other test in this package fakes at least one neighbour; this one fakes
// none, which is the only way the seams between them get exercised.
// policyTOML should contain %WORKSPACE% as a placeholder for the workspace path.
func newFullServerWithPolicy(t *testing.T, policyTOML string, turns ...provider.ScriptTurn) (*Server, *httptest.Server, string) {
	t.Helper()
	workspace := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Build the config through config.Load rather than config.Default. The
	// baseline deny list — the rules no approval can talk past — is appended
	// by Load, NOT by Default, so a hand-built config has no baseline and
	// "fs_read" in the allow list would match by tool name and let a read of
	// /etc/passwd straight through. This test exists to prove the real
	// policy path holds, so it has to load config the way production does.
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	configContent := `default_model = "script/fake"

` + strings.ReplaceAll(policyTOML, "%WORKSPACE%", workspace)
	if err := os.WriteFile(cfgPath, []byte(configContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.DataDir = t.TempDir()

	preg := provider.NewRegistry()
	preg.Register("script", provider.NewScript(turns...), provider.ProviderPrice{In: 1, Out: 2})
	rt, err := router.New(nil, cfg.DefaultModel)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	srv := New(Options{Store: st, Cfg: cfg})

	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	for _, tl := range fs.New(cfg.Policy.MaxOutput) {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register %s: %v", tl.Name(), err)
		}
	}
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	guard := policy.NewGuard(reg, engine, srv.Approver(), st, nil)
	srv.Attach(agent.New(st, preg, rt, cfg, guard), guard)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, workspace
}

// newFullServer calls newFullServerWithPolicy with the default policy.
func newFullServer(t *testing.T, turns ...provider.ScriptTurn) (*Server, *httptest.Server, string) {
	t.Helper()
	return newFullServerWithPolicy(t, `[policy]
workspace = "%WORKSPACE%"
default = "deny"
allow = ["fs_read", "fs_list"]
ask = ["fs_write"]
`, turns...)
}

func attachStream(t *testing.T, ts *httptest.Server, sessionID string) *bufio.Reader {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/sessions/"+sessionID+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return bufio.NewReader(res.Body)
}

func newSession(t *testing.T, ts *httptest.Server, title string) string {
	t.Helper()
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": title})
	defer res.Body.Close()
	var s SessionJSON
	json.NewDecoder(res.Body).Decode(&s)
	if s.ID == "" {
		t.Fatal("no session created")
	}
	return s.ID
}

// An allowed tool call runs for real: the model asks to read a file, the
// guard allows it, the fs builtin reads it, and the content comes back.
func TestEndToEndAllowedToolCallReachesTheRealBuiltin(t *testing.T) {
	srv, ts, workspace := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_read",
			Input: json.RawMessage(`{"path":"note.txt"}`),
		}}},
		provider.ScriptTurn{Text: "the note says hello"},
	)
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("hello from disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := srv.Store().CreateSession(t.Context(), "e2e", workspace)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "read note.txt"})
	post.Body.Close()

	events := readSSE(t, r, 4) // tool_call, tool_result, text, turn_done
	if events[0].Type != WireToolCall || events[0].Tool != "fs_read" {
		t.Fatalf("first event = %+v, want the fs_read call", events[0])
	}
	if events[1].Type != WireToolResult {
		t.Fatalf("second event = %+v, want a tool result", events[1])
	}
	if events[1].IsError {
		t.Fatalf("the tool call failed: %s", events[1].Content)
	}
	if !strings.Contains(events[1].Content, "hello from disk") {
		t.Errorf("tool result = %q, want the file's real content", events[1].Content)
	}
	if events[2].Type != WireText || events[2].Text != "the note says hello" {
		t.Errorf("third event = %+v", events[2])
	}
	if events[3].Type != WireTurnDone {
		t.Errorf("fourth event = %+v, want turn_done", events[3])
	}
}

// A denied call must come back as a tool error the model can read, and must
// never reach the filesystem — deny is absolute and is never escalated to a
// human, so no approval event may appear.
func TestEndToEndDeniedCallNeverReachesTheBuiltin(t *testing.T) {
	srv, ts, workspace := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_read",
			Input: json.RawMessage(`{"path":"/etc/passwd"}`),
		}}},
		provider.ScriptTurn{Text: "I could not read that"},
	)

	id, err := srv.Store().CreateSession(t.Context(), "denied", workspace)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "read /etc/passwd"})
	post.Body.Close()

	events := readSSE(t, r, 4)
	for _, ev := range events {
		if ev.Type == WireApproval {
			t.Fatal("a denied call produced an approval prompt; deny must never be escalated to a human")
		}
	}
	if !events[1].IsError {
		t.Fatalf("the out-of-workspace read was not refused: %+v", events[1])
	}
	if !strings.Contains(events[1].Content, "denied by policy") {
		t.Errorf("refusal text = %q, want it to name the policy", events[1].Content)
	}
}

// The full approval round trip: a turn suspends, the approval arrives over
// SSE, a client answers over HTTP, and the turn resumes and completes.
func TestEndToEndApprovalSuspendsAndResumesTheTurn(t *testing.T) {
	srv, ts, workspace := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_write",
			Input: json.RawMessage(`{"path":"out.txt","content":"written by the agent"}`),
		}}},
		provider.ScriptTurn{Text: "done"},
	)

	id, err := srv.Store().CreateSession(t.Context(), "approve", workspace)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "write out.txt"})
	post.Body.Close()

	events := readSSE(t, r, 2) // tool_call, approval
	var approval WireEvent
	for _, ev := range events {
		if ev.Type == WireApproval {
			approval = ev
		}
	}
	if approval.PendingID == 0 {
		t.Fatalf("no approval arrived; events were %+v", events)
	}
	if approval.Tool != "fs_write" {
		t.Errorf("approval names %q, want fs_write", approval.Tool)
	}

	// Nothing has been written yet: the turn is suspended.
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("the file was written before the approval was answered")
	}

	answer := postJSON(t, ts.URL+"/api/sessions/"+id+"/approvals/"+
		strconv.FormatInt(approval.PendingID, 10), map[string]any{"allow": true, "scope": "once"})
	answer.Body.Close()

	rest := readSSE(t, r, 4) // resolved, tool_result, text, turn_done
	sawDone := false
	for _, ev := range rest {
		if ev.Type == WireTurnDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("the turn did not resume after approval; got %+v", rest)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "out.txt"))
	if err != nil {
		t.Fatalf("the approved write never happened: %v", err)
	}
	if string(body) != "written by the agent" {
		t.Errorf("file content = %q", body)
	}
}

// A second client attaching mid-suspension is told what is waiting, so a
// browser opened after the fact can still answer.
func TestEndToEndSecondClientSeesThePendingApproval(t *testing.T) {
	srv, ts, workspace := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_write",
			Input: json.RawMessage(`{"path":"late.txt","content":"x"}`),
		}}},
		provider.ScriptTurn{Text: "done"},
	)

	id, err := srv.Store().CreateSession(t.Context(), "late", workspace)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "write late.txt"})
	post.Body.Close()
	readSSE(t, first, 2) // wait until the approval exists

	// A client attaching now must be told immediately, before any deltas.
	second := attachStream(t, ts, id)
	got := readSSE(t, second, 1)
	if got[0].Type != WireApproval {
		t.Fatalf("a late client's first event was %+v, want the pending approval", got[0])
	}

	listed, err := http.Get(ts.URL + "/api/sessions/" + id + "/approvals")
	if err != nil {
		t.Fatal(err)
	}
	defer listed.Body.Close()
	var pending []WireEvent
	json.NewDecoder(listed.Body).Decode(&pending)
	if len(pending) != 1 || pending[0].Tool != "fs_write" {
		t.Errorf("GET approvals = %+v, want the one pending fs_write", pending)
	}
}

// The scheduler's callback goes through the same path as a human's message:
// a fresh session, a real turn, a real transcript.
func TestEndToEndScheduledJobOpensAFreshSession(t *testing.T) {
	srv, ts, _ := newFullServer(t, provider.ScriptTurn{Text: "briefing complete"})

	sessionID, err := srv.StartJob(t.Context(), store.Job{
		ID: 1, Kind: "cron", Spec: "0 9 * * *", Prompt: "morning briefing", Enabled: true,
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if sessionID == "" {
		t.Fatal("StartJob returned no session id")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		res, err := http.Get(ts.URL + "/api/sessions/" + sessionID)
		if err != nil {
			t.Fatal(err)
		}
		var tr TranscriptJSON
		json.NewDecoder(res.Body).Decode(&tr)
		res.Body.Close()
		if len(tr.Messages) >= 2 {
			if tr.Messages[0].Role != "user" || tr.Messages[0].Blocks[0].Text != "morning briefing" {
				t.Errorf("first message = %+v, want the job's prompt", tr.Messages[0])
			}
			if tr.Session.Title != "morning briefing" {
				t.Errorf("session title = %q, want the job's prompt", tr.Session.Title)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scheduled turn never completed; transcript has %d messages", len(tr.Messages))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every other test in this file that asserts a tool call succeeds hands the
// model a RELATIVE path ("note.txt"). policy.Resolve joins a relative path
// onto whatever workspace it is given, so those tests pass no matter which
// directory the session is actually rooted at -- a session rooted at the
// wrong place entirely would not fail a single one of them. An absolute path
// is the only shape that exercises the session's own workspace bound rather
// than the path-joining behind it, which is why this test builds the whole
// stack around one.
func TestEndToEndAbsolutePathInsideOwnWorkspaceIsAllowed(t *testing.T) {
	ceiling := t.TempDir()
	own := filepath.Join(ceiling, "session-a")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	notePath := filepath.Join(own, "note.txt")
	if err := os.WriteFile(notePath, []byte("hello from disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"path": notePath})
	if err != nil {
		t.Fatal(err)
	}

	policyTOML := `[policy]
workspace = "` + ceiling + `"
default = "deny"
allow = ["fs_read", "fs_list"]
ask = ["fs_write"]
`
	srv, ts, _ := newFullServerWithPolicy(t, policyTOML,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_read", Input: input,
		}}},
		provider.ScriptTurn{Text: "the note says hello"},
	)

	// The session is rooted at its own directory under the ceiling, not at
	// the ceiling itself -- if the guard were checking the ceiling instead
	// of the session's row, this test could not tell the difference.
	id, err := srv.Store().CreateSession(t.Context(), "abs-inside", own)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "read the absolute path"})
	post.Body.Close()

	events := readSSE(t, r, 4) // tool_call, tool_result, text, turn_done
	if events[1].Type != WireToolResult || events[1].IsError {
		t.Fatalf("an absolute path inside the session's own workspace was refused: %+v", events[1])
	}
	if !strings.Contains(events[1].Content, "hello from disk") {
		t.Errorf("tool result = %q, want the file's real content", events[1].Content)
	}
}

// The companion to the allowed case above: an absolute path that lands
// outside the CALLING session's own workspace, but still inside the
// configured ceiling. If this were denied only by the ceiling, rooting the
// session anywhere under it would wrongly succeed; it must be the session
// bound in the row that refuses this, which is what proves the whole stage
// -- record, expose, honour -- is actually wired together.
func TestEndToEndAbsolutePathOutsideOwnWorkspaceIsDenied(t *testing.T) {
	ceiling := t.TempDir()
	ownA := filepath.Join(ceiling, "session-a")
	ownB := filepath.Join(ceiling, "session-b")
	if err := os.MkdirAll(ownA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ownB, 0o700); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(ownA, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("do not read me"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"path": secretPath})
	if err != nil {
		t.Fatal(err)
	}

	policyTOML := `[policy]
workspace = "` + ceiling + `"
default = "deny"
allow = ["fs_read", "fs_list"]
ask = ["fs_write"]
`
	srv, ts, _ := newFullServerWithPolicy(t, policyTOML,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_read", Input: input,
		}}},
		provider.ScriptTurn{Text: "I could not read that"},
	)

	// sessionB is rooted at its own directory, a sibling of session-a's
	// under the same ceiling -- the ceiling alone would allow reading
	// session-a's file, so a denial here can only be the session bound.
	id, err := srv.Store().CreateSession(t.Context(), "abs-outside", ownB)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "read the absolute path"})
	post.Body.Close()

	events := readSSE(t, r, 4)
	if !events[1].IsError {
		t.Fatalf("a read outside the session's own workspace (but inside the ceiling) was allowed: %+v", events[1])
	}
	if !strings.Contains(events[1].Content, "path outside workspace") {
		t.Errorf("refusal text = %q, want it to name the path-outside-workspace rule", events[1].Content)
	}
	if _, err := os.ReadFile(secretPath); err != nil {
		t.Fatalf("the source file should be untouched: %v", err)
	}
}
