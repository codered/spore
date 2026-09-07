package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// TestStartTurnCarriesTheProfile asserts that the profile parameter reaches
// the policy engine and affects decisions. We use a policy where fs_read is
// allowed for local but not for remote, then run the same tool call under
// each profile and confirm the outcomes differ.
func TestStartTurnCarriesTheProfile(t *testing.T) {
	// Policy: fs_read allowed for local, denied for remote
	policyTOML := `[policy]
workspace = "%WORKSPACE%"
default = "deny"
allow = ["fs_read", "fs_list"]
ask = ["fs_write"]

[policy.profile.remote]
default = "deny"
allow = ["fs_list"]
`

	testCases := []struct {
		name              string
		profile           policy.Profile
		expectError       bool
		expectFileContent bool
	}{
		{
			name:              "local profile allows fs_read",
			profile:           policy.ProfileLocal,
			expectError:       false,
			expectFileContent: true,
		},
		{
			name:              "remote profile denies fs_read",
			profile:           policy.ProfileRemote,
			expectError:       true,
			expectFileContent: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts, workspace := newFullServerWithPolicy(t, policyTOML,
				provider.ScriptTurn{ToolCalls: []provider.Block{{
					Type: provider.BlockToolUse, ID: "call-1", Name: "fs_read",
					Input: json.RawMessage(`{"path":"test.txt"}`),
				}}},
				provider.ScriptTurn{Text: "done"},
			)

			// Create the test file in the workspace
			if err := os.WriteFile(filepath.Join(workspace, "test.txt"), []byte("hello from test"), 0o600); err != nil {
				t.Fatal(err)
			}

			// Create a session and attach to events
			id, err := srv.Store().CreateSession(t.Context(), "profile-test", workspace)
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			// Attach to SSE stream using the existing helper
			reader := attachStream(t, ts, id)

			// Post the turn via StartTurn (not via HTTP)
			if err := srv.StartTurn(id, "read test.txt", "test", tc.profile); err != nil {
				t.Fatalf("StartTurn: %v", err)
			}

			// Read SSE events
			r := readSSE(t, reader, 4) // tool_call, tool_result, text, turn_done

			// Find the tool result event
			var toolResult *WireEvent
			for i := range r {
				if r[i].Type == WireToolResult {
					toolResult = &r[i]
					break
				}
			}

			if toolResult == nil {
				t.Fatal("no tool result event found")
			}

			if tc.expectError {
				if !toolResult.IsError {
					t.Errorf("expected tool error for %s profile, but got success", tc.profile)
				}
				if !strings.Contains(toolResult.Content, "denied by policy") {
					t.Errorf("error message = %q, want it to mention policy denial", toolResult.Content)
				}
			} else {
				if toolResult.IsError {
					t.Errorf("expected tool success for %s profile, but got error: %s", tc.profile, toolResult.Content)
				}
				if !strings.Contains(toolResult.Content, "hello from test") {
					t.Errorf("tool result = %q, want the file's content", toolResult.Content)
				}
			}
		})
	}
}

// TestStartTurnRefusesASecondTurn asserts that StartTurn returns ErrTurnRunning
// when a turn is already in flight, and that it succeeds once the turn slot
// is released.
func TestStartTurnRefusesASecondTurn(t *testing.T) {
	srv, _, _ := newFullServer(t)

	// Create a session
	sid, err := srv.Store().CreateSession(t.Context(), "err-running-test", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Claim the turn slot manually
	if !srv.Hub().Begin(sid) {
		t.Fatal("could not claim the turn slot")
	}

	// Try to start a turn when the slot is taken — should get ErrTurnRunning
	if err := srv.StartTurn(sid, "two", "test", policy.ProfileRemote); !errors.Is(err, ErrTurnRunning) {
		t.Fatalf("StartTurn with slot taken: err = %v, want ErrTurnRunning", err)
	}

	// Release the slot
	srv.Hub().End(sid)

	// Now StartTurn should succeed. agent.Run returns synchronously with the
	// event channel; the scripted provider's "script exhausted" error comes
	// asynchronously into the channel and never surfaces as a return value.
	if err := srv.StartTurn(sid, "two", "test", policy.ProfileRemote); err != nil {
		t.Fatalf("StartTurn after slot released: err = %v, want nil", err)
	}
}

func TestCreateSessionRecordsTheRequestedWorkspace(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.cfg.Policy.Workspace = t.TempDir()
	inside := filepath.Join(srv.cfg.Policy.Workspace, "project")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	out := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{"workspace": inside}), http.StatusCreated)
	if out.Workspace != inside {
		t.Fatalf("workspace = %q, want %q", out.Workspace, inside)
	}
}

