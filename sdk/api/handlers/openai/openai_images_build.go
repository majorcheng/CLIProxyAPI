package openai

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
)

// buildImageToolPayload 把 OpenAI Images 参数规范化成 Codex image_generation tool 定义。
func buildImageToolPayload(payload imagesRequestPayload) map[string]any {
	tool := map[string]any{
		"type":   "image_generation",
		"action": payload.Action,
		"model":  payload.Model,
	}
	setStringField(tool, "size", payload.Size)
	setStringField(tool, "quality", payload.Quality)
	setStringField(tool, "background", payload.Background)
	setStringField(tool, "output_format", payload.OutputFormat)
	setStringField(tool, "input_fidelity", payload.InputFidelity)
	setStringField(tool, "moderation", payload.Moderation)
	setOptionalInt64Field(tool, "output_compression", payload.OutputCompression)
	setOptionalInt64Field(tool, "partial_images", payload.PartialImages)
	if payload.MaskImageURL != "" {
		tool["input_image_mask"] = map[string]any{"image_url": payload.MaskImageURL}
	}
	return tool
}

// buildImagesResponsesRequest 把 OpenAI Images 请求桥接为 OpenAI Responses 请求。
func buildImagesResponsesRequest(payload imagesRequestPayload) ([]byte, error) {
	requestBody := map[string]any{
		"instructions":        "",
		"stream":              true,
		"reasoning":           map[string]any{"effort": "medium", "summary": "auto"},
		"parallel_tool_calls": true,
		"include":             []string{"reasoning.encrypted_content"},
		"model":               defaultImagesMainModel,
		"store":               false,
		"tool_choice":         map[string]any{"type": "image_generation"},
		"tools":               []any{buildImageToolPayload(payload)},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": buildImagesInputContent(payload)},
		},
	}
	return json.Marshal(requestBody)
}

// buildImagesInputContent 统一拼装 prompt 与可选 input_image 数组。
func buildImagesInputContent(payload imagesRequestPayload) []map[string]any {
	content := []map[string]any{{"type": "input_text", "text": payload.Prompt}}
	for _, imageURL := range payload.Images {
		if imageURL == "" {
			continue
		}
		content = append(content, map[string]any{"type": "input_image", "image_url": imageURL})
	}
	return content
}

// setStringField 仅在值非空时写入参数，避免生成无意义空字段。
func setStringField(target map[string]any, key, value string) {
	if strings.TrimSpace(value) != "" {
		target[key] = strings.TrimSpace(value)
	}
}

// setOptionalInt64Field 仅在调用方显式提供时写入数值参数。
func setOptionalInt64Field(target map[string]any, key string, value *int64) {
	if value != nil {
		target[key] = *value
	}
}

// writeInvalidRequestError 统一输出 OpenAI 风格的 invalid_request_error。
func writeInvalidRequestError(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: message,
			Type:    "invalid_request_error",
		},
	})
}
