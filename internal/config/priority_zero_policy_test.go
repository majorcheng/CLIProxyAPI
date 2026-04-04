package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_PriorityZeroDisabledAPIKeysSanitized(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	data := []byte(`
api-keys:
  - "client-a"
priority-zero-disabled-api-keys:
  - " client-a "
  - "client-a"
  - ""
  - "client-b"
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	want := []string{"client-a", "client-b"}
	if len(cfg.PriorityZeroDisabledAPIKeys) != len(want) {
		t.Fatalf("PriorityZeroDisabledAPIKeys length = %d, want %d (%v)", len(cfg.PriorityZeroDisabledAPIKeys), len(want), cfg.PriorityZeroDisabledAPIKeys)
	}
	for i, value := range want {
		if cfg.PriorityZeroDisabledAPIKeys[i] != value {
			t.Fatalf("PriorityZeroDisabledAPIKeys[%d] = %q, want %q (%v)", i, cfg.PriorityZeroDisabledAPIKeys[i], value, cfg.PriorityZeroDisabledAPIKeys)
		}
	}
}

func TestLoadConfigOptional_PriorityZeroRoutingStrategySanitized(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	data := []byte(`
routing:
  priority-zero-strategy: " ff "
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if got := cfg.Routing.PriorityZeroStrategy; got != "fill-first" {
		t.Fatalf("Routing.PriorityZeroStrategy = %q, want %q", got, "fill-first")
	}
}
