package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func newGeminiCLIHandlerForTest(cfg *sdkconfig.SDKConfig) *GeminiCLIAPIHandler {
	if cfg == nil {
		cfg = &sdkconfig.SDKConfig{}
	}
	return NewGeminiCLIAPIHandler(handlers.NewBaseAPIHandlers(cfg, coreauth.NewManager(nil, nil, nil)))
}

func TestGeminiCLIHandlerRejectsWhenEndpointDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.POST("/v1internal:generateContent", newGeminiCLIHandlerForTest(&sdkconfig.SDKConfig{}).CLIHandler)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1internal:generateContent", strings.NewReader(`{"model":"gemini-2.5-pro"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Gemini CLI endpoint is disabled") {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestGeminiCLIHandlerRejectsNonLoopbackHost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &sdkconfig.SDKConfig{EnableGeminiCLIEndpoint: true}
	router := gin.New()
	router.POST("/v1internal:generateContent", newGeminiCLIHandlerForTest(cfg).CLIHandler)

	req := httptest.NewRequest(http.MethodPost, "http://localhost/v1internal:generateContent", strings.NewReader(`{"model":"gemini-2.5-pro"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "localhost"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "CLI reply only allow local access") {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestGeminiCLIHandlerAllowsLoopbackHostThroughGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &sdkconfig.SDKConfig{EnableGeminiCLIEndpoint: true}
	router := gin.New()
	router.POST("/v1internal:generateContent", newGeminiCLIHandlerForTest(cfg).CLIHandler)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1internal:generateContent", strings.NewReader(`{"model":"gemini-2.5-pro"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code == http.StatusForbidden {
		t.Fatalf("expected request to pass local guard, got forbidden: %s", rr.Body.String())
	}
}
