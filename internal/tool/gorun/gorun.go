// Package gorun implements go_run, the tool that hands a model-written Go
// program to internal/kernel. It makes no policy decision: go_run itself
// has no effect, and every spore.* call the program makes goes back through
// the runner it is bound to — the policy guard in production.
package gorun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/kernel"
)

type Tool struct {
	cfg       config.KernelConfig
	maxOutput int
	runner    atomic.Pointer[kernel.Runner]
}

// New builds go_run. maxOutput caps the program output returned to the
// model, the same budget every other tool result gets.
func New(cfg config.KernelConfig, maxOutput int) *Tool {
	return &Tool{cfg: cfg, maxOutput: maxOutput}
}

// Bind attaches the runner helper calls go through. The guard wraps the
// registry that holds this tool, so it cannot exist when the tool is built;
// buildTools binds it once it does.
func (t *Tool) Bind(r kernel.Runner) { t.runner.Store(&r) }

func (*Tool) Name() string { return kernel.ToolName }

func (*Tool) Description() string {
	return "Run a complete Go program (package main) and return what it prints. " +
		"Inside it, the spore package reaches files, the web, the shell and every other tool."
}

// ReadOnly is false: a program's effects are unknown until it runs.
func (*Tool) ReadOnly() bool { return false }

func (*Tool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"code":{"type":"string","description":"A complete Go program: package main, imports, func main. Print the result to stdout."},
"timeout_seconds":{"type":"integer","description":"Interpreter time budget; default 60, maximum 300."}},
"required":["code"]}`)
}

func (t *Tool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	r := t.runner.Load()
	if r == nil {
		return "", errors.New("go_run is not bound to a tool runner")
	}
	var a struct {
		Code           string `json:"code"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Code) == "" {
		return "", errors.New("code is empty")
	}
	secs := t.cfg.TimeoutSeconds
	if a.TimeoutSeconds > 0 {
		secs = min(a.TimeoutSeconds, t.cfg.MaxTimeoutSeconds)
	}
	res, err := kernel.Run(ctx, a.Code, *r, kernel.Options{
		Timeout:   time.Duration(secs) * time.Second,
		Ceiling:   time.Duration(t.cfg.CeilingSeconds) * time.Second,
		MaxOutput: t.maxOutput,
		HelperMax: t.cfg.HelperMaxBytes,
	})
	out := res.Output
	if res.Truncated {
		out += "\n[output truncated]"
	}
	out += footer(res.Calls)
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	return out, nil
}

// footer tells the model which tools its program reached, so a silent
// failure inside the program is still visible.
func footer(calls []kernel.Call) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, len(calls))
	for i, c := range calls {
		parts[i] = c.Tool + " " + c.Outcome
	}
	return "\n--- calls: " + strings.Join(parts, " · ") + " ---"
}
