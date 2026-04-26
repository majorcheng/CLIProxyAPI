package executor

import (
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestUsageReporterBuildRecordIncludesLatency(t *testing.T) {
	reporter := &usageReporter{
		provider:    "openai",
		model:       "gpt-5.4",
		authType:    "apikey",
		requestedAt: time.Now().Add(-1500 * time.Millisecond),
	}

	record := reporter.buildRecord(usage.Detail{TotalTokens: 3}, false)
	if record.Latency < time.Second {
		t.Fatalf("latency = %v, want >= 1s", record.Latency)
	}
	if record.Latency > 3*time.Second {
		t.Fatalf("latency = %v, want <= 3s", record.Latency)
	}
	if record.AuthType != "apikey" {
		t.Fatalf("auth type = %q, want %q", record.AuthType, "apikey")
	}
}

func TestUsageReporterBuildRecordForAdditionalModelOverridesModel(t *testing.T) {
	reporter := &usageReporter{
		provider:    "codex",
		model:       "gpt-5.4",
		authType:    "oauth",
		requestedAt: time.Now(),
	}

	record := reporter.buildRecordForModel("gpt-image-2", usage.Detail{InputTokens: 1, OutputTokens: 2}, false)
	if record.Model != "gpt-image-2" {
		t.Fatalf("model = %q, want %q", record.Model, "gpt-image-2")
	}
	if record.AuthType != "oauth" {
		t.Fatalf("auth type = %q, want %q", record.AuthType, "oauth")
	}
}

func TestParseCodexImageToolUsage(t *testing.T) {
	data := []byte(`{"response":{"tool_usage":{"image_gen":{"input_tokens":11,"output_tokens":22,"total_tokens":33,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":5}}}}}`)

	detail, ok := parseCodexImageToolUsage(data)
	if !ok {
		t.Fatal("expected image tool usage to be parsed")
	}
	if detail.InputTokens != 11 || detail.OutputTokens != 22 || detail.TotalTokens != 33 {
		t.Fatalf("detail = %+v, want input=11 output=22 total=33", detail)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want 4", detail.CachedTokens)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want 5", detail.ReasoningTokens)
	}
}

func TestResolveUsageAuthTypeNormalizesAPIKey(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key": "sk-test",
		},
	}
	if got := resolveUsageAuthType(auth); got != "apikey" {
		t.Fatalf("resolveUsageAuthType() = %q, want %q", got, "apikey")
	}
}
