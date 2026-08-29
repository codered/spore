package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadInterpolatesEnvAndAppliesDefaults(t *testing.T) {
	t.Setenv("SPORE_TEST_KEY", "sk-secret")
	p := write(t, `
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind = "anthropic"
api_key = "${SPORE_TEST_KEY}"
price_in = 5.0
price_out = 25.0

[[route]]
when = "compaction|title|classify"
model = "ollama/qwen3:8b"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["anthropic"].APIKey; got != "sk-secret" {
		t.Errorf("APIKey = %q, want interpolated %q", got, "sk-secret")
	}
	if cfg.Context.KeepRecent == 0 || cfg.Context.CompactAt == 0 || cfg.Context.MaxTokens == 0 {
		t.Errorf("defaults not applied to Context: %+v", cfg.Context)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Model != "ollama/qwen3:8b" {
		t.Errorf("routes = %+v", cfg.Routes)
	}
}

func TestLoadRejectsMissingEnvVar(t *testing.T) {
	p := write(t, `
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind = "anthropic"
api_key = "${SPORE_DEFINITELY_UNSET_VAR}"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load succeeded with an unset env var; want error")
	}
}

func TestLoadRejectsBadModelRef(t *testing.T) {
	p := write(t, `default_model = "claude-opus-5"`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load accepted a model ref with no provider prefix; want error")
	}
}
