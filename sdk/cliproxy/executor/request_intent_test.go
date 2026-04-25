package executor

import "testing"

func TestRequestHasExplicitImageGenerationIntent_DetectsToolChoice(t *testing.T) {
	if !RequestHasExplicitImageGenerationIntent([]byte(`{"tool_choice":{"type":"image_generation"}}`)) {
		t.Fatal("expected image intent when tool_choice.type=image_generation")
	}
}

func TestRequestHasExplicitImageGenerationIntent_DetectsToolsArray(t *testing.T) {
	if !RequestHasExplicitImageGenerationIntent([]byte(`{"tools":[{"type":"function"},{"type":"image_generation"}]}`)) {
		t.Fatal("expected image intent when tools contains image_generation")
	}
}

func TestRequestHasExplicitImageGenerationIntent_IgnoresPlainTextRequest(t *testing.T) {
	if RequestHasExplicitImageGenerationIntent([]byte(`{"input":"hello"}`)) {
		t.Fatal("expected plain text request to have no image intent")
	}
}
