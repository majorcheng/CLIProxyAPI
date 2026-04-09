package config

import "testing"

func TestSanitizeGeminiKeys_DeduplicatesByAPIKeyAndBaseURL(t *testing.T) {
	cfg := &Config{
		GeminiKey: []GeminiKey{
			{APIKey: " shared-key ", BaseURL: " https://a.example.com "},
			{APIKey: "shared-key", BaseURL: "https://b.example.com"},
			{APIKey: "shared-key", BaseURL: "https://a.example.com"},
		},
	}

	cfg.SanitizeGeminiKeys()

	if got := len(cfg.GeminiKey); got != 2 {
		t.Fatalf("GeminiKey len = %d, want 2", got)
	}
	if got := cfg.GeminiKey[0].BaseURL; got != "https://a.example.com" {
		t.Fatalf("GeminiKey[0].BaseURL = %q, want %q", got, "https://a.example.com")
	}
	if got := cfg.GeminiKey[1].BaseURL; got != "https://b.example.com" {
		t.Fatalf("GeminiKey[1].BaseURL = %q, want %q", got, "https://b.example.com")
	}
}
