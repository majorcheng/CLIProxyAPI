package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type imageIntentExecutorCall struct {
	AuthID       string
	Model        string
	DisallowFree bool
}

type imageIntentCaptureExecutor struct {
	id    string
	mu    sync.Mutex
	calls []imageIntentExecutorCall
}

func (e *imageIntentCaptureExecutor) Identifier() string { return e.id }

func (e *imageIntentCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(auth, req, opts)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *imageIntentCaptureExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record(auth, req, opts)
	ch := make(chan coreexecutor.StreamChunk, 1)
	ch <- coreexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *imageIntentCaptureExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, errors.New("not implemented")
}

func (e *imageIntentCaptureExecutor) CountTokens(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(auth, req, opts)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *imageIntentCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *imageIntentCaptureExecutor) record(auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, imageIntentExecutorCall{
		AuthID:       auth.ID,
		Model:        req.Model,
		DisallowFree: metadataBool(opts.Metadata, coreexecutor.DisallowFreeAuthMetadataKey),
	})
}

func (e *imageIntentCaptureExecutor) Calls() []imageIntentExecutorCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]imageIntentExecutorCall(nil), e.calls...)
}

func TestExecuteWithAuthManager_DisallowFreeCodexForExplicitImageIntent(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	executor := &imageIntentCaptureExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	registerHandlerImageIntentAuth(t, manager, "codex-free", "codex", "gpt-5.4", map[string]string{"plan_type": "free"})
	registerHandlerImageIntentAuth(t, manager, "codex-plus", "codex", "gpt-5.4", map[string]string{"plan_type": "plus"})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	resp, headers, errMsg := handler.ExecuteWithAuthManager(
		context.Background(),
		"openai-response",
		"gpt-5.4",
		[]byte(`{"model":"gpt-5.4","input":"draw a cat","tool_choice":{"type":"image_generation"}}`),
		"",
	)
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	if string(resp) != `{"ok":true}` {
		t.Fatalf("response = %s, want ok payload", string(resp))
	}
	if headers != nil {
		t.Fatalf("headers = %v, want nil when passthrough disabled", headers)
	}

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(calls))
	}
	if calls[0].AuthID != "codex-plus" {
		t.Fatalf("selected auth = %q, want %q", calls[0].AuthID, "codex-plus")
	}
	if calls[0].Model != "gpt-5.4" {
		t.Fatalf("request model = %q, want %q", calls[0].Model, "gpt-5.4")
	}
	if !calls[0].DisallowFree {
		t.Fatal("expected disallow_free_auth metadata for explicit image intent request")
	}
}

func TestExecuteStreamWithAuthManager_DisallowFreeCodexForExplicitImageIntent(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	executor := &imageIntentCaptureExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	registerHandlerImageIntentAuth(t, manager, "codex-free", "codex", "gpt-5.4", map[string]string{"plan_type": "free"})
	registerHandlerImageIntentAuth(t, manager, "codex-plus", "codex", "gpt-5.4", map[string]string{"plan_type": "plus"})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(
		context.Background(),
		"openai-response",
		"gpt-5.4",
		[]byte(`{"model":"gpt-5.4","input":"draw a cat","tools":[{"type":"image_generation"}]}`),
		"",
	)
	if dataChan == nil || errChan == nil {
		t.Fatal("expected non-nil stream channels")
	}
	for range dataChan {
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("unexpected stream error: %+v", msg)
		}
	}

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(calls))
	}
	if calls[0].AuthID != "codex-plus" {
		t.Fatalf("selected auth = %q, want %q", calls[0].AuthID, "codex-plus")
	}
	if !calls[0].DisallowFree {
		t.Fatal("expected disallow_free_auth metadata for explicit image intent stream request")
	}
}

func TestExecuteCountWithAuthManager_DisallowFreeCodexForExplicitImageIntent(t *testing.T) {
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	executor := &imageIntentCaptureExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	registerHandlerImageIntentAuth(t, manager, "codex-free", "codex", "gpt-5.4", map[string]string{"plan_type": "free"})
	registerHandlerImageIntentAuth(t, manager, "codex-plus", "codex", "gpt-5.4", map[string]string{"plan_type": "plus"})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	resp, headers, errMsg := handler.ExecuteCountWithAuthManager(
		context.Background(),
		"openai-response",
		"gpt-5.4",
		[]byte(`{"model":"gpt-5.4","input":"draw a cat","tool_choice":{"type":"image_generation"}}`),
		"",
	)
	if errMsg != nil {
		t.Fatalf("ExecuteCountWithAuthManager() error = %+v", errMsg)
	}
	if string(resp) != `{"ok":true}` {
		t.Fatalf("response = %s, want ok payload", string(resp))
	}
	if headers != nil {
		t.Fatalf("headers = %v, want nil when passthrough disabled", headers)
	}

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(calls))
	}
	if calls[0].AuthID != "codex-plus" {
		t.Fatalf("selected auth = %q, want %q", calls[0].AuthID, "codex-plus")
	}
	if !calls[0].DisallowFree {
		t.Fatal("expected disallow_free_auth metadata for explicit image intent count request")
	}
}

func registerHandlerImageIntentAuth(t *testing.T, manager *coreauth.Manager, authID, provider, model string, attrs map[string]string) {
	t.Helper()
	auth := &coreauth.Auth{ID: authID, Provider: provider, Status: coreauth.StatusActive, Attributes: attrs}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register(%s): %v", authID, err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})
}

func metadataBool(meta map[string]any, key string) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return false
	}
	value, _ := raw.(bool)
	return value
}
