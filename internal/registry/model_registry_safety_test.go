package registry

import (
	"reflect"
	"testing"
	"time"
)

func TestGetModelInfoReturnsClone(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Min: 1, Max: 2, Levels: []string{"low", "high"}},
	}})

	first := r.GetModelInfo("m1", "gemini")
	if first == nil {
		t.Fatal("expected model info")
	}
	first.DisplayName = "mutated"
	first.Thinking.Levels[0] = "mutated"

	second := r.GetModelInfo("m1", "gemini")
	if second.DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second.DisplayName)
	}
	if second.Thinking == nil || len(second.Thinking.Levels) == 0 || second.Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second.Thinking)
	}
}

func TestGetModelsForClientReturnsClones(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Levels: []string{"low", "high"}},
	}})

	first := r.GetModelsForClient("client-1")
	if len(first) != 1 || first[0] == nil {
		t.Fatalf("expected one model, got %+v", first)
	}
	first[0].DisplayName = "mutated"
	first[0].Thinking.Levels[0] = "mutated"

	second := r.GetModelsForClient("client-1")
	if len(second) != 1 || second[0] == nil {
		t.Fatalf("expected one model on second fetch, got %+v", second)
	}
	if second[0].DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second[0].DisplayName)
	}
	if second[0].Thinking == nil || len(second[0].Thinking.Levels) == 0 || second[0].Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second[0].Thinking)
	}
}

func TestGetAvailableModelsByProviderReturnsClones(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Levels: []string{"low", "high"}},
	}})

	first := r.GetAvailableModelsByProvider("gemini")
	if len(first) != 1 || first[0] == nil {
		t.Fatalf("expected one model, got %+v", first)
	}
	first[0].DisplayName = "mutated"
	first[0].Thinking.Levels[0] = "mutated"

	second := r.GetAvailableModelsByProvider("gemini")
	if len(second) != 1 || second[0] == nil {
		t.Fatalf("expected one model on second fetch, got %+v", second)
	}
	if second[0].DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second[0].DisplayName)
	}
	if second[0].Thinking == nil || len(second[0].Thinking.Levels) == 0 || second[0].Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second[0].Thinking)
	}
}

func TestCleanupExpiredQuotasInvalidatesAvailableModelsCache(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "openai", []*ModelInfo{{ID: "m1", Created: 1}})
	r.SetModelQuotaExceeded("client-1", "m1")
	if models := r.GetAvailableModels("openai"); len(models) != 1 {
		t.Fatalf("expected cooldown model to remain listed before cleanup, got %d", len(models))
	}

	r.mutex.Lock()
	quotaTime := time.Now().Add(-6 * time.Minute)
	r.models["m1"].QuotaExceededClients["client-1"] = &quotaTime
	r.mutex.Unlock()

	r.CleanupExpiredQuotas()

	if count := r.GetModelCount("m1"); count != 1 {
		t.Fatalf("expected model count 1 after cleanup, got %d", count)
	}
	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected model to stay available after cleanup, got %d", len(models))
	}
	if got := models[0]["id"]; got != "m1" {
		t.Fatalf("expected model id m1, got %v", got)
	}
}

func TestGetAvailableModelsReturnsClonedSupportedParameters(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "openai", []*ModelInfo{{
		ID:                  "m1",
		DisplayName:         "Model One",
		SupportedParameters: []string{"temperature", "top_p"},
	}})

	first := r.GetAvailableModels("openai")
	if len(first) != 1 {
		t.Fatalf("expected one model, got %d", len(first))
	}
	params, ok := first[0]["supported_parameters"].([]string)
	if !ok || len(params) != 2 {
		t.Fatalf("expected supported_parameters slice, got %#v", first[0]["supported_parameters"])
	}
	params[0] = "mutated"

	second := r.GetAvailableModels("openai")
	params, ok = second[0]["supported_parameters"].([]string)
	if !ok || len(params) != 2 || params[0] != "temperature" {
		t.Fatalf("expected cloned supported_parameters, got %#v", second[0]["supported_parameters"])
	}
}

func TestLookupModelInfoReturnsCloneForStaticDefinitions(t *testing.T) {
	first := LookupModelInfo("glm-4.6")
	if first == nil || first.Thinking == nil || len(first.Thinking.Levels) == 0 {
		t.Fatalf("expected static model with thinking levels, got %+v", first)
	}
	first.Thinking.Levels[0] = "mutated"

	second := LookupModelInfo("glm-4.6")
	if second == nil || second.Thinking == nil || len(second.Thinking.Levels) == 0 || second.Thinking.Levels[0] == "mutated" {
		t.Fatalf("expected static lookup clone, got %+v", second)
	}
}

