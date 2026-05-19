package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorImagesGenerationPassesThroughRawResponse(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"https://img.example/out.png"}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("compat-image", &config.Config{})
	resp, errExec := executor.Execute(context.Background(), openAICompatImagesTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "upstream-image",
		Payload: []byte(`{"model":"alias-image","prompt":"draw","stream":true}`),
	}, openAICompatImagesTestOptions("/v1/images/generations", "application/json"))
	if errExec != nil {
		t.Fatalf("Execute error: %v", errExec)
	}

	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q, want /v1/images/generations", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer key", gotAuth)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("non-stream images 请求不应转发 stream 字段：%s", string(gotBody))
	}
	if string(resp.Payload) != `{"created":1,"data":[{"url":"https://img.example/out.png"}]}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorImagesStreamForwardsRawChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			t.Fatalf("path = %q, want /v1/images/edits", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !gjson.GetBytes(body, "stream").Bool() {
			t.Fatalf("stream 未写入 true：%s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"partial\":1}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("compat-image", &config.Config{})
	result, errExec := executor.ExecuteStream(context.Background(), openAICompatImagesTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "upstream-image",
		Payload: []byte(`{"model":"alias-image","prompt":"edit"}`),
	}, openAICompatImagesTestOptions("/v1/images/edits", "application/json"))
	if errExec != nil {
		t.Fatalf("ExecuteStream error: %v", errExec)
	}

	var joined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}
	if got := joined.String(); got != "data: {\"partial\":1}\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream payload = %q", got)
	}
}

// openAICompatImagesTestAuth 构造仅用于 fake upstream 的兼容图片 auth。
func openAICompatImagesTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": strings.TrimSuffix(baseURL, "/") + "/v1",
		"api_key":  "test-key",
	}}
}

// openAICompatImagesTestOptions 构造触发 Images 专用 executor 分支的 Options。
func openAICompatImagesTestOptions(path string, contentType string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(openAICompatImageHandlerType),
		Headers:      http.Header{"Content-Type": []string{contentType}},
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: path},
	}
}
