// Package kernel runs one model-written Go program under the yaegi
// interpreter, in a child process, and serves the program's spore.* calls
// through a Runner — in production the policy guard, so every action the
// program takes is judged exactly as a direct tool call would be.
//
// The child process is not optional. In-process, a panic in a goroutine the
// program starts, or unbounded recursion, terminates the host; goroutines
// left running after main returns cannot be stopped; and a blocked native
// call outlives cancellation. A child takes all of that down with it.
package kernel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/tool"
	sporetrace "github.com/codered/spore/internal/trace"
)

// ToolName is the kernel's tool. A program may not call it: nested kernels
// buy nothing and would break the budget accounting.
const ToolName = "go_run"

// Runner serves one tool call. policy.Guard satisfies it; it is declared
// here so this package does not import policy.
type Runner interface {
	Run(ctx context.Context, call provider.Block) provider.Block
}

type Options struct {
	// Timeout is the interpreter budget. It counts only time with no helper
	// call in flight: a program is not charged for a human reading an
	// approval, and helpers are bounded by their own tools.
	Timeout time.Duration
	// Ceiling is a wall-clock stop with no pauses.
	Ceiling time.Duration
	// MaxOutput caps program output in bytes.
	MaxOutput int
	// HelperMax caps one helper result handed to the program.
	HelperMax int
}

// Call records one spore.* call for the footer the model sees.
type Call struct {
	Tool    string
	Outcome string // "ok" or "error"
}

type Result struct {
	Output    string
	Truncated bool
	Calls     []Call
}

var (
	errBudget  = errors.New("budget")
	errCeiling = errors.New("ceiling")
)

// crashReportBytes bounds the stderr quoted when a child dies. The head is
// kept rather than the tail: the Go runtime prints the cause first and the
// goroutine dump after it.
const crashReportBytes = 2048

