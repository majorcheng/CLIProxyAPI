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

func TestListAuthFiles_IncludesRecentRequestsBuckets(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       "runtime-only-auth-1",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), record); err != nil {
		t.Fatalf("failed to register auth record: %v", err)
	}
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: record.ID, Provider: "codex", Model: "gpt-5", Success: true})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: record.ID, Provider: "codex", Model: "gpt-5", Success: false})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)

	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode list payload: %v", err)
	}
	filesRaw, ok := payload["files"].([]any)
	if !ok || len(filesRaw) != 1 {
		t.Fatalf("expected one files entry, got %#v", payload["files"])
	}
	fileEntry, ok := filesRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file entry object, got %#v", filesRaw[0])
	}
	assertRecentRequestBuckets(t, fileEntry["recent_requests"])
}

func assertRecentRequestBuckets(t *testing.T, raw any) {
	t.Helper()
	recentRaw, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected recent_requests array, got %#v", raw)
	}
	if len(recentRaw) != 20 {
		t.Fatalf("expected 20 recent_requests buckets, got %d", len(recentRaw))
	}
	var successTotal float64
	var failedTotal float64
	for idx, item := range recentRaw {
		bucket, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected bucket object at %d, got %#v", idx, item)
		}
		if _, ok := bucket["time"].(string); !ok {
			t.Fatalf("expected bucket time string at %d, got %#v", idx, bucket["time"])
		}
		success, successOK := bucket["success"].(float64)
		failed, failedOK := bucket["failed"].(float64)
		if !successOK || !failedOK {
			t.Fatalf("expected bucket counts at %d, got %#v", idx, bucket)
		}
		successTotal += success
		failedTotal += failed
	}
	if successTotal != 1 || failedTotal != 1 {
		t.Fatalf("recent request totals = success=%v failed=%v, want 1/1", successTotal, failedTotal)
	}
}
