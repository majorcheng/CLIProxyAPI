package registry

import (
	"encoding/json"
	"testing"
)

func TestGetCodexClientModelsJSONReturnsValidCatalog(t *testing.T) {
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(GetCodexClientModelsJSON(), &payload); err != nil {
		t.Fatalf("codex client models json is invalid: %v", err)
	}
	if len(payload.Models) == 0 {
		t.Fatal("expected codex client model catalog entries")
	}
	if !codexClientCatalogContainsSlug(payload.Models, "gpt-5.5") {
		t.Fatal("expected gpt-5.5 template in codex client model catalog")
	}
}

func codexClientCatalogContainsSlug(models []map[string]any, slug string) bool {
	for _, model := range models {
		if got, _ := model["slug"].(string); got == slug {
			return true
		}
	}
	return false
}
