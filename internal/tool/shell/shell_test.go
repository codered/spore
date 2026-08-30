package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func call(t *testing.T, tl interface {
	Call(context.Context, json.RawMessage) (string, error)
}, args any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tl.Call(context.Background(), raw)
}

func TestExecCapturesOutput(t *testing.T) {
	ws := t.TempDir()
	tl := New(ws, 5*time.Second)
	out, err := call(t, tl, map[string]string{"command": "echo hello"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("out = %q", out)
	}
}

func TestExecRunsInTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "marker.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := New(ws, 5*time.Second)
	// Comparing pwd output would be fragile where the temp dir is reached
	// through a symlink; listing the workspace is not.
	out, err := call(t, tl, map[string]string{"command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "marker.txt") {
		t.Errorf("ls = %q, want the command to run in the workspace", out)
	}
}

func TestNonZeroExitIsReportedNotHidden(t *testing.T) {
	tl := New(t.TempDir(), 5*time.Second)
	out, err := call(t, tl, map[string]string{"command": "echo to-stderr 1>&2; exit 3"})
	if err != nil {
		t.Fatalf("a failing command must return output, not a Go error: %v", err)
	}
	if !strings.Contains(out, "exit status 3") {
		t.Errorf("out = %q, want the exit status reported", out)
	}
	if !strings.Contains(out, "to-stderr") {
		t.Errorf("out = %q, want stderr captured", out)
	}
}

func TestTimeoutKillsTheCommand(t *testing.T) {
	tl := New(t.TempDir(), 5*time.Second)
	start := time.Now()
	out, err := call(t, tl, map[string]any{"command": "sleep 30", "timeout_seconds": 1})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v, want the timeout to kill it", elapsed)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("out = %q, want the timeout reported to the model", out)
	}
}

func TestEmptyCommandIsAnError(t *testing.T) {
	tl := New(t.TempDir(), 5*time.Second)
	if _, err := call(t, tl, map[string]string{"command": "  "}); err == nil {
		t.Error("an empty command must be a tool error")
	}
}

func TestShellIsNotReadOnly(t *testing.T) {
	if New(t.TempDir(), time.Second).ReadOnly() {
		t.Error("shell_exec must never be read-only: it would join concurrent batches")
	}
}
