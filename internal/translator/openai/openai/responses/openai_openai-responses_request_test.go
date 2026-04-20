package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_CompatibleStringReasoning(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"reasoning": " XHIGH ",
		"input": "hi"
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.2", inputJSON, false)

	if got := gjson.GetBytes(output, "reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want %q: %s", got, "xhigh", string(output))
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_CompatibleReasoningEffort(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"reasoning_effort": "high",
		"input": "hi"
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.2", inputJSON, false)

	if got := gjson.GetBytes(output, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want %q: %s", got, "high", string(output))
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_PrefersNestedReasoningEffort(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"reasoning": {"effort": "medium"},
		"reasoning_effort": "xhigh",
		"input": "hi"
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.2", inputJSON, false)

	if got := gjson.GetBytes(output, "reasoning_effort").String(); got != "medium" {
		t.Fatalf("reasoning_effort = %q, want %q: %s", got, "medium", string(output))
	}
}
