//go:build !windows

// Process-tree behaviour at the deadline is Unix-specific: these tests turn on
// process groups and on a wait status that distinguishes a signalled process
// from one that reached its own exit. Windows has neither; see proc_windows.go.

package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimeoutReapsBackgroundedChildren(t *testing.T) {
	ws := t.TempDir()
	marker := filepath.Join(ws, "grandchild-ran.txt")
	tl := New(5*time.Second, 1<<20)
	// The command backgrounds a child that creates the marker well after the
	// timeout. Killing only the direct child leaves the grandchild running and
	// the marker appears — which is exactly what Setpgid exists to prevent.
	if _, err := call(t, tl, ws, map[string]any{
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

func TestExitAtTheDeadlineIsReportedAsAnExitNotAKill(t *testing.T) {
	// bash exits 7 straight away, but a backgrounded grandchild holds the
	// output pipe open past the deadline, so the deadline expires while the
	// call is still waiting. The process exited on its own and was never
	// signalled: reporting a kill would hide the exit status from the model.
	ws := t.TempDir()
	tl := New(20*time.Millisecond, 1<<20)
	out, err := call(t, tl, ws, map[string]string{"command": "sleep 1 & exit 7"})
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

func TestGrandchildIsReapedEvenWhenTheShellExitsFirst(t *testing.T) {
	ws := t.TempDir()
	marker := filepath.Join(ws, "grandchild-ran.txt")
	tl := New(5*time.Second, 1<<20)
	// bash exits straight away, so os/exec never invokes Cancel; the
	// grandchild inherits the output pipe and keeps the call blocked. Nothing
	// kills the process group on this path, so the deadline has to.
	start := time.Now()
	out, err := call(t, tl, ws, map[string]any{
		"command":         "(sleep 4; touch " + marker + ") & exit 7",
		"timeout_seconds": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("call took %v, want it to return at the 1s deadline rather than block on the grandchild", elapsed)
	}
	if !strings.Contains(out, "exit status 7") {
		t.Errorf("out = %q, want the shell's own exit status: it exited before the deadline", out)
	}
	time.Sleep(5 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a backgrounded grandchild outlived the timeout: the process group was not killed")
	}
}
