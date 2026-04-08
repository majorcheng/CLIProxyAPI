package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
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
