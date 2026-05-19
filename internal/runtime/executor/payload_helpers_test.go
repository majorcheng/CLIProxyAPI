package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestApplyPayloadConfigWithRoot_DisableImageGenerationRemovesToolWithoutPayloadRules(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
	}
	payload := []byte(`{"tools":[{"type":"image_generation","output_format":"png"},{"type":"function","name":"demo"}]}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "", "")

	assertToolTypeCount(t, out, "tools", "image_generation", 0)
	assertToolTypeCount(t, out, "tools", "function", 1)
	if tools := gjson.GetBytes(out, "tools"); !tools.Exists() || !tools.IsArray() {
		t.Fatalf("tools = %s, want existing array", tools.Raw)
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationDeletesEmptyTools(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
	}
	payload := []byte(`{"tools":[{"type":"image_generation"}],"input":"hello"}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "", "")

	if tools := gjson.GetBytes(out, "tools"); tools.Exists() {
		t.Fatalf("tools = %s，期望删除最后一个 image_generation 后字段不存在", tools.Raw)
	}
	if got := gjson.GetBytes(out, "input").String(); got != "hello" {
		t.Fatalf("input = %q，期望 hello：%s", got, string(out))
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationRemovesToolWithRoot(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
	}
	payload := []byte(`{"request":{"tools":[{"type":"image_generation"},{"type":"web_search"}]}}`)

	out := applyPayloadConfigWithRoot(cfg, "", "gemini-cli", "request", payload, nil, "", "")

	assertToolTypeCount(t, out, "request.tools", "image_generation", 0)
	assertToolTypeCount(t, out, "request.tools", "web_search", 1)
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationKeepsToolWhenFlagOff(t *testing.T) {
	cfg := &config.Config{}
	payload := []byte(`{"tools":[{"type":"image_generation"},{"type":"function","name":"demo"}]}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "", "")

	assertToolTypeCount(t, out, "tools", "image_generation", 1)
	assertToolTypeCount(t, out, "tools", "function", 1)
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationRemovesToolChoiceTools(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
	}
	payload := []byte(`{
		"tools":[{"type":"function","name":"demo"}],
		"tool_choice":{
			"type":"allowed_tools",
			"tools":[{"type":"image_generation"},{"type":"function","name":"demo"}]
		}
	}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "", "")

	assertToolTypeCount(t, out, "tools", "function", 1)
	assertToolTypeCount(t, out, "tool_choice.tools", "image_generation", 0)
	assertToolTypeCount(t, out, "tool_choice.tools", "function", 1)
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q，期望 allowed_tools：%s", got, string(out))
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationDeletesEmptyAllowedToolsChoice(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
	}
	payload := []byte(`{"tool_choice":{"type":"allowed_tools","tools":[{"type":"image_generation"}]}}`)

	out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "", "")

	if choice := gjson.GetBytes(out, "tool_choice"); choice.Exists() {
		t.Fatalf("tool_choice = %s，期望删除最后一个 allowed image 工具后字段不存在", choice.Raw)
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationRemovesDirectToolChoice(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
	}
	tests := []struct {
		name    string
		payload string
	}{
		{name: "字符串形式", payload: `{"tool_choice":"image_generation"}`},
		{name: "直接类型", payload: `{"tool_choice":{"type":"image_generation"}}`},
		{name: "tool 名称", payload: `{"tool_choice":{"type":"tool","name":"image_generation"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", []byte(tt.payload), nil, "", "")
			if choice := gjson.GetBytes(out, "tool_choice"); choice.Exists() {
				t.Fatalf("tool_choice = %s，期望删除直接指向 image_generation 的选择", choice.Raw)
			}
		})
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationChatModeHonorsImagesPath(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationChat},
	}
	payload := []byte(`{"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)

	imagesOut := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "", "/api/provider/openai/v1/images/generations")
	assertToolTypeCount(t, imagesOut, "tools", "image_generation", 1)
	if choice := gjson.GetBytes(imagesOut, "tool_choice"); !choice.Exists() {
		t.Fatalf("images path tool_choice 不应被删除：%s", string(imagesOut))
	}

	chatOut := applyPayloadConfigWithRoot(cfg, "", "openai-response", "", payload, nil, "", "/v1/responses")
	if tools := gjson.GetBytes(chatOut, "tools"); tools.Exists() {
		t.Fatalf("chat path tools = %s，期望删除 image_generation 后字段不存在", tools.Raw)
	}
	if choice := gjson.GetBytes(chatOut, "tool_choice"); choice.Exists() {
		t.Fatalf("chat path tool_choice = %s，期望删除 image_generation 选择", choice.Raw)
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGenerationPayloadOverrideCannotRestoreImageGeneration(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
		Payload: config.PayloadConfig{
			OverrideRaw: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "openai-response"}},
					Params: map[string]any{
						"tools":       `[{"type":"image_generation"},{"type":"function","name":"demo"}]`,
						"tool_choice": `{"type":"image_generation"}`,
					},
				},
			},
		},
	}
	payload := []byte(`{"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)

	out := applyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai-response", "", payload, nil, "", "")

	assertToolTypeCount(t, out, "tools", "image_generation", 0)
	assertToolTypeCount(t, out, "tools", "function", 1)
	if choice := gjson.GetBytes(out, "tool_choice"); choice.Exists() {
		t.Fatalf("tool_choice = %s，期望最终图片禁用清理删除 payload override 写回的 image_generation", choice.Raw)
	}
}

func TestApplyPayloadConfigWithRequest_ModelRuleConditionsMatch(t *testing.T) {
	cfg := payloadConditionTestConfig()
	headers := http.Header{"X-Client": []string{"cli-pro"}}
	payload := []byte(`{"mode":"json","temperature":1,"messages":[{"role":"user","content":"hi"}]}`)

	out := applyPayloadConfigWithRequest(cfg, "gpt-5.4", "openai-response", "claude", "", payload, nil, "", "", headers)

	if !gjson.GetBytes(out, "enabled").Bool() {
		t.Fatalf("enabled 未写入，期望命中 from/header/match/exist/not-exist 条件：%s", string(out))
	}
}

func TestApplyPayloadConfigWithRequest_ModelRuleConditionsRejectMismatch(t *testing.T) {
	cfg := payloadConditionTestConfig()
	tests := []struct {
		name         string
		fromProtocol string
		headers      http.Header
		payload      string
	}{
		{
			name:         "来源协议不匹配",
			fromProtocol: "openai",
			headers:      http.Header{"X-Client": []string{"cli-pro"}},
			payload:      `{"mode":"json","temperature":1,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:         "header 不匹配",
			fromProtocol: "claude",
			headers:      http.Header{"X-Client": []string{"web"}},
			payload:      `{"mode":"json","temperature":1,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:         "match 条件不匹配",
			fromProtocol: "claude",
			headers:      http.Header{"X-Client": []string{"cli-pro"}},
			payload:      `{"mode":"text","temperature":1,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:         "not-exist 条件不匹配",
			fromProtocol: "claude",
			headers:      http.Header{"X-Client": []string{"cli-pro"}},
			payload:      `{"mode":"json","temperature":1,"blocked":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := applyPayloadConfigWithRequest(cfg, "gpt-5.4", "openai-response", tt.fromProtocol, "", []byte(tt.payload), nil, "", "", tt.headers)
			if got := gjson.GetBytes(out, "enabled"); got.Exists() {
				t.Fatalf("enabled = %s，期望条件不匹配时不写入：%s", got.Raw, string(out))
			}
		})
	}
}

// payloadConditionTestConfig 返回覆盖所有新增匹配条件的 payload 规则。
func payloadConditionTestConfig() *config.Config {
	return &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{
				Name:         "gpt-*",
				Protocol:     "openai-response",
				FromProtocol: "claude",
				Headers:      map[string]string{"X-Client": "cli-*"},
				Match:        []map[string]any{{"mode": "json"}},
				NotMatch:     []map[string]any{{"temperature": float64(2)}},
				Exist:        []string{`messages.#(role=="user").content`},
				NotExist:     []string{"blocked"},
			}},
			Params: map[string]any{"enabled": true},
		}},
	}}
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
