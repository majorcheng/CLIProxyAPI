package logging

import "testing"

func TestIsAIAPIPathIncludesImages(t *testing.T) {
	if !isAIAPIPath("/v1/images/generations") {
		t.Fatalf("expected /v1/images/generations to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/images/edits") {
		t.Fatalf("expected /v1/images/edits to be treated as AI API path")
	}
}

func TestIsAIAPIPathIncludesCodexAliasResponses(t *testing.T) {
	if !isAIAPIPath("/backend-api/codex/responses") {
		t.Fatalf("expected /backend-api/codex/responses to be treated as AI API path")
	}
	if !isAIAPIPath("/backend-api/codex/responses/compact") {
		t.Fatalf("expected /backend-api/codex/responses/compact to be treated as AI API path")
	}
}
