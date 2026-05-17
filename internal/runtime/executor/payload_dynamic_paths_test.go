package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

// TestApplyPayloadConfigWithRoot_DynamicOverrideTargetsSingleTool 验证查询路径会落到真实数组下标再写入。
func TestApplyPayloadConfigWithRoot_DynamicOverrideTargetsSingleTool(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{payloadDynamicPathModel()},
			Params: map[string]any{
				`tools.#(type=="function"&&name=="target").function.strict`: true,
			},
		}},
	}}
	payload := []byte(`{"tools":[{"type":"function","name":"target","function":{"name":"target"}},{"type":"function","name":"other","function":{"name":"other"}}]}`)

	out := applyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai-response", "", payload, nil, "", "")

	if !gjson.GetBytes(out, "tools.0.function.strict").Bool() {
		t.Fatalf("target strict 未写入：%s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.1.function.strict"); got.Exists() {
		t.Fatalf("other strict = %s，期望不写入：%s", got.Raw, string(out))
	}
}

// TestApplyPayloadConfigWithRoot_DynamicFilterDeletesAllMatchesInReverseOrder 验证多匹配删除不会受数组下标漂移影响。
func TestApplyPayloadConfigWithRoot_DynamicFilterDeletesAllMatchesInReverseOrder(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Filter: []config.PayloadFilterRule{{
			Models: []config.PayloadModelRule{payloadDynamicPathModel()},
			Params: []string{`tools.#(type=="image_generation"||name=="blocked")#`},
		}},
	}}
	payload := []byte(`{"tools":[{"type":"image_generation","name":"img"},{"type":"function","name":"keep"},{"type":"function","name":"blocked"}]}`)

	out := applyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai-response", "", payload, nil, "", "")

	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 || tools[0].Get("name").String() != "keep" {
		t.Fatalf("tools = %s，期望只保留 keep：%s", gjson.GetBytes(out, "tools").Raw, string(out))
	}
}

// TestApplyPayloadConfigWithRoot_DynamicDefaultUsesResolvedPathFirstWrite 验证 default 以解析后的路径做首写判重。
func TestApplyPayloadConfigWithRoot_DynamicDefaultUsesResolvedPathFirstWrite(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Default: []config.PayloadRule{
			{
				Models: []config.PayloadModelRule{payloadDynamicPathModel()},
				Params: map[string]any{
					`tools.#(name=="target").function.description`: "first",
				},
			},
			{
				Models: []config.PayloadModelRule{payloadDynamicPathModel()},
				Params: map[string]any{
					`tools.#(name=="target").function.description`: "second",
				},
			},
		},
	}}
	payload := []byte(`{"tools":[{"type":"function","name":"target","function":{"name":"target"}}]}`)

	out := applyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai-response", "", payload, nil, "", "")

	if got := gjson.GetBytes(out, "tools.0.function.description").String(); got != "first" {
		t.Fatalf("description = %q，期望 first：%s", got, string(out))
	}
}

// TestApplyPayloadConfigWithRoot_DynamicOverrideHonorsRoot 验证 root 前缀下的动态路径仍能解析。
func TestApplyPayloadConfigWithRoot_DynamicOverrideHonorsRoot(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		OverrideRaw: []config.PayloadRule{{
			Models: []config.PayloadModelRule{payloadDynamicPathModel()},
			Params: map[string]any{
				`tools.#(type=="function").function.parameters`: `{"type":"object","properties":{"q":{"type":"string"}}}`,
			},
		}},
	}}
	payload := []byte(`{"request":{"tools":[{"type":"function","name":"search","function":{"name":"search"}}]}}`)

	out := applyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai-response", "request", payload, nil, "", "")

	if got := gjson.GetBytes(out, "request.tools.0.function.parameters.properties.q.type").String(); got != "string" {
		t.Fatalf("root dynamic parameters type = %q，期望 string：%s", got, string(out))
	}
}

// TestApplyPayloadConfigWithRoot_DynamicOverrideKeepsNestedQueryLogic 验证嵌套查询里的逻辑符不会被外层拆分。
func TestApplyPayloadConfigWithRoot_DynamicOverrideKeepsNestedQueryLogic(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{payloadDynamicPathModel()},
			Params: map[string]any{
				`tools.#(caps.#(type=="a"||type=="b"))#.enabled`: true,
			},
		}},
	}}
	payload := []byte(`{"tools":[{"name":"one","caps":[{"type":"a"}]},{"name":"two","caps":[{"type":"c"}]},{"name":"three","caps":[{"type":"b"}]}]}`)

	out := applyPayloadConfigWithRoot(cfg, "gpt-5.4", "openai-response", "", payload, nil, "", "")

	if !gjson.GetBytes(out, "tools.0.enabled").Bool() {
		t.Fatalf("tools.0.enabled 未写入：%s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.1.enabled"); got.Exists() {
		t.Fatalf("tools.1.enabled = %s，期望不写入：%s", got.Raw, string(out))
	}
	if !gjson.GetBytes(out, "tools.2.enabled").Bool() {
		t.Fatalf("tools.2.enabled 未写入：%s", string(out))
	}
}

// payloadDynamicPathModel 返回动态路径测试共用的模型匹配条件。
func payloadDynamicPathModel() config.PayloadModelRule {
	return config.PayloadModelRule{Name: "gpt-5.4", Protocol: "openai-response"}
}
