package management

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/synthesizer"
)

type geminiKeyWithAuthIndex struct {
	config.GeminiKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type claudeKeyWithAuthIndex struct {
	config.ClaudeKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type codexKeyWithAuthIndex struct {
	config.CodexKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type vertexCompatKeyWithAuthIndex struct {
	config.VertexCompatKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type openAICompatibilityAPIKeyWithAuthIndex struct {
	config.OpenAICompatibilityAPIKey
	AuthIndex string `json:"auth-index,omitempty"`
}

type openAICompatibilityWithAuthIndex struct {
	Name          string                                   `json:"name"`
	Priority      int                                      `json:"priority,omitempty"`
	Prefix        string                                   `json:"prefix,omitempty"`
	BaseURL       string                                   `json:"base-url"`
	APIKeyEntries []openAICompatibilityAPIKeyWithAuthIndex `json:"api-key-entries,omitempty"`
	Models        []config.OpenAICompatibilityModel        `json:"models,omitempty"`
	Headers       map[string]string                        `json:"headers,omitempty"`
	AuthIndex     string                                   `json:"auth-index,omitempty"`
}

// liveAuthIndexByID 返回当前 live auth manager 中可解析的 id -> auth_index 映射。
// 这里统一基于 live manager 快照回查，保证 management 返回的 auth_index
// 始终能被后续工具接口解析，不会把仅存在于 config 草稿态的索引提前暴露出去。
func (h *Handler) liveAuthIndexByID() map[string]string {
	out := map[string]string{}
	if h == nil {
		return out
	}
	manager := h.currentAuthManager()
	if manager == nil {
		return out
	}
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		id := strings.TrimSpace(auth.ID)
		if id == "" {
			continue
		}
		idx := strings.TrimSpace(auth.Index)
		if idx == "" {
			idx = auth.EnsureIndex()
		}
		if idx == "" {
			continue
		}
		out[id] = idx
	}
	return out
}

// 这里直接按配置项自己的稳定 ID 规则回查 live auth manager，
// 这样可以避开“整份 synth 结果按游标顺序对位”带来的漂移风险。
func (h *Handler) geminiKeysWithAuthIndex() []geminiKeyWithAuthIndex {
	if h == nil {
		return nil
	}
	liveIndexByID := h.liveAuthIndexByID()
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		return nil
	}

	idGen := synthesizer.NewStableIDGenerator()
	out := make([]geminiKeyWithAuthIndex, len(cfg.GeminiKey))
	for i := range cfg.GeminiKey {
		entry := cfg.GeminiKey[i]
		authIndex := ""
		if key := strings.TrimSpace(entry.APIKey); key != "" {
			id, _ := idGen.Next("gemini:apikey", key, entry.BaseURL)
			authIndex = liveIndexByID[id]
		}
		out[i] = geminiKeyWithAuthIndex{GeminiKey: entry, AuthIndex: authIndex}
	}
	return out
}

func (h *Handler) claudeKeysWithAuthIndex() []claudeKeyWithAuthIndex {
	if h == nil {
		return nil
	}
	liveIndexByID := h.liveAuthIndexByID()
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		return nil
	}

	idGen := synthesizer.NewStableIDGenerator()
	out := make([]claudeKeyWithAuthIndex, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		entry := cfg.ClaudeKey[i]
		authIndex := ""
		if key := strings.TrimSpace(entry.APIKey); key != "" {
			id, _ := idGen.Next("claude:apikey", key, entry.BaseURL)
			authIndex = liveIndexByID[id]
		}
		out[i] = claudeKeyWithAuthIndex{ClaudeKey: entry, AuthIndex: authIndex}
	}
	return out
}

func (h *Handler) codexKeysWithAuthIndex() []codexKeyWithAuthIndex {
	if h == nil {
		return nil
	}
	liveIndexByID := h.liveAuthIndexByID()
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		return nil
	}

	idGen := synthesizer.NewStableIDGenerator()
	out := make([]codexKeyWithAuthIndex, len(cfg.CodexKey))
	for i := range cfg.CodexKey {
		entry := cfg.CodexKey[i]
		authIndex := ""
		if key := strings.TrimSpace(entry.APIKey); key != "" {
			id, _ := idGen.Next("codex:apikey", key, entry.BaseURL)
			authIndex = liveIndexByID[id]
		}
		out[i] = codexKeyWithAuthIndex{CodexKey: entry, AuthIndex: authIndex}
	}
	return out
}

