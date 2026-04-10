package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestQwenExecutorParseSuffix(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantBase  string
		wantLevel string
	}{
		{"no suffix", "qwen-max", "qwen-max", ""},
		{"with level suffix", "qwen-max(high)", "qwen-max", "high"},
		{"with budget suffix", "qwen-max(16384)", "qwen-max", "16384"},
		{"complex model name", "qwen-plus-latest(medium)", "qwen-plus-latest", "medium"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := thinking.ParseSuffix(tt.model)
			if result.ModelName != tt.wantBase {
				t.Errorf("ParseSuffix(%q).ModelName = %q, want %q", tt.model, result.ModelName, tt.wantBase)
			}
		})
	}
}

func TestEnsureQwenSystemMessagePrependsDefaultWhenMissing(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, err := ensureQwenSystemMessage(payload)
	if err != nil {
		t.Fatalf("ensureQwenSystemMessage returned error: %v", err)
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if got := messages[0].Get("role").String(); got != "system" {
		t.Fatalf("messages[0].role = %q, want system", got)
	}
	if got := messages[1].Get("role").String(); got != "user" {
		t.Fatalf("messages[1].role = %q, want user", got)
	}
}

func TestEnsureQwenSystemMessageKeepsExistingSystemMessage(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"system","content":"preset"},{"role":"user","content":"hi"}]}`)
	out, err := ensureQwenSystemMessage(payload)
	if err != nil {
		t.Fatalf("ensureQwenSystemMessage returned error: %v", err)
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if got := messages[0].Get("content").String(); got != "preset" {
		t.Fatalf("existing system message changed, got %q", got)
	}
}

func TestEnsureQwenSystemMessageCreatesMessagesArrayWhenMissing(t *testing.T) {
	payload := []byte(`{"model":"qwen-max"}`)
	out, err := ensureQwenSystemMessage(payload)
	if err != nil {
		t.Fatalf("ensureQwenSystemMessage returned error: %v", err)
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(messages))
	}
	if got := messages[0].Get("role").String(); got != "system" {
		t.Fatalf("messages[0].role = %q, want system", got)
	}
}

func TestWrapQwenError_InsufficientQuotaDoesNotSetRetryAfter(t *testing.T) {
	body := []byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota","type":"insufficient_quota"}}`)
	code, retryAfter := wrapQwenError(context.Background(), http.StatusTooManyRequests, body)
	if code != http.StatusTooManyRequests {
		t.Fatalf("wrapQwenError status = %d, want %d", code, http.StatusTooManyRequests)
	}
	if retryAfter != nil {
		t.Fatalf("wrapQwenError retryAfter = %v, want nil", *retryAfter)
	}
}

func TestWrapQwenError_Maps403QuotaTo429WithoutRetryAfter(t *testing.T) {
	body := []byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota","type":"insufficient_quota"}}`)
	code, retryAfter := wrapQwenError(context.Background(), http.StatusForbidden, body)
	if code != http.StatusTooManyRequests {
		t.Fatalf("wrapQwenError status = %d, want %d", code, http.StatusTooManyRequests)
	}
	if retryAfter != nil {
		t.Fatalf("wrapQwenError retryAfter = %v, want nil", *retryAfter)
	}
}

func TestQwenCreds_NormalizesResourceURL(t *testing.T) {
	tests := []struct {
		name        string
		resourceURL string
		wantBaseURL string
	}{
		{name: "host only", resourceURL: "portal.qwen.ai", wantBaseURL: "https://portal.qwen.ai/v1"},
		{name: "scheme without v1", resourceURL: "https://portal.qwen.ai", wantBaseURL: "https://portal.qwen.ai/v1"},
		{name: "scheme with v1", resourceURL: "https://portal.qwen.ai/v1", wantBaseURL: "https://portal.qwen.ai/v1"},
		{name: "scheme with trailing slash", resourceURL: "https://portal.qwen.ai/v1/", wantBaseURL: "https://portal.qwen.ai/v1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				Metadata: map[string]any{
					"access_token": "test-token",
					"resource_url": tt.resourceURL,
				},
			}

			token, baseURL := qwenCreds(auth)
			if token != "test-token" {
				t.Fatalf("qwenCreds token = %q, want %q", token, "test-token")
			}
			if baseURL != tt.wantBaseURL {
				t.Fatalf("qwenCreds baseURL = %q, want %q", baseURL, tt.wantBaseURL)
			}
		})
	}
}

func TestQwenExecutorExecute_429RefreshAndRetry(t *testing.T) {
	qwenRateLimiter.Lock()
	qwenRateLimiter.requests = make(map[string][]time.Time)
	qwenRateLimiter.Unlock()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer old-token":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"quota_exceeded","message":"quota exceeded","type":"quota_exceeded"}}`))
		case "Bearer new-token":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"qwen-max","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-test",
		Provider: "qwen",
		Attributes: map[string]string{
			"base_url": srv.URL + "/v1",
		},
		Metadata: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-token",
		},
	}

	var refresherCalls int32
	exec.refreshForImmediateRetry = func(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
		atomic.AddInt32(&refresherCalls, 1)
		refreshed := auth.Clone()
		if refreshed.Metadata == nil {
			refreshed.Metadata = make(map[string]any)
		}
		refreshed.Metadata["access_token"] = "new-token"
		refreshed.Metadata["refresh_token"] = "refresh-token-2"
		return refreshed, nil
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen-max",
		Payload: []byte(`{"model":"qwen-max","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatalf("Execute() payload is empty")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&refresherCalls); got != 1 {
		t.Fatalf("refresher calls = %d, want 1", got)
	}
}

func TestQwenExecutorExecuteStream_429RefreshAndRetry(t *testing.T) {
	qwenRateLimiter.Lock()
	qwenRateLimiter.requests = make(map[string][]time.Time)
	qwenRateLimiter.Unlock()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer old-token":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"quota_exceeded","message":"quota exceeded","type":"quota_exceeded"}}`))
		case "Bearer new-token":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"qwen-max\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	exec := NewQwenExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "auth-test",
		Provider: "qwen",
		Attributes: map[string]string{
			"base_url": srv.URL + "/v1",
		},
		Metadata: map[string]any{
			"access_token":  "old-token",
			"refresh_token": "refresh-token",
		},
	}

	var refresherCalls int32
	exec.refreshForImmediateRetry = func(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
		atomic.AddInt32(&refresherCalls, 1)
		refreshed := auth.Clone()
		if refreshed.Metadata == nil {
			refreshed.Metadata = make(map[string]any)
		}
		refreshed.Metadata["access_token"] = "new-token"
		refreshed.Metadata["refresh_token"] = "refresh-token-2"
		return refreshed, nil
	}

	stream, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen-max",
		Payload: []byte(`{"model":"qwen-max","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&refresherCalls); got != 1 {
		t.Fatalf("refresher calls = %d, want 1", got)
	}

	var sawPayload bool
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		if len(chunk.Payload) > 0 {
			sawPayload = true
		}
	}
	if !sawPayload {
		t.Fatalf("stream did not produce any payload chunks")
	}
}
