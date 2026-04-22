package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
)

type imageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

type sseFrameAccumulator struct {
	pending []byte
}

// AddChunk 按 SSE 帧边界拆分数据块，兼容上游把事件拆到多次 write 的情况。
func (a *sseFrameAccumulator) AddChunk(chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return nil
	}
	if responsesSSENeedsLineBreak(a.pending, chunk) {
		a.pending = append(a.pending, '\n')
	}
	a.pending = append(a.pending, chunk...)
	return a.drain(false)
}

// Flush 在上游结束时释放剩余可判定的帧，避免最后一帧丢失。
func (a *sseFrameAccumulator) Flush() [][]byte {
	return a.drain(true)
}

// drain 统一完成累积缓冲的拆帧逻辑，避免 AddChunk/Flush 双份实现。
func (a *sseFrameAccumulator) drain(flush bool) [][]byte {
	var frames [][]byte
	for {
		frameLen := responsesSSEFrameLen(a.pending)
		if frameLen == 0 {
			break
		}
		frames = append(frames, a.pending[:frameLen])
		copy(a.pending, a.pending[frameLen:])
		a.pending = a.pending[:len(a.pending)-frameLen]
	}
	if len(bytes.TrimSpace(a.pending)) == 0 {
		a.pending = a.pending[:0]
		return frames
	}
	if !flush || !responsesSSECanEmitWithoutDelimiter(a.pending) {
		return frames
	}
	frames = append(frames, a.pending)
	a.pending = nil
	return frames
}

// collectImagesFromResponsesStream 从 Responses SSE 中提取最终成图并转成 OpenAI Images 响应。
func collectImagesFromResponsesStream(ctx context.Context, data <-chan []byte, errs <-chan *interfaces.ErrorMessage, responseFormat string) ([]byte, *interfaces.ErrorMessage) {
	accumulator := &sseFrameAccumulator{}
	for {
		select {
		case <-ctx.Done():
			return nil, &interfaces.ErrorMessage{StatusCode: 408, Error: ctx.Err()}
		case errMsg, ok := <-errs:
			if ok && errMsg != nil {
				return nil, errMsg
			}
			errs = nil
		case chunk, ok := <-data:
			if !ok {
				return collectImagesFromFrames(accumulator.Flush(), responseFormat, true)
			}
			if out, errMsg := collectImagesFromFrames(accumulator.AddChunk(chunk), responseFormat, false); out != nil || errMsg != nil {
				return out, errMsg
			}
		}
	}
}

// collectImagesFromFrames 扫描已完整到齐的 SSE 帧，命中 response.completed 后立即收口。
func collectImagesFromFrames(frames [][]byte, responseFormat string, streamClosed bool) ([]byte, *interfaces.ErrorMessage) {
	for _, frame := range frames {
		out, done, errMsg := processCompletedImagesFrame(frame, responseFormat)
		if errMsg != nil {
			return nil, errMsg
		}
		if done {
			return out, nil
		}
	}
	if streamClosed {
		return nil, &interfaces.ErrorMessage{StatusCode: 502, Error: fmt.Errorf("stream disconnected before completion")}
	}
	return nil, nil
}

// processCompletedImagesFrame 只关心包含 response.completed 的 SSE 帧，其余事件直接跳过。
func processCompletedImagesFrame(frame []byte, responseFormat string) ([]byte, bool, *interfaces.ErrorMessage) {
	for _, payload := range extractDataPayloads(frame) {
		if !json.Valid(payload) {
			return nil, false, &interfaces.ErrorMessage{StatusCode: 502, Error: fmt.Errorf("invalid SSE data JSON")}
		}
		if !bytes.Equal(jsonType(payload), []byte("response.completed")) {
			continue
		}
		results, createdAt, usageRaw, firstMeta, err := extractImagesFromResponsesCompleted(payload)
		if err != nil {
			return nil, false, &interfaces.ErrorMessage{StatusCode: 502, Error: err}
		}
		if len(results) == 0 {
			return nil, false, &interfaces.ErrorMessage{StatusCode: 502, Error: fmt.Errorf("upstream did not return image output")}
		}
		out, err := buildImagesAPIResponse(results, createdAt, usageRaw, firstMeta, responseFormat)
		if err != nil {
			return nil, false, &interfaces.ErrorMessage{StatusCode: 500, Error: err}
		}
		return out, true, nil
	}
	return nil, false, nil
}

// extractDataPayloads 从一个 SSE frame 中提取所有 `data:` JSON 负载。
func extractDataPayloads(frame []byte) [][]byte {
	var payloads [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// jsonType 读取轻量事件类型，避免为每帧反复完整反序列化。
func jsonType(payload []byte) []byte {
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &envelope)
	return []byte(envelope.Type)
}

// extractImagesFromResponsesCompleted 提取 response.completed 里的最终图片与 tool_usage。
func extractImagesFromResponsesCompleted(payload []byte) ([]imageCallResult, int64, []byte, imageCallResult, error) {
	var event struct {
		Type     string `json:"type"`
		Response struct {
			CreatedAt int64             `json:"created_at"`
			Output    []json.RawMessage `json:"output"`
			ToolUsage struct {
				ImageGen json.RawMessage `json:"image_gen"`
			} `json:"tool_usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, 0, nil, imageCallResult{}, err
	}
	if event.Type != "response.completed" {
		return nil, 0, nil, imageCallResult{}, fmt.Errorf("unexpected event type")
	}
	results, firstMeta := collectImageOutputResults(event.Response.Output)
	createdAt := event.Response.CreatedAt
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	usageRaw := normalizeUsageRaw(event.Response.ToolUsage.ImageGen)
	return results, createdAt, usageRaw, firstMeta, nil
}

// collectImageOutputResults 只保留 image_generation_call，忽略普通 message 输出。
func collectImageOutputResults(output []json.RawMessage) ([]imageCallResult, imageCallResult) {
	results := make([]imageCallResult, 0, len(output))
	var firstMeta imageCallResult
	for _, item := range output {
		result, ok := decodeImageOutputItem(item)
		if !ok {
			continue
		}
		if len(results) == 0 {
			firstMeta = result
		}
		results = append(results, result)
	}
	return results, firstMeta
}

// decodeImageOutputItem 把单个 output item 转成内部 imageCallResult。
func decodeImageOutputItem(item json.RawMessage) (imageCallResult, bool) {
	var entry struct {
		Type          string `json:"type"`
		Result        string `json:"result"`
		RevisedPrompt string `json:"revised_prompt"`
		OutputFormat  string `json:"output_format"`
		Size          string `json:"size"`
		Background    string `json:"background"`
		Quality       string `json:"quality"`
	}
	if err := json.Unmarshal(item, &entry); err != nil {
		return imageCallResult{}, false
	}
	if entry.Type != "image_generation_call" || strings.TrimSpace(entry.Result) == "" {
		return imageCallResult{}, false
	}
	return imageCallResult{
		Result:        strings.TrimSpace(entry.Result),
		RevisedPrompt: strings.TrimSpace(entry.RevisedPrompt),
		OutputFormat:  strings.TrimSpace(entry.OutputFormat),
		Size:          strings.TrimSpace(entry.Size),
		Background:    strings.TrimSpace(entry.Background),
		Quality:       strings.TrimSpace(entry.Quality),
	}, true
}

// normalizeUsageRaw 仅在 JSON 对象有效时保留 tool_usage.image_gen，避免写入空壳。
func normalizeUsageRaw(raw json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !json.Valid(trimmed) || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return append([]byte(nil), trimmed...)
	}
	return nil
}
