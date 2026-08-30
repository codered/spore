// Package config loads spore's single TOML configuration file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	DefaultModel string `toml:"default_model"`
	SystemPrompt string `toml:"system_prompt"`
	DataDir      string `toml:"data_dir"`
	// ShowCost appends the turn's attributed USD cost to the footer printed
	// after each turn. Off by default; cost is recorded per message either way.
	ShowCost  bool                      `toml:"show_cost"`
	Providers map[string]ProviderConfig `toml:"providers"`
	Routes    []Route                   `toml:"route"`
	Context   ContextConfig             `toml:"context"`
	Trace     TraceConfig               `toml:"trace"`
}

// ProviderConfig describes one upstream. Kind selects the adapter
// ("anthropic" or "openai"); prices are USD per million tokens and are used
// to attribute per-turn cost.
type ProviderConfig struct {
	Kind    string `toml:"kind"`
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`
	// WorkspaceID is sent as anthropic-workspace-id; identity-linked API
	// keys require it. Falls back to $ANTHROPIC_WORKSPACE_ID when unset.
	WorkspaceID string  `toml:"workspace_id"`
	PriceIn     float64 `toml:"price_in"`
	PriceOut    float64 `toml:"price_out"`
}

// Route maps call sites to a model ref. When is a regexp matched against the
// whole call-site name.
type Route struct {
	When  string `toml:"when"`
	Model string `toml:"model"`
}

type ContextConfig struct {
	MaxTokens  int     `toml:"max_tokens"`
	CompactAt  float64 `toml:"compact_at"`
	KeepRecent int     `toml:"keep_recent"`
}

type TraceConfig struct {
	Enabled    bool    `toml:"enabled"`
	Endpoint   string  `toml:"endpoint"`
	SampleRate float64 `toml:"sample_rate"`
	Redact     bool    `toml:"redact"`
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		SystemPrompt: "You are spore, a personal assistant running on the user's own machine. " +
			"Never name or speculate about the underlying model or provider that powers you.",
		DataDir:   filepath.Join(home, ".spore"),
		Providers: map[string]ProviderConfig{},
		Context:   ContextConfig{MaxTokens: 180_000, CompactAt: 0.75, KeepRecent: 12},
		Trace:     TraceConfig{Endpoint: "http://localhost:6006/v1/traces", SampleRate: 1.0},
	}
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolate replaces ${VAR} with the environment value, erroring when a
// referenced variable is unset so a missing key fails at load rather than at
// the first API call.
func interpolate(src string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(src, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	body, err := interpolate(string(raw))
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if _, err := toml.Decode(body, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Context.MaxTokens == 0 {
		cfg.Context.MaxTokens = Default().Context.MaxTokens
	}
	if cfg.Context.CompactAt == 0 {
		cfg.Context.CompactAt = Default().Context.CompactAt
	}
	if cfg.Context.KeepRecent == 0 {
		cfg.Context.KeepRecent = Default().Context.KeepRecent
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ValidateModelRef checks the "provider/model" shape.
func ValidateModelRef(ref string) error {
	if i := strings.Index(ref, "/"); i <= 0 || i == len(ref)-1 {
		return fmt.Errorf("model ref %q must be of the form provider/model", ref)
	}
	return nil
}

func (c *Config) Validate() error {
	if c.DefaultModel == "" {
		return fmt.Errorf("default_model is required")
	}
	if err := ValidateModelRef(c.DefaultModel); err != nil {
		return err
	}
	for i, r := range c.Routes {
		if r.When == "" {
			return fmt.Errorf("route %d: when is required", i)
		}
		if err := ValidateModelRef(r.Model); err != nil {
			return fmt.Errorf("route %d: %w", i, err)
		}
	}
	if c.Context.CompactAt <= 0 || c.Context.CompactAt >= 1 {
		return fmt.Errorf("context.compact_at must be between 0 and 1, got %v", c.Context.CompactAt)
	}
	return nil
}

// DBPath is the SQLite file backing every session.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "spore.db") }
