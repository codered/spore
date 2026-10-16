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

// runOnceOn runs one turn in code mode, with tools_models set to listed,
// on a session whose chat model is ref ("" keeps the default).
func runOnceOn(t *testing.T, listed []string, ref string) (provider.Request, *Agent, string) {
	t.Helper()
	script := provider.NewScript(provider.ScriptTurn{Text: "done"})
	a, st := harness(t, script, kernelTools{})
	a.Cfg.Kernel.Mode = config.KernelModeCode
	a.Cfg.Kernel.ToolsModels = listed
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "")
	if ref != "" {
		if err := st.SetSessionModel(ctx, sid, "chat", ref); err != nil {
			t.Fatal(err)
		}
	}
	ch, err := a.Run(ctx, sid, "weather in London?")
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch)
	reqs := script.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times", len(reqs))
	}
	return reqs[0], a, sid
}

// LFM2.5 calls a native tool correctly but writes go_run programs that
// call tools as bare functions. A model that tools_models lists must get
// every tool directly and no go_run reference; any other model keeps code
// mode.
func TestToolsModelsPutsAMatchingSessionModelInToolsMode(t *testing.T) {
	req, a, sid := runOnceOn(t, []string{"test/model-b"}, "test/model-b")
	if len(req.Tools) != 3 {
		t.Fatalf("tools = %+v, want all three", req.Tools)
	}
	if strings.Contains(systemText(req.System), "## Acting through go_run") {
		t.Error("a tools-mode turn carries the go_run section")
	}
	// Compaction measures Snapshot, so it must leave the reference out too.
	snap, err := a.Snapshot(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Kernel != "" {
		t.Error("Snapshot carries the go_run reference that the request leaves out")
	}
}

func TestToolsModelsLeavesOtherModelsInCodeMode(t *testing.T) {
	req, _, _ := runOnceOn(t, []string{"test/model-b"}, "")
	if len(req.Tools) != 1 || req.Tools[0].Name != "go_run" {
		t.Fatalf("tools = %+v, want only go_run", req.Tools)
	}
}

// /model can move a session between modes. History from the other mode
// holds tool_use blocks for tools the new mode does not offer. The turn
// must still send that history, unchanged.
func TestSwitchingModeKeepsHistoryFromTheOtherMode(t *testing.T) {
	script := provider.NewScript(
		provider.ScriptTurn{ToolCalls: []provider.Block{
			{Type: provider.BlockToolUse, ID: "direct", Name: "schedule_list", Input: json.RawMessage(`{}`)},
		}},
		provider.ScriptTurn{Text: "listed"},
		provider.ScriptTurn{Text: "done"},
	)
	a, st := harness(t, script, kernelTools{})
	a.Cfg.Kernel.Mode = config.KernelModeCode
	a.Cfg.Kernel.ToolsModels = []string{"test/model-b"}
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "")
	if err := st.SetSessionModel(ctx, sid, "chat", "test/model-b"); err != nil {
		t.Fatal(err)
	}
	ch, err := a.Run(ctx, sid, "list jobs")
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch)

	if err := st.SetSessionModel(ctx, sid, "chat", ""); err != nil {
		t.Fatal(err)
	}
	ch, err = a.Run(ctx, sid, "again")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range collect(t, ch) {
		if ev.Type == EvError {
			t.Fatalf("the code-mode turn failed: %v", ev.Err)
		}
	}
	reqs := script.Requests()
	last := reqs[len(reqs)-1]
	if len(last.Tools) != 1 || last.Tools[0].Name != "go_run" {
		t.Fatalf("tools = %+v, want only go_run after the switch", last.Tools)
	}
	found := false
	for _, m := range last.Messages {
		for _, b := range m.Blocks {
			if b.Type == provider.BlockToolUse && b.Name == "schedule_list" {
				found = true
			}
		}
	}
	if !found {
		t.Error("the direct schedule_list call from tools mode is missing from history")
	}
}

// The request builder must use the mode Snapshot used, read once per
// round. A /model switch between two reads would otherwise send the go_run
// reference with every tool, or go_run alone with no reference.
func TestSnapshotRecordsTheModeItUsed(t *testing.T) {
	_, a, sid := runOnceOn(t, []string{"test/model-b"}, "test/model-b")
	snap, err := a.Snapshot(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != config.KernelModeTools {
		t.Errorf("Snapshot.Mode = %q, want %q", snap.Mode, config.KernelModeTools)
	}
	if err := a.Store.SetSessionModel(context.Background(), sid, "chat", ""); err != nil {
		t.Fatal(err)
	}
	snap, err = a.Snapshot(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Mode != config.KernelModeCode || snap.Kernel == "" {
		t.Errorf("Snapshot.Mode = %q with kernel section %v, want %q with the section", snap.Mode, snap.Kernel != "", config.KernelModeCode)
	}
}
