package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const deleteFillFirstRegisterOrderGap = time.Millisecond

type deleteFillFirstExecutor struct {
	mu         sync.Mutex
	failStatus map[string]int
	calls      []string
}

func (e *deleteFillFirstExecutor) Identifier() string { return "stress" }

func (e *deleteFillFirstExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.mu.Lock()
	e.calls = append(e.calls, authID)
	status := e.failStatus[authID]
	e.mu.Unlock()
	if status > 0 {
		return coreexecutor.Response{}, &coreauth.Error{HTTPStatus: status, Message: http.StatusText(status)}
	}
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *deleteFillFirstExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (e *deleteFillFirstExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *deleteFillFirstExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (e *deleteFillFirstExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *deleteFillFirstExecutor) snapshotCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func TestDeleteAuthFile_DisablesAllAuthsForSharedBackingPath(t *testing.T) {
	fixture := newDeleteFillFirstFixture(t)
	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(filepath.Base(fixture.sharedPath)), nil)
	fixture.handler.DeleteAuthFile(deleteCtx)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("DeleteAuthFile status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}

	assertDeleteFillFirstAuthDisabled(t, fixture.manager, "shared-primary")
	assertDeleteFillFirstAuthDisabled(t, fixture.manager, "shared-project")
	assertDeleteFillFirstRegistryCleared(t, "shared-primary")
	assertDeleteFillFirstRegistryCleared(t, "shared-project")
	req := coreexecutor.Request{Model: fixture.model, Payload: []byte(`{"model":"delete-fill-first-shared-model"}`)}
	if _, err := fixture.manager.Execute(context.Background(), []string{"stress"}, req, coreexecutor.Options{}); err != nil {
		t.Fatalf("Execute after delete error=%v", err)
	}
	if got := fixture.executor.snapshotCalls(); len(got) != 1 || got[0] != "good-token" {
		t.Fatalf("fill-first should skip deleted shared auths, calls=%#v", got)
	}
}

func TestDeleteAuthFileAll_DisablesAllAuthsForSharedBackingPath(t *testing.T) {
	fixture := newDeleteFillFirstFixture(t)
	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?all=true", nil)
	fixture.handler.DeleteAuthFile(deleteCtx)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("DeleteAuthFile all status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}

	assertDeleteFillFirstAuthDisabled(t, fixture.manager, "shared-primary")
	assertDeleteFillFirstAuthDisabled(t, fixture.manager, "shared-project")
	assertDeleteFillFirstAuthDisabled(t, fixture.manager, "good-token")
	assertDeleteFillFirstRegistryCleared(t, "shared-primary")
	assertDeleteFillFirstRegistryCleared(t, "shared-project")
	assertDeleteFillFirstRegistryCleared(t, "good-token")
	req := coreexecutor.Request{Model: fixture.model, Payload: []byte(`{"model":"delete-fill-first-shared-model"}`)}
	if _, err := fixture.manager.Execute(context.Background(), []string{"stress"}, req, coreexecutor.Options{}); err == nil {
		t.Fatalf("Execute after delete all should fail without active auth")
	}
	if got := fixture.executor.snapshotCalls(); len(got) != 0 {
		t.Fatalf("fill-first should not call deleted auths after all delete, calls=%#v", got)
	}
}

func TestNormalizeAuthDeletePathForCase_CaseInsensitiveKey(t *testing.T) {
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	upperPath := filepath.Join(t.TempDir(), "AuthDir", "Shared-Token.JSON")
	lowerPath := strings.ToLower(upperPath)
	upperKey := handler.normalizeAuthDeletePathForCase(upperPath, true)
	lowerKey := handler.normalizeAuthDeletePathForCase(lowerPath, true)
	if upperKey != lowerKey {
		t.Fatalf("case-insensitive delete path key mismatch: upper=%q lower=%q", upperKey, lowerKey)
	}
	if got := (&Handler{}).normalizeAuthDeletePathForCase("Shared-Token.JSON", true); got != "shared-token.json" {
		t.Fatalf("relative case-insensitive delete path key=%q", got)
	}
}

type deleteFillFirstFixture struct {
	handler    *Handler
	manager    *coreauth.Manager
	executor   *deleteFillFirstExecutor
	model      string
	sharedPath string
}

func newDeleteFillFirstFixture(t *testing.T) *deleteFillFirstFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	executor := newDeleteFillFirstExecutor()
	manager.RegisterExecutor(executor)
	model := "delete-fill-first-shared-model"
	sharedPath := filepath.Join(authDir, "shared-token.json")
	goodPath := filepath.Join(authDir, "good-token.json")
	writeDeleteFillFirstAuthFile(t, sharedPath)
	writeDeleteFillFirstAuthFile(t, goodPath)
	registerDeleteFillFirstAuth(t, manager, "shared-primary", sharedPath, model)
	registerDeleteFillFirstAuth(t, manager, "shared-project", sharedPath, model)
	registerDeleteFillFirstAuth(t, manager, "good-token", goodPath, model)
	t.Cleanup(cleanDeleteFillFirstRegistry)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	handler.tokenStore = &memoryAuthStore{}
	handler.logDir = filepath.Join(t.TempDir(), "logs")
	return &deleteFillFirstFixture{handler: handler, manager: manager, executor: executor, model: model, sharedPath: sharedPath}
}

func newDeleteFillFirstExecutor() *deleteFillFirstExecutor {
	return &deleteFillFirstExecutor{
		failStatus: map[string]int{
			"shared-primary": http.StatusUnauthorized,
			"shared-project": http.StatusUnauthorized,
		},
	}
}

func cleanDeleteFillFirstRegistry() {
	for _, id := range []string{"shared-primary", "shared-project", "good-token"} {
		registry.GetGlobalRegistry().UnregisterClient(id)
	}
}

func writeDeleteFillFirstAuthFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"type":"stress"}`), 0o600); err != nil {
		t.Fatalf("write auth file %s: %v", path, err)
	}
}

func registerDeleteFillFirstAuth(t *testing.T, manager *coreauth.Manager, id string, path string, model string) {
	t.Helper()
	auth := &coreauth.Auth{
		ID:         id,
		FileName:   filepath.Base(path),
		Provider:   "stress",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": path},
		CreatedAt:  time.Now(),
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth %s: %v", id, err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "stress", []*registry.ModelInfo{{ID: model}})
	manager.RefreshSchedulerEntry(id)
	time.Sleep(deleteFillFirstRegisterOrderGap)
}

func assertDeleteFillFirstAuthDisabled(t *testing.T, manager *coreauth.Manager, id string) {
	t.Helper()
	auth, ok := manager.GetByID(id)
	if !ok || auth == nil {
		t.Fatalf("expected auth %s to remain for disabled-state tracking", id)
	}
	if !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("expected auth %s disabled, got disabled=%v status=%q", id, auth.Disabled, auth.Status)
	}
}

func assertDeleteFillFirstRegistryCleared(t *testing.T, id string) {
	t.Helper()
	if models := registry.GetGlobalRegistry().GetModelsForClient(id); len(models) != 0 {
		t.Fatalf("expected registry models for %s cleared, got %d", id, len(models))
	}
}