// The ceiling refuses at creation rather than quietly rooting the session
// somewhere else: a client that asked for the wrong place must be told.
func TestCreateSessionRefusesOutsideTheCeiling(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.cfg.Policy.Workspace = t.TempDir()
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"workspace": t.TempDir()})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestCreateSessionWithoutAWorkspaceGetsASessionDirectory(t *testing.T) {
	srv, ts := newTestServer(t)
	out := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	if want := filepath.Join(srv.Store().SessionsDir(), out.ID); out.Workspace != want {
		t.Fatalf("workspace = %q, want %q", out.Workspace, want)
	}
}

func TestPatchSessionReRoots(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.cfg.Policy.Workspace = t.TempDir()
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)

	moved := filepath.Join(srv.cfg.Policy.Workspace, "elsewhere")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	out := decodeSession(t, patchJSON(t, ts.URL+"/api/sessions/"+created.ID,
		map[string]string{"workspace": moved}), http.StatusOK)
	if out.Workspace != moved {
		t.Fatalf("workspace = %q, want %q", out.Workspace, moved)
	}

	res := patchJSON(t, ts.URL+"/api/sessions/"+created.ID,
		map[string]string{"workspace": t.TempDir()})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("re-rooting outside the ceiling: status = %d, want 400", res.StatusCode)
	}
}

func TestPatchSessionMovesSummaryBoundary(t *testing.T) {
	srv, ts := newTestServer(t)
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	seedSessionMessages(t, srv, created.ID, 10)

	res := patchJSON(t, ts.URL+"/api/sessions/"+created.ID,
		map[string]int{"summary_through": 8})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PATCH summary_through: status = %d, want 200", res.StatusCode)
	}
	text, through, err := srv.store.Summary(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if through != 8 {
		t.Errorf("summary boundary %d, want 8", through)
	}
	if text != "" {
		t.Errorf("summary text %q; boundary move must not write text", text)
	}
}

