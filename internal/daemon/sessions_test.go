package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
