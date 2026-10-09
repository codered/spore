package config

import (
	"os"
	"strings"
	"testing"
)

const routingBase = `default_model = "studio/big"

[[route]]
when  = "title"
model = "studio/small"

[policy]
workspace = "/ws"
`

func TestSetRoutingOverrideCreatesTheBlockAndLoadApplies(t *testing.T) {
	p := write(t, routingBase)
	if err := SetRoutingOverride(p, "title", "jetson/unsloth/gemma-4"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	text := string(body)
	if !strings.Contains(text, RoutingBegin) || !strings.Contains(text, RoutingEnd) {
		t.Fatalf("no managed routing block:\n%s", text)
	}
	if !strings.HasPrefix(text, routingBase) {
		t.Fatalf("text before the block changed:\n%s", text)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Routing.Override["title"]; got != "jetson/unsloth/gemma-4" {
		t.Fatalf("Routing.Override[title] = %q", got)
	}
}

func TestSetRoutingOverrideReplacesAndClears(t *testing.T) {
	p := write(t, routingBase)
	if err := LearnRule(p, "allow", "fs_read"); err != nil {
		t.Fatal(err)
	}
	withPolicy, _ := os.ReadFile(p)

	if err := SetRoutingOverride(p, "title", "a/one"); err != nil {
		t.Fatal(err)
	}
	if err := SetRoutingOverride(p, "compaction", "a/two"); err != nil {
		t.Fatal(err)
	}
	if err := SetRoutingOverride(p, "title", "a/three"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Override["title"] != "a/three" || cfg.Routing.Override["compaction"] != "a/two" {
		t.Fatalf("overrides = %v", cfg.Routing.Override)
	}
	if len(cfg.Policy.Learned.Allow) != 1 {
		t.Fatalf("policy block lost: %+v", cfg.Policy.Learned)
	}

	if err := SetRoutingOverride(p, "title", ""); err != nil {
		t.Fatal(err)
	}
	if err := SetRoutingOverride(p, "compaction", ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if strings.Contains(string(after), RoutingBegin) {
		t.Fatalf("empty block was left behind:\n%s", after)
	}
	if strings.TrimRight(string(after), "\n") != strings.TrimRight(string(withPolicy), "\n") {
		t.Fatalf("file not restored after clearing every override.\nbefore:\n%s\nafter:\n%s", withPolicy, after)
	}
}

func TestSetRoutingOverrideClearingNothingWritesNothing(t *testing.T) {
	p := write(t, routingBase)
	if err := SetRoutingOverride(p, "title", ""); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	if string(body) != routingBase {
		t.Fatalf("file changed:\n%s", body)
	}
}

func TestSetRoutingOverrideRefusesBadInput(t *testing.T) {
	p := write(t, routingBase)
	for _, tc := range []struct{ site, ref string }{
		{"chat", "a/b"},      // per-session site
		{"subagent", "a/b"},  // per-session site
		{"nope", "a/b"},      // not a site
		{"title", "noslash"}, // not provider/model
		{"title", `a/"x`},    // cannot be written as a basic string
	} {
		if err := SetRoutingOverride(p, tc.site, tc.ref); err == nil {
			t.Errorf("SetRoutingOverride(%q, %q) succeeded", tc.site, tc.ref)
		}
	}
}

func TestLoadRejectsAnOverrideForAPerSessionSite(t *testing.T) {
	p := write(t, routingBase+"\n[routing.override]\nchat = \"a/b\"\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "routing.override.chat") {
		t.Fatalf("Load error = %v, want one naming routing.override.chat", err)
	}
}

func TestConfiguredRefs(t *testing.T) {
	p := write(t, routingBase+"\n[routing.override]\ncompaction = \"c/z\"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cfg.ConfiguredRefs(), ",")
	if got != "studio/big,studio/small,c/z" {
		t.Fatalf("ConfiguredRefs = %s", got)
	}
}