// Run executes src and returns its output. A program that fails to parse,
// errors, panics, crashes or runs out of time returns an error together with
// whatever output it produced.
func Run(ctx context.Context, src string, r Runner, opt Options) (Result, error) {
	if err := validate(src); err != nil {
		return Result{}, err
	}
	runID, err := newRunID()
	if err != nil {
		return Result{}, err
	}

	ctx, stopCeiling := context.WithTimeoutCause(ctx, opt.Ceiling, errCeiling)
	defer stopCeiling()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	p, err := startChild()
	if err != nil {
		return Result{}, err
	}
	defer p.stop()
	if err := p.conn.write(msg{Type: msgRun, Code: src, MaxOutput: opt.MaxOutput}); err != nil {
		return Result{}, fmt.Errorf("kernel: send program: %w", err)
	}

	msgs := make(chan msg)
	go func() {
		defer close(msgs)
		for {
			m, err := p.conn.read()
			if err != nil {
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	clk := &clock{since: time.Now()}
	go watch(ctx, clk, opt.Timeout, cancel)

	var (
		out       strings.Builder
		truncated bool
		mu        sync.Mutex
		calls     []Call
	)
	result := func() Result {
		mu.Lock()
		defer mu.Unlock()
		return Result{Output: out.String(), Truncated: truncated, Calls: append([]Call(nil), calls...)}
	}
	serve := func(m msg) {
		defer clk.exit()
		res := serveCall(ctx, r, opt, runID, m)
		outcome := "ok"
		if res.IsError {
			outcome = "error"
		}
		mu.Lock()
		calls = append(calls, Call{Tool: m.Tool, Outcome: outcome})
		mu.Unlock()
		// A write fails only when the child is already gone, which the
		// main loop reports on its own.
		_ = p.conn.write(msg{Type: msgResult, ID: m.ID, Content: res.Content, IsError: res.IsError})
	}

	for {
		select {
		case <-ctx.Done():
			return result(), stoppedErr(context.Cause(ctx), opt)
		case m, ok := <-msgs:
			if !ok {
				p.stop()
				return result(), fmt.Errorf("kernel crashed: %s", p.crashReport())
			}
			switch m.Type {
			case msgOut:
				mu.Lock()
				if room := opt.MaxOutput - out.Len(); room < len(m.Data) {
					m.Data, truncated = m.Data[:max(room, 0)], true
				}
				out.WriteString(m.Data)
				mu.Unlock()
			case msgCall:
				// enter runs here, before the goroutine starts, so the clock
				// can never tick through the gap between request and serve.
				clk.enter()
				go serve(m)
			case msgDone:
				mu.Lock()
				truncated = truncated || m.Truncated
				mu.Unlock()
				if m.Error != "" {
					return result(), errors.New(m.Error)
				}
				return result(), nil
			}
		}
	}
}

func serveCall(ctx context.Context, r Runner, opt Options, runID string, m msg) provider.Block {
	id := fmt.Sprintf("gorun_%s_%d", runID, m.ID)
	if m.Tool == ToolName {
		return provider.Block{Type: provider.BlockToolResult, ID: id, IsError: true,
			Content: "go_run cannot be called from inside a go_run program"}
	}
	args := m.Args
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	hctx := tool.WithOutputLimit(ctx, opt.HelperMax)
	hctx, span := sporetrace.StartTool(hctx, m.Tool, args)
	defer span.End()
	res := r.Run(hctx, provider.Block{Type: provider.BlockToolUse, ID: id, Name: m.Tool, Input: args})
	sporetrace.RecordToolResult(span, res.Content, res.IsError, res.Truncated)
	return res
}

func stoppedErr(cause error, opt Options) error {
	switch {
	case errors.Is(cause, errBudget):
		return fmt.Errorf("go_run timed out after %s", opt.Timeout)
	case errors.Is(cause, errCeiling):
		return fmt.Errorf("go_run hit the %s ceiling", opt.Ceiling)
	default:
		return fmt.Errorf("go_run cancelled: %w", cause)
	}
}

// validate refuses what yaegi would either reject with a worse message or
// run in a way the model did not intend.
func validate(src string) error {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		return err
	}
	if f.Name.Name != "main" {
		return fmt.Errorf("go_run programs must be package main, got package %s", f.Name.Name)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			return nil
		}
	}
	return errors.New("go_run programs must declare func main()")
}

func newRunID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// proc is a running child.
type proc struct {
	cmd    *exec.Cmd
	conn   *conn
	stderr *headBuffer
	once   sync.Once
	closer []*os.File
}

func startChild() (*proc, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("kernel: find own binary: %w", err)
	}
	childIn, parentOut, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	parentIn, childOut, err := os.Pipe()
	if err != nil {
		_ = childIn.Close()
		_ = parentOut.Close()
		return nil, err
	}
	stderr := &headBuffer{max: 64 << 10}
	cmd := exec.Command(exe) //nolint:gosec // G204: re-executes this binary, argument-free
	cmd.Env = []string{childEnv + "=1"}
	cmd.Dir = os.TempDir()
	cmd.ExtraFiles = []*os.File{childIn, childOut}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{childIn, parentOut, parentIn, childOut} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("kernel: start child: %w", err)
	}
	// The child holds its own copies of these ends now. Closing ours means
	// the parent's read sees EOF the moment the child dies.
	_ = childIn.Close()
	_ = childOut.Close()
	return &proc{cmd: cmd, conn: newConn(parentIn, parentOut), stderr: stderr, closer: []*os.File{parentIn, parentOut}}, nil
}

// stop kills the child and reaps it. It is safe to call more than once.
func (p *proc) stop() {
	p.once.Do(func() {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		for _, f := range p.closer {
			_ = f.Close()
		}
	})
}

func (p *proc) crashReport() string {
	s := strings.TrimSpace(p.stderr.String())
	// Keep the cause, drop the dump that follows it.
	for _, marker := range []string{"\ngoroutine ", "\nruntime stack:"} {
		if i := strings.Index(s, marker); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	if len(s) > crashReportBytes {
		s = s[:crashReportBytes] + "…"
	}
	if s == "" {
		s = "the kernel process exited without a result"
	}
	return s
}

// headBuffer keeps the first max bytes written to it.
type headBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room := h.max - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (h *headBuffer) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}

// clock measures interpreter time: it runs only while no helper call is in
// flight.
type clock struct {
	mu       sync.Mutex
	inFlight int
	used     time.Duration
	since    time.Time
}

func (c *clock) enter() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight == 0 {
		c.used += time.Since(c.since)
	}
	c.inFlight++
}

func (c *clock) exit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
	if c.inFlight == 0 {
		c.since = time.Now()
	}
}

func (c *clock) elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight > 0 {
		return c.used
	}
	return c.used + time.Since(c.since)
}

// watch cancels the run once the clock passes the budget.
func watch(ctx context.Context, c *clock, budget time.Duration, cancel context.CancelCauseFunc) {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.elapsed() > budget {
				cancel(errBudget)
				return
			}
		}
	}
}
