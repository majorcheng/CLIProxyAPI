package executor

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stripVertexOpenAIResponsesToolCallIDs 删除 OpenAI Responses 转 Vertex 后上游不接受的 tool call id。
func stripVertexOpenAIResponsesToolCallIDs(payload []byte, sourceFormat string) []byte {
	if !strings.EqualFold(strings.TrimSpace(sourceFormat), "openai-response") {
		return payload
	}
	contents := gjson.GetBytes(payload, "contents")
	if !contents.IsArray() {
		return payload
	}

	out := payload
	for contentIndex, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for partIndex, part := range parts.Array() {
			if part.Get("functionCall.id").Exists() {
				path := fmt.Sprintf("contents.%d.parts.%d.functionCall.id", contentIndex, partIndex)
				if updated, errDelete := sjson.DeleteBytes(out, path); errDelete == nil {
					out = updated
				}
			}
			if part.Get("functionResponse.id").Exists() {
				path := fmt.Sprintf("contents.%d.parts.%d.functionResponse.id", contentIndex, partIndex)
				if updated, errDelete := sjson.DeleteBytes(out, path); errDelete == nil {
					out = updated
				}
			}
		}
	}
	return out
}
