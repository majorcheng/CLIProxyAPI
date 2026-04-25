package executor

import (
	"strings"

	"github.com/tidwall/gjson"
)

const imageGenerationToolType = "image_generation"

// RequestHasExplicitImageGenerationIntent 判断原始请求是否显式声明了图片生成意图。
// handlers 选 auth 与 Codex request-plan 注入图片工具都复用这一个判定口径，避免两边漂移。
func RequestHasExplicitImageGenerationIntent(body []byte) bool {
	if strings.TrimSpace(gjson.GetBytes(body, "tool_choice.type").String()) == imageGenerationToolType {
		return true
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == imageGenerationToolType {
			return true
		}
	}
	return false
}
