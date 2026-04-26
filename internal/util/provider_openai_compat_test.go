package util

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestIsOpenAICompatibilityAliasSkipsDisabledProvider(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "active",
				BaseURL: "https://active.example.com",
				Models:  []config.OpenAICompatibilityModel{{Alias: "active-alias"}},
			},
			{
				Name:     "disabled",
				BaseURL:  "https://disabled.example.com",
				Disabled: true,
				Models:   []config.OpenAICompatibilityModel{{Alias: "disabled-alias"}},
			},
		},
	}

	if !IsOpenAICompatibilityAlias("active-alias", cfg) {
		t.Fatal("expected active alias to be recognized")
	}
	if IsOpenAICompatibilityAlias("disabled-alias", cfg) {
		t.Fatal("expected disabled alias to be skipped")
	}
}

func TestGetOpenAICompatibilityConfigSkipsDisabledProvider(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "active",
				BaseURL: "https://active.example.com",
				Models:  []config.OpenAICompatibilityModel{{Name: "upstream-active", Alias: "active-alias"}},
			},
			{
				Name:     "disabled",
				BaseURL:  "https://disabled.example.com",
				Disabled: true,
				Models:   []config.OpenAICompatibilityModel{{Name: "upstream-disabled", Alias: "disabled-alias"}},
			},
		},
	}

	compat, model := GetOpenAICompatibilityConfig("active-alias", cfg)
	if compat == nil || model == nil || compat.Name != "active" || model.Name != "upstream-active" {
		t.Fatalf("active alias lookup failed: compat=%+v model=%+v", compat, model)
	}
	compat, model = GetOpenAICompatibilityConfig("disabled-alias", cfg)
	if compat != nil || model != nil {
		t.Fatalf("expected disabled alias lookup to be skipped, got compat=%+v model=%+v", compat, model)
	}
}
