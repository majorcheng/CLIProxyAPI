package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeResponseToOpenAIResponses_PopulatesDoneTextPayloads(t *testing.T) {
	var state any

	events := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
	}

	var output []string
	for _, event := range events {
		output = append(output, ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-3-5-sonnet", nil, nil, event, &state)...)
	}

	var outputTextDone, contentPartDone, outputItemDone string
	for _, event := range output {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(event, "\n", 2)[1], "data:"))
		switch gjson.Get(payload, "type").String() {
		case "response.output_text.done":
			outputTextDone = payload
		case "response.content_part.done":
			contentPartDone = payload
		case "response.output_item.done":
			if gjson.Get(payload, "item.type").String() == "message" {
				outputItemDone = payload
			}
		}
	}

	if got := gjson.Get(outputTextDone, "text").String(); got != "hello world" {
		t.Fatalf("response.output_text.done.text = %q, want %q", got, "hello world")
	}
	if got := gjson.Get(contentPartDone, "part.text").String(); got != "hello world" {
		t.Fatalf("response.content_part.done.part.text = %q, want %q", got, "hello world")
	}
	if got := gjson.Get(outputItemDone, "item.content.0.text").String(); got != "hello world" {
		t.Fatalf("response.output_item.done.item.content.0.text = %q, want %q", got, "hello world")
	}
}
