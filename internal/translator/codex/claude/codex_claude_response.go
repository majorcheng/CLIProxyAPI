// Package claude provides response translation functionality for Codex to Claude Code API compatibility.
// This package handles the conversion of Codex API responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// ConvertCodexResponseToClaudeParams holds parameters for response conversion.
type ConvertCodexResponseToClaudeParams struct {
	HasCompletedToolCall      bool
	BlockIndex                int
	HasReceivedArgumentsDelta bool
	HasTextDelta              bool
	TextBlockOpen             bool
	ToolBlockOpen             bool
	ThinkingBlockOpen         bool
	ThinkingStopPending       bool
	ThinkingSignature         string
	ThinkingSummarySeen       bool
}

func appendClaudeSSEEvent(output *strings.Builder, event, payload string) {
	output.WriteString("event: ")
	output.WriteString(event)
	output.WriteByte('\n')
	output.WriteString("data: ")
	output.WriteString(payload)
	output.WriteString("\n\n")
}

// ConvertCodexResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates Codex API responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - []string: A slice of strings, each containing a Claude Code-compatible JSON response
func ConvertCodexResponseToClaude(_ context.Context, _ string, originalRequestRawJSON, _ []byte, rawJSON []byte, param *any) []string {
	if *param == nil {
		*param = &ConvertCodexResponseToClaudeParams{
			BlockIndex: 0,
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return []string{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	params := (*param).(*ConvertCodexResponseToClaudeParams)
	rootResult := gjson.ParseBytes(rawJSON)
	var output strings.Builder

	if params.ThinkingBlockOpen && params.ThinkingStopPending {
		switch rootResult.Get("type").String() {
		case "response.content_part.added", "response.completed", "response.incomplete":
			finalizeCodexThinkingBlock(params, &output)
		}
	}

	typeStr := rootResult.Get("type").String()
	template := ""

	switch typeStr {
	case "response.created":
		template = `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"claude-opus-4-1-20250805","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}`
		template, _ = sjson.Set(template, "message.model", rootResult.Get("response.model").String())
		template, _ = sjson.Set(template, "message.id", rootResult.Get("response.id").String())
		appendClaudeSSEEvent(&output, "message_start", template)

	case "response.reasoning_summary_part.added":
		if params.ThinkingBlockOpen && params.ThinkingStopPending {
			finalizeCodexThinkingBlock(params, &output)
		}
		params.ThinkingSummarySeen = true
		startCodexThinkingBlock(params, &output)

	case "response.reasoning_summary_text.delta":
		template = `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
		template, _ = sjson.Set(template, "index", params.BlockIndex)
		template, _ = sjson.Set(template, "delta.thinking", rootResult.Get("delta").String())
		appendClaudeSSEEvent(&output, "content_block_delta", template)

	case "response.reasoning_summary_part.done":
		params.ThinkingStopPending = true

	case "response.content_part.added":
		template = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
		template, _ = sjson.Set(template, "index", params.BlockIndex)
		params.TextBlockOpen = true
		appendClaudeSSEEvent(&output, "content_block_start", template)

	case "response.output_text.delta":
		params.HasTextDelta = true
		template = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
		template, _ = sjson.Set(template, "index", params.BlockIndex)
		template, _ = sjson.Set(template, "delta.text", rootResult.Get("delta").String())
		appendClaudeSSEEvent(&output, "content_block_delta", template)

	case "response.content_part.done":
		template = `{"type":"content_block_stop","index":0}`
		template, _ = sjson.Set(template, "index", params.BlockIndex)
		params.TextBlockOpen = false
		params.BlockIndex++
		appendClaudeSSEEvent(&output, "content_block_stop", template)

	case "response.completed", "response.incomplete":
		closeOpenClaudeBlocksBeforeStop(params, &output)
		template = `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`
		responseData := rootResult.Get("response")
		hasCompletedToolCall := params.HasCompletedToolCall && typeStr == "response.completed"
		template, _ = sjson.Set(template, "delta.stop_reason", mapCodexStopReasonToClaude(codexStopReason(responseData), hasCompletedToolCall))
		template = setClaudeStopSequence(template, "delta.stop_sequence", responseData)
		inputTokens, outputTokens, cachedTokens := extractResponsesUsage(responseData.Get("usage"))
		template, _ = sjson.Set(template, "usage.input_tokens", inputTokens)
		template, _ = sjson.Set(template, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			template, _ = sjson.Set(template, "usage.cache_read_input_tokens", cachedTokens)
		}
		appendClaudeSSEEvent(&output, "message_delta", template)
		appendClaudeSSEEvent(&output, "message_stop", `{"type":"message_stop"}`)

	case "response.output_item.added":
		itemResult := rootResult.Get("item")
		switch itemResult.Get("type").String() {
		case "function_call":
			finalizeCodexThinkingBlock(params, &output)
			params.ToolBlockOpen = true
			params.HasReceivedArgumentsDelta = false
			template = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`
			template, _ = sjson.Set(template, "index", params.BlockIndex)
			template, _ = sjson.Set(template, "content_block.id", util.SanitizeClaudeToolID(itemResult.Get("call_id").String()))
			name := itemResult.Get("name").String()
			rev := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)
			if orig, ok := rev[name]; ok {
				name = orig
			}
			template, _ = sjson.Set(template, "content_block.name", name)
			appendClaudeSSEEvent(&output, "content_block_start", template)

			template = `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
			template, _ = sjson.Set(template, "index", params.BlockIndex)
			appendClaudeSSEEvent(&output, "content_block_delta", template)

		case "reasoning":
			params.ThinkingSignature = itemResult.Get("encrypted_content").String()
		}

	case "response.output_item.done":
		itemResult := rootResult.Get("item")
		switch itemResult.Get("type").String() {
		case "message":
			// 兜底：当 Codex 没有逐段下发 output_text.delta 时，
			// 仍要从最终 message 的 output_text 中补出 Claude 文本块。
			if params.HasTextDelta {
				break
			}
			contentResult := itemResult.Get("content")
			if !contentResult.Exists() || !contentResult.IsArray() {
				break
			}
			var textBuilder strings.Builder
			contentResult.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() != "output_text" {
					return true
				}
				if txt := part.Get("text").String(); txt != "" {
					textBuilder.WriteString(txt)
				}
				return true
			})
			text := textBuilder.String()
			if text == "" {
				break
			}

			finalizeCodexThinkingBlock(params, &output)
			if !params.TextBlockOpen {
				template = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
				template, _ = sjson.Set(template, "index", params.BlockIndex)
				params.TextBlockOpen = true
				appendClaudeSSEEvent(&output, "content_block_start", template)
			}

			template = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
			template, _ = sjson.Set(template, "index", params.BlockIndex)
			template, _ = sjson.Set(template, "delta.text", text)
			appendClaudeSSEEvent(&output, "content_block_delta", template)

			template = `{"type":"content_block_stop","index":0}`
			template, _ = sjson.Set(template, "index", params.BlockIndex)
			params.TextBlockOpen = false
			params.BlockIndex++
			params.HasTextDelta = true
			appendClaudeSSEEvent(&output, "content_block_stop", template)
		case "function_call":
			params.HasCompletedToolCall = true
			closeOpenToolBlock(params, &output)
		case "reasoning":
			if signature := itemResult.Get("encrypted_content").String(); signature != "" {
				params.ThinkingSignature = signature
			}
			if params.ThinkingSummarySeen {
				finalizeCodexThinkingBlock(params, &output)
			} else {
				finalizeCodexSignatureOnlyThinkingBlock(params, &output)
			}
		}

	case "response.function_call_arguments.delta":
		params.HasReceivedArgumentsDelta = true
		template = `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
		template, _ = sjson.Set(template, "index", params.BlockIndex)
		template, _ = sjson.Set(template, "delta.partial_json", rootResult.Get("delta").String())
		appendClaudeSSEEvent(&output, "content_block_delta", template)

	case "response.function_call_arguments.done":
		if !params.HasReceivedArgumentsDelta {
			if args := rootResult.Get("arguments").String(); args != "" {
				template = `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
				template, _ = sjson.Set(template, "index", params.BlockIndex)
				template, _ = sjson.Set(template, "delta.partial_json", args)
				appendClaudeSSEEvent(&output, "content_block_delta", template)
			}
		}
	}

	return []string{output.String()}
}

// ConvertCodexResponseToClaudeNonStream converts a non-streaming Codex response to a non-streaming Claude Code response.
// This function processes the complete Codex response and transforms it into a single Claude Code-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Claude Code API format.
func ConvertCodexResponseToClaudeNonStream(_ context.Context, _ string, originalRequestRawJSON, _ []byte, rawJSON []byte, _ *any) string {
	revNames := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)

	rootResult := gjson.ParseBytes(rawJSON)
	typeStr := rootResult.Get("type").String()
	if typeStr != "response.completed" && typeStr != "response.incomplete" {
		return ""
	}

	responseData := rootResult.Get("response")
	if !responseData.Exists() {
		return ""
	}

	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	out, _ = sjson.Set(out, "id", responseData.Get("id").String())
	out, _ = sjson.Set(out, "model", responseData.Get("model").String())
	inputTokens, outputTokens, cachedTokens := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.Set(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.Set(out, "usage.output_tokens", outputTokens)
	if cachedTokens > 0 {
		out, _ = sjson.Set(out, "usage.cache_read_input_tokens", cachedTokens)
	}

	hasCompletedToolCall := false

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "reasoning":
				thinkingBuilder := strings.Builder{}
				signature := item.Get("encrypted_content").String()
				if summary := item.Get("summary"); summary.Exists() {
					if summary.IsArray() {
						summary.ForEach(func(_, part gjson.Result) bool {
							if txt := part.Get("text"); txt.Exists() {
								thinkingBuilder.WriteString(txt.String())
							} else {
								thinkingBuilder.WriteString(part.String())
							}
							return true
						})
					} else {
						thinkingBuilder.WriteString(summary.String())
					}
				}
				if thinkingBuilder.Len() == 0 {
					if content := item.Get("content"); content.Exists() {
						if content.IsArray() {
							content.ForEach(func(_, part gjson.Result) bool {
								if txt := part.Get("text"); txt.Exists() {
									thinkingBuilder.WriteString(txt.String())
								} else {
									thinkingBuilder.WriteString(part.String())
								}
								return true
							})
						} else {
							thinkingBuilder.WriteString(content.String())
						}
					}
				}
				if thinkingBuilder.Len() > 0 || signature != "" {
					block := `{"type":"thinking","thinking":""}`
					block, _ = sjson.Set(block, "thinking", thinkingBuilder.String())
					if signature != "" {
						block, _ = sjson.Set(block, "signature", signature)
					}
					out, _ = sjson.SetRaw(out, "content.-1", block)
				}
			case "message":
				if content := item.Get("content"); content.Exists() {
					if content.IsArray() {
						content.ForEach(func(_, part gjson.Result) bool {
							if part.Get("type").String() == "output_text" {
								text := part.Get("text").String()
								if text != "" {
									block := `{"type":"text","text":""}`
									block, _ = sjson.Set(block, "text", text)
									out, _ = sjson.SetRaw(out, "content.-1", block)
								}
							}
							return true
						})
					} else {
						text := content.String()
						if text != "" {
							block := `{"type":"text","text":""}`
							block, _ = sjson.Set(block, "text", text)
							out, _ = sjson.SetRaw(out, "content.-1", block)
						}
					}
				}
			case "function_call":
				if !shouldEmitNonStreamToolUse(typeStr, item) {
					return true
				}
				hasCompletedToolCall = true
				name := item.Get("name").String()
				if original, ok := revNames[name]; ok {
					name = original
				}

				toolBlock := `{"type":"tool_use","id":"","name":"","input":{}}`
				toolBlock, _ = sjson.Set(toolBlock, "id", util.SanitizeClaudeToolID(item.Get("call_id").String()))
				toolBlock, _ = sjson.Set(toolBlock, "name", name)
				inputRaw := "{}"
				if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
					argsJSON := gjson.Parse(argsStr)
					if argsJSON.IsObject() {
						inputRaw = argsJSON.Raw
					}
				}
				toolBlock, _ = sjson.SetRaw(toolBlock, "input", inputRaw)
				out, _ = sjson.SetRaw(out, "content.-1", toolBlock)
			}
			return true
		})
	}

	out, _ = sjson.Set(out, "stop_reason", mapCodexStopReasonToClaude(codexStopReason(responseData), hasCompletedToolCall))
	out = setClaudeStopSequence(out, "stop_sequence", responseData)

	return out
}

// shouldEmitNonStreamToolUse 只允许完整响应里的合法工具参数转成 Claude tool_use。
func shouldEmitNonStreamToolUse(responseType string, item gjson.Result) bool {
	if responseType != "response.completed" {
		return false
	}
	args := item.Get("arguments").String()
	if args == "" || !gjson.Valid(args) {
		return false
	}
	return gjson.Parse(args).IsObject()
}

// codexStopReason 统一提取 Codex 完成原因，优先保留显式 stop_sequence 与 incomplete reason。
func codexStopReason(responseData gjson.Result) string {
	if stopReason := responseData.Get("stop_reason"); stopReason.Exists() && stopReason.String() != "" {
		if stopReason.String() == "stop" && codexStopSequence(responseData).String() != "" {
			return "stop_sequence"
		}
		return stopReason.String()
	}
	if reason := responseData.Get("incomplete_details.reason"); reason.Exists() && reason.String() != "" {
		return reason.String()
	}
	if codexStopSequence(responseData).String() != "" {
		return "stop_sequence"
	}
	return ""
}

// mapCodexStopReasonToClaude 将 Codex/OpenAI finish reason 映射为 Claude 兼容 stop_reason。
func mapCodexStopReasonToClaude(stopReason string, hasCompletedToolCall bool) string {
	if hasCompletedToolCall && canReportClaudeToolUse(stopReason) {
		return "tool_use"
	}

	switch stopReason {
	case "", "stop", "completed":
		return "end_turn"
	case "max_tokens", "max_output_tokens":
		return "max_tokens"
	case "tool_use", "tool_calls", "function_call":
		return "tool_use"
	case "end_turn", "stop_sequence", "pause_turn", "refusal", "model_context_window_exceeded":
		return stopReason
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// canReportClaudeToolUse 只在完成原因兼容工具完成语义时允许 tool_use 覆盖。
func canReportClaudeToolUse(stopReason string) bool {
	switch stopReason {
	case "", "stop", "completed", "stop_sequence", "tool_use", "tool_calls", "function_call":
		return true
	default:
		return false
	}
}

// codexStopSequence 封装 stop_sequence 读取，确保 stream 与 non-stream 走同一字段。
func codexStopSequence(responseData gjson.Result) gjson.Result {
	return responseData.Get("stop_sequence")
}

// setClaudeStopSequence 仅在 Codex 明确返回 stop_sequence 时写入 Claude stop_sequence。
func setClaudeStopSequence(out string, path string, responseData gjson.Result) string {
	if stopSequence := codexStopSequence(responseData); stopSequence.Exists() && stopSequence.String() != "" {
		out, _ = sjson.SetRaw(out, path, stopSequence.Raw)
	}
	return out
}

// closeOpenClaudeBlocksBeforeStop 在最终 message_delta 前补齐未关闭的 Claude content block。
func closeOpenClaudeBlocksBeforeStop(params *ConvertCodexResponseToClaudeParams, output *strings.Builder) {
	if params == nil {
		return
	}
	if params.ThinkingBlockOpen {
		finalizeCodexThinkingBlock(params, output)
	}
	closeOpenTextBlock(params, output)
	closeOpenToolBlock(params, output)
}

func closeOpenTextBlock(params *ConvertCodexResponseToClaudeParams, output *strings.Builder) {
	if params == nil || !params.TextBlockOpen {
		return
	}
	template := `{"type":"content_block_stop","index":0}`
	template, _ = sjson.Set(template, "index", params.BlockIndex)
	params.TextBlockOpen = false
	params.BlockIndex++
	appendClaudeSSEEvent(output, "content_block_stop", template)
}

func closeOpenToolBlock(params *ConvertCodexResponseToClaudeParams, output *strings.Builder) {
	if params == nil || !params.ToolBlockOpen {
		return
	}
	template := `{"type":"content_block_stop","index":0}`
	template, _ = sjson.Set(template, "index", params.BlockIndex)
	params.ToolBlockOpen = false
	params.BlockIndex++
	appendClaudeSSEEvent(output, "content_block_stop", template)
}

func extractResponsesUsage(usage gjson.Result) (int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0
	}

	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()

	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}

	return inputTokens, outputTokens, cachedTokens
}

// buildReverseMapFromClaudeOriginalShortToOriginal builds a map[short]original from original Claude request tools.
func buildReverseMapFromClaudeOriginalShortToOriginal(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
	}
	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		n := arr[i].Get("name").String()
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

func ClaudeTokenCount(ctx context.Context, count int64) string {
	return fmt.Sprintf(`{"input_tokens":%d}`, count)
}

func finalizeCodexThinkingBlock(params *ConvertCodexResponseToClaudeParams, output *strings.Builder) {
	if !params.ThinkingBlockOpen {
		params.ThinkingSignature = ""
		params.ThinkingSummarySeen = false
		return
	}

	if params.ThinkingSignature != "" {
		signatureDelta := `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`
		signatureDelta, _ = sjson.Set(signatureDelta, "index", params.BlockIndex)
		signatureDelta, _ = sjson.Set(signatureDelta, "delta.signature", params.ThinkingSignature)
		appendClaudeSSEEvent(output, "content_block_delta", signatureDelta)
	}

	contentBlockStop := `{"type":"content_block_stop","index":0}`
	contentBlockStop, _ = sjson.Set(contentBlockStop, "index", params.BlockIndex)
	appendClaudeSSEEvent(output, "content_block_stop", contentBlockStop)

	params.BlockIndex++
	params.ThinkingBlockOpen = false
	params.ThinkingStopPending = false
	params.ThinkingSignature = ""
	params.ThinkingSummarySeen = false
}

func startCodexThinkingBlock(params *ConvertCodexResponseToClaudeParams, output *strings.Builder) {
	if params.ThinkingBlockOpen {
		params.ThinkingStopPending = false
		return
	}
	template := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
	template, _ = sjson.Set(template, "index", params.BlockIndex)
	params.ThinkingBlockOpen = true
	params.ThinkingStopPending = false
	appendClaudeSSEEvent(output, "content_block_start", template)
}

func finalizeCodexSignatureOnlyThinkingBlock(params *ConvertCodexResponseToClaudeParams, output *strings.Builder) {
	// 某些上游序列只在 reasoning item 里给 encrypted_content，没有 summary 事件。
	// 这里要主动补一个空的 thinking block，才能把 signature 还给 Claude 客户端。
	if strings.TrimSpace(params.ThinkingSignature) == "" {
		params.ThinkingSummarySeen = false
		return
	}
	startCodexThinkingBlock(params, output)
	finalizeCodexThinkingBlock(params, output)
}
