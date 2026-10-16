package config

import (
	"strings"
	"testing"
)

// agent_result waits for a child before it answers. How long it waits by
// default, and the most a model may ask for, come from [subagents].
func TestSubagentResultWaitDefaults(t *testing.T) {
	cfg := loadTestConfig(t, "")
	if cfg.Subagents.ResultWaitSeconds != 60 || cfg.Subagents.ResultMaxWaitSeconds != 300 {
		t.Errorf("result waits = %d / %d, want 60 / 300", cfg.Subagents.ResultWaitSeconds, cfg.Subagents.ResultMaxWaitSeconds)
	}
}

func TestSubagentResultWaitIsConfigurable(t *testing.T) {
	cfg := loadTestConfig(t, "[subagents]\nresult_wait_seconds = 20\nresult_max_wait_seconds = 90\n")
	if cfg.Subagents.ResultWaitSeconds != 20 || cfg.Subagents.ResultMaxWaitSeconds != 90 {
		t.Errorf("result waits = %d / %d, want 20 / 90", cfg.Subagents.ResultWaitSeconds, cfg.Subagents.ResultMaxWaitSeconds)
	}
	if cfg.Subagents.MaxConcurrent != 4 {
		t.Errorf("unset fields still take their defaults, max_concurrent = %d", cfg.Subagents.MaxConcurrent)
	}
}

func TestSubagentResultWaitRejectsNegatives(t *testing.T) {
	for _, body := range []string{
		"[subagents]\nresult_wait_seconds = -1\n",
		"[subagents]\nresult_max_wait_seconds = -1\n",
	} {
		_, err := loadTestConfigErr(t, body)
		if err == nil || !strings.Contains(err.Error(), "result_") {
			t.Errorf("%q: err = %v, want one naming the result wait", body, err)
		}
	}
}
