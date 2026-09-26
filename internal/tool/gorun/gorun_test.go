package gorun

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/kernel"
	"github.com/codered/spore/internal/provider"
)

// The kernel re-executes the running binary; under go test that is this
// test binary, so it must turn into a kernel child when asked to.
func TestMain(m *testing.M) {
	if kernel.IsChild() {
		os.Exit(kernel.ChildMain())
	}
	os.Exit(m.Run())
}

type okRunner struct{}

func (okRunner) Run(_ context.Context, call provider.Block) provider.Block {
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "fetched"}
}

func cfg() config.KernelConfig { return config.Default().Kernel }

func args(code string, timeout int) json.RawMessage {
	m := map[string]any{"code": code}
	if timeout > 0 {
		m["timeout_seconds"] = timeout
	}
	b, _ := json.Marshal(m)
	return b
}

func TestUnboundToolRefuses(t *testing.T) {
	_, err := New(cfg(), 30_000).Call(context.Background(), args("package main\nfunc main(){}", 0))
	if err == nil {
		t.Fatal("an unbound go_run ran a program")
	}
}

func TestRunsAProgram(t *testing.T) {
	tl := New(cfg(), 30_000)
	tl.Bind(okRunner{})
	out, err := tl.Call(context.Background(), args("package main\nimport \"fmt\"\nfunc main(){ fmt.Println(\"ok\") }", 0))
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok\n" {
		t.Errorf("out = %q, want %q (no footer when no helper ran)", out, "ok\n")
	}
}

func TestFooterListsHelperCalls(t *testing.T) {
	tl := New(cfg(), 30_000)
	tl.Bind(okRunner{})
	out, err := tl.Call(context.Background(), args("package main\nimport (\"fmt\"; \"spore\")\nfunc main(){ s, _ := spore.Fetch(\"https://x.test\"); fmt.Println(s) }", 0))
	if err != nil {
		t.Fatal(err)
	}
	if out != "fetched\n\n--- calls: web_fetch ok ---" {
		t.Errorf("out = %q", out)
	}
}

func TestTimeoutIsClampedToTheMaximum(t *testing.T) {
	c := cfg()
	c.MaxTimeoutSeconds = 1
	tl := New(c, 30_000)
	tl.Bind(okRunner{})
	start := time.Now()
	_, err := tl.Call(context.Background(), args("package main\nfunc main(){ for {} }", 9999))
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("err = %v, want a 1s timeout", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestErrorsCarryTheOutputSoFar(t *testing.T) {
	tl := New(cfg(), 30_000)
	tl.Bind(okRunner{})
	_, err := tl.Call(context.Background(), args("package main\nimport \"fmt\"\nfunc main(){ fmt.Println(\"partial\"); panic(\"bad\") }", 0))
	if err == nil || !strings.Contains(err.Error(), "partial") || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v, want the panic and the output printed before it", err)
	}
}

func TestSchemaAndName(t *testing.T) {
	tl := New(cfg(), 30_000)
	if tl.Name() != kernel.ToolName || tl.ReadOnly() {
		t.Errorf("name %q readOnly %v", tl.Name(), tl.ReadOnly())
	}
	if !json.Valid(tl.Schema()) {
		t.Error("schema is not valid JSON")
	}
}
