package responses

import (
	"encoding/base64"
	"strings"
	"testing"

	sigcompat "github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConvertOpenAIResponsesRequestToClaude_MapsNonStringFunctionCallOutputContent(t *testing.T) {
	input := []byte(`{
		"model":"claude-3-5-sonnet",
		"input":[
			{"type":"function_call_output","call_id":"call_1","output":[
				{"type":"text","text":"处理完成"},
				{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}
			]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-3-5-sonnet", input, false)

	toolResult := gjson.GetBytes(out, "messages.0.content.0")
	if got := toolResult.Get("type").String(); got != "tool_result" {
		t.Fatalf("tool_result.type = %q, want %q, out=%s", got, "tool_result", string(out))
	}
	content := toolResult.Get("content")
	if !content.IsArray() {
		t.Fatalf("tool_result.content should be array, got %s", content.Raw)
	}
	if got := content.Get("0.type").String(); got != "text" {
		t.Fatalf("content.0.type = %q, want %q", got, "text")
	}
	if got := content.Get("0.text").String(); got != "处理完成" {
		t.Fatalf("content.0.text = %q, want %q", got, "处理完成")
	}
	if got := content.Get("1.type").String(); got != "image" {
		t.Fatalf("content.1.type = %q, want %q", got, "image")
	}
	if got := content.Get("1.source.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("content.1.source.url = %q, want %q", got, "https://example.com/image.png")
	}
}

func TestConvertOpenAIResponsesRequestToClaude_PreservesRawToolResultArrayWhenPartIsUnsupported(t *testing.T) {
	input := []byte(`{
		"model":"claude-3-5-sonnet",
		"input":[
			{"type":"function_call_output","call_id":"call_1","output":[
				{"type":"text","text":"处理完成"},
				{"foo":"bar"}
			]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-3-5-sonnet", input, false)

	content := gjson.GetBytes(out, "messages.0.content.0.content")
	if !content.IsArray() {
		t.Fatalf("tool_result.content should stay array, got %s", content.Raw)
	}
	if got := content.Get("1.foo").String(); got != "bar" {
		t.Fatalf("content.1.foo = %q, want %q, out=%s", got, "bar", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_StripsFileDataURLPrefix(t *testing.T) {
	input := []byte(`{
		"model":"claude-3-5-sonnet",
		"input":[
			{"type":"function_call_output","call_id":"call_1","output":[
				{"type":"file","file":{"file_data":"data:text/plain;base64,SEVMTE8="}}
			]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-3-5-sonnet", input, false)

	content := gjson.GetBytes(out, "messages.0.content.0.content")
	if got := content.Get("0.type").String(); got != "document" {
		t.Fatalf("content.0.type = %q, want %q, out=%s", got, "document", string(out))
	}
	if got := content.Get("0.source.data").String(); got != "SEVMTE8=" {
		t.Fatalf("content.0.source.data = %q, want %q", got, "SEVMTE8=")
	}
	if got := content.Get("0.source.media_type").String(); got != "text/plain" {
		t.Fatalf("content.0.source.media_type = %q, want %q", got, "text/plain")
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ConvertsNamespaceAndWebSearchTools(t *testing.T) {
	input := []byte(`{
		"model":"claude-3-5-sonnet",
		"tools":[
			{
				"type":"namespace",
				"name":"mcp__fs",
				"tools":[
					{
						"type":"function",
						"name":"read_file",
						"description":"读取文件",
						"parameters":{"type":"object","properties":{"path":{"type":"string"}}}
					}
				]
			},
			{
				"type":"web_search",
				"name":"web_search",
				"max_uses":3,
				"filters":{"allowed_domains":["example.com"]}
			},
			{
				"type":"image_generation",
				"name":"ignored_image_generation"
			}
		],
		"tool_choice":{"type":"function","function":{"name":"read_file"}}
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-3-5-sonnet", input, true)

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "mcp__fs__read_file" {
		t.Fatalf("tools.0.name = %q, want %q, out=%s", got, "mcp__fs__read_file", string(out))
	}
	if got := gjson.GetBytes(out, "tools.1.type").String(); got != "web_search_20250305" {
		t.Fatalf("tools.1.type = %q, want %q", got, "web_search_20250305")
	}
	if got := gjson.GetBytes(out, "tools.1.allowed_domains.0").String(); got != "example.com" {
		t.Fatalf("tools.1.allowed_domains.0 = %q, want %q", got, "example.com")
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "mcp__fs__read_file" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "mcp__fs__read_file")
	}

	raw := string(out)
	if strings.Contains(raw, "ignored_image_generation") {
		t.Fatalf("unsupported builtin tool should be filtered, out=%s", raw)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_ReasoningItemToThinkingBlock(t *testing.T) {
	rawSignature, expectedSignature := testClaudeResponsesThinkingSignature(t)
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + rawSignature + `",
				"summary":[{"type":"summary_text","text":"internal reasoning"}]
			},
			{
				"type":"message",
				"role":"assistant",
				"content":[{"type":"output_text","text":"visible answer"}]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	assistant := root.Get("messages.0")
	if got := assistant.Get("role").String(); got != "assistant" {
		t.Fatalf("first message role = %q, want assistant. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.0.signature").String(); got != expectedSignature {
		t.Fatalf("thinking signature = %q, want %q", got, expectedSignature)
	}
	if got := assistant.Get("content.0.thinking").String(); got != "internal reasoning" {
		t.Fatalf("thinking text = %q, want internal reasoning", got)
	}
	if got := assistant.Get("content.1.type").String(); got != "text" {
		t.Fatalf("second content type = %q, want text. Output: %s", got, string(out))
	}
	if got := assistant.Get("content.1.text").String(); got != "visible answer" {
		t.Fatalf("assistant text = %q, want visible answer", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SignatureOnlyReasoningFlushesBeforeUser(t *testing.T) {
	rawSignature, expectedSignature := testClaudeResponsesThinkingSignature(t)
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + rawSignature + `",
				"summary":[]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	root := gjson.ParseBytes(out)

	thinking := root.Get("messages.0.content.0")
	if got := thinking.Get("type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Output: %s", got, string(out))
	}
	if got := thinking.Get("signature").String(); got != expectedSignature {
		t.Fatalf("thinking signature = %q, want %q", got, expectedSignature)
	}
	if got := thinking.Get("thinking").String(); got != "" {
		t.Fatalf("thinking text = %q, want empty", got)
	}
	if got := root.Get("messages.1.role").String(); got != "user" {
		t.Fatalf("second message role = %q, want user. Output: %s", got, string(out))
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DropsIncompatibleReasoningSignature(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"input":[
			{
				"type":"reasoning",
				"encrypted_content":"` + testGPTResponsesReasoningSignature() + `",
				"summary":[{"type":"summary_text","text":"must not become Claude thinking"}]
			},
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"continue"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-test", raw, false)
	if gjson.GetBytes(out, "messages.0.content.0.type").String() == "thinking" {
		t.Fatalf("GPT encrypted_content should not become Claude thinking. Output: %s", string(out))
	}
	if gjson.GetBytes(out, "messages.0.content.0.signature").Exists() {
		t.Fatalf("incompatible signature should not be forwarded. Output: %s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Fatalf("first message role = %q, want user. Output: %s", got, string(out))
	}
}

func testClaudeResponsesThinkingSignature(t *testing.T) (string, string) {
	t.Helper()
	channelBlock := []byte{}
	channelBlock = protowire.AppendTag(channelBlock, 1, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, 12)
	channelBlock = protowire.AppendTag(channelBlock, 2, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, 2)
	channelBlock = protowire.AppendTag(channelBlock, 6, protowire.BytesType)
	channelBlock = protowire.AppendString(channelBlock, "claude-sonnet-4-6")

	container := []byte{}
	container = protowire.AppendTag(container, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channelBlock)

	payload := []byte{}
	payload = protowire.AppendTag(payload, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	payload = protowire.AppendTag(payload, 3, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 1)

	rawSignature := base64.StdEncoding.EncodeToString(payload)
	normalized, ok := sigcompat.CompatibleSignatureForProvider(sigcompat.SignatureProviderClaude, rawSignature)
	if !ok {
		t.Fatal("test Claude signature should be compatible")
	}
	return rawSignature, normalized
}

func testGPTResponsesReasoningSignature() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	payload[8] = 1
	for i := 9; i < len(payload); i++ {
		payload[i] = byte(i)
	}
	return base64.URLEncoding.EncodeToString(payload)
}
