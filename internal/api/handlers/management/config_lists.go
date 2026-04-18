package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

var (
	errOpenAICompatNotFound         = errors.New("未找到 OpenAI 兼容提供商")
	errOpenAICompatNameConflict     = errors.New("OpenAI 兼容提供商名称冲突")
	errOpenAICompatRevisionConflict = errors.New("OpenAI 兼容提供商配置已变化，请刷新后重试")
	errClientAPIKeyConflict         = errors.New("api key 已存在")
)

// Generic helpers for list[string]
func (h *Handler) putStringList(c *gin.Context, set func([]string), after func()) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []string
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	h.mutateConfig(c, func(cfg *config.Config) {
		set(arr)
		if after != nil {
			after()
		}
	})
}

func (h *Handler) patchStringList(c *gin.Context, target *[]string, after func()) {
	var body struct {
		Old   *string `json:"old"`
		New   *string `json:"new"`
		Index *int    `json:"index"`
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if body.Index != nil && body.Value != nil && *body.Index >= 0 && *body.Index < len(*target) {
		h.mutateConfig(c, func(cfg *config.Config) {
			(*target)[*body.Index] = *body.Value
			if after != nil {
				after()
			}
		})
		return
	}
	if body.Old != nil && body.New != nil {
		h.mutateConfig(c, func(cfg *config.Config) {
			for i := range *target {
				if (*target)[i] == *body.Old {
					(*target)[i] = *body.New
					if after != nil {
						after()
					}
					return
				}
			}
			*target = append(*target, *body.New)
			if after != nil {
				after()
			}
		})
		return
	}
	c.JSON(400, gin.H{"error": "missing fields"})
}

func (h *Handler) deleteFromStringList(c *gin.Context, target *[]string, after func()) {
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(*target) {
			h.mutateConfig(c, func(cfg *config.Config) {
				*target = append((*target)[:idx], (*target)[idx+1:]...)
				if after != nil {
					after()
				}
			})
			return
		}
	}
	if val := strings.TrimSpace(c.Query("value")); val != "" {
		h.mutateConfig(c, func(cfg *config.Config) {
			out := make([]string, 0, len(*target))
			for _, v := range *target {
				if strings.TrimSpace(v) != val {
					out = append(out, v)
				}
			}
			*target = out
			if after != nil {
				after()
			}
		})
		return
	}
	c.JSON(400, gin.H{"error": "missing index or value"})
}

func (h *Handler) putClientAPIKeys(c *gin.Context, set func([]config.ClientAPIKey), after func()) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	entries, err := decodeClientAPIKeysPayload(data)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if conflictKey, ok := findDuplicateClientAPIKey(entries, -1); ok {
		c.JSON(http.StatusConflict, gin.H{"error": "api_key_conflict", "message": fmt.Sprintf("%s: %s", errClientAPIKeyConflict.Error(), conflictKey)})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	set(config.NormalizeClientAPIKeys(entries))
	if after != nil {
		after()
	}
	h.persistLocked(c)
}

func (h *Handler) patchClientAPIKeys(c *gin.Context, target *[]config.ClientAPIKey, after func()) {
	var body struct {
		Old   *config.ClientAPIKey `json:"old"`
		New   *config.ClientAPIKey `json:"new"`
		Index *int                 `json:"index"`
		Value *config.ClientAPIKey `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if body.Index != nil && body.Value != nil && *body.Index >= 0 && *body.Index < len(*target) {
		updated, err := normalizeClientAPIKeyValue(*body.Value)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if conflictKey, ok := findDuplicateClientAPIKeyAfterReplace(*target, *body.Index, updated); ok {
			c.JSON(http.StatusConflict, gin.H{"error": "api_key_conflict", "message": fmt.Sprintf("%s: %s", errClientAPIKeyConflict.Error(), conflictKey)})
			return
		}
		(*target)[*body.Index] = updated
		*target = config.NormalizeClientAPIKeys(*target)
		if after != nil {
			after()
		}
		h.persistLocked(c)
		return
	}
	if body.New != nil {
		updated, err := normalizeClientAPIKeyValue(*body.New)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if body.Old != nil {
			oldKey := strings.TrimSpace(body.Old.Key)
			for i := range *target {
				if strings.TrimSpace((*target)[i].Key) == oldKey {
					if conflictKey, ok := findDuplicateClientAPIKeyAfterReplace(*target, i, updated); ok {
						c.JSON(http.StatusConflict, gin.H{"error": "api_key_conflict", "message": fmt.Sprintf("%s: %s", errClientAPIKeyConflict.Error(), conflictKey)})
						return
					}
					(*target)[i] = updated
					*target = config.NormalizeClientAPIKeys(*target)
					if after != nil {
						after()
					}
					h.persistLocked(c)
					return
				}
			}
		}
		if conflictKey, ok := findDuplicateClientAPIKeyAfterReplace(*target, -1, updated); ok {
			c.JSON(http.StatusConflict, gin.H{"error": "api_key_conflict", "message": fmt.Sprintf("%s: %s", errClientAPIKeyConflict.Error(), conflictKey)})
			return
		}
		*target = config.NormalizeClientAPIKeys(append(*target, updated))
		if after != nil {
			after()
		}
		h.persistLocked(c)
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "missing fields"})
}

func (h *Handler) deleteFromClientAPIKeys(c *gin.Context, target *[]config.ClientAPIKey, after func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(*target) {
			*target = append((*target)[:idx], (*target)[idx+1:]...)
			if after != nil {
				after()
			}
			h.persistLocked(c)
			return
		}
	}
	if key := strings.TrimSpace(c.Query("key")); key != "" {
		out := make([]config.ClientAPIKey, 0, len(*target))
		for _, entry := range *target {
			if strings.TrimSpace(entry.Key) != key {
				out = append(out, entry)
			}
		}
		*target = out
		if after != nil {
			after()
		}
		h.persistLocked(c)
		return
	}
	if value := strings.TrimSpace(c.Query("value")); value != "" {
		out := make([]config.ClientAPIKey, 0, len(*target))
		for _, entry := range *target {
			if strings.TrimSpace(entry.Key) != value {
				out = append(out, entry)
			}
		}
		*target = out
		if after != nil {
			after()
		}
		h.persistLocked(c)
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "missing index or key"})
}

func decodeClientAPIKeysPayload(data []byte) ([]config.ClientAPIKey, error) {
	var entries []config.ClientAPIKey
	if err := json.Unmarshal(data, &entries); err == nil {
		return sanitizeClientAPIKeysKeepDuplicates(entries), nil
	}
	var wrapper struct {
		Items []config.ClientAPIKey `json:"items"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Items) > 0 {
		return sanitizeClientAPIKeysKeepDuplicates(wrapper.Items), nil
	}
	return nil, errors.New("invalid body")
}

