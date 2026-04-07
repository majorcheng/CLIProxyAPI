package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_AuthMaintenancePreserves429StatusCode(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	data := []byte(`
auth-maintenance:
  enable: true
  delete-status-codes: [401, 429, 429, 700, 0]
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	want := []int{401, 429}
	if len(cfg.AuthMaintenance.DeleteStatusCodes) != len(want) {
		t.Fatalf("DeleteStatusCodes length = %d, want %d (%v)", len(cfg.AuthMaintenance.DeleteStatusCodes), len(want), cfg.AuthMaintenance.DeleteStatusCodes)
	}
	for i, code := range want {
		if cfg.AuthMaintenance.DeleteStatusCodes[i] != code {
			t.Fatalf("DeleteStatusCodes[%d] = %d, want %d (%v)", i, cfg.AuthMaintenance.DeleteStatusCodes[i], code, cfg.AuthMaintenance.DeleteStatusCodes)
		}
	}
}

func TestLoadConfigOptional_AuthMaintenanceDefaultDoesNotDelete429WhenUnconfigured(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	data := []byte(`
auth-maintenance:
  enable: true
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	want := []int{401, 402, 403, 404}
	if len(cfg.AuthMaintenance.DeleteStatusCodes) != len(want) {
		t.Fatalf("DeleteStatusCodes length = %d, want %d (%v)", len(cfg.AuthMaintenance.DeleteStatusCodes), len(want), cfg.AuthMaintenance.DeleteStatusCodes)
	}
	for i, code := range want {
		if cfg.AuthMaintenance.DeleteStatusCodes[i] != code {
			t.Fatalf("DeleteStatusCodes[%d] = %d, want %d (%v)", i, cfg.AuthMaintenance.DeleteStatusCodes[i], code, cfg.AuthMaintenance.DeleteStatusCodes)
		}
	}
	if cfg.AuthMaintenance.DeleteQuotaExceeded {
		t.Fatal("expected delete-quota-exceeded to stay disabled when config omits it")
	}
	if !cfg.AuthMaintenance.Refresh401Requires429 {
		t.Fatal("expected refresh-401-requires-429 to default to true when config omits it")
	}
	if cfg.AuthMaintenance.QuotaStrikeThreshold != 6 {
		t.Fatalf("QuotaStrikeThreshold = %d, want 6", cfg.AuthMaintenance.QuotaStrikeThreshold)
	}
}

func TestLoadConfigOptional_AuthMaintenanceAllowsDisablingRefresh401Requires429(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	data := []byte(`
auth-maintenance:
  enable: true
  refresh-401-requires-429: false
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if cfg.AuthMaintenance.Refresh401Requires429 {
		t.Fatal("expected refresh-401-requires-429=false to be preserved")
	}
}