// The boundary is a real message's sequence number, never a sentinel. A
// boundary past the newest message would hide every message appended after it
// too -- including the next thing the user types -- and nothing ever moves it
// back, so the session would assemble an empty prompt for the rest of its
// life. Clamping here closes that from every client at once.
func TestPatchSessionClampsSummaryBoundaryToTheNewestMessage(t *testing.T) {
	srv, ts := newTestServer(t)
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	seedSessionMessages(t, srv, created.ID, 3)

	res := patchJSON(t, ts.URL+"/api/sessions/"+created.ID,
		map[string]int{"summary_through": math.MaxInt32})
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PATCH summary_through: status = %d, want 200", res.StatusCode)
	}
	_, through, err := srv.store.Summary(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if through != 3 {
		t.Fatalf("boundary = %d, want it clamped to the newest message (3)", through)
	}

	// The next message the user sends must be visible to the model again.
	seedSessionMessages(t, srv, created.ID, 1)
	snap, err := srv.agent.Snapshot(policy.WithSession(context.Background(),
		policy.Session{ID: created.ID, Workspace: created.Workspace}), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 1 {
		t.Fatalf("after a clear, the next message must reach the prompt; assembled %d", len(snap.Messages))
	}
}

// /compact is a manual override, so it must fold whatever sits outside the
// protected recent window even when the session is nowhere near the
// auto-compaction threshold. Calling MaybeCompact here did nothing at all
// below the threshold while still answering "ok".
func TestCompactFoldsBelowTheAutoThreshold(t *testing.T) {
	srv, ts := newTestServer(t, provider.ScriptTurn{Text: "SUMMARY: the user rambled"})
	srv.cfg.Context.MaxTokens = 200_000 // far above anything this session assembles
	srv.cfg.Context.KeepRecent = 2
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	seedSessionMessages(t, srv, created.ID, 6)

	res := postJSON(t, ts.URL+"/api/sessions/"+created.ID+"/compact", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST compact: status = %d, want 200", res.StatusCode)
	}
	var out CompactJSON
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Folded != 4 {
		t.Fatalf("folded = %d, want the 4 messages outside the protected window", out.Folded)
	}
	if out.After >= out.Before {
		t.Fatalf("compaction must shrink the estimate: %d -> %d", out.Before, out.After)
	}
	_, through, err := srv.store.Summary(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if through != 4 {
		t.Fatalf("summary boundary = %d, want 4", through)
	}
}

// Nothing outside the protected window is a legitimate answer, not a failure
// -- but it must be reported, or the client says "compacted" over a no-op.
func TestCompactWithNothingToFoldSaysSo(t *testing.T) {
	srv, ts := newTestServer(t) // no turns: a provider call would error
	srv.cfg.Context.KeepRecent = 12
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	seedSessionMessages(t, srv, created.ID, 3)

	res := postJSON(t, ts.URL+"/api/sessions/"+created.ID+"/compact", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST compact: status = %d, want 200", res.StatusCode)
	}
	var out CompactJSON
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Folded != 0 {
		t.Fatalf("folded = %d, want 0", out.Folded)
	}
}

// Compaction rewrites the summary boundary a running turn is reading.
func TestCompactConflictsWithARunningTurn(t *testing.T) {
	srv, ts := newTestServer(t)
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	if !srv.hub.Begin(created.ID) {
		t.Fatal("could not mark the session running")
	}
	defer srv.hub.End(created.ID)

	res := postJSON(t, ts.URL+"/api/sessions/"+created.ID+"/compact", nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("compact during a turn: status = %d, want 409", res.StatusCode)
	}
}

// seedSessionMessages appends n plain user messages to a session.
func seedSessionMessages(t *testing.T, srv *Server, sessionID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		blocks, err := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: "hello"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.AppendMessage(context.Background(), store.Message{
			SessionID: sessionID, Role: "user", BlocksJSON: blocks,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// spore allocated the directory, so spore creates it -- on the first turn,
// not at creation.
func TestFirstTurnCreatesAnAllocatedSessionDirectory(t *testing.T) {
	srv, ts := newTestServer(t, provider.ScriptTurn{Text: "hello"})
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	if _, err := os.Stat(created.Workspace); !os.IsNotExist(err) {
		t.Fatalf("directory exists before the first turn: %v", err)
	}

	res := postJSON(t, ts.URL+"/api/sessions/"+created.ID+"/messages",
		map[string]string{"text": "hi"})
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("post message: status = %d", res.StatusCode)
	}
	// The turn runs on the server's context, so poll the hub the way
	// TestSecondTurnIsRejectedWhileOneIsRunning does.
	for i := 0; i < 200 && srv.hub.Running(created.ID); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(created.Workspace); err != nil {
		t.Fatalf("first turn did not create the session directory: %v", err)
	}
}

// A remote creator is confined to its own directory, whatever it asks for.
func TestRemoteSessionIsConfined(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.cfg.Policy.Workspace = t.TempDir()
	id, err := srv.CreateSession(context.Background(), "", srv.cfg.Policy.Workspace, policy.ProfileRemote)
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := srv.Store().Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sess.Workspace, srv.Store().SessionsDir()) {
		t.Fatalf("remote session rooted at %q, want a session directory", sess.Workspace)
	}
}
