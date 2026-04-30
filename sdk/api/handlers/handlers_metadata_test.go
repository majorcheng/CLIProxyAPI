package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

func TestRequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey(t *testing.T) {
	ctx := WithExecutionSessionID(context.Background(), "session-1")

	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.ExecutionSessionMetadataKey]; got != "session-1" {
		t.Fatalf("ExecutionSessionMetadataKey = %v, want %q", got, "session-1")
	}
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("unexpected idempotency key in metadata: %v", meta[idempotencyKeyMetadataKey])
	}
}

func TestRequestExecutionMetadataIncludesDisallowFreeAuth(t *testing.T) {
	ctx := WithDisallowFreeAuth(context.Background())

	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.DisallowFreeAuthMetadataKey]; got != true {
		t.Fatalf("DisallowFreeAuthMetadataKey = %v, want true", got)
	}
}

func TestRequestExecutionMetadataIncludesGinRequestPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/provider/:provider/v1/images/generations", func(c *gin.Context) {
		ctx := context.WithValue(context.Background(), "gin", c)
		meta := requestExecutionMetadata(ctx)
		if got := meta[coreexecutor.RequestPathMetadataKey]; got != "/api/provider/:provider/v1/images/generations" {
			t.Fatalf("RequestPathMetadataKey = %#v, want route full path", got)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/api/provider/openai/v1/images/generations", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
}

func TestHeadersFromContextClonesGinRequestHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Session-ID", "session-123")
	req.Header.Set("X-Amp-Thread-Id", "amp-thread-1")
	ginCtx.Request = req

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	headers := headersFromContext(ctx)
	if headers == nil {
		t.Fatal("headersFromContext() = nil, want cloned headers")
	}
	if got := headers.Get("X-Session-ID"); got != "session-123" {
		t.Fatalf("X-Session-ID = %q, want %q", got, "session-123")
	}
	if got := headers.Get("X-Amp-Thread-Id"); got != "amp-thread-1" {
		t.Fatalf("X-Amp-Thread-Id = %q, want %q", got, "amp-thread-1")
	}

	headers.Set("X-Session-ID", "mutated")
	if got := ginCtx.Request.Header.Get("X-Session-ID"); got != "session-123" {
		t.Fatalf("original header = %q, want %q after clone mutation", got, "session-123")
	}
}
