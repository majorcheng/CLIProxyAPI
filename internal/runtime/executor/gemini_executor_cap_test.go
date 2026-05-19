package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
)

func TestCapGeminiMaxOutputTokensUsesRegistryOutputLimit(t *testing.T) {
	modelID := "gemini-cap-" + t.Name()
	clientID := "client-" + t.Name()
	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(clientID)
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})
	reg.RegisterClient(clientID, "gemini", []*registry.ModelInfo{{
		ID:               modelID,
		OutputTokenLimit: 1024,
	}})

	payload := []byte(`{"generationConfig":{"maxOutputTokens":9999}}`)
	out := capGeminiMaxOutputTokens(payload, modelID)

	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 1024 {
		t.Fatalf("maxOutputTokens = %d, want 1024: %s", got, string(out))
	}
}

func TestCapGeminiMaxOutputTokensKeepsValidLimit(t *testing.T) {
	modelID := "gemini-cap-keep-" + t.Name()
	clientID := "client-" + t.Name()
	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(clientID)
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})
	reg.RegisterClient(clientID, "gemini", []*registry.ModelInfo{{
		ID:               modelID,
		OutputTokenLimit: 2048,
	}})

	payload := []byte(`{"generationConfig":{"maxOutputTokens":1024}}`)
	out := capGeminiMaxOutputTokens(payload, modelID)

	if got := gjson.GetBytes(out, "generationConfig.maxOutputTokens").Int(); got != 1024 {
		t.Fatalf("maxOutputTokens = %d, want 1024: %s", got, string(out))
	}
}
