package cliproxy

import (
	"context"
	"path/filepath"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestScanAuthMaintenanceCandidates_CodexTerminalRefresh401Without429SkipsDeleteWhenProtectionEnabled(t *testing.T) {
	authDir := t.TempDir()
	service := &Service{
		cfg: &config.Config{
			AuthDir: authDir,
			AuthMaintenance: config.AuthMaintenanceConfig{
				Enable:                true,
				DeleteStatusCodes:     []int{401},
				Refresh401Requires429: true,
			},
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	filePath := filepath.Join(authDir, "terminal-refresh-401-no-429.json")
	auth := &coreauth.Auth{
		ID:       "terminal-refresh-401-no-429",
		FileName: filepath.Base(filePath),
		Provider: "codex",
		Status:   coreauth.StatusError,
		LastError: &coreauth.Error{
			Code:       codexauth.RefreshTokenReusedErrorCode,
			HTTPStatus: 401,
			Message:    "token refresh failed with status 401: refresh_token_reused",
		},
		Attributes: map[string]string{
			"path":      filePath,
			"plan_type": "free",
		},
		UpdatedAt:   timeNowForTest(),
		Unavailable: true,
	}
	if _, err := service.coreManager.Register(context.Background(), auth); err != nil {
		t.Fatalf("failed to register auth: %v", err)
	}

	candidates := service.scanAuthMaintenanceCandidates(timeNowForTest(), service.cfg.AuthMaintenance, authDir)
	if len(candidates) != 0 {
		t.Fatalf("expected terminal refresh 401 without 429 not to queue delete, got %#v", candidates)
	}
}

func TestScanAuthMaintenanceCandidates_CodexTerminalRefresh401Without429QueuesDeleteWhenProtectionDisabled(t *testing.T) {
	authDir := t.TempDir()
	service := &Service{
		cfg: &config.Config{
			AuthDir: authDir,
			AuthMaintenance: config.AuthMaintenanceConfig{
				Enable:                true,
				DeleteStatusCodes:     []int{401},
				Refresh401Requires429: false,
			},
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	filePath := filepath.Join(authDir, "terminal-refresh-401-no-429-direct-delete.json")
	auth := &coreauth.Auth{
		ID:       "terminal-refresh-401-no-429-direct-delete",
		FileName: filepath.Base(filePath),
		Provider: "codex",
		Status:   coreauth.StatusError,
		LastError: &coreauth.Error{
			Code:       codexauth.RefreshTokenReusedErrorCode,
			HTTPStatus: 401,
			Message:    "token refresh failed with status 401: refresh_token_reused",
		},
		Attributes: map[string]string{
			"path":      filePath,
			"plan_type": "free",
		},
		UpdatedAt:   timeNowForTest(),
		Unavailable: true,
	}
	if _, err := service.coreManager.Register(context.Background(), auth); err != nil {
		t.Fatalf("failed to register auth: %v", err)
	}

	candidates := service.scanAuthMaintenanceCandidates(timeNowForTest(), service.cfg.AuthMaintenance, authDir)
	if len(candidates) != 1 {
		t.Fatalf("expected terminal refresh 401 without 429 to queue delete when protection disabled, got %d", len(candidates))
	}
	if got := candidates[0].Reason; got != "terminal_refresh_token_reused" {
		t.Fatalf("expected semantic terminal reason, got %q", got)
	}
}