func normalizeClientAPIKeyValue(entry config.ClientAPIKey) (config.ClientAPIKey, error) {
	normalized := config.NormalizeClientAPIKeys([]config.ClientAPIKey{entry})
	if len(normalized) == 0 {
		return config.ClientAPIKey{}, errors.New("invalid api key entry")
	}
	return normalized[0], nil
}

func sanitizeClientAPIKeysKeepDuplicates(entries []config.ClientAPIKey) []config.ClientAPIKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.ClientAPIKey, 0, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		out = append(out, config.ClientAPIKey{
			Key:         key,
			MaxPriority: cloneIntPointer(entry.MaxPriority),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func getClientAPIKeysResponse(cfg *config.Config, c *gin.Context) any {
	if shouldReturnClientAPIKeyObjects(c) {
		return cfg.ClientAPIKeyEntries()
	}
	return []string(cfg.APIKeys)
}

func shouldReturnClientAPIKeyObjects(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	queryValue := strings.TrimSpace(c.Query("format"))
	if strings.EqualFold(queryValue, "object") || strings.EqualFold(queryValue, "objects") {
		return true
	}
	accept := strings.ToLower(strings.TrimSpace(c.GetHeader("Accept")))
	return strings.Contains(accept, "application/vnd.router-for-me.apikeys+json")
}

func findDuplicateClientAPIKey(entries []config.ClientAPIKey, skipIndex int) (string, bool) {
	seen := make(map[string]int, len(entries))
	for index, entry := range entries {
		if skipIndex >= 0 && index == skipIndex {
			continue
		}
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			return key, true
		}
		seen[key] = index
	}
	return "", false
}

func findDuplicateClientAPIKeyAfterReplace(entries []config.ClientAPIKey, replaceIndex int, updated config.ClientAPIKey) (string, bool) {
	candidate := make([]config.ClientAPIKey, 0, len(entries)+1)
	if replaceIndex >= 0 && replaceIndex < len(entries) {
		candidate = append(candidate, entries...)
		candidate[replaceIndex] = updated
		return findDuplicateClientAPIKey(candidate, -1)
	}
	candidate = append(candidate, entries...)
	candidate = append(candidate, updated)
	return findDuplicateClientAPIKey(candidate, -1)
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// api-keys
func (h *Handler) GetAPIKeys(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"api-keys": []string{}})
		return
	}
	c.JSON(200, gin.H{"api-keys": getClientAPIKeysResponse(cfg, c)})
}
func (h *Handler) PutAPIKeys(c *gin.Context) {
	h.putClientAPIKeys(c, func(v []config.ClientAPIKey) {
		h.cfg.SetClientAPIKeyEntries(v)
	}, nil)
}
func (h *Handler) PatchAPIKeys(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	entries := cfg.ClientAPIKeyEntries()
	h.patchClientAPIKeys(c, &entries, func() {
		h.cfg.SetClientAPIKeyEntries(entries)
	})
}
func (h *Handler) DeleteAPIKeys(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	entries := cfg.ClientAPIKeyEntries()
	h.deleteFromClientAPIKeys(c, &entries, func() {
		h.cfg.SetClientAPIKeyEntries(entries)
	})
}

