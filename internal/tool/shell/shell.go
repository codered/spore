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
}

func New(workspace string, defaultTimeout time.Duration) tool.Tool {
	if defaultTimeout <= 0 {
		defaultTimeout = 120 * time.Second
	}
	return &execTool{ws: workspace, defaultTimeout: defaultTimeout}
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

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	out := buf.String()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
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
