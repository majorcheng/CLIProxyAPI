package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

type imageRouteExecutorCall struct {
	AuthID         string
	Model          string
	RequestedModel string
	SelectionModel string
	DisallowFree   bool
}

type imageRouteCaptureExecutor struct {
	id    string
	mu    sync.Mutex
	calls []imageRouteExecutorCall
}

func (e *imageRouteCaptureExecutor) Identifier() string { return e.id }

func (e *imageRouteCaptureExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *imageRouteCaptureExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls = append(e.calls, imageRouteExecutorCall{
		AuthID:         auth.ID,
		Model:          req.Model,
		RequestedModel: metadataString(opts.Metadata, coreexecutor.RequestedModelMetadataKey),
		SelectionModel: metadataString(opts.Metadata, coreexecutor.SelectionModelMetadataKey),
		DisallowFree:   metadataBool(opts.Metadata, coreexecutor.DisallowFreeAuthMetadataKey),
	})
	e.mu.Unlock()

	ch := make(chan coreexecutor.StreamChunk, 1)
	ch <- coreexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":1700000000,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aGVsbG8=\",\"output_format\":\"png\"}]}}\n\n")}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *imageRouteCaptureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *imageRouteCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *imageRouteCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *imageRouteCaptureExecutor) Calls() []imageRouteExecutorCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]imageRouteExecutorCall(nil), e.calls...)
}

func TestImagesGenerations_RoutesByImageModelAndExecutesWithMainModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	codexExecutor := &imageRouteCaptureExecutor{id: "codex"}
	openaiExecutor := &imageRouteCaptureExecutor{id: "openai"}
	manager.RegisterExecutor(codexExecutor)
	manager.RegisterExecutor(openaiExecutor)

	registerImageTestAuth(t, manager, "auth-codex-image", "codex", []string{"gpt-image-2"})
	registerImageTestAuth(t, manager, "auth-openai-main", "openai", []string{"gpt-5.4-mini"})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"draw a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if got := gjson.Get(resp.Body.String(), "data.0.b64_json").String(); got != "aGVsbG8=" {
		t.Fatalf("data.0.b64_json = %q, want %q", got, "aGVsbG8=")
	}

	codexCalls := codexExecutor.Calls()
	if len(codexCalls) != 1 {
		t.Fatalf("codex executor calls = %d, want 1", len(codexCalls))
	}
	if codexCalls[0].AuthID != "auth-codex-image" {
		t.Fatalf("codex auth id = %q, want %q", codexCalls[0].AuthID, "auth-codex-image")
	}
	if codexCalls[0].Model != defaultImagesMainModel {
		t.Fatalf("codex request model = %q, want %q", codexCalls[0].Model, defaultImagesMainModel)
	}
	if codexCalls[0].RequestedModel != defaultImagesMainModel {
		t.Fatalf("requested_model = %q, want %q", codexCalls[0].RequestedModel, defaultImagesMainModel)
	}
	if codexCalls[0].SelectionModel != defaultImagesToolModel {
		t.Fatalf("selection_model = %q, want %q", codexCalls[0].SelectionModel, defaultImagesToolModel)
	}
	if !codexCalls[0].DisallowFree {
		t.Fatal("expected disallow_free_auth metadata for images route")
	}
	if got := len(openaiExecutor.Calls()); got != 0 {
		t.Fatalf("openai executor calls = %d, want 0", got)
	}
}

func TestImagesGenerations_PrefixedImageModelAlsoPrefixesExecutionModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	codexExecutor := &imageRouteCaptureExecutor{id: "codex"}
	manager.RegisterExecutor(codexExecutor)

	registerImageTestAuth(t, manager, "auth-codex-prefixed", "codex", []string{"team-a/gpt-image-2"})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"draw a cat","model":"team-a/gpt-image-2"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	calls := codexExecutor.Calls()
	if len(calls) != 1 {
		t.Fatalf("codex executor calls = %d, want 1", len(calls))
	}
	if calls[0].AuthID != "auth-codex-prefixed" {
		t.Fatalf("codex auth id = %q, want %q", calls[0].AuthID, "auth-codex-prefixed")
	}
	if calls[0].Model != "team-a/"+defaultImagesMainModel {
		t.Fatalf("codex request model = %q, want %q", calls[0].Model, "team-a/"+defaultImagesMainModel)
	}
	if calls[0].RequestedModel != "team-a/"+defaultImagesMainModel {
		t.Fatalf("requested_model = %q, want %q", calls[0].RequestedModel, "team-a/"+defaultImagesMainModel)
	}
	if calls[0].SelectionModel != "team-a/gpt-image-2" {
		t.Fatalf("selection_model = %q, want %q", calls[0].SelectionModel, "team-a/gpt-image-2")
	}
}

