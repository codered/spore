// Package shell implements the shell_exec builtin. It applies a timeout and
// reports exit status back to the model; it makes no policy decision — the
// command string is judged by internal/policy before it reaches here.
package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tool"
)

type execTool struct {
	defaultTimeout time.Duration
	maxOutput      int
}

// New builds shell_exec. Like the filesystem tools it holds no workspace: the
// session's root arrives on the context of each call.
func New(defaultTimeout time.Duration, maxOutput int) tool.Tool {
	if defaultTimeout <= 0 {
		defaultTimeout = 120 * time.Second
	}
	if maxOutput <= 0 {
		maxOutput = 30_000
	}
	return &execTool{defaultTimeout: defaultTimeout, maxOutput: maxOutput}
}

// capWriter accepts output up to a byte budget and counts the rest. The
// registry truncates the returned string, but only once the whole thing is
// already in memory: a chatty command left running for the full timeout could
// allocate without bound before truncation ever happens. Write always reports
// a full write, so an over-budget command is starved of output rather than
// killed with ErrShortWrite.
type capWriter struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	switch {
	case room <= 0:
		w.dropped += len(p)
	case len(p) > room:
		w.buf.Write(p[:room])
		w.dropped += len(p) - room
	default:
		w.buf.Write(p)
	}
	return len(p), nil
}

func (*execTool) Name() string { return "shell_exec" }
func (*execTool) Description() string {
	return "Run a shell command in the workspace and return its combined output. Long-running commands are killed at the timeout."
}

// ReadOnly is always false. A command's effects cannot be known from its
// text, so shell_exec never joins a concurrent batch.
func (*execTool) ReadOnly() bool { return false }

func (*execTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"command":{"type":"string","description":"Command line, interpreted by bash."},
"timeout_seconds":{"type":"integer","description":"Kill the command after this many seconds."}},
"required":["command"]}`)
}

func (t *execTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	ws := policy.WorkspaceFrom(ctx)
	if ws == "" {
		return "", errors.New("no session workspace on the context, so there is nowhere to run this command")
	}

	var a struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command is required")
	}
	timeout := t.defaultTimeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return "", err
	}

	// bash on every platform: the command strings the model writes, and the
	// rules internal/policy matches them against, are bash. On Windows that
	// means shell_exec needs a bash on PATH (Git Bash, WSL, MSYS2).
	cmd := exec.Command("bash", "-c", a.Command)
	cmd.Dir = ws
	// Group the child with its descendants so the whole tree can be killed
	// together; a command that spawns children must not leave them running
	// after a timeout.
	setProcessGroup(cmd)

	// One writer for both streams: os/exec special-cases identical Stdout and
	// Stderr writers and routes them through a single goroutine, so this needs
	// no lock of its own.
	w := &capWriter{limit: t.maxOutput}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting command: %w", err)
	}

	// The deadline gets its own kill path rather than exec.CommandContext's.
	// os/exec settles its cancel watch the moment the shell exits and never
	// calls Cancel after that, so a shell that exits before the deadline
	// leaves its children alive and the call blocked indefinitely on the
	// output pipe they inherited. This fires whenever ctx ends, however the
	// shell finished.
	//
	// Killing by group id is safe against pid reuse: the kernel keeps a pid
	// reserved for as long as it is still in use as a process group id, so it
	// cannot be recycled while any member of the group is alive. Once the
	// group is empty the kill is a no-op.
	pid := cmd.Process.Pid
	waited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessTree(pid)
		case <-waited:
		}
	}()

	err := cmd.Wait()
	close(waited)

	// Whether the deadline killed the command is a fact about the process, not
	// about the context. ctx can expire without exec ever cancelling anything:
	// if the process exits before the deadline, os/exec settles the cancel
	// watch on the spot and never calls Cancel, yet Run can still block well
	// past the deadline waiting for output pipes a surviving grandchild holds
	// open. Keying the report off ctx.Err() there would relabel an ordinary
	// exit as a kill and throw the exit status away.
	killed := wasKilled(cmd.ProcessState)

	out := w.buf.String()
	if w.dropped > 0 {
		out += fmt.Sprintf("\n[%d further bytes of output were dropped at the %d-byte budget]", w.dropped, t.maxOutput)
	}
	switch {
	// A timeout is only reported when the deadline expired *and* the process
	// was actually signalled. A command that reached its own exit at the
	// instant the deadline passed was not killed, and must not be reported as
	// though it were.
	case err != nil && killed && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return out + fmt.Sprintf("\n[timed out after %s and was killed]", timeout), nil
	case err != nil:
		// A non-zero exit is information for the model, not a tool failure.
		return out + fmt.Sprintf("\n[%v]", err), nil
	}
	if out == "" {
		return "(no output; exit status 0)", nil
	}
	return out, nil
}
