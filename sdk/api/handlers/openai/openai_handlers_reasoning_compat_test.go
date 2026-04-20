package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type reasoningCompatCaptureExecutor struct {
	payload      []byte
	calls        int
	sourceFormat string
}

func (e *reasoningCompatCaptureExecutor) Identifier() string { return "codex" }

func (e *reasoningCompatCaptureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	e.sourceFormat = opts.SourceFormat.String()
	e.payload = sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FromString("codex"), req.Model, req.Payload, opts.Stream)
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *reasoningCompatCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *reasoningCompatCaptureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *reasoningCompatCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *reasoningCompatCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestChatCompletionsResponsesMisroutePreservesCompatibleReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		request    string
		wantEffort string
	}{
		{
			name: "string reasoning compatibility",
			request: `{
				"model":"gpt-5.2",
				"reasoning":"xhigh",
				"input":"hi"
			}`,
			wantEffort: "xhigh",
		},
		{
			name: "reasoning_effort compatibility",
			request: `{
				"model":"gpt-5.2",
				"reasoning_effort":"high",
				"input":"hi"
			}`,
			wantEffort: "high",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &reasoningCompatCaptureExecutor{}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)

			auth := &coreauth.Auth{ID: "auth-" + strings.ReplaceAll(tt.wantEffort, " ", "-"), Provider: executor.Identifier(), Status: coreauth.StatusActive}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("Register auth: %v", err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.2"}})
			t.Cleanup(func() {
				registry.GetGlobalRegistry().UnregisterClient(auth.ID)
			})

			base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
			h := NewOpenAIAPIHandler(base)
			router := gin.New()
			router.POST("/v1/chat/completions", h.ChatCompletions)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.request))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
			}
			if executor.calls != 1 {
				t.Fatalf("executor calls = %d, want 1", executor.calls)
			}
			if executor.sourceFormat != "openai" {
				t.Fatalf("source format = %q, want %q", executor.sourceFormat, "openai")
			}
			if got := gjson.GetBytes(executor.payload, "reasoning.effort").String(); got != tt.wantEffort {
				t.Fatalf("translated reasoning.effort = %q, want %q: %s", got, tt.wantEffort, string(executor.payload))
			}
		})
	}
}