func TestImagesGenerations_DisallowFreeCodexAuthDuringSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	codexExecutor := &imageRouteCaptureExecutor{id: "codex"}
	manager.RegisterExecutor(codexExecutor)

	registerImageTestAuthWithAttributes(t, manager, "auth-codex-free", "codex", []string{"gpt-image-2"}, map[string]string{"plan_type": "free"})
	registerImageTestAuthWithAttributes(t, manager, "auth-codex-plus", "codex", []string{"gpt-image-2"}, map[string]string{"plan_type": "plus"})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"draw a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	calls := codexExecutor.Calls()
	if len(calls) != 1 {
		t.Fatalf("codex executor calls = %d, want 1", len(calls))
	}
	if calls[0].AuthID != "auth-codex-plus" {
		t.Fatalf("selected auth = %q, want %q", calls[0].AuthID, "auth-codex-plus")
	}
	if !calls[0].DisallowFree {
		t.Fatal("expected disallow_free_auth metadata for images route")
	}
}

func TestImagesGenerations_WithoutImageModelProviderReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	openaiExecutor := &imageRouteCaptureExecutor{id: "openai"}
	manager.RegisterExecutor(openaiExecutor)
	registerImageTestAuth(t, manager, "auth-openai-main-only", "openai", []string{"gpt-5.4-mini"})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.ImagesGenerations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"draw a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusBadGateway, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "unknown provider for model gpt-image-2") {
		t.Fatalf("body = %s, want unknown provider for gpt-image-2", resp.Body.String())
	}
}

func TestStartImagesStream_ClosedBeforeFirstChunkReturnsJSON502(t *testing.T) {
	h, c, rec, flusher := newImageStreamTestHarness(t)
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	close(data)
	close(errs)

	var cancelErr error
	h.startImagesStream(c, flusher, func(err error) { cancelErr = err }, data, errs, nil, "b64_json", "image_generation")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stream disconnected before completion") {
		t.Fatalf("body = %s, want disconnect error", rec.Body.String())
	}
	if cancelErr == nil || !strings.Contains(cancelErr.Error(), "stream disconnected before completion") {
		t.Fatalf("cancelErr = %v, want disconnect error", cancelErr)
	}
}

func TestStartImagesStream_PartialThenCloseEmitsSSEError(t *testing.T) {
	h, c, rec, flusher := newImageStreamTestHarness(t)
	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("data: {\"type\":\"response.image_generation_call.partial_image\",\"partial_image_b64\":\"aGVsbG8=\",\"partial_image_index\":0,\"output_format\":\"png\"}\n\n")
	close(data)
	close(errs)

	var cancelErr error
	h.startImagesStream(c, flusher, func(err error) { cancelErr = err }, data, errs, nil, "b64_json", "image_generation")

	body := rec.Body.String()
	if !strings.Contains(body, "event: image_generation.partial_image") {
		t.Fatalf("body = %s, want partial image event", body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("body = %s, want error event", body)
	}
	if cancelErr == nil || !strings.Contains(cancelErr.Error(), "stream disconnected before completion") {
		t.Fatalf("cancelErr = %v, want disconnect error", cancelErr)
	}
}

func TestStartImagesStream_CompletedThenCloseFinishesWithoutError(t *testing.T) {
	h, c, rec, flusher := newImageStreamTestHarness(t)
	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":1700000000,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aGVsbG8=\",\"output_format\":\"png\",\"revised_prompt\":\"cat\"}]}}\n\n")
	close(data)
	close(errs)

	var cancelErr error
	h.startImagesStream(c, flusher, func(err error) { cancelErr = err }, data, errs, nil, "b64_json", "image_generation")

	body := rec.Body.String()
	if !strings.Contains(body, "event: image_generation.completed") {
		t.Fatalf("body = %s, want completed event", body)
	}
	if !strings.Contains(body, `"revised_prompt":"cat"`) {
		t.Fatalf("body = %s, want revised_prompt", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("body = %s, want no error event", body)
	}
	if cancelErr != nil {
		t.Fatalf("cancelErr = %v, want nil", cancelErr)
	}
}

func registerImageTestAuth(t *testing.T, manager *coreauth.Manager, authID, provider string, models []string) {
	registerImageTestAuthWithAttributes(t, manager, authID, provider, models, nil)
}

func registerImageTestAuthWithAttributes(t *testing.T, manager *coreauth.Manager, authID, provider string, models []string, attrs map[string]string) {
	t.Helper()
	auth := &coreauth.Auth{ID: authID, Provider: provider, Status: coreauth.StatusActive, Attributes: attrs}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register(%s): %v", authID, err)
	}
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, infos)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authID)
	})
}

func newImageStreamTestHarness(t *testing.T) (*OpenAIAPIHandler, *gin.Context, *httptest.ResponseRecorder, http.Flusher) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	h := NewOpenAIAPIHandler(&handlers.BaseAPIHandler{})
	flusher, ok := any(rec).(http.Flusher)
	if !ok {
		t.Fatal("httptest.ResponseRecorder does not implement http.Flusher")
	}
	return h, c, rec, flusher
}

func metadataString(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return ""
	}
	value, _ := raw.(string)
	return value
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
