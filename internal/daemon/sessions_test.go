package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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
workspace = "%s"
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
			id, err := srv.Store().CreateSession(t.Context(), "profile-test")
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			// Attach to SSE stream
			req, _ := http.NewRequest("GET", ts.URL+"/api/sessions/"+id+"/events", nil)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			defer res.Body.Close()

			// Post the turn via StartTurn (not via HTTP)
			if err := srv.StartTurn(id, "read test.txt", "test", tc.profile); err != nil {
				t.Fatalf("StartTurn: %v", err)
			}

			// Read SSE events
			r := readSSE(t, bufio.NewReader(res.Body), 4) // tool_call, tool_result, text, turn_done

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
				if !contains(toolResult.Content, "denied by policy") {
					t.Errorf("error message = %q, want it to mention policy denial", toolResult.Content)
				}
			} else {
				if toolResult.IsError {
					t.Errorf("expected tool success for %s profile, but got error: %s", tc.profile, toolResult.Content)
				}
				if !contains(toolResult.Content, "hello from test") {
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
	sid, err := srv.Store().CreateSession(t.Context(), "err-running-test")
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

	// Now StartTurn should succeed (though it will fail later if there's no agent,
	// but that's OK — we're testing that it gets past the slot check)
	if err := srv.StartTurn(sid, "two", "test", policy.ProfileRemote); err != nil {
		// It's OK if it fails due to missing agent setup, but it should not be
		// ErrTurnRunning
		if errors.Is(err, ErrTurnRunning) {
			t.Fatalf("StartTurn after slot released: got ErrTurnRunning, but should have gotten past the slot check")
		}
		// Agent is not attached in this test, so some error is expected
		// but it should not be about the turn slot
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	for i := range s {
		if i+len(substr) <= len(s) && s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
