package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	authstore "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSaveCodexTokenRecord_ReusesExistingAccountFile(t *testing.T) {
	authDir := t.TempDir()
	handler := newCodexSaveTestHandler(authDir)

	registeredAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	existingPath := filepath.Join(authDir, "codex-old@example.com-plus.json")
	writeCodexAuthFixture(t, existingPath, map[string]any{
		"type":                                "codex",
		"account_id":                          "acct_123",
		"email":                               "old@example.com",
		"disabled":                            true,
		"proxy_url":                           "http://proxy.example.com",
		coreauth.FirstRegisteredAtMetadataKey: registeredAt.Format(time.RFC3339Nano),
	})

	savedPath, err := handler.saveCodexTokenRecord(context.Background(), &codex.CodexTokenStorage{
		AccessToken:  "new-access-token",
		RefreshToken: "new-refresh-token",
		AccountID:    "acct_123",
		Email:        "new@example.com",
	}, "plus", "")
	if err != nil {
		t.Fatalf("saveCodexTokenRecord() error = %v", err)
	}
	if savedPath != existingPath {
		t.Fatalf("saved path = %q, want %q", savedPath, existingPath)
	}

	files := codexJSONFiles(t, authDir)
	if len(files) != 1 || files[0] != existingPath {
		t.Fatalf("auth files = %#v, want only %q", files, existingPath)
	}

	payload := readCodexAuthFixture(t, existingPath)
	if got := stringValue(payload, "email"); got != "new@example.com" {
		t.Fatalf("email = %q, want %q", got, "new@example.com")
	}
	if got := stringValue(payload, "access_token"); got != "new-access-token" {
		t.Fatalf("access_token = %q, want %q", got, "new-access-token")
	}
	if got := stringValue(payload, "refresh_token"); got != "new-refresh-token" {
		t.Fatalf("refresh_token = %q, want %q", got, "new-refresh-token")
	}
	if got := stringValue(payload, "proxy_url"); got != "http://proxy.example.com" {
		t.Fatalf("proxy_url = %q, want %q", got, "http://proxy.example.com")
	}
	if disabled, _ := payload["disabled"].(bool); !disabled {
		t.Fatal("expected disabled flag to be preserved")
	}
	if got := stringValue(payload, coreauth.FirstRegisteredAtMetadataKey); got != registeredAt.Format(time.RFC3339Nano) {
		t.Fatalf("%s = %q, want %q", coreauth.FirstRegisteredAtMetadataKey, got, registeredAt.Format(time.RFC3339Nano))
	}
}

func TestSaveCodexTokenRecord_PrefersOldestExistingDuplicate(t *testing.T) {
	authDir := t.TempDir()
	handler := newCodexSaveTestHandler(authDir)

	olderPath := filepath.Join(authDir, "codex-older@example.com-plus.json")
	newerPath := filepath.Join(authDir, "codex-newer@example.com-plus.json")
	writeCodexAuthFixture(t, olderPath, map[string]any{
		"type":                                "codex",
		"account_id":                          "acct_dup",
		"email":                               "older@example.com",
		coreauth.FirstRegisteredAtMetadataKey: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
	})
	writeCodexAuthFixture(t, newerPath, map[string]any{
		"type":                                "codex",
		"account_id":                          "acct_dup",
		"email":                               "newer@example.com",
		coreauth.FirstRegisteredAtMetadataKey: time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
	})

	savedPath, err := handler.saveCodexTokenRecord(context.Background(), &codex.CodexTokenStorage{
		AccessToken:  "fresh-token",
		RefreshToken: "fresh-refresh",
		AccountID:    "acct_dup",
		Email:        "latest@example.com",
	}, "plus", "")
	if err != nil {
		t.Fatalf("saveCodexTokenRecord() error = %v", err)
	}
	if savedPath != olderPath {
		t.Fatalf("saved path = %q, want oldest duplicate %q", savedPath, olderPath)
	}

	files := codexJSONFiles(t, authDir)
	if len(files) != 2 {
		t.Fatalf("auth files len = %d, want %d", len(files), 2)
	}
	payload := readCodexAuthFixture(t, olderPath)
	if got := stringValue(payload, "email"); got != "latest@example.com" {
		t.Fatalf("older file email = %q, want %q", got, "latest@example.com")
	}
}

