package handlers

import (
	"context"
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
		PriorityZeroDisabledAPIKeys: []string{" key-b ", "key-b"},
	}

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	got, ok := meta[coreexecutor.DisallowPriorityZeroAuthMetadataKey]
	if !ok {
		t.Fatalf("metadata missing %q", coreexecutor.DisallowPriorityZeroAuthMetadataKey)
	}
	flag, ok := got.(bool)
	if !ok || !flag {
		t.Fatalf("metadata[%q] = %#v, want true", coreexecutor.DisallowPriorityZeroAuthMetadataKey, got)
	}
}

func TestApplyClientRoutingPolicyMetadata_IgnoresNonInlineAccessProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("apiKey", "key-b")
	ginCtx.Set("accessProvider", "external-provider")

	meta := map[string]any{"idempotency_key": "req-2"}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &sdkconfig.SDKConfig{
		PriorityZeroDisabledAPIKeys: []string{"key-b"},
	}

	applyClientRoutingPolicyMetadata(meta, ctx, cfg)

	if _, ok := meta[coreexecutor.DisallowPriorityZeroAuthMetadataKey]; ok {
		t.Fatalf("metadata should not contain %q for non-inline provider", coreexecutor.DisallowPriorityZeroAuthMetadataKey)
	}
}
