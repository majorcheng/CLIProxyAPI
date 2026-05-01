package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const apiKeyUsageRecentBucketCount = 20

// apiKeyUsageAuthFixture 描述测试 auth 的关键账号字段。
type apiKeyUsageAuthFixture struct {
	id       string
	provider string
	apiKey   string
	email    string
}

// apiKeyUsageTotalExpectation 描述单个 provider/api_key 分组的预期总量。
type apiKeyUsageTotalExpectation struct {
	provider    string
	apiKey      string
	wantSuccess int64
	wantFailed  int64
}

func TestGetAPIKeyUsage_GroupsByProviderAndAPIKey(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	registerAPIKeyUsageAuth(t, manager, apiKeyUsageAuthFixture{id: "codex-auth", provider: "codex", apiKey: "codex-key"})
	registerAPIKeyUsageAuth(t, manager, apiKeyUsageAuthFixture{id: "claude-auth", provider: "claude", apiKey: "claude-key"})
	registerAPIKeyUsageAuth(t, manager, apiKeyUsageAuthFixture{id: "iflow-auth", provider: "iflow", apiKey: "iflow-key", email: "iflow@example.com"})
	registerAPIKeyUsageAuth(t, manager, apiKeyUsageAuthFixture{id: "codex-oauth", provider: "codex", email: "oauth@example.com"})

	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-auth", Provider: "codex", Model: "gpt-5", Success: true})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-auth", Provider: "codex", Model: "gpt-5", Success: false})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "claude-auth", Provider: "claude", Model: "claude-4", Success: true})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "iflow-auth", Provider: "iflow", Model: "iflow", Success: true})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-oauth", Provider: "codex", Model: "gpt-5", Success: true})

	payload := requestAPIKeyUsage(t, manager)
	assertAPIKeyUsageTotals(t, payload, apiKeyUsageTotalExpectation{provider: "codex", apiKey: "codex-key", wantSuccess: 1, wantFailed: 1})
	assertAPIKeyUsageTotals(t, payload, apiKeyUsageTotalExpectation{provider: "claude", apiKey: "claude-key", wantSuccess: 1})
	assertAPIKeyUsageTotals(t, payload, apiKeyUsageTotalExpectation{provider: "iflow", apiKey: "iflow-key", wantSuccess: 1})
	if _, exists := payload["codex"]["oauth@example.com"]; exists {
		t.Fatalf("oauth account should not be included in api-key usage: %#v", payload["codex"])
	}
}

func TestGetAPIKeyUsage_MergesSameProviderAndAPIKey(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	registerAPIKeyUsageAuth(t, manager, apiKeyUsageAuthFixture{id: "codex-a", provider: "codex", apiKey: "shared-key"})
	registerAPIKeyUsageAuth(t, manager, apiKeyUsageAuthFixture{id: "codex-b", provider: "CoDeX", apiKey: "shared-key"})

	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-a", Provider: "codex", Model: "gpt-5", Success: true})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-b", Provider: "codex", Model: "gpt-5", Success: false})

	payload := requestAPIKeyUsage(t, manager)
	assertAPIKeyUsageTotals(t, payload, apiKeyUsageTotalExpectation{provider: "codex", apiKey: "shared-key", wantSuccess: 1, wantFailed: 1})
	if len(payload["codex"]) != 1 {
		t.Fatalf("codex api-key groups = %#v, want only shared-key", payload["codex"])
	}
}

// registerAPIKeyUsageAuth 注册测试 auth，可同时模拟纯 API-key、纯 OAuth 和 email+api_key 形态。
func registerAPIKeyUsageAuth(t *testing.T, manager *coreauth.Manager, fixture apiKeyUsageAuthFixture) {
	t.Helper()
	auth := &coreauth.Auth{
		ID:       fixture.id,
		Provider: fixture.provider,
		Status:   coreauth.StatusActive,
	}
	if fixture.apiKey != "" {
		auth.Attributes = map[string]string{"api_key": fixture.apiKey}
	}
	if fixture.email != "" {
		auth.Metadata = map[string]any{"email": fixture.email}
	}
	_, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth)
	if err != nil {
		t.Fatalf("register usage auth %s: %v", fixture.id, err)
	}
}

// requestAPIKeyUsage 直接调用 handler，返回解码后的 API-key usage 响应。
func requestAPIKeyUsage(t *testing.T, manager *coreauth.Manager) map[string]map[string][]coreauth.RecentRequestBucket {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	h.GetAPIKeyUsage(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]map[string][]coreauth.RecentRequestBucket
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}

// assertAPIKeyUsageTotals 校验指定 provider/api_key 的合并后成功/失败总量。
func assertAPIKeyUsageTotals(t *testing.T, payload map[string]map[string][]coreauth.RecentRequestBucket, want apiKeyUsageTotalExpectation) {
	t.Helper()
	buckets := payload[want.provider][want.apiKey]
	if len(buckets) != apiKeyUsageRecentBucketCount {
		t.Fatalf("%s/%s buckets len = %d, want %d", want.provider, want.apiKey, len(buckets), apiKeyUsageRecentBucketCount)
	}
	success, failed := sumAPIKeyUsageBuckets(buckets)
	if success != want.wantSuccess || failed != want.wantFailed {
		t.Fatalf("%s/%s totals = %d/%d, want %d/%d", want.provider, want.apiKey, success, failed, want.wantSuccess, want.wantFailed)
	}
}

// sumAPIKeyUsageBuckets 汇总所有 recent request 桶里的成功与失败计数。
func sumAPIKeyUsageBuckets(buckets []coreauth.RecentRequestBucket) (int64, int64) {
	var success int64
	var failed int64
	for _, bucket := range buckets {
		success += bucket.Success
		failed += bucket.Failed
	}
	return success, failed
}
