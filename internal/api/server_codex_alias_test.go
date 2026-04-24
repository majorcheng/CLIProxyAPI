package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type serverCodexAliasTestExecutor struct {
	lastAlt          string
	lastSourceFormat string
	calls            int
}

func (e *serverCodexAliasTestExecutor) Identifier() string { return "codex" }

func (e *serverCodexAliasTestExecutor) Execute(_ context.Context, _ *auth.Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls++
	e.lastAlt = opts.Alt
	e.lastSourceFormat = opts.SourceFormat.String()
	return cliproxyexecutor.Response{Payload: []byte(`{"id":"resp_alias","object":"response","status":"completed","output":[]}`)}, nil
}

func (e *serverCodexAliasTestExecutor) ExecuteStream(context.Context, *auth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *serverCodexAliasTestExecutor) Refresh(_ context.Context, a *auth.Auth) (*auth.Auth, error) {
	return a, nil
}

func (e *serverCodexAliasTestExecutor) CountTokens(context.Context, *auth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *serverCodexAliasTestExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newCodexAliasServer(t *testing.T, executor *serverCodexAliasTestExecutor) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)
	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")

	cfg := &proxyconfig.Config{
		SDKConfig:              sdkconfig.SDKConfig{APIKeys: []string{"test-key"}},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}

	authManager := auth.NewManager(nil, nil, nil)
	authManager.RegisterExecutor(executor)
	registered, err := authManager.Register(context.Background(), &auth.Auth{
		ID:       "codex-alias-auth",
		Provider: "codex",
		Status:   auth.StatusActive,
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(registered.ID, registered.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(registered.ID)
	})

	return NewServer(cfg, authManager, sdkaccess.NewManager(), filepath.Join(tmpDir, "config.yaml"))
}

func TestServerRegistersCodexResponsesAliases(t *testing.T) {
	server := newCodexAliasServer(t, &serverCodexAliasTestExecutor{})

	want := map[string]bool{
		"GET /backend-api/codex/responses":          false,
		"POST /backend-api/codex/responses":         false,
		"POST /backend-api/codex/responses/compact": false,
	}
	for _, route := range server.engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, ok := range want {
		if !ok {
			t.Fatalf("missing codex alias route %s", key)
		}
	}
}

func TestServerCodexResponsesAliasUsesExistingHandlers(t *testing.T) {
	executor := &serverCodexAliasTestExecutor{}
	server := newCodexAliasServer(t, executor)

	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	server.engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("calls = %d, want 1", executor.calls)
	}
	if executor.lastAlt != "" {
		t.Fatalf("lastAlt = %q, want empty", executor.lastAlt)
	}
	if executor.lastSourceFormat != "openai-response" {
		t.Fatalf("lastSourceFormat = %q, want %q", executor.lastSourceFormat, "openai-response")
	}
}

func TestServerCodexResponsesCompactAliasUsesExistingHandlers(t *testing.T) {
	executor := &serverCodexAliasTestExecutor{}
	server := newCodexAliasServer(t, executor)

	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses/compact", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	server.engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("calls = %d, want 1", executor.calls)
	}
	if executor.lastAlt != "responses/compact" {
		t.Fatalf("lastAlt = %q, want %q", executor.lastAlt, "responses/compact")
	}
}
