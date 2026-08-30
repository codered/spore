package config

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestLearnRuleCreatesTheManagedBlock(t *testing.T) {
	p := write(t, `default_model = "anthropic/claude-opus-5"

[policy]
workspace = "/ws"
ask = ["fs_write"]
`)
	if err := LearnRule(p, "allow", `fs_write(path matches /ws/src/**)`); err != nil {
		t.Fatalf("LearnRule: %v", err)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, ManagedBegin) || !strings.Contains(text, ManagedEnd) {
		t.Fatalf("markers missing:\n%s", text)
	}
	// The user's own configuration is untouched above the marker.
	if !strings.Contains(text, `ask = ["fs_write"]`) {
		t.Errorf("hand-written config was lost:\n%s", text)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("the rewritten file no longer parses: %v", err)
	}
	if len(cfg.Policy.Learned.Allow) != 1 || !strings.Contains(cfg.Policy.Learned.Allow[0], "/ws/src/**") {
		t.Errorf("learned rules = %+v", cfg.Policy.Learned)
	}
}

func TestLearnRuleAppendsAndDeduplicates(t *testing.T) {
	p := write(t, "default_model = \"a/b\"\n")
	for i := 0; i < 2; i++ {
		if err := LearnRule(p, "allow", "fs_write"); err != nil {
			t.Fatal(err)
		}
	}
	if err := LearnRule(p, "deny", "shell_exec"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Policy.Learned.Allow) != 1 {
		t.Errorf("allow = %v, want the duplicate collapsed", cfg.Policy.Learned.Allow)
	}
	if len(cfg.Policy.Learned.Deny) != 1 {
		t.Errorf("deny = %v", cfg.Policy.Learned.Deny)
	}
	body, _ := os.ReadFile(p)
	if strings.Count(string(body), ManagedBegin) != 1 {
		t.Errorf("the managed block was duplicated:\n%s", body)
	}
}

func TestLearnRulePreservesTextAfterTheBlock(t *testing.T) {
	p := write(t, "default_model = \"a/b\"\n"+
		ManagedBegin+"\n[policy.learned]\nallow = [\"fs_read\"]\n"+ManagedEnd+"\n\n[trace]\nenabled = true\n")
	if err := LearnRule(p, "allow", "fs_write"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Trace.Enabled {
		t.Error("configuration after the managed block was lost")
	}
	if len(cfg.Policy.Learned.Allow) != 2 {
		t.Errorf("allow = %v, want both the existing and the new rule", cfg.Policy.Learned.Allow)
	}
}

func TestLearnRuleRefusesWhenMarkersAreDuplicated(t *testing.T) {
	body := "default_model = \"a/b\"\n\n" +
		ManagedBegin + "\n[policy.learned]\nallow = [\"fs_read\"]\n" + ManagedEnd + "\n\n" +
		ManagedBegin + "\n[policy.learned]\nallow = [\"stale\"]\n" + ManagedEnd + "\n"
	p := write(t, body)
	before, _ := os.ReadFile(p)
	err := LearnRule(p, "allow", "fs_write")
	if err == nil {
		t.Fatal("LearnRule wrote into a file carrying two managed blocks")
	}
	if !strings.Contains(err.Error(), "by hand") {
		t.Errorf("error %q must tell the user what to do about it", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("the file was modified despite the refusal")
	}
}

func TestLearnRuleNeverLeavesAnUnloadableConfig(t *testing.T) {
	// The marker text inside the user's own comment makes splitManaged miss
	// the real block. Whatever the function decides to do, the file it leaves
	// behind must still load — a config that will not parse locks the user
	// out of their own agent.
	p := write(t, "default_model = \"a/b\"\n# note: "+ManagedBegin+" in my own comment\n[trace]\nenabled = true\n")
	_ = LearnRule(p, "allow", "fs_read")
	if _, err := Load(p); err != nil {
		t.Fatalf("config no longer loads after the first LearnRule: %v", err)
	}
	// The second call now sees two markers and must refuse, not guess.
	if err := LearnRule(p, "allow", "fs_write"); err == nil {
		t.Error("LearnRule guessed at a file with an orphaned marker")
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("config no longer loads after the refused LearnRule: %v", err)
	}
}

func TestLearnRuleRejectsAnUnparseableRule(t *testing.T) {
	p := write(t, "default_model = \"a/b\"\n")
	// A rule that cannot be quoted safely must be refused rather than
	// corrupting the file.
	if err := LearnRule(p, "allow", "fs_write(path matches \"x\")"); err == nil {
		t.Error("LearnRule accepted a rule containing a quote")
	}
	if err := LearnRule(p, "sometimes", "fs_write"); err == nil {
		t.Error("LearnRule accepted an unknown decision")
	}
}

func TestLearnRuleIsSafeUnderConcurrentCallers(t *testing.T) {
	p := write(t, "default_model = \"a/b\"\n")
	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := LearnRule(p, "allow", fmt.Sprintf("fs_write_%02d", i)); err != nil {
				t.Errorf("LearnRule %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("config unloadable after concurrent writes: %v", err)
	}
	if len(cfg.Policy.Learned.Allow) != n {
		t.Errorf("learned %d rules, want %d — a concurrent write dropped one", len(cfg.Policy.Learned.Allow), n)
	}
}
