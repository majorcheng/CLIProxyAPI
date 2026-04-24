package executor

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var codexImageGenerationToolJSON = []byte(`{"type":"image_generation","output_format":"png"}`)
var codexImageGenerationToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)

// ensureCodexImageGenerationTool 只在请求已显式表达图片生成意图时补齐内建图片工具。
// 这样既保住图片入口的最小兼容兜底，也避免普通文本请求被静默扩大工具集合。
func ensureCodexImageGenerationTool(body []byte, baseModel string) []byte {
	if shouldSkipCodexImageGenerationTool(baseModel) || !codexRequestWantsImageGeneration(body) {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		updated, err := sjson.SetRawBytes(body, "tools", codexImageGenerationToolArrayJSON)
		if err != nil {
			return body
		}
		return updated
	}

	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
			return body
		}
	}

	updated, err := sjson.SetRawBytes(body, "tools.-1", codexImageGenerationToolJSON)
	if err != nil {
		return body
	}
	return updated
}

func shouldSkipCodexImageGenerationTool(baseModel string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(baseModel)), "spark")
}

func codexRequestWantsImageGeneration(body []byte) bool {
	if strings.TrimSpace(gjson.GetBytes(body, "tool_choice.type").String()) == "image_generation" {
		return true
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
			return true
		}
	}
	return false
}
