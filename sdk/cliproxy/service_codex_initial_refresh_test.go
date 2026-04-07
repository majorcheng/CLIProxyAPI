package cliproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	authstore "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type codexInitialRefreshPersistenceExecutor struct{}

func (codexInitialRefreshPersistenceExecutor) Identifier() string { return "codex" }

func (codexInitialRefreshPersistenceExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (codexInitialRefreshPersistenceExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (codexInitialRefreshPersistenceExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	updated := auth.Clone()
	if updated == nil {
		updated = &coreauth.Auth{Provider: "codex"}
	}
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["type"] = "codex"
	updated.Metadata["email"] = "watcher@example.com"
	updated.Metadata["access_token"] = "rotated-access-token"
	updated.Metadata["refresh_token"] = "rotated-refresh-token"
	updated.Metadata["expired"] = time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	updated.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	return updated, nil
}

func (codexInitialRefreshPersistenceExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (codexInitialRefreshPersistenceExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

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

func TestCodexInitialRefreshTrigger_PersistsRefreshResultEvenWhenCallerSkipsPersist(t *testing.T) {
	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "watcher-codex.json")

	// 先落一份“watcher 已经看到的磁盘源文件”，模拟真实问题场景：
	// manager 更新路径本身带 skipPersist，但其后派生出的初始 refresh 成功结果必须回写到这份文件。
	initialFile := map[string]any{
		"type":          "codex",
		"email":         "watcher@example.com",
		"access_token":  "original-access-token",
		"refresh_token": "original-refresh-token",
		"expired":       time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		"cli_proxy_codex_initial_refresh_pending": true,
	}
	raw, err := json.Marshal(initialFile)
	if err != nil {
		t.Fatalf("marshal initial auth file: %v", err)
	}
	if err := os.WriteFile(authPath, raw, 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	store := authstore.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	cfg := &config.Config{
		AuthDir: authDir,
		SDKConfig: config.SDKConfig{
			CodexInitialRefreshOnLoad: true,
		},
	}
	manager.SetConfig(cfg)
	manager.RegisterExecutor(codexInitialRefreshPersistenceExecutor{})

	auth := &coreauth.Auth{
		ID:       "watcher-codex.json",
		FileName: "watcher-codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "watcher@example.com",
			"access_token":  "original-access-token",
			"refresh_token": "original-refresh-token",
			"expired":       time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		},
	}
	coreauth.MarkCodexInitialRefreshPendingForNewFile(auth)

	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth with skipPersist: %v", err)
	}
	manager.TriggerCodexInitialRefreshOnLoadIfNeeded(coreauth.WithSkipPersist(context.Background()), auth.ID)

	var persisted map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, errRead := os.ReadFile(authPath)
		if errRead == nil && len(data) > 0 {
			var current map[string]any
			if errUnmarshal := json.Unmarshal(data, &current); errUnmarshal == nil {
				if got, _ := current["refresh_token"].(string); got == "rotated-refresh-token" {
					persisted = current
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected initial refresh result to persist to disk despite skipPersist caller")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got, _ := persisted["refresh_token"].(string); got != "rotated-refresh-token" {
		t.Fatalf("persisted refresh_token = %q, want %q", got, "rotated-refresh-token")
	}
	if got, _ := persisted["access_token"].(string); got != "rotated-access-token" {
		t.Fatalf("persisted access_token = %q, want %q", got, "rotated-access-token")
	}
	if pending, _ := persisted["cli_proxy_codex_initial_refresh_pending"].(bool); pending {
		t.Fatal("expected persisted auth file to clear initial refresh pending flag")
	}
	if got, _ := persisted["last_refresh"].(string); got == "" {
		t.Fatal("expected persisted auth file to contain last_refresh after successful refresh")
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %q to remain registered", auth.ID)
	}
	if got, _ := updated.Metadata["refresh_token"].(string); got != "rotated-refresh-token" {
		t.Fatalf("manager refresh_token = %q, want %q", got, "rotated-refresh-token")
	}
	if coreauth.CodexInitialRefreshPending(updated) {
		t.Fatal("expected manager state to clear initial refresh pending after persistence")
	}
}

func TestServiceApplyCoreAuthAddOrUpdate_PreservesRuntimeQuota429StateAgainstCleanIncomingSnapshot(t *testing.T) {
	authID := "quota-codex.json"
	modelID := "gpt-5.4"
	authDir := t.TempDir()
	nextRetry := time.Now().Add(30 * time.Minute)

	reg := GlobalModelRegistry()
	reg.UnregisterClient(authID)
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{
		cfg:         &config.Config{AuthDir: authDir},
		coreManager: manager,
	}

	existing := &coreauth.Auth{
		ID:                authID,
		FileName:          authID,
		Provider:          "codex",
		Status:            coreauth.StatusError,
		StatusMessage:     "quota exhausted",
		Unavailable:       true,
		FailureHTTPStatus: http.StatusTooManyRequests,
		NextRetryAfter:    nextRetry,
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: nextRetry,
			StrikeCount:   3,
		},
		Attributes: map[string]string{
			"path": filepath.Join(authDir, authID),
		},
		ModelStates: map[string]*coreauth.ModelState{
			modelID: {
				Status:            coreauth.StatusError,
				StatusMessage:     "quota exhausted",
				Unavailable:       true,
				FailureHTTPStatus: http.StatusTooManyRequests,
				NextRetryAfter:    nextRetry,
				LastError: &coreauth.Error{
					HTTPStatus: http.StatusTooManyRequests,
					Message:    `{"error":{"type":"usage_limit_reached","message":"quota"}}`,
				},
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: nextRetry,
					StrikeCount:   3,
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), existing); err != nil {
		t.Fatalf("register existing auth: %v", err)
	}

	incoming := &coreauth.Auth{
		ID:       authID,
		FileName: authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filepath.Join(authDir, authID),
		},
		Metadata: map[string]any{
			"type":  "codex",
			"email": "quota@example.com",
		},
	}
	service.applyCoreAuthAddOrUpdate(context.Background(), incoming)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %q to remain registered", authID)
	}
	if updated.Status != coreauth.StatusError {
		t.Fatalf("status = %q, want %q", updated.Status, coreauth.StatusError)
	}
	if updated.StatusMessage != "quota exhausted" {
		t.Fatalf("status_message = %q, want %q", updated.StatusMessage, "quota exhausted")
	}
	if !updated.Unavailable {
		t.Fatal("expected unavailable=true after add/update")
	}
	if updated.FailureHTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("FailureHTTPStatus = %d, want %d", updated.FailureHTTPStatus, http.StatusTooManyRequests)
	}
	if !updated.Quota.Exceeded {
		t.Fatal("expected quota exceeded to remain true after add/update")
	}
	state := updated.ModelStates[modelID]
	if state == nil {
		t.Fatalf("expected model state %q to remain present", modelID)
	}
	if state.Status != coreauth.StatusError {
		t.Fatalf("model status = %q, want %q", state.Status, coreauth.StatusError)
	}
	if !state.Unavailable {
		t.Fatal("expected model unavailable=true after add/update")
	}
	if state.FailureHTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("model FailureHTTPStatus = %d, want %d", state.FailureHTTPStatus, http.StatusTooManyRequests)
	}
	if !state.Quota.Exceeded {
		t.Fatal("expected model quota exceeded to remain true after add/update")
	}
}
