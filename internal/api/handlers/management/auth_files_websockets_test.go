package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestListAuthFilesIncludesWebsocketsFromLiveAuth 验证内存态列表不会遗漏 false 配置。
func TestListAuthFilesIncludesWebsocketsFromLiveAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":       path,
			"websockets": "false",
		},
		Metadata: map[string]any{"type": "codex", "websockets": true},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	file := listSingleAuthFileForTest(t, NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager))
	if got := file["websockets"]; got != false {
		t.Fatalf("websockets = %#v, want false", got)
	}
}

// TestListAuthFilesFromDiskIncludesWebsocketsFalse 验证磁盘回退列表能暴露 false。
func TestListAuthFilesFromDiskIncludesWebsocketsFalse(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","websockets":false}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	file := listSingleAuthFileForTest(t, NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil))
	if got := file["websockets"]; got != false {
		t.Fatalf("websockets = %#v, want false", got)
	}
}

// TestPatchAuthFileFieldsWebsocketsFalseUpdatesRuntimeAndFile 验证 PATCH false 会同步 attributes 并落盘。
func TestPatchAuthFileFieldsWebsocketsFalseUpdatesRuntimeAndFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	manager := fileBackedAuthManagerForTest(t, authDir, path)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	patchAuthFileFieldsForTest(t, handler, `{"name":"codex.json","websockets":false}`)
	updated, ok := manager.GetByID("codex.json")
	if !ok {
		t.Fatal("updated auth not found")
	}
	if got := updated.Attributes["websockets"]; got != "false" {
		t.Fatalf("attribute websockets = %q, want false", got)
	}
	assertAuthFileJSONPathForTest(t, path, "websockets", false)
}

// TestPatchAuthFileFieldsArbitraryNestedMetadataPersists 验证任意 metadata 路径按原结构落盘。
func TestPatchAuthFileFieldsArbitraryNestedMetadataPersists(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	manager := fileBackedAuthManagerForTest(t, authDir, path)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	body := `{"name":"codex.json","abc":true,"nested.cde":true,"fgh":{"ijk":true}}`
	patchAuthFileFieldsForTest(t, handler, body)
	assertAuthFileJSONPathForTest(t, path, "abc", true)
	assertAuthFileJSONPathForTest(t, path, "nested.cde", true)
	assertAuthFileJSONPathForTest(t, path, "fgh.ijk", true)
}

// TestApplyAuthFileMetadataPatchClonesNestedMetadata 验证 PATCH 不会写穿 GetByID 浅拷贝共享的子 map。
func TestApplyAuthFileMetadataPatchClonesNestedMetadata(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":   "codex",
			"nested": map[string]any{"existing": true},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	targetAuth, ok := manager.GetByID("codex.json")
	if !ok {
		t.Fatal("target auth not found")
	}
	changed, err := applyAuthFileMetadataPatch(targetAuth, map[string]json.RawMessage{
		"nested.injected": json.RawMessage(`true`),
	})
	if err != nil || !changed {
		t.Fatalf("applyAuthFileMetadataPatch changed=%v err=%v", changed, err)
	}
	currentAuth, ok := manager.GetByID("codex.json")
	if !ok {
		t.Fatal("current auth not found")
	}
	if got := nestedJSONValueForTest(currentAuth.Metadata, "nested.injected"); got != nil {
		t.Fatalf("manager metadata was polluted: nested.injected=%#v", got)
	}
	if got := nestedJSONValueForTest(targetAuth.Metadata, "nested.injected"); got != true {
		t.Fatalf("patched clone nested.injected=%#v, want true", got)
	}
}

// fileBackedAuthManagerForTest 创建带真实 FileTokenStore 的 manager，覆盖落盘链路。
func fileBackedAuthManagerForTest(t *testing.T, authDir, path string) *coreauth.Manager {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"type":"codex","websockets":true}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       filepath.Base(path),
		FileName: filepath.Base(path),
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":       path,
			"source":     path,
			"websockets": "true",
		},
		Metadata: map[string]any{"type": "codex", "websockets": true},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return manager
}

// patchAuthFileFieldsForTest 调用 handler，确保测试覆盖真实 HTTP JSON 解码路径。
func patchAuthFileFieldsForTest(t *testing.T, h *Handler, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchAuthFileFields(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("PatchAuthFileFields status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// assertAuthFileJSONPathForTest 校验落盘 JSON 的点号路径值。
func assertAuthFileJSONPathForTest(t *testing.T, path, fieldPath string, want any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var payload map[string]any
	if err = json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode auth file: %v; content=%s", err, string(data))
	}
	got := nestedJSONValueForTest(payload, fieldPath)
	if got != want {
		t.Fatalf("%s = %#v, want %#v; content=%s", fieldPath, got, want, string(data))
	}
}

// nestedJSONValueForTest 读取测试用点号路径，避免引入额外运行时依赖。
func nestedJSONValueForTest(payload map[string]any, fieldPath string) any {
	var current any = payload
	for _, part := range strings.Split(fieldPath, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[part]
	}
	return current
}
