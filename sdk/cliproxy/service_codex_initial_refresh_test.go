package cliproxy

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestServiceApplyCoreAuthAddOrUpdate_MarksNewCodexFileInitialRefreshPending(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{
		cfg:         &config.Config{AuthDir: t.TempDir(), SDKConfig: config.SDKConfig{CodexInitialRefreshOnLoad: true}},
		coreManager: manager,
	}

	auth := &coreauth.Auth{
		ID:       "new-codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
			"expired":       time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		},
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), auth)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %q to be registered", auth.ID)
	}
	if !coreauth.CodexInitialRefreshPending(updated) {
		t.Fatal("expected new codex file auth to be marked as pending initial refresh")
	}
}

func TestServiceApplyCoreAuthAddOrUpdate_PreservesExistingCodexMetadataWhenPendingStateMismatches(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{
		cfg:         &config.Config{AuthDir: t.TempDir(), SDKConfig: config.SDKConfig{CodexInitialRefreshOnLoad: true}},
		coreManager: manager,
	}

	current := &coreauth.Auth{
		ID:       "codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"refresh_token": "new-refresh-token",
			"access_token":  "new-access-token",
			"last_refresh":  time.Now().Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), current); err != nil {
		t.Fatalf("register existing auth: %v", err)
	}

	stale := current.Clone()
	stale.Metadata["refresh_token"] = "old-refresh-token"
	coreauth.MarkCodexInitialRefreshPendingForNewFile(stale)

	service.applyCoreAuthAddOrUpdate(context.Background(), stale)

	updated, ok := manager.GetByID(current.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %q to remain registered", current.ID)
	}
	if got, _ := updated.Metadata["refresh_token"].(string); got != "new-refresh-token" {
		t.Fatalf("refresh_token = %q, want %q", got, "new-refresh-token")
	}
	if coreauth.CodexInitialRefreshPending(updated) {
		t.Fatal("expected stale pending metadata not to resurrect initial refresh state")
	}
}
