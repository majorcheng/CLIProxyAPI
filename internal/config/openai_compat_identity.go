package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const OpenAICompatProviderKeyPrefix = "oaic_"

// NormalizeOpenAICompatName trims, validates and normalizes an OpenAI-compatible
// provider name for internal identity matching.
func NormalizeOpenAICompatName(name string) (trimmed string, normalized string, err error) {
	trimmed = strings.TrimSpace(name)
	if trimmed == "" {
		return "", "", fmt.Errorf("名称不能为空")
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", "", fmt.Errorf("名称不能包含控制字符")
		}
	}
	normalized = strings.ToLower(norm.NFC.String(trimmed))
	return trimmed, normalized, nil
}

// BuildOpenAICompatProviderKey builds the internal provider key used by runtime
// auth identity. The key is derived from the normalized provider name, so the
// visible display name can keep its original characters safely.
func BuildOpenAICompatProviderKey(name string) (string, error) {
	_, normalized, err := NormalizeOpenAICompatName(name)
	if err != nil {
		return "", err
	}
	return BuildOpenAICompatProviderKeyFromNormalized(normalized), nil
}

// BuildOpenAICompatProviderKeyFromNormalized builds a stable internal provider
// key from a normalized provider name.
func BuildOpenAICompatProviderKeyFromNormalized(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return OpenAICompatProviderKeyPrefix + hex.EncodeToString(sum[:])
}

// ValidateOpenAICompatNames validates OpenAI-compatible provider names and
// rejects duplicates after normalization.
func ValidateOpenAICompatNames(entries []OpenAICompatibility) error {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]string, len(entries))
	for i := range entries {
		trimmed, normalized, err := NormalizeOpenAICompatName(entries[i].Name)
		if err != nil {
			return fmt.Errorf("openai-compatibility[%d].name: %w", i, err)
		}
		if previous, ok := seen[normalized]; ok {
			return fmt.Errorf(
				"openai-compatibility[%d].name 与 %q 规范化后冲突，请修改为唯一名称",
				i,
				previous,
			)
		}
		seen[normalized] = trimmed
	}
	return nil
}

// MatchOpenAICompatIdentity matches runtime identifiers against an
// OpenAI-compatible config entry. It accepts the new hashed provider_key,
// compat_name, and the historical lower(trim(name)) provider key for backward
// compatibility during rollout.
func MatchOpenAICompatIdentity(providerKey, compatName, authProvider string, compat *OpenAICompatibility) bool {
	if compat == nil {
		return false
	}
	trimmed, normalized, err := NormalizeOpenAICompatName(compat.Name)
	if err != nil {
		return false
	}
	hashedKey := BuildOpenAICompatProviderKeyFromNormalized(normalized)
	legacyKey := strings.ToLower(strings.TrimSpace(trimmed))
	candidates := []string{
		strings.TrimSpace(providerKey),
		strings.TrimSpace(compatName),
		strings.TrimSpace(authProvider),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if strings.EqualFold(candidate, trimmed) {
			return true
		}
		if strings.EqualFold(candidate, hashedKey) {
			return true
		}
		if strings.EqualFold(candidate, legacyKey) {
			return true
		}
	}
	return false
}
