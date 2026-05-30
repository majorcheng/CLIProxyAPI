package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToGemini_SystemAndDeveloperRoles(t *testing.T) {
	tests := []struct {
		name      string
		role      string
		wantText  string
		forbidden string
	}{
		{name: "system", role: "system", wantText: "System message text", forbidden: "system"},
		{name: "developer", role: "developer", wantText: "Developer message text", forbidden: "developer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(`{
				"instructions": "Be a helpful assistant",
				"input": [
					{"type": "message", "role": "` + tt.role + `", "content": [{"type": "input_text", "text": "` + tt.wantText + `"}]},
					{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Hello"}]}
				]
			}`)

			out := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", input, false)
			root := gjson.ParseBytes(out)
			parts := root.Get("systemInstruction.parts")
			if got := parts.Get("#").Int(); got != 2 {
				t.Fatalf("systemInstruction.parts length = %d, want 2; out=%s", got, string(out))
			}
			if got := parts.Get("0.text").String(); got != "Be a helpful assistant" {
				t.Fatalf("systemInstruction.parts.0.text = %q", got)
			}
			if got := parts.Get("1.text").String(); got != tt.wantText {
				t.Fatalf("systemInstruction.parts.1.text = %q, want %q", got, tt.wantText)
			}
			root.Get("contents").ForEach(func(_, value gjson.Result) bool {
				if role := value.Get("role").String(); role == tt.forbidden {
					t.Fatalf("role %q leaked into contents: %s", tt.forbidden, string(out))
				}
				return true
			})
		})
	}
}
