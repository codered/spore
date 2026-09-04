package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
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
			srv, ts, _ := newFullServerWithPolicy(t, policyTOML,
				provider.ScriptTurn{ToolCalls: []provider.Block{{
					Type: provider.BlockToolUse, ID: "call-1", Name: "fs_read",
					Input: json.RawMessage(`{"path":"test.txt"}`),
				}}},
				provider.ScriptTurn{Text: "done"},
			)

			// Create a session with its own per-session workspace directory
			id, err := srv.Store().CreateSession(t.Context(), "profile-test", "")
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			// Get the session to find its workspace (allocated as SessionsDir()/id)
			sess, found, err := srv.Store().Session(t.Context(), id)
			if !found || err != nil {
				t.Fatalf("could not load session: %v", err)
			}

			// Create the directory and test file in the session's workspace
			if err := os.MkdirAll(sess.Workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sess.Workspace, "test.txt"), []byte("hello from test"), 0o600); err != nil {
				t.Fatal(err)
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
	for i := 0; i < 200 && srv.Hub().Running(created.ID); i++ {
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