// gemini-api-key: []GeminiKey
func (h *Handler) GetGeminiKeys(c *gin.Context) {
	c.JSON(200, gin.H{"gemini-api-key": h.geminiKeysWithAuthIndex()})
}
func (h *Handler) PutGeminiKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.GeminiKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.GeminiKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.GeminiKey = append([]config.GeminiKey(nil), arr...)
	h.cfg.SanitizeGeminiKeys()
	h.persistLocked(c)
}
func (h *Handler) PatchGeminiKey(c *gin.Context) {
	type geminiKeyPatch struct {
		APIKey         *string            `json:"api-key"`
		Prefix         *string            `json:"prefix"`
		BaseURL        *string            `json:"base-url"`
		ProxyURL       *string            `json:"proxy-url"`
		Headers        *map[string]string `json:"headers"`
		ExcludedModels *[]string          `json:"excluded-models"`
	}
	var body struct {
		Index *int            `json:"index"`
		Match *string         `json:"match"`
		Value *geminiKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.GeminiKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		if match != "" {
			for i := range h.cfg.GeminiKey {
				if h.cfg.GeminiKey[i].APIKey == match {
					targetIndex = i
					break
				}
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.GeminiKey[targetIndex]
	if body.Value.APIKey != nil {
		trimmed := strings.TrimSpace(*body.Value.APIKey)
		if trimmed == "" {
			h.cfg.GeminiKey = append(h.cfg.GeminiKey[:targetIndex], h.cfg.GeminiKey[targetIndex+1:]...)
			h.cfg.SanitizeGeminiKeys()
			h.persistLocked(c)
			return
		}
		entry.APIKey = trimmed
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		entry.BaseURL = strings.TrimSpace(*body.Value.BaseURL)
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	h.cfg.GeminiKey[targetIndex] = entry
	h.cfg.SanitizeGeminiKeys()
	h.persistLocked(c)
}

func (h *Handler) DeleteGeminiKey(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		if baseRaw, okBase := c.GetQuery("base-url"); okBase {
			base := strings.TrimSpace(baseRaw)
			out := make([]config.GeminiKey, 0, len(h.cfg.GeminiKey))
			for _, v := range h.cfg.GeminiKey {
				if strings.TrimSpace(v.APIKey) == val && strings.TrimSpace(v.BaseURL) == base {
					continue
				}
				out = append(out, v)
			}
			if len(out) != len(h.cfg.GeminiKey) {
				h.cfg.GeminiKey = out
				h.cfg.SanitizeGeminiKeys()
				h.persistLocked(c)
			} else {
				c.JSON(404, gin.H{"error": "item not found"})
			}
			return
		}

		matchIndex := -1
		matchCount := 0
		for i := range h.cfg.GeminiKey {
			if strings.TrimSpace(h.cfg.GeminiKey[i].APIKey) == val {
				matchCount++
				if matchIndex == -1 {
					matchIndex = i
				}
			}
		}
		if matchCount == 0 {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		if matchCount > 1 {
			c.JSON(400, gin.H{"error": "multiple items match api-key; base-url is required"})
			return
		}
		h.cfg.GeminiKey = append(h.cfg.GeminiKey[:matchIndex], h.cfg.GeminiKey[matchIndex+1:]...)
		h.cfg.SanitizeGeminiKeys()
		h.persistLocked(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && idx >= 0 && idx < len(h.cfg.GeminiKey) {
			h.cfg.GeminiKey = append(h.cfg.GeminiKey[:idx], h.cfg.GeminiKey[idx+1:]...)
			h.cfg.SanitizeGeminiKeys()
			h.persistLocked(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// claude-api-key: []ClaudeKey
func (h *Handler) GetClaudeKeys(c *gin.Context) {
	c.JSON(200, gin.H{"claude-api-key": h.claudeKeysWithAuthIndex()})
}
func (h *Handler) PutClaudeKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.ClaudeKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.ClaudeKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	for i := range arr {
		normalizeClaudeKey(&arr[i])
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ClaudeKey = arr
	h.cfg.SanitizeClaudeKeys()
	h.persistLocked(c)
}
func (h *Handler) PatchClaudeKey(c *gin.Context) {
	type claudeKeyPatch struct {
		APIKey         *string               `json:"api-key"`
		Prefix         *string               `json:"prefix"`
		BaseURL        *string               `json:"base-url"`
		ProxyURL       *string               `json:"proxy-url"`
		Models         *[]config.ClaudeModel `json:"models"`
		Headers        *map[string]string    `json:"headers"`
		ExcludedModels *[]string             `json:"excluded-models"`
	}
	var body struct {
		Index *int            `json:"index"`
		Match *string         `json:"match"`
		Value *claudeKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.ClaudeKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		for i := range h.cfg.ClaudeKey {
			if h.cfg.ClaudeKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.ClaudeKey[targetIndex]
	if body.Value.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*body.Value.APIKey)
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		entry.BaseURL = strings.TrimSpace(*body.Value.BaseURL)
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.Models != nil {
		entry.Models = append([]config.ClaudeModel(nil), (*body.Value.Models)...)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	normalizeClaudeKey(&entry)
	h.cfg.ClaudeKey[targetIndex] = entry
	h.cfg.SanitizeClaudeKeys()
	h.persistLocked(c)
}

func (h *Handler) DeleteClaudeKey(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		if baseRaw, okBase := c.GetQuery("base-url"); okBase {
			base := strings.TrimSpace(baseRaw)
			out := make([]config.ClaudeKey, 0, len(h.cfg.ClaudeKey))
			for _, v := range h.cfg.ClaudeKey {
				if strings.TrimSpace(v.APIKey) == val && strings.TrimSpace(v.BaseURL) == base {
					continue
				}
				out = append(out, v)
			}
			h.cfg.ClaudeKey = out
			h.cfg.SanitizeClaudeKeys()
			h.persistLocked(c)
			return
		}

		matchIndex := -1
		matchCount := 0
		for i := range h.cfg.ClaudeKey {
			if strings.TrimSpace(h.cfg.ClaudeKey[i].APIKey) == val {
				matchCount++
				if matchIndex == -1 {
					matchIndex = i
				}
			}
		}
		if matchCount > 1 {
			c.JSON(400, gin.H{"error": "multiple items match api-key; base-url is required"})
			return
		}
		if matchIndex != -1 {
			h.cfg.ClaudeKey = append(h.cfg.ClaudeKey[:matchIndex], h.cfg.ClaudeKey[matchIndex+1:]...)
		}
		h.cfg.SanitizeClaudeKeys()
		h.persistLocked(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(h.cfg.ClaudeKey) {
			h.cfg.ClaudeKey = append(h.cfg.ClaudeKey[:idx], h.cfg.ClaudeKey[idx+1:]...)
			h.cfg.SanitizeClaudeKeys()
			h.persistLocked(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// openai-compatibility: []OpenAICompatibility
func (h *Handler) GetOpenAICompat(c *gin.Context) {
	entries, revision, err := h.readOpenAICompatState()
	if err != nil {
		c.JSON(500, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	c.JSON(200, gin.H{"openai-compatibility": openAICompatibilityWithAuthIndexFromEntries(entries, h.liveAuthIndexByID()), "revision": revision})
}

func (h *Handler) writeOpenAICompatListResponse(c *gin.Context, entries []config.OpenAICompatibility, revision string) {
	c.JSON(200, gin.H{
		"openai-compatibility": openAICompatibilityWithAuthIndexFromEntries(entries, h.liveAuthIndexByID()),
		"revision":             revision,
	})
}

func (h *Handler) PostOpenAICompat(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var body struct {
		Revision string                      `json:"revision"`
		Value    *config.OpenAICompatibility `json:"value"`
	}
	if err = json.Unmarshal(data, &body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	var entry config.OpenAICompatibility
	if body.Value != nil {
		entry = *body.Value
	} else if err = json.Unmarshal(data, &entry); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	normalizeOpenAICompatibilityEntry(&entry)
	if err = validateOpenAICompatEntry(entry); err != nil {
		c.JSON(400, gin.H{"error": "invalid_openai_provider", "message": err.Error()})
		return
	}

	entries, revision, err := h.mutateOpenAICompat(strings.TrimSpace(body.Revision), func(fresh *config.Config) error {
		if idx, findErr := findOpenAICompatIndexByName(fresh.OpenAICompatibility, entry.Name); findErr != nil {
			return findErr
		} else if idx >= 0 {
			return errOpenAICompatNameConflict
		}
		fresh.OpenAICompatibility = append(fresh.OpenAICompatibility, entry)
		return nil
	})
	if err != nil {
		h.writeOpenAICompatMutationError(c, err)
		return
	}
	h.writeOpenAICompatListResponse(c, entries, revision)
}

func (h *Handler) PutOpenAICompat(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.OpenAICompatibility
	revision := strings.TrimSpace(c.Query("revision"))
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Revision string                       `json:"revision"`
			Items    []config.OpenAICompatibility `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		revision = strings.TrimSpace(obj.Revision)
		arr = obj.Items
	}
	filtered := make([]config.OpenAICompatibility, 0, len(arr))
	for i := range arr {
		normalizeOpenAICompatibilityEntry(&arr[i])
		if strings.TrimSpace(arr[i].BaseURL) != "" {
			filtered = append(filtered, arr[i])
		}
	}
	if err = config.ValidateOpenAICompatNames(filtered); err != nil {
		status := 400
		if strings.Contains(err.Error(), "规范化后冲突") {
			status = 409
		}
		c.JSON(status, gin.H{"error": "invalid_openai_provider", "message": err.Error()})
		return
	}

	entries, nextRevision, err := h.mutateOpenAICompat(revision, func(fresh *config.Config) error {
		fresh.OpenAICompatibility = append([]config.OpenAICompatibility(nil), filtered...)
		return nil
	})
	if err != nil {
		h.writeOpenAICompatMutationError(c, err)
		return
	}
	h.writeOpenAICompatListResponse(c, entries, nextRevision)
}

func (h *Handler) PatchOpenAICompat(c *gin.Context) {
	type openAICompatPatch struct {
		Name          *string                             `json:"name"`
		Priority      *int                                `json:"priority"`
		Prefix        *string                             `json:"prefix"`
		BaseURL       *string                             `json:"base-url"`
		APIKeyEntries *[]config.OpenAICompatibilityAPIKey `json:"api-key-entries"`
		Models        *[]config.OpenAICompatibilityModel  `json:"models"`
		Headers       *map[string]string                  `json:"headers"`
	}
	var body struct {
		Revision  string             `json:"revision"`
		MatchName *string            `json:"matchName"`
		Name      *string            `json:"name"`
		Index     *int               `json:"index"`
		Value     *openAICompatPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	matchName := ""
	if body.MatchName != nil {
		matchName = strings.TrimSpace(*body.MatchName)
	}
	if matchName == "" && body.Name != nil {
		matchName = strings.TrimSpace(*body.Name)
	}

	entries, revision, err := h.mutateOpenAICompat(strings.TrimSpace(body.Revision), func(fresh *config.Config) error {
		targetIndex := -1
		if body.Index != nil && *body.Index >= 0 && *body.Index < len(fresh.OpenAICompatibility) {
			targetIndex = *body.Index
		}
		if targetIndex == -1 && matchName != "" {
			idx, findErr := findOpenAICompatIndexByName(fresh.OpenAICompatibility, matchName)
			if findErr != nil {
				return findErr
			}
			targetIndex = idx
		}
		if targetIndex == -1 {
			return errOpenAICompatNotFound
		}

		entry := fresh.OpenAICompatibility[targetIndex]
		if body.Value.Name != nil {
			entry.Name = strings.TrimSpace(*body.Value.Name)
		}
		if body.Value.Priority != nil {
			entry.Priority = *body.Value.Priority
		}
		if body.Value.Prefix != nil {
			entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
		}
		if body.Value.BaseURL != nil {
			trimmed := strings.TrimSpace(*body.Value.BaseURL)
			if trimmed == "" {
				fresh.OpenAICompatibility = append(fresh.OpenAICompatibility[:targetIndex], fresh.OpenAICompatibility[targetIndex+1:]...)
				return nil
			}
			entry.BaseURL = trimmed
		}
		if body.Value.APIKeyEntries != nil {
			entry.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), (*body.Value.APIKeyEntries)...)
		}
		if body.Value.Models != nil {
			entry.Models = append([]config.OpenAICompatibilityModel(nil), (*body.Value.Models)...)
		}
		if body.Value.Headers != nil {
			entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
		}
		normalizeOpenAICompatibilityEntry(&entry)

		if nameConflictWithOthers(fresh.OpenAICompatibility, targetIndex, entry.Name) {
			return errOpenAICompatNameConflict
		}
		if err := validateOpenAICompatEntry(entry); err != nil {
			return err
		}
		fresh.OpenAICompatibility[targetIndex] = entry
		return nil
	})
	if err != nil {
		h.writeOpenAICompatMutationError(c, err)
		return
	}
	h.writeOpenAICompatListResponse(c, entries, revision)
}

func (h *Handler) DeleteOpenAICompat(c *gin.Context) {
	var body struct {
		Revision string `json:"revision"`
		Name     string `json:"name"`
	}
	_ = c.ShouldBindJSON(&body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(c.Query("name"))
	}
	revision := strings.TrimSpace(body.Revision)
	if revision == "" {
		revision = strings.TrimSpace(c.Query("revision"))
	}
	if name == "" {
		c.JSON(400, gin.H{"error": "missing_name", "message": "请提供要删除的 OpenAI 兼容提供商名称"})
		return
	}

	entries, nextRevision, err := h.mutateOpenAICompat(revision, func(fresh *config.Config) error {
		idx, findErr := findOpenAICompatIndexByName(fresh.OpenAICompatibility, name)
		if findErr != nil {
			return findErr
		}
		if idx == -1 {
			return errOpenAICompatNotFound
		}
		fresh.OpenAICompatibility = append(fresh.OpenAICompatibility[:idx], fresh.OpenAICompatibility[idx+1:]...)
		return nil
	})
	if err != nil {
		h.writeOpenAICompatMutationError(c, err)
		return
	}
	h.writeOpenAICompatListResponse(c, entries, nextRevision)
}

// vertex-api-key: []VertexCompatKey
func (h *Handler) GetVertexCompatKeys(c *gin.Context) {
	c.JSON(200, gin.H{"vertex-api-key": h.vertexCompatKeysWithAuthIndex()})
}
func (h *Handler) PutVertexCompatKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.VertexCompatKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.VertexCompatKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	for i := range arr {
		normalizeVertexCompatKey(&arr[i])
		if arr[i].APIKey == "" {
			c.JSON(400, gin.H{"error": fmt.Sprintf("vertex-api-key[%d].api-key is required", i)})
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.VertexCompatAPIKey = append([]config.VertexCompatKey(nil), arr...)
	h.cfg.SanitizeVertexCompatKeys()
	h.persistLocked(c)
}
func (h *Handler) PatchVertexCompatKey(c *gin.Context) {
	type vertexCompatPatch struct {
		APIKey         *string                     `json:"api-key"`
		Prefix         *string                     `json:"prefix"`
		BaseURL        *string                     `json:"base-url"`
		ProxyURL       *string                     `json:"proxy-url"`
		Headers        *map[string]string          `json:"headers"`
		Models         *[]config.VertexCompatModel `json:"models"`
		ExcludedModels *[]string                   `json:"excluded-models"`
	}
	var body struct {
		Index *int               `json:"index"`
		Match *string            `json:"match"`
		Value *vertexCompatPatch `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.VertexCompatAPIKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		if match != "" {
			for i := range h.cfg.VertexCompatAPIKey {
				if h.cfg.VertexCompatAPIKey[i].APIKey == match {
					targetIndex = i
					break
				}
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.VertexCompatAPIKey[targetIndex]
	if body.Value.APIKey != nil {
		trimmed := strings.TrimSpace(*body.Value.APIKey)
		if trimmed == "" {
			h.cfg.VertexCompatAPIKey = append(h.cfg.VertexCompatAPIKey[:targetIndex], h.cfg.VertexCompatAPIKey[targetIndex+1:]...)
			h.cfg.SanitizeVertexCompatKeys()
			h.persistLocked(c)
			return
		}
		entry.APIKey = trimmed
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		trimmed := strings.TrimSpace(*body.Value.BaseURL)
		if trimmed == "" {
			h.cfg.VertexCompatAPIKey = append(h.cfg.VertexCompatAPIKey[:targetIndex], h.cfg.VertexCompatAPIKey[targetIndex+1:]...)
			h.cfg.SanitizeVertexCompatKeys()
			h.persistLocked(c)
			return
		}
		entry.BaseURL = trimmed
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.Models != nil {
		entry.Models = append([]config.VertexCompatModel(nil), (*body.Value.Models)...)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	normalizeVertexCompatKey(&entry)
	h.cfg.VertexCompatAPIKey[targetIndex] = entry
	h.cfg.SanitizeVertexCompatKeys()
	h.persistLocked(c)
}

func (h *Handler) DeleteVertexCompatKey(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		if baseRaw, okBase := c.GetQuery("base-url"); okBase {
			base := strings.TrimSpace(baseRaw)
			out := make([]config.VertexCompatKey, 0, len(h.cfg.VertexCompatAPIKey))
			for _, v := range h.cfg.VertexCompatAPIKey {
				if strings.TrimSpace(v.APIKey) == val && strings.TrimSpace(v.BaseURL) == base {
					continue
				}
				out = append(out, v)
			}
			h.cfg.VertexCompatAPIKey = out
			h.cfg.SanitizeVertexCompatKeys()
			h.persistLocked(c)
			return
		}

		matchIndex := -1
		matchCount := 0
		for i := range h.cfg.VertexCompatAPIKey {
			if strings.TrimSpace(h.cfg.VertexCompatAPIKey[i].APIKey) == val {
				matchCount++
				if matchIndex == -1 {
					matchIndex = i
				}
			}
		}
		if matchCount > 1 {
			c.JSON(400, gin.H{"error": "multiple items match api-key; base-url is required"})
			return
		}
		if matchIndex != -1 {
			h.cfg.VertexCompatAPIKey = append(h.cfg.VertexCompatAPIKey[:matchIndex], h.cfg.VertexCompatAPIKey[matchIndex+1:]...)
		}
		h.cfg.SanitizeVertexCompatKeys()
		h.persistLocked(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, errScan := fmt.Sscanf(idxStr, "%d", &idx)
		if errScan == nil && idx >= 0 && idx < len(h.cfg.VertexCompatAPIKey) {
			h.cfg.VertexCompatAPIKey = append(h.cfg.VertexCompatAPIKey[:idx], h.cfg.VertexCompatAPIKey[idx+1:]...)
			h.cfg.SanitizeVertexCompatKeys()
			h.persistLocked(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// oauth-excluded-models: map[string][]string
func (h *Handler) GetOAuthExcludedModels(c *gin.Context) {
	cfg := h.requireConfigSnapshot(c)
	if cfg == nil {
		return
	}
	c.JSON(200, gin.H{"oauth-excluded-models": config.NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)})
}

func (h *Handler) PutOAuthExcludedModels(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]string
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		entries = wrapper.Items
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.OAuthExcludedModels = config.NormalizeOAuthExcludedModels(entries)
	h.persistLocked(c)
}

func (h *Handler) PatchOAuthExcludedModels(c *gin.Context) {
	var body struct {
		Provider *string  `json:"provider"`
		Models   []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Provider == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(*body.Provider))
	if provider == "" {
		c.JSON(400, gin.H{"error": "invalid provider"})
		return
	}
	normalized := config.NormalizeExcludedModels(body.Models)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(normalized) == 0 {
		if h.cfg.OAuthExcludedModels == nil {
			c.JSON(404, gin.H{"error": "provider not found"})
			return
		}
		if _, ok := h.cfg.OAuthExcludedModels[provider]; !ok {
			c.JSON(404, gin.H{"error": "provider not found"})
			return
		}
		delete(h.cfg.OAuthExcludedModels, provider)
		if len(h.cfg.OAuthExcludedModels) == 0 {
			h.cfg.OAuthExcludedModels = nil
		}
		h.persistLocked(c)
		return
	}
	if h.cfg.OAuthExcludedModels == nil {
		h.cfg.OAuthExcludedModels = make(map[string][]string)
	}
	h.cfg.OAuthExcludedModels[provider] = normalized
	h.persistLocked(c)
}

func (h *Handler) DeleteOAuthExcludedModels(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "" {
		c.JSON(400, gin.H{"error": "missing provider"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg.OAuthExcludedModels == nil {
		c.JSON(404, gin.H{"error": "provider not found"})
		return
	}
	if _, ok := h.cfg.OAuthExcludedModels[provider]; !ok {
		c.JSON(404, gin.H{"error": "provider not found"})
		return
	}
	delete(h.cfg.OAuthExcludedModels, provider)
	if len(h.cfg.OAuthExcludedModels) == 0 {
		h.cfg.OAuthExcludedModels = nil
	}
	h.persistLocked(c)
}

// oauth-model-alias: map[string][]OAuthModelAlias
func (h *Handler) GetOAuthModelAlias(c *gin.Context) {
	cfg := h.requireConfigSnapshot(c)
	if cfg == nil {
		return
	}
	c.JSON(200, gin.H{"oauth-model-alias": sanitizedOAuthModelAlias(cfg.OAuthModelAlias)})
}

func (h *Handler) PutOAuthModelAlias(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]config.OAuthModelAlias
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]config.OAuthModelAlias `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		entries = wrapper.Items
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.OAuthModelAlias = sanitizedOAuthModelAlias(entries)
	h.persistLocked(c)
}

func (h *Handler) PatchOAuthModelAlias(c *gin.Context) {
	var body struct {
		Provider *string                  `json:"provider"`
		Channel  *string                  `json:"channel"`
		Aliases  []config.OAuthModelAlias `json:"aliases"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	channelRaw := ""
	if body.Channel != nil {
		channelRaw = *body.Channel
	} else if body.Provider != nil {
		channelRaw = *body.Provider
	}
	channel := strings.ToLower(strings.TrimSpace(channelRaw))
	if channel == "" {
		c.JSON(400, gin.H{"error": "invalid channel"})
		return
	}

	normalizedMap := sanitizedOAuthModelAlias(map[string][]config.OAuthModelAlias{channel: body.Aliases})
	normalized := normalizedMap[channel]
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(normalized) == 0 {
		if h.cfg.OAuthModelAlias == nil {
			c.JSON(404, gin.H{"error": "channel not found"})
			return
		}
		if _, ok := h.cfg.OAuthModelAlias[channel]; !ok {
			c.JSON(404, gin.H{"error": "channel not found"})
			return
		}
		delete(h.cfg.OAuthModelAlias, channel)
		if len(h.cfg.OAuthModelAlias) == 0 {
			h.cfg.OAuthModelAlias = nil
		}
		h.persistLocked(c)
		return
	}
	if h.cfg.OAuthModelAlias == nil {
		h.cfg.OAuthModelAlias = make(map[string][]config.OAuthModelAlias)
	}
	h.cfg.OAuthModelAlias[channel] = normalized
	h.persistLocked(c)
}

func (h *Handler) DeleteOAuthModelAlias(c *gin.Context) {
	channel := strings.ToLower(strings.TrimSpace(c.Query("channel")))
	if channel == "" {
		channel = strings.ToLower(strings.TrimSpace(c.Query("provider")))
	}
	if channel == "" {
		c.JSON(400, gin.H{"error": "missing channel"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg.OAuthModelAlias == nil {
		c.JSON(404, gin.H{"error": "channel not found"})
		return
	}
	if _, ok := h.cfg.OAuthModelAlias[channel]; !ok {
		c.JSON(404, gin.H{"error": "channel not found"})
		return
	}
	delete(h.cfg.OAuthModelAlias, channel)
	if len(h.cfg.OAuthModelAlias) == 0 {
		h.cfg.OAuthModelAlias = nil
	}
	h.persistLocked(c)
}

// codex-api-key: []CodexKey
func (h *Handler) GetCodexKeys(c *gin.Context) {
	c.JSON(200, gin.H{"codex-api-key": h.codexKeysWithAuthIndex()})
}
func (h *Handler) PutCodexKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.CodexKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.CodexKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	// Filter out codex entries with empty base-url (treat as removed)
	filtered := make([]config.CodexKey, 0, len(arr))
	for i := range arr {
		entry := arr[i]
		normalizeCodexKey(&entry)
		if entry.BaseURL == "" {
			continue
		}
		filtered = append(filtered, entry)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.CodexKey = filtered
	h.cfg.SanitizeCodexKeys()
	h.persistLocked(c)
}
func (h *Handler) PatchCodexKey(c *gin.Context) {
	type codexKeyPatch struct {
		APIKey         *string              `json:"api-key"`
		Prefix         *string              `json:"prefix"`
		BaseURL        *string              `json:"base-url"`
		ProxyURL       *string              `json:"proxy-url"`
		Models         *[]config.CodexModel `json:"models"`
		Headers        *map[string]string   `json:"headers"`
		ExcludedModels *[]string            `json:"excluded-models"`
	}
	var body struct {
		Index *int           `json:"index"`
		Match *string        `json:"match"`
		Value *codexKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.CodexKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		for i := range h.cfg.CodexKey {
			if h.cfg.CodexKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.CodexKey[targetIndex]
	if body.Value.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*body.Value.APIKey)
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		trimmed := strings.TrimSpace(*body.Value.BaseURL)
		if trimmed == "" {
			h.cfg.CodexKey = append(h.cfg.CodexKey[:targetIndex], h.cfg.CodexKey[targetIndex+1:]...)
			h.cfg.SanitizeCodexKeys()
			h.persistLocked(c)
			return
		}
		entry.BaseURL = trimmed
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.Models != nil {
		entry.Models = append([]config.CodexModel(nil), (*body.Value.Models)...)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	normalizeCodexKey(&entry)
	h.cfg.CodexKey[targetIndex] = entry
	h.cfg.SanitizeCodexKeys()
	h.persistLocked(c)
}

func (h *Handler) DeleteCodexKey(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		if baseRaw, okBase := c.GetQuery("base-url"); okBase {
			base := strings.TrimSpace(baseRaw)
			out := make([]config.CodexKey, 0, len(h.cfg.CodexKey))
			for _, v := range h.cfg.CodexKey {
				if strings.TrimSpace(v.APIKey) == val && strings.TrimSpace(v.BaseURL) == base {
					continue
				}
				out = append(out, v)
			}
			h.cfg.CodexKey = out
			h.cfg.SanitizeCodexKeys()
			h.persistLocked(c)
			return
		}

		matchIndex := -1
		matchCount := 0
		for i := range h.cfg.CodexKey {
			if strings.TrimSpace(h.cfg.CodexKey[i].APIKey) == val {
				matchCount++
				if matchIndex == -1 {
					matchIndex = i
				}
			}
		}
		if matchCount > 1 {
			c.JSON(400, gin.H{"error": "multiple items match api-key; base-url is required"})
			return
		}
		if matchIndex != -1 {
			h.cfg.CodexKey = append(h.cfg.CodexKey[:matchIndex], h.cfg.CodexKey[matchIndex+1:]...)
		}
		h.cfg.SanitizeCodexKeys()
		h.persistLocked(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(h.cfg.CodexKey) {
			h.cfg.CodexKey = append(h.cfg.CodexKey[:idx], h.cfg.CodexKey[idx+1:]...)
			h.cfg.SanitizeCodexKeys()
			h.persistLocked(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

func validateOpenAICompatEntry(entry config.OpenAICompatibility) error {
	if strings.TrimSpace(entry.BaseURL) == "" {
		return fmt.Errorf("base-url 不能为空")
	}
	return config.ValidateOpenAICompatNames([]config.OpenAICompatibility{entry})
}

func findOpenAICompatIndexByName(entries []config.OpenAICompatibility, name string) (int, error) {
	_, normalizedName, err := config.NormalizeOpenAICompatName(name)
	if err != nil {
		return -1, err
	}
	for i := range entries {
		_, normalizedEntryName, normalizeErr := config.NormalizeOpenAICompatName(entries[i].Name)
		if normalizeErr != nil {
			return -1, normalizeErr
		}
		if normalizedEntryName == normalizedName {
			return i, nil
		}
	}
	return -1, nil
}

func nameConflictWithOthers(entries []config.OpenAICompatibility, targetIndex int, name string) bool {
	_, normalizedName, err := config.NormalizeOpenAICompatName(name)
	if err != nil {
		return false
	}
	for i := range entries {
		if i == targetIndex {
			continue
		}
		_, normalizedEntryName, normalizeErr := config.NormalizeOpenAICompatName(entries[i].Name)
		if normalizeErr != nil {
			continue
		}
		if normalizedEntryName == normalizedName {
			return true
		}
	}
	return false
}

func (h *Handler) readOpenAICompatState() ([]config.OpenAICompatibility, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	freshCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		return nil, "", err
	}
	revision, err := readConfigRevision(h.configFilePath)
	if err != nil {
		return nil, "", err
	}
	return normalizedOpenAICompatibilityEntries(freshCfg.OpenAICompatibility), revision, nil
}

func (h *Handler) mutateOpenAICompat(
	expectedRevision string,
	mutate func(*config.Config) error,
) ([]config.OpenAICompatibility, string, error) {
	h.mu.Lock()

	freshCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		h.mu.Unlock()
		return nil, "", err
	}
	currentRevision, err := readConfigRevision(h.configFilePath)
	if err != nil {
		h.mu.Unlock()
		return nil, "", err
	}
	if expectedRevision != "" && expectedRevision != currentRevision {
		h.mu.Unlock()
		return nil, currentRevision, errOpenAICompatRevisionConflict
	}

	if err := mutate(freshCfg); err != nil {
		h.mu.Unlock()
		return nil, currentRevision, err
	}

	freshCfg.SanitizeOpenAICompatibility()
	if err := config.ValidateOpenAICompatNames(freshCfg.OpenAICompatibility); err != nil {
		h.mu.Unlock()
		return nil, currentRevision, err
	}
	if err := config.SaveConfigPreserveComments(h.configFilePath, freshCfg); err != nil {
		h.mu.Unlock()
		return nil, "", err
	}

	savedCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		h.mu.Unlock()
		return nil, "", err
	}
	revision, err := readConfigRevision(h.configFilePath)
	if err != nil {
		h.mu.Unlock()
		return nil, "", err
	}
	h.cfg = savedCfg
	applier := h.configApplied
	entries := normalizedOpenAICompatibilityEntries(savedCfg.OpenAICompatibility)
	h.mu.Unlock()

	if applier != nil {
		applier(savedCfg)
	}
	return entries, revision, nil
}

func (h *Handler) writeOpenAICompatMutationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errOpenAICompatRevisionConflict):
		c.JSON(409, gin.H{"error": "revision_conflict", "message": err.Error()})
	case errors.Is(err, errOpenAICompatNameConflict):
		c.JSON(409, gin.H{"error": "name_conflict", "message": err.Error()})
	case errors.Is(err, errOpenAICompatNotFound):
		c.JSON(404, gin.H{"error": "not_found", "message": err.Error()})
	case strings.Contains(err.Error(), "规范化后冲突"):
		c.JSON(409, gin.H{"error": "name_conflict", "message": err.Error()})
	default:
		c.JSON(400, gin.H{"error": "invalid_openai_provider", "message": err.Error()})
	}
}

func normalizeOpenAICompatibilityEntry(entry *config.OpenAICompatibility) {
	if entry == nil {
		return
	}
	// Trim base-url; empty base-url indicates provider should be removed by sanitization
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	existing := make(map[string]struct{}, len(entry.APIKeyEntries))
	for i := range entry.APIKeyEntries {
		trimmed := strings.TrimSpace(entry.APIKeyEntries[i].APIKey)
		entry.APIKeyEntries[i].APIKey = trimmed
		if trimmed != "" {
			existing[trimmed] = struct{}{}
		}
	}
}

func normalizedOpenAICompatibilityEntries(entries []config.OpenAICompatibility) []config.OpenAICompatibility {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OpenAICompatibility, len(entries))
	for i := range entries {
		copyEntry := entries[i]
		if len(copyEntry.APIKeyEntries) > 0 {
			copyEntry.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), copyEntry.APIKeyEntries...)
		}
		normalizeOpenAICompatibilityEntry(&copyEntry)
		out[i] = copyEntry
	}
	return out
}

func normalizeClaudeKey(entry *config.ClaudeKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.ClaudeModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" && model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}

func normalizeCodexKey(entry *config.CodexKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.CodexModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" && model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}

func normalizeVertexCompatKey(entry *config.VertexCompatKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.VertexCompatModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" || model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}

func sanitizedOAuthModelAlias(entries map[string][]config.OAuthModelAlias) map[string][]config.OAuthModelAlias {
	if len(entries) == 0 {
		return nil
	}
	copied := make(map[string][]config.OAuthModelAlias, len(entries))
	for channel, aliases := range entries {
		if len(aliases) == 0 {
			continue
		}
		copied[channel] = append([]config.OAuthModelAlias(nil), aliases...)
	}
	if len(copied) == 0 {
		return nil
	}
	cfg := config.Config{OAuthModelAlias: copied}
	cfg.SanitizeOAuthModelAlias()
	if len(cfg.OAuthModelAlias) == 0 {
		return nil
	}
	return cfg.OAuthModelAlias
}

// GetAmpCode returns the complete ampcode configuration.
func (h *Handler) GetAmpCode(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"ampcode": config.AmpCode{}})
		return
	}
	c.JSON(200, gin.H{"ampcode": cfg.AmpCode})
}

// GetAmpUpstreamURL returns the ampcode upstream URL.
func (h *Handler) GetAmpUpstreamURL(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"upstream-url": ""})
		return
	}
	c.JSON(200, gin.H{"upstream-url": cfg.AmpCode.UpstreamURL})
}

// PutAmpUpstreamURL updates the ampcode upstream URL.
func (h *Handler) PutAmpUpstreamURL(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.AmpCode.UpstreamURL = strings.TrimSpace(v) })
}

// DeleteAmpUpstreamURL clears the ampcode upstream URL.
func (h *Handler) DeleteAmpUpstreamURL(c *gin.Context) {
	h.mutateConfig(c, func(cfg *config.Config) {
		cfg.AmpCode.UpstreamURL = ""
	})
}

// GetAmpUpstreamAPIKey returns the ampcode upstream API key.
func (h *Handler) GetAmpUpstreamAPIKey(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"upstream-api-key": ""})
		return
	}
	c.JSON(200, gin.H{"upstream-api-key": cfg.AmpCode.UpstreamAPIKey})
}

// PutAmpUpstreamAPIKey updates the ampcode upstream API key.
func (h *Handler) PutAmpUpstreamAPIKey(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.AmpCode.UpstreamAPIKey = strings.TrimSpace(v) })
}

// DeleteAmpUpstreamAPIKey clears the ampcode upstream API key.
func (h *Handler) DeleteAmpUpstreamAPIKey(c *gin.Context) {
	h.mutateConfig(c, func(cfg *config.Config) {
		cfg.AmpCode.UpstreamAPIKey = ""
	})
}

// GetAmpRestrictManagementToLocalhost returns the localhost restriction setting.
func (h *Handler) GetAmpRestrictManagementToLocalhost(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"restrict-management-to-localhost": true})
		return
	}
	c.JSON(200, gin.H{"restrict-management-to-localhost": cfg.AmpCode.RestrictManagementToLocalhost})
}

// PutAmpRestrictManagementToLocalhost updates the localhost restriction setting.
func (h *Handler) PutAmpRestrictManagementToLocalhost(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.AmpCode.RestrictManagementToLocalhost = v })
}

// GetAmpModelMappings returns the ampcode model mappings.
func (h *Handler) GetAmpModelMappings(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"model-mappings": []config.AmpModelMapping{}})
		return
	}
	c.JSON(200, gin.H{"model-mappings": cfg.AmpCode.ModelMappings})
}

// PutAmpModelMappings replaces all ampcode model mappings.
func (h *Handler) PutAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mutateConfig(c, func(cfg *config.Config) {
		cfg.AmpCode.ModelMappings = body.Value
	})
}

// PatchAmpModelMappings adds or updates model mappings.
func (h *Handler) PatchAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	h.mutateConfig(c, func(cfg *config.Config) {
		existing := make(map[string]int)
		for i, m := range cfg.AmpCode.ModelMappings {
			existing[strings.TrimSpace(m.From)] = i
		}
		for _, newMapping := range body.Value {
			from := strings.TrimSpace(newMapping.From)
			if idx, ok := existing[from]; ok {
				cfg.AmpCode.ModelMappings[idx] = newMapping
			} else {
				cfg.AmpCode.ModelMappings = append(cfg.AmpCode.ModelMappings, newMapping)
				existing[from] = len(cfg.AmpCode.ModelMappings) - 1
			}
		}
	})
}

// DeleteAmpModelMappings removes specified model mappings by "from" field.
func (h *Handler) DeleteAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Value) == 0 {
		h.mutateConfig(c, func(cfg *config.Config) {
			cfg.AmpCode.ModelMappings = nil
		})
		return
	}

	toRemove := make(map[string]bool)
	for _, from := range body.Value {
		toRemove[strings.TrimSpace(from)] = true
	}

	h.mutateConfig(c, func(cfg *config.Config) {
		newMappings := make([]config.AmpModelMapping, 0, len(cfg.AmpCode.ModelMappings))
		for _, m := range cfg.AmpCode.ModelMappings {
			if !toRemove[strings.TrimSpace(m.From)] {
				newMappings = append(newMappings, m)
			}
		}
		cfg.AmpCode.ModelMappings = newMappings
	})
}

// GetAmpForceModelMappings returns whether model mappings are forced.
func (h *Handler) GetAmpForceModelMappings(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"force-model-mappings": false})
		return
	}
	c.JSON(200, gin.H{"force-model-mappings": cfg.AmpCode.ForceModelMappings})
}

