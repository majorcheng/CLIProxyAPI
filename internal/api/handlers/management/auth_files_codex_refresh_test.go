package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	authstore "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type codexRefreshHandlerTestExecutor struct {
	after func(*coreauth.Auth)
	err   error
}

func (e codexRefreshHandlerTestExecutor) Identifier() string { return "codex" }

func (e codexRefreshHandlerTestExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e codexRefreshHandlerTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e codexRefreshHandlerTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if e.after != nil {
		e.after(auth)
	}
	return auth, e.err
}

func (e codexRefreshHandlerTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e codexRefreshHandlerTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestRefreshCodexAuthFile_SucceedsAndPersistsUpdatedTokens(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "manual-codex.json")
	store := authstore.NewFileTokenStore()
	store.SetBaseDir(authDir)

	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(codexRefreshHandlerTestExecutor{
		after: func(auth *coreauth.Auth) {
			if auth == nil {
				return
			}
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["type"] = "codex"
			auth.Metadata["email"] = "manual@example.com"
			auth.Metadata["refresh_token"] = "rotated-refresh-token"
			auth.Metadata["access_token"] = "rotated-access-token"
			auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
			auth.Metadata["expired"] = time.Now().Add(2 * time.Hour).Format(time.RFC3339)
		},
	})

	auth := &coreauth.Auth{
		ID:       "manual-codex.json",
		FileName: "manual-codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "manual@example.com",
			"refresh_token": "original-refresh-token",
			"access_token":  "original-access-token",
			"expired":       time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("注册测试 auth 失败: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	body := `{"auth_index":"` + auth.EnsureIndex() + `","name":"manual-codex.json"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/codex/refresh", bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.RefreshCodexAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 %d，响应 = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got, _ := resp["status"].(string); got != "ok" {
		t.Fatalf("status = %q，期望 ok", got)
	}

	file, ok := resp["file"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 file 字段: %#v", resp)
	}
	if got, _ := file["auth_index"].(string); got != auth.EnsureIndex() {
		t.Fatalf("auth_index = %q，期望 %q", got, auth.EnsureIndex())
	}
	if got, _ := file["has_refresh_token"].(bool); !got {
		t.Fatalf("has_refresh_token = %v，期望 true", file["has_refresh_token"])
	}

	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("读取落盘 auth 文件失败: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("解析落盘 auth 文件失败: %v", err)
	}
	if got, _ := persisted["refresh_token"].(string); got != "rotated-refresh-token" {
		t.Fatalf("落盘 refresh_token = %q，期望 %q", got, "rotated-refresh-token")
	}
	if got, _ := persisted["access_token"].(string); got != "rotated-access-token" {
		t.Fatalf("落盘 access_token = %q，期望 %q", got, "rotated-access-token")
	}
}

func TestRefreshCodexAuthFile_RejectsMismatchedNameAndAuthIndex(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRefreshHandlerTestExecutor{})

	first := &coreauth.Auth{ID: "first.json", FileName: "first.json", Provider: "codex", Metadata: map[string]any{"refresh_token": "rt-1"}}
	second := &coreauth.Auth{ID: "second.json", FileName: "second.json", Provider: "codex", Metadata: map[string]any{"refresh_token": "rt-2"}}
	if _, err := manager.Register(context.Background(), first); err != nil {
		t.Fatalf("注册 first 失败: %v", err)
	}
	if _, err := manager.Register(context.Background(), second); err != nil {
		t.Fatalf("注册 second 失败: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := `{"auth_index":"` + first.EnsureIndex() + `","name":"second.json"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/codex/refresh", bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.RefreshCodexAuthFile(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 %d，响应 = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "name 与 auth_index 指向的凭证不一致") {
		t.Fatalf("错误信息未命中预期: %s", rec.Body.String())
	}
}

func TestRefreshCodexAuthFile_ReturnsFailureWithCurrentFileState(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "failed-codex.json")
	store := authstore.NewFileTokenStore()
	store.SetBaseDir(authDir)

	manager := coreauth.NewManager(store, nil, nil)
	manager.RegisterExecutor(codexRefreshHandlerTestExecutor{
		err: errors.New("refresh_token_reused"),
	})

	auth := &coreauth.Auth{
		ID:       "failed-codex.json",
		FileName: "failed-codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "failed@example.com",
			"refresh_token": "original-refresh-token",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("注册测试 auth 失败: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	body := `{"name":"failed-codex.json"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/codex/refresh", bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.RefreshCodexAuthFile(ctx)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 %d，响应 = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "refresh_token_reused") {
		t.Fatalf("错误信息未透传上游失败详情: %s", rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	file, ok := resp["file"].(map[string]any)
	if !ok {
		t.Fatalf("失败响应缺少 file 字段: %#v", resp)
	}
	if got, _ := file["name"].(string); got != "failed-codex.json" {
		t.Fatalf("file.name = %q，期望 %q", got, "failed-codex.json")
	}
}

func TestRefreshCodexAuthFile_RejectsCodexWithoutRefreshToken(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRefreshHandlerTestExecutor{})

	auth := &coreauth.Auth{
		ID:       "no-rt.json",
		FileName: "no-rt.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":  "codex",
			"email": "no-rt@example.com",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("注册测试 auth 失败: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := `{"name":"no-rt.json"}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/codex/refresh", bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.RefreshCodexAuthFile(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 %d，响应 = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "缺少 refresh_token") {
		t.Fatalf("错误信息未命中预期: %s", rec.Body.String())
	}
}

func TestRefreshCodexAuthFile_ReturnsConflictWhenRefreshAlreadyInFlight(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	started := make(chan struct{}, 1)
	release := make(chan struct{})

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(codexRefreshHandlerTestExecutor{
		after: func(auth *coreauth.Auth) {
			started <- struct{}{}
			<-release
			if auth == nil {
				return
			}
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["refresh_token"] = "rotated-refresh-token"
		},
	})

	auth := &coreauth.Auth{
		ID:       "busy.json",
		FileName: "busy.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"type":          "codex",
			"refresh_token": "original-refresh-token",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("注册测试 auth 失败: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	body := `{"name":"busy.json"}`

	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/codex/refresh", bytes.NewBufferString(body))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		h.RefreshCodexAuthFile(firstCtx)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("预期第一条刷新请求已经进入执行态")
	}

	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/codex/refresh", bytes.NewBufferString(body))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	h.RefreshCodexAuthFile(secondCtx)

	if secondRec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 %d，响应 = %s", secondRec.Code, http.StatusConflict, secondRec.Body.String())
	}
	if !strings.Contains(secondRec.Body.String(), "正在刷新中") {
		t.Fatalf("错误信息未命中预期: %s", secondRec.Body.String())
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("释放阻塞后第一条刷新请求仍未结束")
	}
	if firstRec.Code != http.StatusOK {
		t.Fatalf("第一条刷新请求状态码 = %d，期望 %d，响应 = %s", firstRec.Code, http.StatusOK, firstRec.Body.String())
	}
}
