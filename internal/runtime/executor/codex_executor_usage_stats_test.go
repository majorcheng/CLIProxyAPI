package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	runtimeusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecute_DoesNotPublishImageUsageForPlainTextRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+plainTextCodexCompletedWithImageUsageMetadataJSON("resp_text_only", "gpt-5.2")+"\n")
	}))
	defer server.Close()

	apiKey := fmt.Sprintf("plain-text-no-image-%d", time.Now().UnixNano())
	ctx := newCodexUsageTestContext(apiKey)
	executor := NewCodexExecutor(&config.Config{})
	resp, err := executor.Execute(
		ctx,
		newCodexTestAuth(server.URL, "plain-text-key"),
		newCodexTextRequest("gpt-5.2", "just answer with text"),
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotID := gjson.GetBytes(resp.Payload, "id").String(); gotID != "resp_text_only" {
		t.Fatalf("response id = %q, want %q", gotID, "resp_text_only")
	}

	waitForUsageModel(t, apiKey, "gpt-5.2")
	assertUsageModelAbsent(t, apiKey, "gpt-image-2", 300*time.Millisecond)
}

func TestCodexExecutorExecute_PublishesImageUsageForRealImageCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+codexCompletedWithImageCallJSON("resp_image_call", "gpt-5.2")+"\n")
	}))
	defer server.Close()

	apiKey := fmt.Sprintf("real-image-call-%d", time.Now().UnixNano())
	ctx := newCodexUsageTestContext(apiKey)
	executor := NewCodexExecutor(&config.Config{})
	resp, err := executor.Execute(
		ctx,
		newCodexTestAuth(server.URL, "image-call-key"),
		newCodexImageIntentRequest("gpt-5.2", "draw a cat", "team-a/gpt-image-2"),
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotID := gjson.GetBytes(resp.Payload, "id").String(); gotID != "resp_image_call" {
		t.Fatalf("response id = %q, want %q", gotID, "resp_image_call")
	}

	modelStats := waitForUsageModel(t, apiKey, "team-a/gpt-image-2")
	if modelStats.TotalRequests < 1 {
		t.Fatalf("image model requests = %d, want >= 1", modelStats.TotalRequests)
	}
	if modelStats.TotalTokens != 0 {
		t.Fatalf("image model total tokens = %d, want 0 when completed payload has no image usage token block", modelStats.TotalTokens)
	}
	assertUsageModelAbsent(t, apiKey, "gpt-image-2", 300*time.Millisecond)
}

func TestCodexExecutorExecuteStream_PreservesMainUsageCountWithoutUsageFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+plainTextCodexCompletedWithoutUsageWithImageMetadataJSON("resp_stream_text_only", "gpt-5.2")+"\n")
	}))
	defer server.Close()

	apiKey := fmt.Sprintf("stream-no-usage-no-image-%d", time.Now().UnixNano())
	ctx := newCodexUsageTestContext(apiKey)
	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(
		ctx,
		newCodexTestAuth(server.URL, "stream-text-key"),
		newCodexStreamTextRequest("gpt-5.2", "stream plain text only"),
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	modelStats := waitForUsageModel(t, apiKey, "gpt-5.2")
	if modelStats.TotalRequests < 1 {
		t.Fatalf("main model requests = %d, want >= 1", modelStats.TotalRequests)
	}
	assertUsageModelAbsent(t, apiKey, "gpt-image-2", 300*time.Millisecond)
}

// newCodexUsageTestContext 构造带唯一 apiKey 的 gin 上下文，便于从全局 usage 快照中隔离当前测试记录。
func newCodexUsageTestContext(apiKey string) context.Context {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("apiKey", apiKey)
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func newCodexTextRequest(model string, prompt string) cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model: model,
		Payload: []byte(fmt.Sprintf(`{
			"model":%q,
			"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}],
			"stream":false
		}`, model, prompt)),
	}
}

func newCodexImageIntentRequest(model string, prompt string, imageModel string) cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model: model,
		Payload: []byte(fmt.Sprintf(`{
			"model":%q,
			"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}],
			"tool_choice":{"type":"image_generation"},
			"tools":[{"type":"image_generation","model":%q,"output_format":"png"}],
			"stream":false
		}`, model, prompt, imageModel)),
	}
}

func newCodexStreamTextRequest(model string, prompt string) cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model: model,
		Payload: []byte(fmt.Sprintf(`{
			"model":%q,
			"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}],
			"stream":true
		}`, model, prompt)),
	}
}

func plainTextCodexCompletedWithImageUsageMetadataJSON(id string, model string) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"object":"response","model":%q,"status":"completed","output":[{"type":"message","id":"msg_%s","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"tool_usage":{"image_gen":{"num_images":1}}}}`, id, model, id)
}

func codexCompletedWithImageCallJSON(id string, model string) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"object":"response","model":%q,"status":"completed","output":[{"type":"message","id":"msg_%s","role":"assistant","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","result":"aGVsbG8=","output_format":"png"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`, id, model, id)
}

func plainTextCodexCompletedWithoutUsageWithImageMetadataJSON(id string, model string) string {
	return fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"object":"response","model":%q,"status":"completed","output":[{"type":"message","id":"msg_%s","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"tool_usage":{"image_gen":{"num_images":1}}}}`, id, model, id)
}

func waitForUsageModel(t *testing.T, apiKey string, model string) runtimeusage.ModelSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if modelStats, ok := usageModelSnapshot(apiKey, model); ok {
			return modelStats
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for usage statistics record for apiKey=%q model=%q", apiKey, model)
	return runtimeusage.ModelSnapshot{}
}

func assertUsageModelAbsent(t *testing.T, apiKey string, model string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if _, ok := usageModelSnapshot(apiKey, model); ok {
			t.Fatalf("unexpected usage statistics record for apiKey=%q model=%q", apiKey, model)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func usageModelSnapshot(apiKey string, model string) (runtimeusage.ModelSnapshot, bool) {
	snapshot := runtimeusage.GetRequestStatistics().Snapshot()
	apiStats, ok := snapshot.APIs[apiKey]
	if !ok {
		return runtimeusage.ModelSnapshot{}, false
	}
	modelStats, ok := apiStats.Models[model]
	return modelStats, ok
}