// PutAmpForceModelMappings updates the force model mappings setting.
func (h *Handler) PutAmpForceModelMappings(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.AmpCode.ForceModelMappings = v })
}

// GetAmpUpstreamAPIKeys returns the ampcode upstream API keys mapping.
func (h *Handler) GetAmpUpstreamAPIKeys(c *gin.Context) {
	cfg := h.currentConfigSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"upstream-api-keys": []config.AmpUpstreamAPIKeyEntry{}})
		return
	}
	c.JSON(200, gin.H{"upstream-api-keys": cfg.AmpCode.UpstreamAPIKeys})
}

// PutAmpUpstreamAPIKeys replaces all ampcode upstream API keys mappings.
func (h *Handler) PutAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	// Normalize entries: trim whitespace, filter empty
	normalized := normalizeAmpUpstreamAPIKeyEntries(body.Value)
	h.mutateConfig(c, func(cfg *config.Config) {
		cfg.AmpCode.UpstreamAPIKeys = normalized
	})
}

// PatchAmpUpstreamAPIKeys adds or updates upstream API keys entries.
// Matching is done by upstream-api-key value.
func (h *Handler) PatchAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	h.mutateConfig(c, func(cfg *config.Config) {
		existing := make(map[string]int)
		for i, entry := range cfg.AmpCode.UpstreamAPIKeys {
			existing[strings.TrimSpace(entry.UpstreamAPIKey)] = i
		}
		for _, newEntry := range body.Value {
			upstreamKey := strings.TrimSpace(newEntry.UpstreamAPIKey)
			if upstreamKey == "" {
				continue
			}
			normalizedEntry := config.AmpUpstreamAPIKeyEntry{
				UpstreamAPIKey: upstreamKey,
				APIKeys:        normalizeAPIKeysList(newEntry.APIKeys),
			}
			if idx, ok := existing[upstreamKey]; ok {
				cfg.AmpCode.UpstreamAPIKeys[idx] = normalizedEntry
			} else {
				cfg.AmpCode.UpstreamAPIKeys = append(cfg.AmpCode.UpstreamAPIKeys, normalizedEntry)
				existing[upstreamKey] = len(cfg.AmpCode.UpstreamAPIKeys) - 1
			}
		}
	})
}

