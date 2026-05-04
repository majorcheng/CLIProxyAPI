package responses

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
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
