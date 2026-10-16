package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestKernelDefaultsFillIn(t *testing.T) {
	cfg := loadTestConfig(t, "")
	want := KernelConfig{Mode: KernelModeCode, TimeoutSeconds: 60, MaxTimeoutSeconds: 300, CeilingSeconds: 1800, HelperMaxBytes: 4 << 20}
	if !reflect.DeepEqual(cfg.Kernel, want) {
		t.Errorf("kernel defaults = %+v, want %+v", cfg.Kernel, want)
	}
}

func TestKernelToolsModeRoundTrips(t *testing.T) {
	cfg := loadTestConfig(t, "[kernel]\nmode = \"tools\"\ntimeout_seconds = 5\n")
	if cfg.Kernel.Mode != KernelModeTools || cfg.Kernel.TimeoutSeconds != 5 {
		t.Errorf("kernel = %+v", cfg.Kernel)
	}
	if cfg.Kernel.CeilingSeconds != 1800 {
		t.Errorf("unset fields still take their defaults, ceiling = %d", cfg.Kernel.CeilingSeconds)
	}
}

func TestKernelRejectsAnUnknownMode(t *testing.T) {
	_, err := loadTestConfigErr(t, "[kernel]\nmode = \"python\"\n")
	if err == nil || !strings.Contains(err.Error(), "kernel.mode") {
		t.Fatalf("err = %v, want one naming kernel.mode", err)
	}
}

func TestKernelRejectsNegativeNumbers(t *testing.T) {
	if _, err := loadTestConfigErr(t, "[kernel]\ntimeout_seconds = -1\n"); err == nil {
		t.Fatal("a negative timeout was accepted")
	}
}

// tools_models is a plain list of model names: no regexp, so nothing to
// escape. An entry names the full provider/model ref or the model id alone.
func TestKernelToolsModelsForcesToolsModeForAListedModel(t *testing.T) {
	cfg := loadTestConfig(t, "[kernel]\ntools_models = [\"studio/unsloth/LFM2.5-24B-A2B-GGUF\", \"gemma-4-e2b-qat\"]\n")
	for _, ref := range []string{"studio/unsloth/LFM2.5-24B-A2B-GGUF", "jetson/gemma-4-e2b-qat"} {
		if got := cfg.Kernel.ModeFor(ref); got != KernelModeTools {
			t.Errorf("ModeFor(%q) = %q, want %q", ref, got, KernelModeTools)
		}
	}
	for _, ref := range []string{
		"studio/ISTA-DASLab/Qwen3.8-27B-GSQ-RCO-GGUF",
		"studio/unsloth/LFM2.5-24B-A2B-GGUF-Q8", // a different model, not a prefix match
	} {
		if got := cfg.Kernel.ModeFor(ref); got != KernelModeCode {
			t.Errorf("ModeFor(%q) = %q, want %q", ref, got, KernelModeCode)
		}
	}
}

func TestKernelToolsModelsUnsetLeavesModeAlone(t *testing.T) {
	cfg := loadTestConfig(t, "[kernel]\nmode = \"tools\"\n")
	if got := cfg.Kernel.ModeFor("any/model"); got != KernelModeTools {
		t.Errorf("ModeFor = %q, want the configured mode %q", got, KernelModeTools)
	}
	cfg = loadTestConfig(t, "")
	if got := cfg.Kernel.ModeFor("any/model"); got != KernelModeCode {
		t.Errorf("ModeFor = %q, want the default mode %q", got, KernelModeCode)
	}
}

func TestKernelRejectsAnEmptyToolsModelsEntry(t *testing.T) {
	_, err := loadTestConfigErr(t, "[kernel]\ntools_models = [\"a/b\", \" \"]\n")
	if err == nil || !strings.Contains(err.Error(), "kernel.tools_models") {
		t.Fatalf("err = %v, want one naming kernel.tools_models", err)
	}
}

// The first form of tools_models was a regexp string. A config still
// written that way must fail to load, naming the key, not silently match
// nothing.
func TestKernelToolsModelsAsAStringIsRefused(t *testing.T) {
	_, err := loadTestConfigErr(t, "[kernel]\ntools_models = \"LFM2\\\\.5\"\n")
	if err == nil || !strings.Contains(err.Error(), "tools_models") {
		t.Fatalf("err = %v, want one naming tools_models", err)
	}
}