// DeleteAmpUpstreamAPIKeys removes specified upstream API keys entries.
// Body must be JSON: {"value": ["<upstream-api-key>", ...]}.
// If "value" is an empty array, clears all entries.
// If JSON is invalid or "value" is missing/null, returns 400 and does not persist any change.
func (h *Handler) DeleteAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	if body.Value == nil {
		c.JSON(400, gin.H{"error": "missing value"})
		return
	}

	// Empty array means clear all
	if len(body.Value) == 0 {
		h.mutateConfig(c, func(cfg *config.Config) {
			cfg.AmpCode.UpstreamAPIKeys = nil
		})
		return
	}

	toRemove := make(map[string]bool)
	for _, key := range body.Value {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		toRemove[trimmed] = true
	}
	if len(toRemove) == 0 {
		c.JSON(400, gin.H{"error": "empty value"})
		return
	}

	h.mutateConfig(c, func(cfg *config.Config) {
		newEntries := make([]config.AmpUpstreamAPIKeyEntry, 0, len(cfg.AmpCode.UpstreamAPIKeys))
		for _, entry := range cfg.AmpCode.UpstreamAPIKeys {
			if !toRemove[strings.TrimSpace(entry.UpstreamAPIKey)] {
				newEntries = append(newEntries, entry)
			}
		}
		cfg.AmpCode.UpstreamAPIKeys = newEntries
	})
}

// normalizeAmpUpstreamAPIKeyEntries normalizes a list of upstream API key entries.
func normalizeAmpUpstreamAPIKeyEntries(entries []config.AmpUpstreamAPIKeyEntry) []config.AmpUpstreamAPIKeyEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.AmpUpstreamAPIKeyEntry, 0, len(entries))
	for _, entry := range entries {
		upstreamKey := strings.TrimSpace(entry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		apiKeys := normalizeAPIKeysList(entry.APIKeys)
		out = append(out, config.AmpUpstreamAPIKeyEntry{
			UpstreamAPIKey: upstreamKey,
			APIKeys:        apiKeys,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeAPIKeysList trims and filters empty strings from a list of API keys.
func normalizeAPIKeysList(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
