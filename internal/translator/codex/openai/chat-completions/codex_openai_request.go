// Package openai provides utilities to translate OpenAI Chat Completions
// request JSON into OpenAI Responses API request JSON.
// It supports tools, multimodal text/image inputs, and Structured Outputs.
// The package handles the conversion of OpenAI API requests into the format
// expected by the OpenAI Responses API, including proper mapping of messages,
// tools, and generation parameters.
package chat_completions

import (
	"encoding/json"
	"strconv"
	"strings"
)

const pseudoToolResultPrefix = "[Tool result for "

// ---------------------------------------------------------------------------
// Input structures (minimal – only fields actually used)
// ---------------------------------------------------------------------------

type chatReqInput struct {
	ReasoningEffort string          `json:"reasoning_effort"`
	Messages        []chatMessage   `json:"messages"`
	Tools           []chatTool      `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	ResponseFormat  *chatRespFormat `json:"response_format"`
	Text            *chatTextCfg    `json:"text"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatTool struct {
	Type     string        `json:"type"`
	Function *chatToolFunc `json:"function"`
	// everything else kept as raw so we can pass it through untouched
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON for chatTool: store the raw bytes too.
func (t *chatTool) UnmarshalJSON(data []byte) error {
	type alias chatTool
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*t = chatTool(a)
	t.Raw = data
	return nil
}

type chatToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

type chatToolCall struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRespFormat struct {
	Type       string          `json:"type"`
	JSONSchema *chatJSONSchema `json:"json_schema"`
}

type chatJSONSchema struct {
	Name   string          `json:"name"`
	Strict *bool           `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatTextCfg struct {
	Verbosity json.RawMessage `json:"verbosity"`
}

type chatContentPart struct {
	Type     string               `json:"type"`
	Text     string               `json:"text"`
	ImageURL *chatContentImageURL `json:"image_url"`
	File     *chatContentFile     `json:"file"`
}

type chatContentImageURL struct {
	URL string `json:"url"`
}

type chatContentFile struct {
	FileData string `json:"file_data"`
	Filename string `json:"filename"`
}

// ---------------------------------------------------------------------------
// ConvertOpenAIRequestToCodex – new fast implementation (Unmarshal/Marshal)
// ---------------------------------------------------------------------------

// ConvertOpenAIRequestToCodex converts an OpenAI Chat Completions request JSON
// into an OpenAI Responses API request JSON. The transformation follows the
// examples defined in docs/2.md exactly, including tools, multi-turn dialog,
// multimodal text/image handling, and Structured Outputs mapping.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI Chat Completions API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI Responses API format
func ConvertOpenAIRequestToCodex(modelName string, inputRawJSON []byte, stream bool) []byte {
	req, ok := cachedOpenAIRequest(inputRawJSON)
	if !ok {
		_ = json.Unmarshal(inputRawJSON, &req)
		PrimeOpenAIRequest(inputRawJSON)
	}
	req.Messages = normalizePseudoToolResultMessages(req.Messages)

	// Build tool-name shortening map from all function tools in the request.
	var funcNames []string
	for _, t := range req.Tools {
		if t.Type == "function" && t.Function != nil {
			funcNames = append(funcNames, t.Function.Name)
		}
	}
	originalToolNameMap := buildShortNameMap(funcNames)

	// ------------------------------------------------------------------
	// Build output map
	// ------------------------------------------------------------------
	out := map[string]any{
		"instructions":        "",
		"stream":              stream,
		"parallel_tool_calls": true,
		"include":             []string{"reasoning.encrypted_content"},
		"model":               modelName,
		"store":               false,
	}

	// reasoning
	effort := req.ReasoningEffort
	if effort == "" {
		effort = "medium"
	}
	out["reasoning"] = map[string]any{
		"effort":  effort,
		"summary": "auto",
	}

	// ------------------------------------------------------------------
	// Build input array
	// ------------------------------------------------------------------
	input := make([]any, 0, len(req.Messages))

	for _, m := range req.Messages {
		role := m.Role
		switch role {
		case "tool":
			// Decode content string
			contentStr := rawToString(m.Content)
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  contentStr,
			})

		default:
			displayRole := role
			if role == "system" {
				displayRole = "developer"
			}

			contentParts := buildContentParts(role, m.Content)

			msg := map[string]any{
				"type":    "message",
				"role":    displayRole,
				"content": contentParts,
			}
			// 只有 tool_calls 且没有文本内容的 assistant 空消息不能保留，
			// 否则会在 Responses 输入里插入多余 turn，破坏 call_id 对齐。
			if role != "assistant" || len(contentParts) > 0 {
				input = append(input, msg)
			}

			// Append function_call objects for assistant tool calls
			if role == "assistant" {
				for _, tc := range m.ToolCalls {
					if tc.Type == "function" {
						name := tc.Function.Name
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						input = append(input, map[string]any{
							"type":      "function_call",
							"call_id":   tc.ID,
							"name":      name,
							"arguments": tc.Function.Arguments,
						})
					}
				}
			}
		}
	}
	out["input"] = input

	// ------------------------------------------------------------------
	// response_format / text
	// ------------------------------------------------------------------
	textObj := buildTextObject(req.ResponseFormat, req.Text)
	if textObj != nil {
		out["text"] = textObj
	}

	// ------------------------------------------------------------------
	// tools
	// ------------------------------------------------------------------
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				// Built-in tool – pass through raw
				var v any
				_ = json.Unmarshal(t.Raw, &v)
				tools = append(tools, v)
				continue
			}
			if t.Type == "function" && t.Function != nil {
				item := map[string]any{
					"type": "function",
				}
				name := t.Function.Name
				if short, ok := originalToolNameMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				item["name"] = name
				if t.Function.Description != "" {
					item["description"] = t.Function.Description
				}
				if len(t.Function.Parameters) > 0 {
					var params any
					_ = json.Unmarshal(t.Function.Parameters, &params)
					item["parameters"] = params
				}
				if t.Function.Strict != nil {
					item["strict"] = *t.Function.Strict
				}
				tools = append(tools, item)
			}
		}
		out["tools"] = tools
	}

	// ------------------------------------------------------------------
	// tool_choice
	// ------------------------------------------------------------------
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		// Determine if it's a JSON string or object
		var strVal string
		if err := json.Unmarshal(req.ToolChoice, &strVal); err == nil {
			out["tool_choice"] = strVal
		} else {
			var objVal map[string]any
			if err2 := json.Unmarshal(req.ToolChoice, &objVal); err2 == nil {
				tcType, _ := objVal["type"].(string)
				if tcType == "function" {
					name := ""
					if fn, ok := objVal["function"].(map[string]any); ok {
						name, _ = fn["name"].(string)
					}
					if name != "" {
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
					}
					choice := map[string]any{"type": "function"}
					if name != "" {
						choice["name"] = name
					}
					out["tool_choice"] = choice
				} else if tcType != "" {
					out["tool_choice"] = objVal
				}
			}
		}
	}

	b, _ := json.Marshal(out)
	return b
}

// ---------------------------------------------------------------------------
// ConvertOpenAIRequestToCodexLegacy – original gjson/sjson implementation
// kept for equivalence testing.
// ---------------------------------------------------------------------------

// ConvertOpenAIRequestToCodexLegacy is the original implementation using
// gjson/sjson for equivalence testing against the new implementation.
func ConvertOpenAIRequestToCodexLegacy(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertOpenAIRequestToCodexLegacyImpl(modelName, inputRawJSON, stream)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// normalizePseudoToolResultMessages 会把 OpenClaw 风格的伪装 tool result
// user 消息，在“当前仍处于待提交 tool output 的窗口”内收口成标准
// tool 消息，复用现有 function_call_output 翻译链。
func normalizePseudoToolResultMessages(messages []chatMessage) []chatMessage {
	if len(messages) == 0 {
		return nil
	}

	normalized := append([]chatMessage(nil), messages...)
	pendingCallIDs := make(map[string]struct{}, len(messages))
	for i, message := range normalized {
		if callID, output, ok := parsePseudoToolResultMessage(message, pendingCallIDs); ok {
			normalized[i].Role = "tool"
			normalized[i].ToolCallID = callID
			normalized[i].Content = marshalStringRawMessage(output)
			delete(pendingCallIDs, callID)
			continue
		}

		switch message.Role {
		case "assistant":
			clearPendingToolCallIDs(pendingCallIDs)
			for _, toolCall := range message.ToolCalls {
				if toolCall.ID != "" {
					pendingCallIDs[toolCall.ID] = struct{}{}
				}
			}
		case "tool":
			if message.ToolCallID != "" {
				delete(pendingCallIDs, message.ToolCallID)
			}
		case "user":
			clearPendingToolCallIDs(pendingCallIDs)
		}
	}
	return normalized
}

// parsePseudoToolResultMessage 只识别纯字符串 user 内容，格式固定为
// [Tool result for <call_id>]: <output>，且 call_id 必须仍处于最近一轮
// assistant tool_calls 打开的 pending 窗口中，避免把后续引用日志的
// 普通用户消息误判成 tool output。
func parsePseudoToolResultMessage(message chatMessage, pendingCallIDs map[string]struct{}) (string, string, bool) {
	if message.Role != "user" || firstNonSpaceByte(message.Content) != '"' {
		return "", "", false
	}

	contentStr, ok := unmarshalStringMessage(message.Content)
	if !ok || !strings.HasPrefix(contentStr, pseudoToolResultPrefix) {
		return "", "", false
	}

	callIDEnd := strings.Index(contentStr[len(pseudoToolResultPrefix):], "]:")
	if callIDEnd < 0 {
		return "", "", false
	}

	callIDStart := len(pseudoToolResultPrefix)
	callIDEnd += callIDStart
	callID := contentStr[callIDStart:callIDEnd]
	if callID == "" {
		return "", "", false
	}
	if _, ok := pendingCallIDs[callID]; !ok {
		return "", "", false
	}

	output := contentStr[callIDEnd+2:]
	if strings.HasPrefix(output, " ") {
		output = output[1:]
	}
	return callID, output, true
}

func clearPendingToolCallIDs(pendingCallIDs map[string]struct{}) {
	for callID := range pendingCallIDs {
		delete(pendingCallIDs, callID)
	}
}

func unmarshalStringMessage(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func marshalStringRawMessage(value string) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Keep behavior aligned with legacy gjson String() for null.
	if string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func buildContentParts(role string, raw json.RawMessage) []any {
	parts := make([]any, 0)
	if len(raw) == 0 {
		return parts
	}

	first := firstNonSpaceByte(raw)
	switch first {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return parts
		}
		if s == "" {
			return parts
		}
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		parts = append(parts, map[string]any{
			"type": partType,
			"text": s,
		})
		return parts
	case '[':
	default:
		return parts
	}

	var arr []chatContentPart
	if err := json.Unmarshal(raw, &arr); err != nil {
		return parts
	}
	for _, item := range arr {
		switch item.Type {
		case "text":
			partType := "input_text"
			if role == "assistant" {
				partType = "output_text"
			}
			parts = append(parts, map[string]any{
				"type": partType,
				"text": item.Text,
			})
		case "image_url":
			if role == "user" && item.ImageURL != nil && item.ImageURL.URL != "" {
				part := map[string]any{"type": "input_image"}
				part["image_url"] = item.ImageURL.URL
				parts = append(parts, part)
			}
		case "file":
			if role == "user" && item.File != nil {
				if item.File.FileData == "" {
					continue
				}
				part := map[string]any{
					"type":      "input_file",
					"file_data": item.File.FileData,
				}
				if item.File.Filename != "" {
					part["filename"] = item.File.Filename
				}
				parts = append(parts, part)
			}
		}
	}
	return parts
}

func firstNonSpaceByte(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			return b
		}
	}
	return 0
}

func buildTextObject(rf *chatRespFormat, tc *chatTextCfg) map[string]any {
	if rf == nil && tc == nil {
		return nil
	}

	textObj := map[string]any{}

	if rf != nil {
		format := map[string]any{}
		switch rf.Type {
		case "text":
			format["type"] = "text"
		case "json_schema":
			format["type"] = "json_schema"
			if rf.JSONSchema != nil {
				if rf.JSONSchema.Name != "" {
					format["name"] = rf.JSONSchema.Name
				}
				if rf.JSONSchema.Strict != nil {
					format["strict"] = *rf.JSONSchema.Strict
				}
				if len(rf.JSONSchema.Schema) > 0 {
					var schema any
					_ = json.Unmarshal(rf.JSONSchema.Schema, &schema)
					format["schema"] = schema
				}
			}
		}
		if len(format) > 0 {
			textObj["format"] = format
		}
	}

	if tc != nil && len(tc.Verbosity) > 0 && string(tc.Verbosity) != "null" {
		var v any
		_ = json.Unmarshal(tc.Verbosity, &v)
		textObj["verbosity"] = v
	}

	if len(textObj) == 0 {
		return nil
	}
	return textObj
}

// shortenNameIfNeeded applies the simple shortening rule for a single name.
// If the name length exceeds 64, it will try to preserve the "mcp__" prefix and last segment.
// Otherwise it truncates to 64 characters.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		// Keep prefix and last segment after '__'
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return name[:limit]
}

// buildShortNameMap generates unique short names (<=64) for the given list of names.
// It preserves the "mcp__" prefix with the last segment when possible and ensures uniqueness
// by appending suffixes like "_1", "_2" if needed.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}
