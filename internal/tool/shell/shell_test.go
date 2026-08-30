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
	tl := New(ws, 5*time.Second, 1<<20)
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
	tl := New(ws, 5*time.Second, 1<<20)
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
	tl := New(t.TempDir(), 5*time.Second, 1<<20)
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
	tl := New(t.TempDir(), 5*time.Second, 1<<20)
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
	tl := New(t.TempDir(), 5*time.Second, 1<<20)
	if _, err := call(t, tl, map[string]string{"command": "  "}); err == nil {
		t.Error("an empty command must be a tool error")
	}
}

func TestTimeoutReapsBackgroundedChildren(t *testing.T) {
	ws := t.TempDir()
	marker := filepath.Join(ws, "grandchild-ran.txt")
	tl := New(ws, 5*time.Second, 1<<20)
	// The command backgrounds a child that creates the marker well after the
	// timeout. Killing only the direct child leaves the grandchild running and
	// the marker appears — which is exactly what Setpgid exists to prevent.
	if _, err := call(t, tl, map[string]any{
		"command":         "(sleep 2; touch " + marker + ") & sleep 30",
		"timeout_seconds": 1,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a backgrounded grandchild outlived the timeout: the process group was not killed")
	}
}

func TestOutputIsCappedInMemory(t *testing.T) {
	tl := New(t.TempDir(), 10*time.Second, 4096)
	out, err := call(t, tl, map[string]string{"command": "head -c 200000 /dev/zero | tr '\\0' 'x'"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 4096+200 {
		t.Errorf("output is %d bytes, want it capped near the 4096-byte budget", len(out))
	}
	if !strings.Contains(out, "were dropped") {
		t.Errorf("out = %.120q, want the dropped bytes reported so the model knows it is not the whole output", out)
	}
}

func TestFastCommandIsNeverReportedAsTimedOut(t *testing.T) {
	// The timeout is generous on purpose: the premise of this test is that the
	// command really did finish, which a deadline shorter than bash's own
	// startup would not establish.
	for i := 0; i < 50; i++ {
		tl := New(t.TempDir(), 2*time.Second, 1<<20)
		out, err := call(t, tl, map[string]string{"command": "true"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "timed out") {
			t.Fatalf("a command that exited cleanly was reported as timed out: %q", out)
		}
	}
}

func TestExitAtTheDeadlineIsReportedAsAnExitNotAKill(t *testing.T) {
	// bash exits 7 straight away, but a backgrounded grandchild holds the
	// output pipe open past the deadline, so the deadline expires while the
	// call is still waiting. The process exited on its own and was never
	// signalled: reporting a kill would hide the exit status from the model.
	tl := New(t.TempDir(), 20*time.Millisecond, 1<<20)
	out, err := call(t, tl, map[string]string{"command": "sleep 1 & exit 7"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "timed out") {
		t.Errorf("out = %q, want the exit status: the command exited on its own and was never killed", out)
	}
	if !strings.Contains(out, "exit status 7") {
		t.Errorf("out = %q, want it to report exit status 7", out)
	}
}

func TestShellIsNotReadOnly(t *testing.T) {
	if New(t.TempDir(), time.Second, 1<<20).ReadOnly() {
		t.Error("shell_exec must never be read-only: it would join concurrent batches")
	}
}
