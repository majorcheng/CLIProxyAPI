package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestApplyClientRoutingPolicyMetadata_MarksPriorityZeroDisabledClientKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("apiKey", "key-b")
	ginCtx.Set("accessProvider", sdkaccess.DefaultAccessProviderName)

	meta := map[string]any{"idempotency_key": "req-1"}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &sdkconfig.SDKConfig{
		APIKeys: []string{"key-b"},
	}
	cfg.SetClientAPIKeyEntries([]sdkconfig.ClientAPIKey{
		{Key: " key-b ", MaxPriority: intPtr(0)},
		{Key: "key-b", MaxPriority: intPtr(-1)},
	})

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	got, ok := meta[coreexecutor.MaxAuthPriorityMetadataKey]
	if !ok {
		t.Fatalf("metadata missing %q", coreexecutor.MaxAuthPriorityMetadataKey)
	}
	value, ok := got.(int)
	if !ok || value != 0 {
		t.Fatalf("metadata[%q] = %#v, want 0", coreexecutor.MaxAuthPriorityMetadataKey, got)
	}
}

func TestApplyClientRoutingPolicyMetadata_IgnoresNonInlineAccessProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=key-b", nil)
	ginCtx.Request = req
	ginCtx.Set("apiKey", "key-b")
	ginCtx.Set("accessProvider", "external-provider")

	meta := map[string]any{"idempotency_key": "req-2"}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &sdkconfig.SDKConfig{APIKeys: []string{"key-b"}}
	cfg.SetClientAPIKeyEntries([]sdkconfig.ClientAPIKey{{Key: "key-b", MaxPriority: intPtr(0)}})

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	if _, ok := meta[coreexecutor.MaxAuthPriorityMetadataKey]; ok {
		t.Fatalf("metadata should not contain %q for non-inline provider", coreexecutor.MaxAuthPriorityMetadataKey)
	}
}

func TestApplyClientRoutingPolicyMetadata_UsesAuthenticatedPrincipalFromGin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=key-limited", nil)
	req.Header.Set("Authorization", "Bearer key-open")
	ginCtx.Request = req
	ginCtx.Set("apiKey", "key-open")
	ginCtx.Set("accessProvider", sdkaccess.DefaultAccessProviderName)

	meta := map[string]any{"idempotency_key": "req-3"}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &sdkconfig.SDKConfig{APIKeys: []string{"key-open", "key-limited"}}
	cfg.SetClientAPIKeyEntries([]sdkconfig.ClientAPIKey{
		{Key: "key-open"},
		{Key: "key-limited", MaxPriority: intPtr(1)},
	})

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	if _, ok := meta[coreexecutor.MaxAuthPriorityMetadataKey]; ok {
		t.Fatalf("metadata should follow gin principal key-open instead of request fallback key-limited")
	}
}

func TestApplyClientRoutingPolicyMetadata_ReadsBearerKeyDirectlyFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer key-direct")
	ginCtx.Request = req

	meta := map[string]any{"idempotency_key": "req-3"}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &sdkconfig.SDKConfig{APIKeys: []string{"key-direct"}}
	cfg.SetClientAPIKeyEntries([]sdkconfig.ClientAPIKey{{Key: "key-direct", MaxPriority: intPtr(5)}})

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	got, ok := meta[coreexecutor.MaxAuthPriorityMetadataKey]
	if !ok {
		t.Fatalf("metadata missing %q", coreexecutor.MaxAuthPriorityMetadataKey)
	}
	value, ok := got.(int)
	if !ok || value != 5 {
		t.Fatalf("metadata[%q] = %#v, want 5", coreexecutor.MaxAuthPriorityMetadataKey, got)
	}
}

func TestApplyClientRoutingPolicyMetadata_AllowsRequestPathOnlyMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer key-direct")
	ginCtx.Request = req

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.RequestPathMetadataKey]; got != "/v1/chat/completions" {
		t.Fatalf("request path metadata = %#v, want /v1/chat/completions", got)
	}
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("metadata should not fabricate %q before routing policy: %#v", idempotencyKeyMetadataKey, meta)
	}

	cfg := &sdkconfig.SDKConfig{APIKeys: []string{"key-direct"}}
	cfg.SetClientAPIKeyEntries([]sdkconfig.ClientAPIKey{{Key: "key-direct", MaxPriority: intPtr(5)}})

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("metadata should not fabricate %q: %#v", idempotencyKeyMetadataKey, meta)
	}
	got, ok := meta[coreexecutor.MaxAuthPriorityMetadataKey]
	if !ok {
		t.Fatalf("metadata missing %q after empty metadata path", coreexecutor.MaxAuthPriorityMetadataKey)
	}
	value, ok := got.(int)
	if !ok || value != 5 {
		t.Fatalf("metadata[%q] = %#v, want 5", coreexecutor.MaxAuthPriorityMetadataKey, got)
	}
}

func TestApplyClientRoutingPolicyMetadata_ReadsQueryKeyDirectlyFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/v1/models?key=query-key", nil)
	ginCtx.Request = req

	meta := map[string]any{"idempotency_key": "req-4"}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &sdkconfig.SDKConfig{APIKeys: []string{"query-key"}}
	cfg.SetClientAPIKeyEntries([]sdkconfig.ClientAPIKey{{Key: "query-key", MaxPriority: intPtr(2)}})

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	got, ok := meta[coreexecutor.MaxAuthPriorityMetadataKey]
	if !ok {
		t.Fatalf("metadata missing %q", coreexecutor.MaxAuthPriorityMetadataKey)
	}
	value, ok := got.(int)
	if !ok || value != 2 {
		t.Fatalf("metadata[%q] = %#v, want 2", coreexecutor.MaxAuthPriorityMetadataKey, got)
	}
}

func TestApplyClientRoutingPolicyMetadata_RequestFallbackKeepsFirstAuthenticatedCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=key-limited", nil)
	req.Header.Set("Authorization", "Bearer key-open")
	ginCtx.Request = req

	meta := map[string]any{"idempotency_key": "req-5"}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &sdkconfig.SDKConfig{APIKeys: []string{"key-open", "key-limited"}}
	cfg.SetClientAPIKeyEntries([]sdkconfig.ClientAPIKey{
		{Key: "key-open"},
		{Key: "key-limited", MaxPriority: intPtr(1)},
	})

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	if _, ok := meta[coreexecutor.MaxAuthPriorityMetadataKey]; ok {
		t.Fatalf("metadata should follow first authenticated request candidate key-open")
	}
}

func TestApplyClientRoutingPolicyMetadata_IgnoresUnknownRequestKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer unknown-key")
	ginCtx.Request = req

	meta := map[string]any{"idempotency_key": "req-6"}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &sdkconfig.SDKConfig{APIKeys: []string{"known-key"}}
	cfg.SetClientAPIKeyEntries([]sdkconfig.ClientAPIKey{{Key: "known-key", MaxPriority: intPtr(1)}})

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	if _, ok := meta[coreexecutor.MaxAuthPriorityMetadataKey]; ok {
		t.Fatalf("metadata should not contain %q for unknown request key", coreexecutor.MaxAuthPriorityMetadataKey)
	}
}

func intPtr(v int) *int { return &v }
