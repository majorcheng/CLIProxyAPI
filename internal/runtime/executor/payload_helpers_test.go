package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestApplyPayloadConfigWithRoot_DisableImageGenerationRemovesToolWithoutPayloadRules(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: true},
	}
	payload := []byte(`{"tools":[{"type":"image_generation","output_format":"png"},{"type":"function","name":"demo"}]}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "")

	assertToolTypeCount(t, out, "tools", "image_generation", 0)
	assertToolTypeCount(t, out, "tools", "function", 1)
	if tools := gjson.GetBytes(out, "tools"); !tools.Exists() || !tools.IsArray() {
		t.Fatalf("tools = %s, want existing array", tools.Raw)
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationDeletesEmptyTools(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: true},
	}
	payload := []byte(`{"tools":[{"type":"image_generation"}],"input":"hello"}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "")

	if tools := gjson.GetBytes(out, "tools"); tools.Exists() {
		t.Fatalf("tools = %s，期望删除最后一个 image_generation 后字段不存在", tools.Raw)
	}
	if got := gjson.GetBytes(out, "input").String(); got != "hello" {
		t.Fatalf("input = %q，期望 hello：%s", got, string(out))
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationRemovesToolWithRoot(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: true},
	}
	payload := []byte(`{"request":{"tools":[{"type":"image_generation"},{"type":"web_search"}]}}`)

	out := applyPayloadConfigWithRoot(cfg, "", "gemini-cli", "request", payload, nil, "")

	assertToolTypeCount(t, out, "request.tools", "image_generation", 0)
	assertToolTypeCount(t, out, "request.tools", "web_search", 1)
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationKeepsToolWhenFlagOff(t *testing.T) {
	cfg := &config.Config{}
	payload := []byte(`{"tools":[{"type":"image_generation"},{"type":"function","name":"demo"}]}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "")

	assertToolTypeCount(t, out, "tools", "image_generation", 1)
	assertToolTypeCount(t, out, "tools", "function", 1)
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationRemovesToolChoiceTools(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: true},
	}
	payload := []byte(`{
		"tools":[{"type":"function","name":"demo"}],
		"tool_choice":{
			"type":"allowed_tools",
			"tools":[{"type":"image_generation"},{"type":"function","name":"demo"}]
		}
	}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "")

	assertToolTypeCount(t, out, "tools", "function", 1)
	assertToolTypeCount(t, out, "tool_choice.tools", "image_generation", 0)
	assertToolTypeCount(t, out, "tool_choice.tools", "function", 1)
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q，期望 allowed_tools：%s", got, string(out))
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationDeletesEmptyAllowedToolsChoice(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: true},
	}
	payload := []byte(`{"tool_choice":{"type":"allowed_tools","tools":[{"type":"image_generation"}]}}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "")

	if choice := gjson.GetBytes(out, "tool_choice"); choice.Exists() {
		t.Fatalf("tool_choice = %s，期望删除最后一个 allowed image 工具后字段不存在", choice.Raw)
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationRunsAfterPayloadRules(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: true},
		Payload: config.PayloadConfig{
			OverrideRaw: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "openai-response"}},
					Params: map[string]any{
						"tools": `[{"type":"image_generation"},{"type":"function","name":"demo"}]`,
					},
				},
			},
		},
	}

	out := applyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai-response", "", []byte(`{"input":"hello"}`), nil, "")

	assertToolTypeCount(t, out, "tools", "image_generation", 0)
	assertToolTypeCount(t, out, "tools", "function", 1)
}

// assertToolTypeCount 统计指定 tools 路径下的工具类型数量，避免测试依赖数组顺序。
func assertToolTypeCount(t *testing.T, payload []byte, toolsPath string, toolType string, want int) {
	t.Helper()
	got := 0
	for _, item := range gjson.GetBytes(payload, toolsPath).Array() {
		if item.Get("type").String() == toolType {
			got++
		}
	}
	if got != want {
		t.Fatalf("%s type %q count = %d, want %d: %s", toolsPath, toolType, got, want, string(payload))
	}
}
