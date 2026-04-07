package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func parseOpenAIResponsesSSEEvent(t *testing.T, chunk string) (string, gjson.Result) {
	t.Helper()

	lines := strings.Split(chunk, "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected SSE chunk: %q", chunk)
	}

	event := strings.TrimSpace(strings.TrimPrefix(lines[0], "event:"))
	dataLine := strings.TrimSpace(strings.TrimPrefix(lines[1], "data:"))
	if !gjson.Valid(dataLine) {
		t.Fatalf("invalid SSE data JSON: %q", dataLine)
	}
	return event, gjson.Parse(dataLine)
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_ResponseCompletedWaitsForDone(t *testing.T) {
	t.Parallel()

	request := []byte(`{"model":"gpt-5.4","tool_choice":"auto","parallel_tool_calls":true}`)

	tests := []struct {
		name           string
		in             []string
		doneInputIndex int
		hasUsage       bool
		inputTokens    int64
		outputTokens   int64
		totalTokens    int64
	}{
		{
			name: "late usage after finish reason",
			in: []string{
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_late_usage","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: {"id":"resp_late_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
				`data: [DONE]`,
			},
			doneInputIndex: 3,
			hasUsage:       true,
			inputTokens:    11,
			outputTokens:   7,
			totalTokens:    18,
		},
		{
			name: "usage on finish reason chunk",
			in: []string{
				`data: {"id":"resp_usage_same_chunk","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_usage_same_chunk","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_usage_same_chunk","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":13,"completion_tokens":5,"total_tokens":18}}`,
				`data: [DONE]`,
			},
			doneInputIndex: 2,
			hasUsage:       true,
			inputTokens:    13,
			outputTokens:   5,
			totalTokens:    18,
		},
		{
			name: "no usage chunk",
			in: []string{
				`data: {"id":"resp_no_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":[{"index":0,"id":"call_no_usage","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_no_usage","object":"chat.completion.chunk","created":1773896263,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"filePath\":\"C:\\\\repo\\\\README.md\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			},
			doneInputIndex: 2,
			hasUsage:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completedCount := 0
			completedInputIndex := -1
			var completedData gjson.Result
			var param any

			for i, line := range tt.in {
				for _, chunk := range ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "model", request, request, []byte(line), &param) {
					event, data := parseOpenAIResponsesSSEEvent(t, chunk)
					if event != "response.completed" {
						continue
					}
					completedCount++
					completedInputIndex = i
					completedData = data
					if i < tt.doneInputIndex {
						t.Fatalf("unexpected early response.completed on input index %d", i)
					}
				}
			}

			if completedCount != 1 {
				t.Fatalf("expected exactly 1 response.completed event, got %d", completedCount)
			}
			if completedInputIndex != tt.doneInputIndex {
				t.Fatalf("expected response.completed on terminal [DONE] chunk at input index %d, got %d", tt.doneInputIndex, completedInputIndex)
			}

			if !tt.hasUsage {
				if completedData.Get("response.usage").Exists() {
					t.Fatalf("expected response.completed to omit usage when none was provided, got %s", completedData.Get("response.usage").Raw)
				}
				return
			}

			if got := completedData.Get("response.usage.input_tokens").Int(); got != tt.inputTokens {
				t.Fatalf("unexpected response.usage.input_tokens: got %d want %d", got, tt.inputTokens)
			}
			if got := completedData.Get("response.usage.output_tokens").Int(); got != tt.outputTokens {
				t.Fatalf("unexpected response.usage.output_tokens: got %d want %d", got, tt.outputTokens)
			}
			if got := completedData.Get("response.usage.total_tokens").Int(); got != tt.totalTokens {
				t.Fatalf("unexpected response.usage.total_tokens: got %d want %d", got, tt.totalTokens)
			}
		})
	}
}

