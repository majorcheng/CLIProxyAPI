package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToCodexSystemInstructionCamelCaseFallback(t *testing.T) {
	raw := []byte(`{
		"systemInstruction": {
			"parts": [
				{"text": "Follow the project rules."}
			]
		},
		"contents": [
			{"role": "user", "parts": [{"text": "hello"}]}
		]
	}`)

	converted := ConvertGeminiRequestToCodex("gpt-5", raw, false)
	parsed := gjson.ParseBytes(converted)

	if got := parsed.Get("input.0.role").String(); got != "developer" {
		t.Fatalf("system instruction role = %q, want developer: %s", got, converted)
	}
	if got := parsed.Get("input.0.content.0.text").String(); got != "Follow the project rules." {
		t.Fatalf("system instruction text = %q: %s", got, converted)
	}
	if got := parsed.Get("input.1.role").String(); got != "user" {
		t.Fatalf("user message role = %q, want user: %s", got, converted)
	}
}
