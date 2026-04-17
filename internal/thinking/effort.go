package thinking

import "strings"

// ExtractReasoningEffort returns the normalized reasoning effort value inferred
// from the final upstream request body. It falls back across supported request
// formats so custom OpenAI-compatible providers can still expose effort.
func ExtractReasoningEffort(body []byte, provider string) string {
	for _, candidate := range reasoningEffortCandidateProviders(provider) {
		if effort := reasoningEffortFromConfig(extractThinkingConfig(body, candidate)); effort != "" {
			return effort
		}
	}
	return ""
}

func reasoningEffortCandidateProviders(provider string) []string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "gemini", "aistudio", "vertex":
		return []string{"gemini"}
	case "gemini-cli":
		return []string{"gemini-cli", "gemini"}
	case "antigravity":
		return []string{"antigravity", "gemini-cli", "gemini"}
	case "claude":
		return []string{"claude"}
	case "codex":
		return []string{"codex", "openai"}
	case "openai", "kimi":
		return []string{"openai"}
	case "iflow":
		return []string{"iflow", "openai"}
	default:
		return []string{"codex", "openai", "claude", "gemini", "gemini-cli", "antigravity", "iflow"}
	}
}

func reasoningEffortFromConfig(config ThinkingConfig) string {
	if !hasThinkingConfig(config) {
		return ""
	}

	switch config.Mode {
	case ModeLevel:
		return normalizeReasoningLevel(string(config.Level))
	case ModeBudget:
		level, ok := ConvertBudgetToLevel(config.Budget)
		if !ok {
			return ""
		}
		return level
	case ModeNone:
		return string(LevelNone)
	case ModeAuto:
		return string(LevelAuto)
	default:
		return ""
	}
}

func normalizeReasoningLevel(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
