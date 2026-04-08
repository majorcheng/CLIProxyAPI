package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildOpenAICompatProviderKeyStable(t *testing.T) {
	keyA, err := BuildOpenAICompatProviderKey("OpenRouter HK")
	if err != nil {
		t.Fatalf("BuildOpenAICompatProviderKey() error = %v", err)
	}
	keyB, err := BuildOpenAICompatProviderKey(" openrouter hk ")
	if err != nil {
		t.Fatalf("BuildOpenAICompatProviderKey() second error = %v", err)
	}
	if keyA != keyB {
		t.Fatalf("provider key mismatch: %q != %q", keyA, keyB)
	}
	if !strings.HasPrefix(keyA, OpenAICompatProviderKeyPrefix) {
		t.Fatalf("provider key %q missing prefix %q", keyA, OpenAICompatProviderKeyPrefix)
	}
}

func TestLoadConfigOptional_OpenAICompatNameRejectsControlCharacters(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := "" +
		"openai-compatibility:\n" +
		"  - name: \"bad\\tname\"\n" +
		"    base-url: \"https://example.com\"\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	_, err := LoadConfigOptional(configPath, false)
	if err == nil {
		t.Fatal("expected control-character name to be rejected")
	}
	if !strings.Contains(err.Error(), "控制字符") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigOptional_OpenAICompatNameRejectsNormalizedConflicts(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := "" +
		"openai-compatibility:\n" +
		"  - name: \"OpenRouter\"\n" +
		"    base-url: \"https://a.example.com\"\n" +
		"  - name: \" openrouter \"\n" +
		"    base-url: \"https://b.example.com\"\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	_, err := LoadConfigOptional(configPath, false)
	if err == nil {
		t.Fatal("expected normalized name conflict to be rejected")
	}
	if !strings.Contains(err.Error(), "规范化后冲突") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigOptional_OpenAICompatNameAllowsOrdinarySpecialCharacters(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := "" +
		"openai-compatibility:\n" +
		"  - name: \"OpenRouter / 香港\"\n" +
		"    base-url: \"https://example.com\"\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("expected 1 openai-compat entry, got %d", len(cfg.OpenAICompatibility))
	}
	if got := cfg.OpenAICompatibility[0].Name; got != "OpenRouter / 香港" {
		t.Fatalf("name = %q, want %q", got, "OpenRouter / 香港")
	}
}
