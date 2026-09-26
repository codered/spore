package config

import (
	"strings"
	"testing"
)

func TestKernelDefaultsFillIn(t *testing.T) {
	cfg := loadTestConfig(t, "")
	want := KernelConfig{Mode: KernelModeCode, TimeoutSeconds: 60, MaxTimeoutSeconds: 300, CeilingSeconds: 1800, HelperMaxBytes: 4 << 20}
	if cfg.Kernel != want {
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
