package executor

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func validCodexReplayEncryptedContentForTest(seed byte) string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for index := 9; index < len(payload); index++ {
		payload[index] = seed + byte(index)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func TestCodexPrepareRequestPlan_InsertsClaudeReasoningReplay(t *testing.T) {
	internalcache.ClearCodexReasoningReplayCache()
	t.Cleanup(internalcache.ClearCodexReasoningReplayCache)

	encryptedContent := validCodexReplayEncryptedContentForTest(11)
	item := []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + encryptedContent + `"}`)
	if !internalcache.CacheCodexReasoningReplayItem("gpt-5.4", "claude:session-plan", item) {
		t.Fatal("cache replay item")
	}

	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"device_id\":\"device-a\",\"session_id\":\"session-plan\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}],
			"max_tokens":100
		}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	plan, err := (&CodexExecutor{}).prepareCodexRequestPlan(context.Background(), req, opts, codexPreparedRequestPlanExecute)
	if err != nil {
		t.Fatalf("prepareCodexRequestPlan() error = %v", err)
	}

	if plan.replayScope.sessionKey != "claude:session-plan" {
		t.Fatalf("replay session key = %q, want claude:session-plan", plan.replayScope.sessionKey)
	}
	if got := gjson.GetBytes(plan.body, `input.#(type=="reasoning").encrypted_content`).String(); got != encryptedContent {
		t.Fatalf("replay encrypted_content = %q, want %q; body=%s", got, encryptedContent, string(plan.body))
	}
	if got := gjson.GetBytes(plan.body, "prompt_cache_key").String(); got == "" {
		t.Fatalf("prompt_cache_key should still be set for Claude Code session; body=%s", string(plan.body))
	}
}

func TestCodexPrepareRequestPlan_DoesNotCacheInjectedReplay(t *testing.T) {
	internalcache.ClearCodexReasoningReplayCache()
	t.Cleanup(internalcache.ClearCodexReasoningReplayCache)

	oldContent := validCodexReplayEncryptedContentForTest(21)
	newContent := validCodexReplayEncryptedContentForTest(23)
	oldItem := []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + oldContent + `"}`)
	newItem := []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + newContent + `"}`)
	if !internalcache.CacheCodexReasoningReplayItem("gpt-5.4", "claude:session-prepared", oldItem) {
		t.Fatal("cache old replay item")
	}

	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(fmt.Sprintf(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"device_id\":\"device-a\",\"session_id\":\"session-prepared\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":%q}]}],
			"max_tokens":100
		}`, strings.Repeat("hello ", 2048))),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.4",
		},
	}

	executor := &CodexExecutor{}
	firstPlan, err := executor.prepareCodexRequestPlan(context.Background(), req, opts, codexPreparedRequestPlanExecute)
	if err != nil {
		t.Fatalf("first prepareCodexRequestPlan() error = %v", err)
	}
	if got := gjson.GetBytes(firstPlan.body, `input.#(type=="reasoning").encrypted_content`).String(); got != oldContent {
		t.Fatalf("first replay encrypted_content = %q, want old item", got)
	}

	invalidBody := []byte(`{"error":{"message":"invalid_encrypted_content"}}`)
	if !clearCodexReasoningReplayOnInvalidSignature(firstPlan.replayScope, 400, invalidBody) {
		t.Fatal("expected injected replay invalidation marker")
	}
	if !internalcache.CacheCodexReasoningReplayItem("gpt-5.4", "claude:session-prepared", newItem) {
		t.Fatal("cache new replay item")
	}

	secondPlan, err := executor.prepareCodexRequestPlan(context.Background(), req, opts, codexPreparedRequestPlanExecute)
	if err != nil {
		t.Fatalf("second prepareCodexRequestPlan() error = %v", err)
	}
	if got := gjson.GetBytes(secondPlan.body, `input.#(type=="reasoning").encrypted_content`).String(); got != newContent {
		t.Fatalf("second replay encrypted_content = %q, want new item; body=%s", got, string(secondPlan.body))
	}
	if strings.Contains(string(secondPlan.body), oldContent) {
		t.Fatalf("prepared request cache reused stale replay body: %s", string(secondPlan.body))
	}
}

