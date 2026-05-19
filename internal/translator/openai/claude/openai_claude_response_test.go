package claude

import (
	"context"
	"strings"
	"testing"
)

func TestConvertOpenAIResponseToClaude_StreamIgnoresNullToolNameDelta(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	firstChunks := ConvertOpenAIResponseToClaude(
		context.Background(),
		"test-model",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"test-model","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`),
		&param,
	)
	firstOutput := strings.Join(firstChunks, "")
	if !strings.Contains(firstOutput, `"name":"read_file"`) {
		t.Fatalf("expected first chunk to start read_file tool block, got %s", firstOutput)
	}

	secondChunks := ConvertOpenAIResponseToClaude(
		context.Background(),
		"test-model",
		originalRequest,
		nil,
		[]byte(`data: {"id":"chatcmpl_1","model":"test-model","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":null,"arguments":"{\"path\":\"/tmp/a\"}"}}]},"finish_reason":null}]}`),
		&param,
	)
	secondOutput := strings.Join(secondChunks, "")
	if strings.Contains(secondOutput, "content_block_start") {
		t.Fatalf("did not expect null tool name delta to start a new content block, got %s", secondOutput)
	}
	if strings.Contains(secondOutput, `"name":""`) {
		t.Fatalf("did not expect null tool name delta to emit an empty tool name, got %s", secondOutput)
	}
}

func TestConvertOpenAIResponseToClaude_StreamDowngradesIncompleteToolCallFinish(t *testing.T) {
	originalRequest := []byte(`{"stream":true}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"id":"chatcmpl_2","model":"test-model","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/tmp/a\"}"}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_2","model":"test-model","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":null}`),
		[]byte(`data: [DONE]`),
	}

	var output strings.Builder
	for _, chunk := range chunks {
		output.WriteString(strings.Join(ConvertOpenAIResponseToClaude(context.Background(), "test-model", originalRequest, nil, chunk, &param), ""))
	}
	got := output.String()
	if strings.Contains(got, `"tool_use"`) {
		t.Fatalf("incomplete tool_call 不应输出 tool_use：%s", got)
	}
	if !strings.Contains(got, `"stop_reason":"end_turn"`) {
		t.Fatalf("stop_reason 未降级为 end_turn：%s", got)
	}
}