func TestSaveCodexTokenRecord_ConcurrentSameAccountCreatesSingleFile(t *testing.T) {
	authDir := t.TempDir()
	handler := newCodexSaveTestHandler(authDir)

	storages := []*codex.CodexTokenStorage{
		{
			AccessToken:  "access-a",
			RefreshToken: "refresh-a",
			AccountID:    "acct_race",
			Email:        "alpha@example.com",
		},
		{
			AccessToken:  "access-b",
			RefreshToken: "refresh-b",
			AccountID:    "acct_race",
			Email:        "beta@example.com",
		},
	}

	paths := make([]string, len(storages))
	errs := make([]error, len(storages))
	var wg sync.WaitGroup
	for i := range storages {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			paths[index], errs[index] = handler.saveCodexTokenRecord(context.Background(), storages[index], "plus", "")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("saveCodexTokenRecord[%d]() error = %v", i, err)
		}
	}
	if paths[0] == "" || paths[1] == "" || paths[0] != paths[1] {
		t.Fatalf("saved paths = %#v, want identical non-empty paths", paths)
	}

	files := codexJSONFiles(t, authDir)
	if len(files) != 1 {
		t.Fatalf("auth files len = %d, want %d", len(files), 1)
	}
	if files[0] != paths[0] {
		t.Fatalf("stored file = %q, want %q", files[0], paths[0])
	}
}

func TestWriteAuthFile_CodexSameAccountReusesExistingFile(t *testing.T) {
	authDir := t.TempDir()
	handler := newCodexSaveTestHandler(authDir)

	registeredAt := time.Date(2026, time.March, 3, 4, 5, 6, 0, time.UTC)
	existingPath := filepath.Join(authDir, "codex-old@example.com-plus.json")
	writeCodexAuthFixture(t, existingPath, map[string]any{
		"type":                                "codex",
		"account_id":                          "acct_upload",
		"email":                               "old@example.com",
		"disabled":                            true,
		"proxy_url":                           "http://proxy.example.com",
		coreauth.FirstRegisteredAtMetadataKey: registeredAt.Format(time.RFC3339Nano),
	})

	savedName, err := handler.writeAuthFile(
		context.Background(),
		"codex-new@example.com-plus.json",
		[]byte(`{"type":"codex","account_id":"acct_upload","email":"new@example.com","access_token":"upload-access"}`),
	)
	if err != nil {
		t.Fatalf("writeAuthFile() error = %v", err)
	}
	if savedName != filepath.Base(existingPath) {
		t.Fatalf("saved name = %q, want %q", savedName, filepath.Base(existingPath))
	}

	files := codexJSONFiles(t, authDir)
	if len(files) != 1 || files[0] != existingPath {
		t.Fatalf("auth files = %#v, want only %q", files, existingPath)
	}

	payload := readCodexAuthFixture(t, existingPath)
	if got := stringValue(payload, "email"); got != "new@example.com" {
		t.Fatalf("email = %q, want %q", got, "new@example.com")
	}
	if got := stringValue(payload, "access_token"); got != "upload-access" {
		t.Fatalf("access_token = %q, want %q", got, "upload-access")
	}
	if disabled, _ := payload["disabled"].(bool); !disabled {
		t.Fatal("expected disabled flag to be preserved when upload omits it")
	}
	if got := stringValue(payload, "proxy_url"); got != "http://proxy.example.com" {
		t.Fatalf("proxy_url = %q, want %q", got, "http://proxy.example.com")
	}
	if got := stringValue(payload, coreauth.FirstRegisteredAtMetadataKey); got != registeredAt.Format(time.RFC3339Nano) {
		t.Fatalf("%s = %q, want %q", coreauth.FirstRegisteredAtMetadataKey, got, registeredAt.Format(time.RFC3339Nano))
	}
}

