package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
)

// kernelTools offers a direct tool and go_run, as the real registry does.
type kernelTools struct{}

func (kernelTools) Specs() []provider.ToolSpec {
	return []provider.ToolSpec{
		{Name: "fs_read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "go_run", Description: "run Go", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "schedule_list", Description: "list jobs", Schema: json.RawMessage(`{"type":"object"}`)},
	}
}
func (kernelTools) ReadOnly(string) bool { return true }
func (kernelTools) Run(_ context.Context, call provider.Block) provider.Block {
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "ok"}
}

func runOnce(t *testing.T, mode string) provider.Request {
	t.Helper()
	script := provider.NewScript(provider.ScriptTurn{Text: "done"})
	a, st := harness(t, script, kernelTools{})
	a.Cfg.Kernel.Mode = mode
	sid, _ := st.CreateSession(context.Background(), "t", "")
	ch, err := a.Run(context.Background(), sid, "weather in London?")
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch)
	reqs := script.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times", len(reqs))
	}
	return reqs[0]
}

func TestCodeModeOffersOnlyGoRun(t *testing.T) {
	req := runOnce(t, config.KernelModeCode)
	if len(req.Tools) != 1 || req.Tools[0].Name != "go_run" {
		t.Fatalf("tools = %+v, want only go_run", req.Tools)
	}
	sys := systemText(req.System)
	if !strings.Contains(sys, "## Acting through go_run") {
		t.Error("system prompt lacks the go_run section")
	}
	if !strings.Contains(sys, "schedule_list") {
		t.Error("the go_run section does not list the tools reachable through spore.Call")
	}
}

func TestToolsModeSendsEveryToolAndNoReference(t *testing.T) {
	req := runOnce(t, config.KernelModeTools)
	if len(req.Tools) != 3 {
		t.Fatalf("tools = %+v, want all three", req.Tools)
	}
	if strings.Contains(systemText(req.System), "## Acting through go_run") {
		t.Error("tools mode must not carry the go_run section")
	}
}

// The reference is part of what the model is sent, so it must count toward
// the size that triggers compaction.
func TestKernelSectionCountsTowardTheSnapshotSize(t *testing.T) {
	cfg := config.Default().Context
	without := SnapshotTokens(Snapshot{System: "x"}, cfg)
	with := SnapshotTokens(Snapshot{System: "x", Kernel: strings.Repeat("k", 4000)}, cfg)
	if with-without < 900 {
		t.Errorf("a 4000-byte kernel section added %d tokens, want about 1000", with-without)
	}
}
