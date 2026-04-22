package openai

import (
	"encoding/json"
	"strings"
)

// buildImagesAPIResponse 组装 OpenAI Images 非流式响应，并保留 usage/meta 信息。
func buildImagesAPIResponse(results []imageCallResult, createdAt int64, usageRaw []byte, firstMeta imageCallResult, responseFormat string) ([]byte, error) {
	responseBody := map[string]any{
		"created": createdAt,
		"data":    buildImagesResponseItems(results, responseFormat),
	}
	applyImagesResponseMeta(responseBody, usageRaw, firstMeta)
	return json.Marshal(responseBody)
}

// buildImagesResponseItems 根据 response_format 输出 `b64_json` 或 data URL。
func buildImagesResponseItems(results []imageCallResult, responseFormat string) []map[string]any {
	items := make([]map[string]any, 0, len(results))
	for _, result := range results {
		item := map[string]any{}
		if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
			item["url"] = buildImageDataURL(result.OutputFormat, result.Result)
		} else {
			item["b64_json"] = result.Result
		}
		if result.RevisedPrompt != "" {
			item["revised_prompt"] = result.RevisedPrompt
		}
		items = append(items, item)
	}
	return items
}

// applyImagesResponseMeta 把 usage 与首张图的元信息挂回最终响应。
func applyImagesResponseMeta(responseBody map[string]any, usageRaw []byte, firstMeta imageCallResult) {
	if len(usageRaw) > 0 {
		var usage any
		if err := json.Unmarshal(usageRaw, &usage); err == nil {
			responseBody["usage"] = usage
		}
	}
	setStringField(responseBody, "background", firstMeta.Background)
	setStringField(responseBody, "output_format", firstMeta.OutputFormat)
	setStringField(responseBody, "quality", firstMeta.Quality)
	setStringField(responseBody, "size", firstMeta.Size)
}

// buildImageDataURL 把 base64 成图包装成 OpenAI Images `url` 模式所需的 data URL。
func buildImageDataURL(outputFormat, b64 string) string {
	return "data:" + mimeTypeFromOutputFormat(outputFormat) + ";base64," + b64
}

// mimeTypeFromOutputFormat 兼容 `png` / `jpeg` / `image/png` 等常见格式写法。
func mimeTypeFromOutputFormat(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "", "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		if strings.Contains(outputFormat, "/") {
			return outputFormat
		}
		return "image/png"
	}
}
