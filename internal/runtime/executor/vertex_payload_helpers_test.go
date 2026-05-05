package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestStripVertexOpenAIResponsesToolCallIDsStripsIDs(t *testing.T) {
	payload := []byte(`{
		"contents":[
			{"parts":[
				{"functionCall":{"id":"call_1","name":"read_file","args":{"path":"/tmp/a"}}},
				{"functionResponse":{"id":"call_1","name":"read_file","response":{"ok":true}}}
			]},
			{"parts":[{"text":"done"}]}
		]
	}`)

	out := stripVertexOpenAIResponsesToolCallIDs(payload, "openai-response")
	if gjson.GetBytes(out, "contents.0.parts.0.functionCall.id").Exists() {
		t.Fatalf("functionCall.id should be stripped: %s", string(out))
	}
	if gjson.GetBytes(out, "contents.0.parts.1.functionResponse.id").Exists() {
		t.Fatalf("functionResponse.id should be stripped: %s", string(out))
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.functionCall.name").String(); got != "read_file" {
		t.Fatalf("functionCall.name = %q, want read_file", got)
	}
}

func TestStripVertexOpenAIResponsesToolCallIDsKeepsOtherFormats(t *testing.T) {
	payload := []byte(`{"contents":[{"parts":[{"functionCall":{"id":"call_1","name":"read_file"}}]}]}`)
	out := stripVertexOpenAIResponsesToolCallIDs(payload, "openai")
	if string(out) != string(payload) {
		t.Fatalf("non openai-response payload changed: %s", string(out))
	}
}
