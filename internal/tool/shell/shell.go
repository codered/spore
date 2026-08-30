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
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/codered/spore/internal/tool"
)

type execTool struct {
	ws             string
	defaultTimeout time.Duration
	maxOutput      int
}

func New(workspace string, defaultTimeout time.Duration, maxOutput int) tool.Tool {
	if defaultTimeout <= 0 {
		defaultTimeout = 120 * time.Second
	}
	if maxOutput <= 0 {
		maxOutput = 30_000
	}
	return &execTool{ws: workspace, defaultTimeout: defaultTimeout, maxOutput: maxOutput}
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

	cmd := exec.CommandContext(ctx, "bash", "-c", a.Command)
	cmd.Dir = t.ws
	// Put the child in its own process group and kill the group, so a command
	// that spawns children does not leave them running after a timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// Returning os.ErrProcessDone tells exec the kill is the expected
		// outcome, so Run reports the deadline rather than the signal.
		return os.ErrProcessDone
	}

	// One writer for both streams: os/exec special-cases identical Stdout and
	// Stderr writers and routes them through a single goroutine, so this needs
	// no lock of its own.
	w := &capWriter{limit: t.maxOutput}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()

	out := w.buf.String()
	if w.dropped > 0 {
		out += fmt.Sprintf("\n[%d further bytes of output were dropped at the %d-byte budget]", w.dropped, t.maxOutput)
	}
	switch {
	// err != nil is part of the timeout test on purpose: a command that
	// completes successfully at the instant the deadline expires was not
	// killed, and must not be reported as though it were.
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
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