func (h *Handler) vertexCompatKeysWithAuthIndex() []vertexCompatKeyWithAuthIndex {
	if h == nil {
		return nil
	}
	liveIndexByID := h.liveAuthIndexByID()
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		return nil
	}

	idGen := synthesizer.NewStableIDGenerator()
	out := make([]vertexCompatKeyWithAuthIndex, len(cfg.VertexCompatAPIKey))
	for i := range cfg.VertexCompatAPIKey {
		entry := cfg.VertexCompatAPIKey[i]
		authIndex := ""
		if key := strings.TrimSpace(entry.APIKey); key != "" {
			id, _ := idGen.Next("vertex:apikey", key, entry.BaseURL, entry.ProxyURL)
			authIndex = liveIndexByID[id]
		}
		out[i] = vertexCompatKeyWithAuthIndex{VertexCompatKey: entry, AuthIndex: authIndex}
	}
	return out
}

func openAICompatIDKind(name string) string {
	providerKey := "openai-compatibility"
	if _, normalized, err := config.NormalizeOpenAICompatName(name); err == nil {
		providerKey = config.BuildOpenAICompatProviderKeyFromNormalized(normalized)
	}
	return fmt.Sprintf("openai-compatibility:%s", providerKey)
}

func openAICompatibilityWithAuthIndexFromEntries(entries []config.OpenAICompatibility, liveIndexByID map[string]string) []openAICompatibilityWithAuthIndex {
	normalized := normalizedOpenAICompatibilityEntries(entries)
	if len(normalized) == 0 {
		return nil
	}
	idGen := synthesizer.NewStableIDGenerator()
	out := make([]openAICompatibilityWithAuthIndex, len(normalized))
	for i := range normalized {
		entry := normalized[i]
		response := openAICompatibilityWithAuthIndex{
			Name:     entry.Name,
			Priority: entry.Priority,
			Prefix:   entry.Prefix,
			BaseURL:  entry.BaseURL,
			Models:   entry.Models,
			Headers:  entry.Headers,
		}
		idKind := openAICompatIDKind(entry.Name)
		if len(entry.APIKeyEntries) == 0 {
			id, _ := idGen.Next(idKind, entry.BaseURL)
			response.AuthIndex = liveIndexByID[id]
			out[i] = response
			continue
		}
		response.APIKeyEntries = make([]openAICompatibilityAPIKeyWithAuthIndex, len(entry.APIKeyEntries))
		for j := range entry.APIKeyEntries {
			apiKeyEntry := entry.APIKeyEntries[j]
			id, _ := idGen.Next(idKind, apiKeyEntry.APIKey, entry.BaseURL, apiKeyEntry.ProxyURL)
			response.APIKeyEntries[j] = openAICompatibilityAPIKeyWithAuthIndex{
				OpenAICompatibilityAPIKey: apiKeyEntry,
				AuthIndex:                 liveIndexByID[id],
			}
		}
		out[i] = response
	}
	return out
}

func (h *Handler) openAICompatibilityWithAuthIndex() []openAICompatibilityWithAuthIndex {
	if h == nil {
		return nil
	}
	liveIndexByID := h.liveAuthIndexByID()
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		return nil
	}
	return openAICompatibilityWithAuthIndexFromEntries(cfg.OpenAICompatibility, liveIndexByID)
}
