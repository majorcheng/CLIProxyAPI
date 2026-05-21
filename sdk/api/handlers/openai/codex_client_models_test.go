package openai

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestCodexClientModelsResponseUsesTemplateAndCustomMetadata(t *testing.T) {
	resp := CodexClientModelsResponse([]map[string]any{
		{"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
		{
			"id":             "custom-codex-client-model-test",
			"object":         "model",
			"owned_by":       "openai",
			"display_name":   "Custom Codex Model",
			"description":    "Custom model from registry",
			"context_length": 123456,
		},
		{"id": "gpt-image-2", "object": "model", "owned_by": "openai"},
	})

	models, ok := resp["models"].([]map[string]any)
	if !ok || len(models) == 0 {
		t.Fatalf("models = %#v, want non-empty []map[string]any", resp["models"])
	}
	assertCodexClientTemplateModelForTest(t, models)
	assertCodexClientCustomModelForTest(t, models)
	assertCodexClientHiddenImageModelForTest(t, models)
}

func TestApplyCodexClientThinkingMetadataKeepsOnlySupportedReasoningLevels(t *testing.T) {
	entry := map[string]any{}
	applyCodexClientThinkingMetadata(entry, &registry.ThinkingSupport{
		Levels: []string{"none", "minimal", "low", "medium", "unsupported", "high", "xhigh"},
	})
	sanitizeCodexClientReasoningMetadata(entry)

	levels := codexClientReasoningLevelsForTest(t, entry)
	want := []string{"none", "low", "medium", "high", "xhigh"}
	if !reflect.DeepEqual(levels, want) {
		t.Fatalf("reasoning levels = %#v, want %#v", levels, want)
	}
	if got, _ := entry["default_reasoning_level"].(string); got != "medium" {
		t.Fatalf("default_reasoning_level = %q, want medium", got)
	}
}

func TestSanitizeCodexClientReasoningMetadataFallsBackToFirstAllowedLevel(t *testing.T) {
	entry := map[string]any{
		"default_reasoning_level": "minimal",
		"supported_reasoning_levels": []any{
			map[string]any{"effort": "minimal"},
			map[string]any{"effort": "none"},
			map[string]any{"effort": "low"},
		},
	}

	sanitizeCodexClientReasoningMetadata(entry)

	levels := codexClientReasoningLevelsForTest(t, entry)
	want := []string{"none", "low"}
	if !reflect.DeepEqual(levels, want) {
		t.Fatalf("reasoning levels = %#v, want %#v", levels, want)
	}
	if got, _ := entry["default_reasoning_level"].(string); got != "none" {
		t.Fatalf("default_reasoning_level = %q, want none", got)
	}
}

func assertCodexClientTemplateModelForTest(t *testing.T, models []map[string]any) {
	t.Helper()
	gpt55 := findCodexClientModelForTest(models, "gpt-5.5")
	if gpt55 == nil {
		t.Fatal("expected gpt-5.5 codex catalog entry")
	}
	if _, ok := gpt55["minimal_client_version"]; !ok {
		t.Fatal("expected gpt-5.5 to keep template metadata")
	}
}

func assertCodexClientCustomModelForTest(t *testing.T, models []map[string]any) {
	t.Helper()
	custom := findCodexClientModelForTest(models, "custom-codex-client-model-test")
	if custom == nil {
		t.Fatal("expected custom codex catalog entry")
	}
	if got, _ := custom["display_name"].(string); got != "Custom Codex Model" {
		t.Fatalf("custom display_name = %q, want Custom Codex Model", got)
	}
	if got, _ := custom["description"].(string); got != "Custom model from registry" {
		t.Fatalf("custom description = %q, want Custom model from registry", got)
	}
	if got := intCodexClientModelValueForTest(custom["context_window"]); got != 123456 {
		t.Fatalf("custom context_window = %v, want 123456", custom["context_window"])
	}
	if got, _ := custom["prefer_websockets"].(bool); got {
		t.Fatalf("custom prefer_websockets = %v, want false", got)
	}
	if _, ok := custom["apply_patch_tool_type"]; ok {
		t.Fatal("expected custom model to omit apply_patch_tool_type")
	}
	if _, ok := custom["upgrade"]; ok {
		t.Fatal("expected custom model to omit upgrade")
	}
	if _, ok := custom["availability_nux"]; ok {
		t.Fatal("expected custom model to omit availability_nux")
	}
}

func assertCodexClientHiddenImageModelForTest(t *testing.T, models []map[string]any) {
	t.Helper()
	imageModel := findCodexClientModelForTest(models, "gpt-image-2")
	if imageModel == nil {
		t.Fatal("expected gpt-image-2 codex catalog entry")
	}
	if got, _ := imageModel["visibility"].(string); got != "hide" {
		t.Fatalf("gpt-image-2 visibility = %q, want hide", got)
	}
}

func findCodexClientModelForTest(models []map[string]any, slug string) map[string]any {
	for _, model := range models {
		if got, _ := model["slug"].(string); got == slug {
			return model
		}
	}
	return nil
}

func codexClientReasoningLevelsForTest(t *testing.T, entry map[string]any) []string {
	t.Helper()
	rawLevels, ok := entry["supported_reasoning_levels"].([]any)
	if !ok {
		t.Fatalf("supported_reasoning_levels = %#v, want []any", entry["supported_reasoning_levels"])
	}
	levels := make([]string, 0, len(rawLevels))
	for _, rawLevel := range rawLevels {
		levelEntry, ok := rawLevel.(map[string]any)
		if !ok {
			t.Fatalf("reasoning level entry = %#v, want map[string]any", rawLevel)
		}
		levels = append(levels, stringModelValue(levelEntry, "effort"))
	}
	return levels
}

func intCodexClientModelValueForTest(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}
