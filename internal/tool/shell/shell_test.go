package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
)

func ctxFor(ws string) context.Context {
	return policy.WithSession(context.Background(),
		policy.Session{ID: "test", Profile: policy.ProfileLocal, Workspace: ws})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func call(t *testing.T, tl interface {
	Call(context.Context, json.RawMessage) (string, error)
}, args any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tl.Call(context.Background(), raw)
}

func TestExecCapturesOutput(t *testing.T) {
	ws := t.TempDir()
	tl := New(5*time.Second, 1<<20)
	ctx := ctxFor(ws)
	raw, _ := json.Marshal(map[string]string{"command": "echo hello"})
	out, err := tl.Call(ctx, raw)
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
	tl := New(5*time.Second, 1<<20)
	ctx := ctxFor(ws)
	raw, _ := json.Marshal(map[string]string{"command": "ls"})
	out, err := tl.Call(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "marker.txt") {
		t.Errorf("ls = %q, want the command to run in the workspace", out)
	}
}

func TestNonZeroExitIsReportedNotHidden(t *testing.T) {
	ws := t.TempDir()
	tl := New(5*time.Second, 1<<20)
	ctx := ctxFor(ws)
	raw, _ := json.Marshal(map[string]string{"command": "echo to-stderr 1>&2; exit 3"})
	out, err := tl.Call(ctx, raw)
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
	ws := t.TempDir()
	tl := New(5*time.Second, 1<<20)
	ctx := ctxFor(ws)
	start := time.Now()
	raw, _ := json.Marshal(map[string]any{"command": "sleep 30", "timeout_seconds": 1})
	out, err := tl.Call(ctx, raw)
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
	ws := t.TempDir()
	tl := New(5*time.Second, 1<<20)
	ctx := ctxFor(ws)
	raw, _ := json.Marshal(map[string]string{"command": "  "})
	if _, err := tl.Call(ctx, raw); err == nil {
		t.Error("an empty command must be a tool error")
	}
}

func TestOutputIsCappedInMemory(t *testing.T) {
	ws := t.TempDir()
	tl := New(10*time.Second, 4096)
	ctx := ctxFor(ws)
	raw, _ := json.Marshal(map[string]string{"command": "head -c 200000 /dev/zero | tr '\\0' 'x'"})
	out, err := tl.Call(ctx, raw)
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
		ws := t.TempDir()
		tl := New(2*time.Second, 1<<20)
		ctx := ctxFor(ws)
		raw, _ := json.Marshal(map[string]string{"command": "true"})
		out, err := tl.Call(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "timed out") {
			t.Fatalf("a command that exited cleanly was reported as timed out: %q", out)
		}
	}
}

func TestShellIsNotReadOnly(t *testing.T) {
	if New(time.Second, 1<<20).ReadOnly() {
		t.Error("shell_exec must never be read-only: it would join concurrent batches")
	}
}

func TestShellRunsInTheSessionWorkspace(t *testing.T) {
	ws := t.TempDir()
	tl := New(5*time.Second, 1<<16)
	ctx := ctxFor(ws)
	out, err := tl.Call(ctx, json.RawMessage(`{"command":"pwd"}`))
	if err != nil {
		t.Fatal(err)
	}
	// macOS resolves /var to /private/var, so compare resolved paths.
	want, _ := filepath.EvalSymlinks(ws)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(firstLine(out)))
	if got != want {
		t.Fatalf("pwd = %q, want %q", got, want)
	}
}

func TestShellRefusesWithoutASessionWorkspace(t *testing.T) {
	tl := New(5*time.Second, 1<<16)
	if _, err := tl.Call(context.Background(), json.RawMessage(`{"command":"pwd"}`)); err == nil {
		t.Fatal("a shell call with no workspace on the context must be refused")
	}
}
