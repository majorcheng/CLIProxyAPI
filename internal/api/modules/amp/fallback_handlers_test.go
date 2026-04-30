package amp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/tidwall/gjson"
)

func TestFallbackHandler_ModelMapping_PreservesThinkingSuffixAndRewritesResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-client-amp-fallback", "codex", []*registry.ModelInfo{
		{ID: "test/gpt-5.2", OwnedBy: "openai", Type: "codex"},
	})
	defer reg.UnregisterClient("test-client-amp-fallback")

	mapper := NewModelMapper([]config.AmpModelMapping{
		{From: "gpt-5.2", To: "test/gpt-5.2"},
	})

	fallback := NewFallbackHandlerWithMapper(func() *httputil.ReverseProxy { return nil }, mapper, nil)

	handler := func(c *gin.Context) {
		var req struct {
			Model string `json:"model"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"model":      req.Model,
			"seen_model": req.Model,
		})
	}

	r := gin.New()
	r.POST("/chat/completions", fallback.WrapHandler(handler))

	reqBody := []byte(`{"model":"gpt-5.2(xhigh)"}`)
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", w.Code)
	}

	var resp struct {
		Model     string `json:"model"`
		SeenModel string `json:"seen_model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse response JSON: %v", err)
	}

	if resp.Model != "gpt-5.2(xhigh)" {
		t.Errorf("Expected response model gpt-5.2(xhigh), got %s", resp.Model)
	}
	if resp.SeenModel != "test/gpt-5.2(xhigh)" {
		t.Errorf("Expected handler to see test/gpt-5.2(xhigh), got %s", resp.SeenModel)
	}
}

func TestFallbackHandler_DisableImageGenerationBlocksAmpImagesProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	proxyCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalled = true
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse() 失败：%v", err)
	}

	fallback := NewFallbackHandler(func() *httputil.ReverseProxy {
		return httputil.NewSingleHostReverseProxy(upstreamURL)
	})
	fallback.SetDisableImageGeneration(func() bool { return true })

	handlerCalled := false
	r := gin.New()
	r.POST("/api/provider/openai/v1/images/generations", fallback.WrapHandler(func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/provider/openai/v1/images/generations", bytes.NewReader([]byte(`{"model":"no-local-provider"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 %d", w.Code, http.StatusNotFound)
	}
	if handlerCalled {
		t.Fatal("全局禁用图片生成时不应调用 wrapped handler")
	}
	if proxyCalled {
		t.Fatal("全局禁用图片生成时不应调用 amp fallback proxy")
	}
}

func TestFallbackHandler_DisableImageGenerationChatModeStripsNonImagesProxyBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll(upstream body) 失败：%v", err)
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("url.Parse() 失败：%v", err)
	}

	fallback := NewFallbackHandler(func() *httputil.ReverseProxy {
		return httputil.NewSingleHostReverseProxy(upstreamURL)
	})
	fallback.SetDisableImageGenerationMode(func() config.DisableImageGenerationMode {
		return config.DisableImageGenerationChat
	})

	handlerCalled := false
	router := gin.New()
	router.POST("/api/provider/openai/v1/chat/completions", fallback.WrapHandler(func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusOK)
	}))
	server := httptest.NewServer(router)
	defer server.Close()

	reqBody := []byte(`{"model":"no-local-provider","tools":[{"type":"image_generation"},{"type":"function","name":"demo"}],"tool_choice":{"type":"image_generation"}}`)
	resp, err := http.Post(server.URL+"/api/provider/openai/v1/chat/completions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("http.Post() 失败：%v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d，期望 %d", resp.StatusCode, http.StatusTeapot)
	}
	if handlerCalled {
		t.Fatal("无本地 provider 时不应调用 wrapped handler")
	}
	assertAmpFallbackBodyWithoutImageGeneration(t, upstreamBody)
}

// assertAmpFallbackBodyWithoutImageGeneration 校验 fallback 上游请求不会携带已禁用的图片工具。
func assertAmpFallbackBodyWithoutImageGeneration(t *testing.T, body []byte) {
	t.Helper()
	if got := gjson.GetBytes(body, "tools.#(type==\"image_generation\")"); got.Exists() {
		t.Fatalf("upstream body tools 仍包含 image_generation：%s", string(body))
	}
	if got := gjson.GetBytes(body, "tools.#(type==\"function\").name").String(); got != "demo" {
		t.Fatalf("function tool name = %q，期望 demo：%s", got, string(body))
	}
	if choice := gjson.GetBytes(body, "tool_choice"); choice.Exists() {
		t.Fatalf("upstream body tool_choice = %s，期望删除 image_generation 选择", choice.Raw)
	}
}
