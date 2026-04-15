package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigOptional_ClientAPIKeysSanitized(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	data := []byte(`
api-keys:
  - " client-a "
  - key: "client-b"
    max-priority: 0
  - key: "client-b"
    max-priority: 9
  - ""
  - key: ""
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if len(cfg.APIKeys) != 2 {
		t.Fatalf("APIKeys length = %d, want 2 (%v)", len(cfg.APIKeys), cfg.APIKeys)
	}
	if got := cfg.APIKeys[0]; got != "client-a" {
		t.Fatalf("APIKeys[0].Key = %q, want %q", got, "client-a")
	}
	if got := cfg.APIKeys[1]; got != "client-b" {
		t.Fatalf("APIKeys[1].Key = %q, want %q", got, "client-b")
	}

	entries := cfg.ClientAPIKeyEntries()
	if len(entries) != 2 {
		t.Fatalf("ClientAPIKeyEntries length = %d, want 2 (%v)", len(entries), entries)
	}
	if entries[0].MaxPriority != nil {
		t.Fatalf("ClientAPIKeyEntries[0].MaxPriority = %v, want nil", *entries[0].MaxPriority)
	}
	if entries[1].MaxPriority == nil || *entries[1].MaxPriority != 0 {
		t.Fatalf("ClientAPIKeyEntries[1].MaxPriority = %v, want 0", entries[1].MaxPriority)
	}
}

func TestLoadConfigOptional_DeprecatedPriorityZeroDisabledAPIKeysRejected(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	data := []byte(`
api-keys:
  - "client-a"
priority-zero-disabled-api-keys:
  - "client-a"
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	_, err := LoadConfigOptional(configPath, false)
	if err == nil {
		t.Fatal("LoadConfigOptional() error = nil, want invalid config")
	}
	if !strings.Contains(err.Error(), "priority-zero-disabled-api-keys") {
		t.Fatalf("LoadConfigOptional() error = %v, want mention deprecated field", err)
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

func TestSaveConfigPreserveComments_PersistsClientAPIKeyObjectShape(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("api-keys:\n  - legacy\n"), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg := &Config{}
	cfg.SetClientAPIKeyEntries([]ClientAPIKey{
		{Key: "legacy"},
		{Key: "beta", MaxPriority: intPtr(7)},
	})
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "- legacy") {
		t.Fatalf("config content = %s, want legacy string item", content)
	}
	if !strings.Contains(content, "key: beta") || !strings.Contains(content, "max-priority: 7") {
		t.Fatalf("config content = %s, want object api-key entry", content)
	}
}

func intPtr(v int) *int { return &v }
