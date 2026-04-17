package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestThinkingIFlowQwen3MaxPreviewUsesEnableThinking(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	uid := fmt.Sprintf("thinking-iflow-qwen3-%d", time.Now().UnixNano())
	reg.RegisterClient(uid, "test", []*registry.ModelInfo{
		{
			ID:          "qwen3-max-preview",
			Object:      "model",
			Created:     1700000000,
			OwnedBy:     "test",
			Type:        "iflow",
			DisplayName: "Qwen3 Max Preview",
			Thinking:    &registry.ThinkingSupport{Levels: []string{"none", "auto", "minimal", "low", "medium", "high", "xhigh"}},
		},
	})
	defer reg.UnregisterClient(uid)

	cases := []struct {
		name       string
		model      string
		inputJSON  string
		expectBool bool
	}{
		{
			name:       "suffix high",
			model:      "qwen3-max-preview(high)",
			inputJSON:  `{"model":"qwen3-max-preview(high)","messages":[{"role":"user","content":"hi"}]}`,
			expectBool: true,
		},
		{
			name:       "body none",
			model:      "qwen3-max-preview",
			inputJSON:  `{"model":"qwen3-max-preview","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`,
			expectBool: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseModel := thinking.ParseSuffix(tc.model).ModelName
			body := sdktranslator.TranslateRequest(
				sdktranslator.FromString("openai"),
				sdktranslator.FromString("openai"),
				baseModel,
				[]byte(tc.inputJSON),
				true,
			)

			out, err := thinking.ApplyThinking(body, tc.model, "openai", "iflow", "iflow")
			if err != nil {
				t.Fatalf("ApplyThinking() error = %v", err)
			}

			got := gjson.GetBytes(out, "chat_template_kwargs.enable_thinking")
			if !got.Exists() {
				t.Fatalf("expected chat_template_kwargs.enable_thinking, body=%s", string(out))
			}
			if got.Bool() != tc.expectBool {
				t.Fatalf("enable_thinking = %v, want %v, body=%s", got.Bool(), tc.expectBool, string(out))
			}
			if gjson.GetBytes(out, "reasoning_split").Exists() {
				t.Fatalf("unexpected reasoning_split for qwen3-max-preview, body=%s", string(out))
			}
			if gjson.GetBytes(out, "chat_template_kwargs.clear_thinking").Exists() {
				t.Fatalf("unexpected clear_thinking for non-GLM qwen3-max-preview, body=%s", string(out))
			}
		})
	}
}