func TestWriteAuthFile_CodexUploadExplicitDisabledOverridesExisting(t *testing.T) {
	authDir := t.TempDir()
	handler := newCodexSaveTestHandler(authDir)

	existingPath := filepath.Join(authDir, "codex-old@example.com-plus.json")
	writeCodexAuthFixture(t, existingPath, map[string]any{
		"type":       "codex",
		"account_id": "acct_upload_override",
		"email":      "old@example.com",
		"disabled":   true,
		"proxy_url":  "http://proxy.example.com",
	})

	savedName, err := handler.writeAuthFile(
		context.Background(),
		"codex-new@example.com-plus.json",
		[]byte(`{"type":"codex","account_id":"acct_upload_override","email":"new@example.com","disabled":false,"proxy_url":"http://new-proxy.example.com"}`),
	)
	if err != nil {
		t.Fatalf("writeAuthFile() error = %v", err)
	}
	if savedName != filepath.Base(existingPath) {
		t.Fatalf("saved name = %q, want %q", savedName, filepath.Base(existingPath))
	}

	payload := readCodexAuthFixture(t, existingPath)
	if disabled, ok := payload["disabled"].(bool); !ok || disabled {
		t.Fatalf("disabled = %#v, want false", payload["disabled"])
	}
	if got := stringValue(payload, "proxy_url"); got != "http://new-proxy.example.com" {
		t.Fatalf("proxy_url = %q, want %q", got, "http://new-proxy.example.com")
	}
	auth, ok := handler.currentAuthManager().GetByID(filepath.Base(existingPath))
	if !ok || auth == nil {
		t.Fatal("expected updated auth to remain registered")
	}
	if auth.Disabled {
		t.Fatal("expected live auth.Disabled = false")
	}
	if auth.Status != coreauth.StatusActive {
		t.Fatalf("auth.Status = %q, want %q", auth.Status, coreauth.StatusActive)
	}
	if auth.ProxyURL != "http://new-proxy.example.com" {
		t.Fatalf("auth.ProxyURL = %q, want %q", auth.ProxyURL, "http://new-proxy.example.com")
	}
}

func TestWriteAuthFile_CodexUploadPreservesExistingPlanType(t *testing.T) {
	authDir := t.TempDir()
	handler := newCodexSaveTestHandler(authDir)

	existingPath := filepath.Join(authDir, "codex-old@example.com-free.json")
	writeCodexAuthFixture(t, existingPath, map[string]any{
		"type":       "codex",
		"account_id": "acct_plan",
		"email":      "old@example.com",
		"plan_type":  "free",
	})

	savedName, err := handler.writeAuthFile(
		context.Background(),
		"codex-new@example.com-free.json",
		[]byte(`{"type":"codex","account_id":"acct_plan","email":"new@example.com","access_token":"upload-access"}`),
	)
	if err != nil {
		t.Fatalf("writeAuthFile() error = %v", err)
	}
	if savedName != filepath.Base(existingPath) {
		t.Fatalf("saved name = %q, want %q", savedName, filepath.Base(existingPath))
	}

	payload := readCodexAuthFixture(t, existingPath)
	if got := stringValue(payload, "plan_type"); got != "free" {
		t.Fatalf("payload plan_type = %q, want %q", got, "free")
	}
	auth, ok := handler.currentAuthManager().GetByID(filepath.Base(existingPath))
	if !ok || auth == nil {
		t.Fatal("expected updated auth to remain registered")
	}
	if got := auth.Attributes["plan_type"]; got != "free" {
		t.Fatalf("auth.Attributes[plan_type] = %q, want %q", got, "free")
	}
	if got := coreauth.AuthChatGPTPlanType(auth); got != "free" {
		t.Fatalf("AuthChatGPTPlanType(auth) = %q, want %q", got, "free")
	}
}

func newCodexSaveTestHandler(authDir string) *Handler {
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	handler.tokenStore = authstore.NewFileTokenStore()
	return handler
}

func writeCodexAuthFixture(t *testing.T, path string, payload map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir fixture dir: %v", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func readCodexAuthFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth fixture: %v", err)
	}
	var payload map[string]any
	if err = json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal auth fixture: %v", err)
	}
	return payload
}

func codexJSONFiles(t *testing.T, authDir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(authDir, "*.json"))
	if err != nil {
		t.Fatalf("glob json files: %v", err)
	}
	return files
}