// collectModelIDs 把静态模型切片收口成 ID 列表，便于直接断言套餐模型清单。
func collectModelIDs(models []*ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model != nil {
			ids = append(ids, model.ID)
		}
	}
	return ids
}

func TestCodexPlanStaticModelsMatchSelectivePortedCatalog(t *testing.T) {
	tests := []struct {
		name    string
		models  []*ModelInfo
		wantIDs []string
	}{
		{name: "codex-free", models: GetCodexFreeModels(), wantIDs: []string{"gpt-5.2", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-image-2"}},
		{name: "codex-team", models: GetCodexTeamModels(), wantIDs: []string{"gpt-5.2", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-image-2"}},
		{name: "codex-plus", models: GetCodexPlusModels(), wantIDs: []string{"gpt-5.2", "gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-image-2"}},
		{name: "codex-pro", models: GetCodexProModels(), wantIDs: []string{"gpt-5.2", "gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-image-2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIDs := collectModelIDs(tt.models)
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("static model ids = %v, want %v", gotIDs, tt.wantIDs)
			}
			info := LookupStaticModelInfo("gpt-image-2")
			if info == nil || info.DisplayName != "GPT Image 2" {
				t.Fatalf("expected builtin gpt-image-2 in static lookup, got %+v", info)
			}
			gpt54Mini := LookupStaticModelInfo("gpt-5.4-mini")
			if gpt54Mini == nil || gpt54Mini.Thinking == nil || len(gpt54Mini.Thinking.Levels) == 0 {
				t.Fatalf("expected gpt-5.4-mini thinking metadata, got %+v", gpt54Mini)
			}
		})
	}
}

func TestLookupStaticModelInfo_GPT55MatchesSelectivePortedCatalog(t *testing.T) {
	info := LookupStaticModelInfo("gpt-5.5")
	if info == nil {
		t.Fatal("LookupStaticModelInfo returned nil for gpt-5.5")
	}
	if info.DisplayName != "GPT 5.5" {
		t.Fatalf("display name = %q, want %q", info.DisplayName, "GPT 5.5")
	}
	if info.Description != "Frontier model for complex coding, research, and real-world work." {
		t.Fatalf("description = %q, want %q", info.Description, "Frontier model for complex coding, research, and real-world work.")
	}
	if info.ContextLength != 272000 {
		t.Fatalf("context length = %d, want %d", info.ContextLength, 272000)
	}
	if info.MaxCompletionTokens != 128000 {
		t.Fatalf("max completion tokens = %d, want %d", info.MaxCompletionTokens, 128000)
	}
	if info.Thinking == nil || !reflect.DeepEqual(info.Thinking.Levels, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("thinking levels = %+v, want [low medium high xhigh]", info.Thinking)
	}
}

func TestLookupStaticModelInfo_KimiK26Exists(t *testing.T) {
	info := LookupStaticModelInfo("kimi-k2.6")
	if info == nil {
		t.Fatalf("LookupStaticModelInfo returned nil for kimi-k2.6")
	}
	if info.DisplayName != "Kimi K2.6" {
		t.Fatalf("display name = %q, want %q", info.DisplayName, "Kimi K2.6")
	}
	if info.MaxCompletionTokens != 65536 {
		t.Fatalf("max completion tokens = %d, want %d", info.MaxCompletionTokens, 65536)
	}
	if info.Thinking == nil || !info.Thinking.DynamicAllowed {
		t.Fatalf("expected kimi-k2.6 thinking metadata, got %+v", info.Thinking)
	}
}

func TestLookupStaticModelInfo_ClaudeOpus47Exists(t *testing.T) {
	info := LookupStaticModelInfo("claude-opus-4-7")
	if info == nil {
		t.Fatalf("LookupStaticModelInfo returned nil for claude-opus-4-7")
	}
	if info.DisplayName != "Claude Opus 4.7" {
		t.Fatalf("display name = %q, want %q", info.DisplayName, "Claude Opus 4.7")
	}
	if info.MaxCompletionTokens != 128000 {
		t.Fatalf("max completion tokens = %d, want %d", info.MaxCompletionTokens, 128000)
	}
	if info.Thinking == nil {
		t.Fatalf("thinking config should not be nil")
	}
}
