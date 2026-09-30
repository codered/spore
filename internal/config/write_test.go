package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
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

func TestSetRecallBackendWritesAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("default_model = \"x/y\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetRecallBackend(path, RecallWeaviate); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Recall.Backend != RecallWeaviate {
		t.Errorf("backend %q after the write, want %q", cfg.Recall.Backend, RecallWeaviate)
	}
	// Setup must not eat the rest of the file.
	if cfg.DefaultModel != "x/y" {
		t.Errorf("default_model = %q, want it preserved", cfg.DefaultModel)
	}

	// Running setup twice must not leave two [recall] sections: duplicates
	// make the file fail to load, turning a successful setup into a broken
	// install.
	if err := SetRecallBackend(path, RecallSQLiteFTS); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(path)
	if n := strings.Count(string(body), "[recall]"); n != 1 {
		t.Errorf("file has %d [recall] sections, want 1:\n%s", n, body)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
	if cfg.Recall.Backend != RecallSQLiteFTS {
		t.Errorf("backend %q, want the second write to have taken", cfg.Recall.Backend)
	}
}

func TestSetRecallBackendKeepsAnExistingSectionsOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "default_model = \"x/y\"\n\n[recall]\nurl = \"http://box:8080\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetRecallBackend(path, RecallWeaviate); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Recall.URL != "http://box:8080" {
		t.Errorf("url = %q, want it preserved", cfg.Recall.URL)
	}
	if cfg.Recall.Backend != RecallWeaviate {
		t.Errorf("backend = %q, want it set", cfg.Recall.Backend)
	}
}

func TestSetRecallBackendRejectsAnUnknownBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("default_model = \"x/y\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetRecallBackend(path, "pinecone"); err == nil {
		t.Fatal("an unknown backend was written into the config")
	}
}

func TestSetTraceEnabledAddsSectionWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("default_model = \"p/m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetTraceEnabled(path, true); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	if !cfg.Trace.Enabled {
		t.Error("trace.enabled was not turned on")
	}
}

func TestSetTraceEnabledReplacesExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "default_model = \"p/m\"\n\n[trace]\nenabled = true\nsample_rate = 0.5\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetTraceEnabled(path, false); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trace.Enabled {
		t.Error("trace.enabled was not turned off")
	}
	// The rewrite owns one line and must not disturb its neighbours.
	if cfg.Trace.SampleRate != 0.5 {
		t.Errorf("sample_rate = %v, want 0.5 preserved", cfg.Trace.SampleRate)
	}
}

func TestSetTraceEnabledPreservesTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "# a comment the operator wrote\ndefault_model = \"p/m\"\n\n[recall]\nbackend = \"weaviate\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetTraceEnabled(path, true); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# a comment the operator wrote", "[recall]", "backend = \"weaviate\""} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rewrite lost %q:\n%s", want, out)
		}
	}
}

func TestSetSectionKeyLeavesSimilarlyNamedKeysAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "default_model = \"p/m\"\n\n[trace]\nenabled_at = \"never\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetTraceEnabled(path, true); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "enabled_at = \"never\"") {
		t.Errorf("a key that merely starts with the target name was overwritten:\n%s", out)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Trace.Enabled {
		t.Errorf("trace.enabled was not set:\n%s", out)
	}
}

func TestUnlearnRuleRemovesOnlyThatLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	head := "# my config\n[policy]\ndefault = \"ask\"\nallow = [\"web_fetch\"]\n"
	if err := os.WriteFile(path, []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ d, rule string }{{"allow", "fs_write(path under /ws/**)"}, {"allow", "web_search"}, {"deny", "shell_exec(matches wget)"}} {
		if err := LearnRule(path, r.d, r.rule); err != nil {
			t.Fatal(err)
		}
	}
	if err := UnlearnRule(path, "allow", "web_search"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	body := string(raw)
	if !strings.HasPrefix(body, head) {
		t.Errorf("text before the block changed:\n%s", body)
	}
	l, err := ReadLearned(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Allow) != 1 || l.Allow[0] != "fs_write(path under /ws/**)" || len(l.Deny) != 1 {
		t.Errorf("learned after unlearn = %+v", l)
	}
	var probe Config
	if _, err := toml.Decode(body, &probe); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
}

func TestUnlearnRuleRefusesARuleThatWasNotLearned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[policy]\nallow = [\"web_fetch\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LearnRule(path, "allow", "web_search"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for _, c := range []struct{ d, rule string }{
		{"allow", "web_fetch"}, // hand-written, outside the block
		{"deny", "web_search"}, // learned, but under another decision
		{"allow", "nothing"},
	} {
		if err := UnlearnRule(path, c.d, c.rule); !errors.Is(err, ErrNotLearned) {
			t.Errorf("UnlearnRule(%s, %s) = %v, want ErrNotLearned", c.d, c.rule, err)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("a refused unlearn changed the file")
	}
}

func TestUnlearnRuleIgnoresSurroundingWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[policy]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LearnRule(path, "allow", "web_search "); err != nil {
		t.Fatal(err)
	}
	if err := UnlearnRule(path, "allow", "web_search"); err != nil {
		t.Fatalf("the engine shows rules trimmed, so a trimmed revoke must match: %v", err)
	}
	if l, _ := ReadLearned(path); len(l.Allow) != 0 {
		t.Errorf("allow = %v, want empty", l.Allow)
	}
}

func TestUnlearnRuleRejectsABadDecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(path, []byte("[policy]\n"), 0o600)
	if err := UnlearnRule(path, "maybe", "x"); err == nil || errors.Is(err, ErrNotLearned) {
		t.Fatalf("err = %v, want a decision error", err)
	}
}

func TestReadLearned(t *testing.T) {
	dir := t.TempDir()
	none := filepath.Join(dir, "none.toml")
	_ = os.WriteFile(none, []byte("[policy]\n"), 0o600)
	if l, err := ReadLearned(none); err != nil || len(l.Allow)+len(l.Ask)+len(l.Deny) != 0 {
		t.Errorf("no block: %+v %v", l, err)
	}
	one := filepath.Join(dir, "one.toml")
	_ = os.WriteFile(one, []byte("[policy]\n"), 0o600)
	_ = LearnRule(one, "ask", "memory")
	if l, err := ReadLearned(one); err != nil || len(l.Ask) != 1 || l.Ask[0] != "memory" {
		t.Errorf("one block: %+v %v", l, err)
	}
	two := filepath.Join(dir, "two.toml")
	_ = os.WriteFile(two, []byte(ManagedBegin+"\n"+ManagedEnd+"\n"+ManagedBegin+"\n"+ManagedEnd+"\n"), 0o600)
	if _, err := ReadLearned(two); err == nil {
		t.Error("two markers must be refused")
	}
}

func TestBaselineDenyIsACopy(t *testing.T) {
	b := BaselineDeny()
	if len(b) == 0 || b[0] != "fs_*(path outside workspace)" {
		t.Fatalf("BaselineDeny() = %v", b)
	}
	b[0] = "changed"
	if BaselineDeny()[0] == "changed" {
		t.Error("BaselineDeny must return a copy")
	}
}
