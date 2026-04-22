package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
)

// writeImageStreamPayload 把单个 Responses 事件转换成 OpenAI Images SSE 事件。
func writeImageStreamPayload(w io.Writer, flusher http.Flusher, payload []byte, responseFormat string, streamPrefix string) (bool, error) {
	var envelope struct {
		Type              string `json:"type"`
		PartialImageB64   string `json:"partial_image_b64"`
		PartialImageIndex int64  `json:"partial_image_index"`
		OutputFormat      string `json:"output_format"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return false, err
	}
	switch envelope.Type {
	case "response.image_generation_call.partial_image":
		return false, writePartialImageEvent(w, flusher, envelope, responseFormat, streamPrefix)
	case "response.completed":
		return true, writeCompletedImageEvents(w, flusher, payload, responseFormat, streamPrefix)
	default:
		return false, nil
	}
}

// writePartialImageEvent 把 partial_image 映射成单条 SSE 事件。
func writePartialImageEvent(w io.Writer, flusher http.Flusher, envelope struct {
	Type              string `json:"type"`
	PartialImageB64   string `json:"partial_image_b64"`
	PartialImageIndex int64  `json:"partial_image_index"`
	OutputFormat      string `json:"output_format"`
}, responseFormat string, streamPrefix string) error {
	if strings.TrimSpace(envelope.PartialImageB64) == "" {
		return nil
	}
	eventName := streamPrefix + ".partial_image"
	data := map[string]any{
		"type":                eventName,
		"partial_image_index": envelope.PartialImageIndex,
	}
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		data["url"] = buildImageDataURL(envelope.OutputFormat, envelope.PartialImageB64)
	} else {
		data["b64_json"] = envelope.PartialImageB64
	}
	return writeImagesStreamEvent(w, flusher, eventName, data)
}

// writeCompletedImageEvents 把最终成图列表转换成 completed SSE 事件。
func writeCompletedImageEvents(w io.Writer, flusher http.Flusher, payload []byte, responseFormat string, streamPrefix string) error {
	results, _, usageRaw, _, err := extractImagesFromResponsesCompleted(payload)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return fmt.Errorf("upstream did not return image output")
	}
	eventName := streamPrefix + ".completed"
	return writeCompletedImageEventItems(w, flusher, eventName, results, usageRaw, responseFormat)
}

// writeCompletedImageEventItems 为每张结果图分别发一条 completed 事件。
func writeCompletedImageEventItems(w io.Writer, flusher http.Flusher, eventName string, results []imageCallResult, usageRaw []byte, responseFormat string) error {
	usage := decodeImagesUsage(usageRaw)
	for _, result := range results {
		data := map[string]any{"type": eventName}
		if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
			data["url"] = buildImageDataURL(result.OutputFormat, result.Result)
		} else {
			data["b64_json"] = result.Result
		}
		if strings.TrimSpace(result.RevisedPrompt) != "" {
			data["revised_prompt"] = result.RevisedPrompt
		}
		if usage != nil {
			data["usage"] = usage
		}
		if err := writeImagesStreamEvent(w, flusher, eventName, data); err != nil {
			return err
		}
	}
	return nil
}

// decodeImagesUsage 把非空 usage JSON 解码成对象，便于挂到 SSE data。
func decodeImagesUsage(usageRaw []byte) any {
	if len(usageRaw) == 0 {
		return nil
	}
	var usage any
	if err := json.Unmarshal(usageRaw, &usage); err != nil {
		return nil
	}
	return usage
}

// writeImagesStreamEvent 统一输出带 event/data 的 SSE 事件并立刻 flush。
func writeImagesStreamEvent(w io.Writer, flusher http.Flusher, eventName string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", eventName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeImagesStreamError 复用现有 OpenAI 错误体格式，通过 SSE `error` 事件暴露终态错误。
func writeImagesStreamError(w io.Writer, flusher http.Flusher, errMsg *interfaces.ErrorMessage) {
	if errMsg == nil {
		return
	}
	status := http.StatusInternalServerError
	if errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	errText := http.StatusText(status)
	if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
		errText = errMsg.Error.Error()
	}
	_ = writeImagesStreamEvent(w, flusher, "error", json.RawMessage(handlers.BuildErrorResponseBody(status, errText)))
}