// TestCodexReasoningReplayDropsFunctionCallWithoutMatchingOutput 验证 replay 不注入孤儿 tool call。
func TestCodexReasoningReplayDropsFunctionCallWithoutMatchingOutput(t *testing.T) {
	internalcache.ClearCodexReasoningReplayCache()
	t.Cleanup(internalcache.ClearCodexReasoningReplayCache)

	encryptedContent := validCodexReplayEncryptedContentForTest(25)
	scope := codexReasoningReplayScope{
		modelName:  "gpt-5.4",
		sessionKey: "claude:session-dropped-tool",
	}
	completed := []byte(`{"response":{"output":[` +
		`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + encryptedContent + `"},` +
		`{"type":"function_call","call_id":"call_dropped","name":"TaskCreate","arguments":"{}"}` +
		`]}}`)
	cacheCodexReasoningReplayFromCompleted(scope, completed)

	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)
	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"device_id\":\"device-test\",\"session_id\":\"session-dropped-tool\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}]
		}`),
	}

	updated, replayScope := applyCodexReasoningReplayCache(
		context.Background(),
		sdktranslator.FormatClaude,
		req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
		body,
	)
	if replayScope.modelName != scope.modelName || replayScope.sessionKey != scope.sessionKey {
		t.Fatalf("replay scope = %#v, want model=%q session=%q", replayScope, scope.modelName, scope.sessionKey)
	}
	if got := gjson.GetBytes(updated, "input.0.type").String(); got != "reasoning" {
		t.Fatalf("input.0.type = %q, want reasoning; body=%s", got, string(updated))
	}
	if got := gjson.GetBytes(updated, "input.0.encrypted_content").String(); got != encryptedContent {
		t.Fatalf("input.0.encrypted_content = %q, want cached reasoning; body=%s", got, string(updated))
	}
	if gjson.GetBytes(updated, `input.#(call_id=="call_dropped")`).Exists() {
		t.Fatalf("cached function_call without matching output should not be replayed; body=%s", string(updated))
	}
	if got := gjson.GetBytes(updated, "input.1.role").String(); got != "user" {
		t.Fatalf("input.1.role = %q, want user; body=%s", got, string(updated))
	}
}

func TestCodexReasoningReplaySkipsNativeOpenAIResponses(t *testing.T) {
	internalcache.ClearCodexReasoningReplayCache()
	t.Cleanup(internalcache.ClearCodexReasoningReplayCache)

	encryptedContent := validCodexReplayEncryptedContentForTest(13)
	item := []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + encryptedContent + `"}`)
	if !internalcache.CacheCodexReasoningReplayItem("gpt-5.4", "prompt-cache:native-session", item) {
		t.Fatal("cache replay item")
	}

	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"native-session","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`),
	}
	body, scope := applyCodexReasoningReplayCache(context.Background(), sdktranslator.FormatOpenAIResponse, req, cliproxyexecutor.Options{}, req.Payload)
	if scope.valid() {
		t.Fatalf("native OpenAI Responses replay scope should be invalid, got %+v", scope)
	}
	if got := gjson.GetBytes(body, `input.#(type=="reasoning")`); got.Exists() {
		t.Fatalf("native OpenAI Responses should not inject replay item: %s", string(body))
	}

	completed := []byte(`{"response":{"output":[{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + encryptedContent + `"}]}}`)
	cacheCodexReasoningReplayFromCompleted(scope, completed)
	if _, ok := internalcache.GetCodexReasoningReplayItem("gpt-5.4", "prompt-cache:native-session"); !ok {
		t.Fatal("seeded native-session cache should be untouched by invalid scope")
	}
}

func TestCodexReasoningReplayClearsInvalidSignatureCache(t *testing.T) {
	internalcache.ClearCodexReasoningReplayCache()
	t.Cleanup(internalcache.ClearCodexReasoningReplayCache)

	encryptedContent := validCodexReplayEncryptedContentForTest(17)
	item := []byte(`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"` + encryptedContent + `"}`)
	if !internalcache.CacheCodexReasoningReplayItem("gpt-5.4", "claude:session-clear", item) {
		t.Fatal("cache replay item")
	}

	scope := codexReasoningReplayScope{modelName: "gpt-5.4", sessionKey: "claude:session-clear"}
	body := []byte(`{"error":{"message":"invalid_encrypted_content"}}`)
	if clearCodexReasoningReplayOnInvalidSignature(scope, 400, body) {
		t.Fatal("non-injected scope must not be marked as internal replay failure")
	}
	if _, ok := internalcache.GetCodexReasoningReplayItem("gpt-5.4", "claude:session-clear"); ok {
		t.Fatal("invalid signature should clear replay cache")
	}
}

func TestCodexReasoningReplayInvalidErrorSkipsRequestBlocker(t *testing.T) {
	scope := codexReasoningReplayScope{modelName: "gpt-5.4", sessionKey: "claude:session-error", injected: true}
	body := []byte(`{"error":{"message":"invalid_encrypted_content"}}`)
	err := wrapCodexReasoningReplayInvalidErr(scope, 400, body, newCodexStatusErr(400, body))

	skipper, ok := err.(interface{ SkipInvalidRequestBlock() bool })
	if !ok || !skipper.SkipInvalidRequestBlock() {
		t.Fatalf("wrapped replay error should skip request blocker, got %#v", err)
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != 400 {
		t.Fatalf("wrapped replay error status = %#v, want 400", err)
	}
}

func TestCodexClaudePromptCacheRejectsBareUserID(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"metadata":{"user_id":"same-user-across-chats"},"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`),
	}

	if got := codexPromptCacheID(context.Background(), sdktranslator.FormatClaude, req); got != "" {
		t.Fatalf("bare metadata.user_id should not create prompt_cache_key, got %q", got)
	}
	body, headers := applyCodexPromptCacheHeaders(sdktranslator.FormatClaude, req, []byte(`{"model":"gpt-5.4","stream":true}`))
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "" {
		t.Fatalf("websocket prompt_cache_key = %q, want empty", got)
	}
	if got := headers.Get("Session_id"); got != "" {
		t.Fatalf("websocket Session_id = %q, want empty", got)
	}
}
