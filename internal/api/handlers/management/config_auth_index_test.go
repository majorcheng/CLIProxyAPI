package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func registerConfigSynthesizedAuths(t *testing.T, manager *coreauth.Manager, cfg *config.Config) {
	t.Helper()
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("synthesize auths: %v", err)
	}
	for _, auth := range auths {
		if _, err = manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}
}

func decodeObjectListField(t *testing.T, rec *httptest.ResponseRecorder, key string) []map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, rec.Body.String())
	}
	raw, ok := payload[key].([]any)
	if !ok {
		t.Fatalf("%s type = %T, body=%s", key, payload[key], rec.Body.String())
	}
	out := make([]map[string]any, len(raw))
	for i := range raw {
		item, ok := raw[i].(map[string]any)
		if !ok {
			t.Fatalf("%s[%d] type = %T", key, i, raw[i])
		}
		out[i] = item
	}
	return out
}

func TestGetProviderKeysIncludesStableAuthIndex(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "gem-a", BaseURL: "https://gem.example.com"},
		},
		ClaudeKey: []config.ClaudeKey{
			{APIKey: "claude-a", BaseURL: "https://claude.example.com"},
		},
		CodexKey: []config.CodexKey{
			{APIKey: "codex-a", BaseURL: "https://codex.example.com"},
		},
		VertexCompatAPIKey: []config.VertexCompatKey{
			{APIKey: "vertex-a", BaseURL: "https://vertex.example.com", ProxyURL: "http://127.0.0.1:1080"},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registerConfigSynthesizedAuths(t, manager, cfg)
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	tests := []struct {
		name         string
		path         string
		call         func(*Handler, *gin.Context)
		responseKey  string
		expectedKind string
	}{{
		name:         "gemini",
		path:         "/v0/management/gemini-api-key",
		call:         (*Handler).GetGeminiKeys,
		responseKey:  "gemini-api-key",
		expectedKind: "gemini:apikey",
	}, {
		name:         "claude",
		path:         "/v0/management/claude-api-key",
		call:         (*Handler).GetClaudeKeys,
		responseKey:  "claude-api-key",
		expectedKind: "claude:apikey",
	}, {
		name:         "codex",
		path:         "/v0/management/codex-api-key",
		call:         (*Handler).GetCodexKeys,
		responseKey:  "codex-api-key",
		expectedKind: "codex:apikey",
	}, {
		name:         "vertex",
		path:         "/v0/management/vertex-api-key",
		call:         (*Handler).GetVertexCompatKeys,
		responseKey:  "vertex-api-key",
		expectedKind: "vertex:apikey",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, tc.path, nil)

			tc.call(h, ctx)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			items := decodeObjectListField(t, rec, tc.responseKey)
			if len(items) != 1 {
				t.Fatalf("items len = %d, want 1", len(items))
			}
			gotIndex, _ := items[0]["auth-index"].(string)
			if gotIndex == "" {
				t.Fatalf("auth-index missing in %s response: %v", tc.responseKey, items[0])
			}
			if _, ok := items[0]["api-key"].(string); !ok {
				t.Fatalf("api-key missing in %s response: %v", tc.responseKey, items[0])
			}
		})
	}
}

func TestGetOpenAICompatIncludesEntryAndFallbackAuthIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "Alpha Compat",
				BaseURL: "https://alpha.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "alpha-k1", ProxyURL: "http://127.0.0.1:8001"},
					{APIKey: "alpha-k2"},
				},
			},
			{
				Name:    "Beta Compat",
				BaseURL: "https://beta.example.com",
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	registerConfigSynthesizedAuths(t, manager, cfg)
	h, configPath := newOpenAICompatTestHandler(t, ""+
		"openai-compatibility:\n"+
		"  - name: \"Alpha Compat\"\n"+
		"    base-url: \"https://alpha.example.com\"\n"+
		"    api-key-entries:\n"+
		"      - api-key: \"alpha-k1\"\n"+
		"        proxy-url: \"http://127.0.0.1:8001\"\n"+
		"      - api-key: \"alpha-k2\"\n"+
		"  - name: \"Beta Compat\"\n"+
		"    base-url: \"https://beta.example.com\"\n")
	cfgFromDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	h.SetConfig(cfgFromDisk)
	h.SetAuthManager(manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)

	h.GetOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err = json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, rec.Body.String())
	}
	if _, ok := payload["revision"].(string); !ok {
		t.Fatalf("revision missing: %#v", payload)
	}
	items, ok := payload["openai-compatibility"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("openai-compatibility items = %#v", payload["openai-compatibility"])
	}
	alpha := items[0].(map[string]any)
	entries, ok := alpha["api-key-entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("alpha api-key-entries = %#v", alpha["api-key-entries"])
	}
	for i := range entries {
		entry := entries[i].(map[string]any)
		if got, _ := entry["auth-index"].(string); got == "" {
			t.Fatalf("alpha api-key-entries[%d] missing auth-index: %#v", i, entry)
		}
	}
	beta := items[1].(map[string]any)
	if got, _ := beta["auth-index"].(string); got == "" {
		t.Fatalf("beta fallback auth-index missing: %#v", beta)
	}
}

func TestPatchOpenAICompatResponseIncludesAuthIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h, _ := newOpenAICompatTestHandler(t, ""+
		"openai-compatibility:\n"+
		"  - name: \"Alpha Compat\"\n"+
		"    base-url: \"https://alpha.example.com\"\n"+
		"    api-key-entries:\n"+
		"      - api-key: \"alpha-k1\"\n"+
		"      - api-key: \"alpha-k2\"\n")

	cfg := h.currentConfigSnapshot()
	manager := coreauth.NewManager(nil, nil, nil)
	registerConfigSynthesizedAuths(t, manager, cfg)
	h.SetAuthManager(manager)

	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	h.GetOpenAICompat(getCtx)
	if getRec.Code != http.StatusOK {
		t.Fatalf("initial get status = %d, body=%s", getRec.Code, getRec.Body.String())
	}
	revision, _ := decodeJSONBody(t, getRec)["revision"].(string)
	if revision == "" {
		t.Fatal("expected revision from initial GET")
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", bytes.NewBufferString(`{
	  "revision":"`+revision+`",
	  "matchName":"Alpha Compat",
	  "value":{"priority":7}
	}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeJSONBody(t, rec)
	items, ok := payload["openai-compatibility"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("openai-compatibility items = %#v", payload["openai-compatibility"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected response item type: %T", items[0])
	}
	if got, _ := item["priority"].(float64); got != 7 {
		t.Fatalf("priority = %v, want 7", got)
	}
	entries, ok := item["api-key-entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("api-key-entries = %#v", item["api-key-entries"])
	}
	for i := range entries {
		entry := entries[i].(map[string]any)
		if got, _ := entry["auth-index"].(string); got == "" {
			t.Fatalf("entry[%d] missing auth-index: %#v", i, entry)
		}
	}
}

func TestGetGeminiKeysSkipsAuthIndexWhenLiveManagerNotReady(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{{APIKey: "gem-a", BaseURL: "https://gem.example.com"}},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/gemini-api-key", nil)

	h.GetGeminiKeys(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	items := decodeObjectListField(t, rec, "gemini-api-key")
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	if _, ok := items[0]["auth-index"]; ok {
		t.Fatalf("unexpected auth-index without live manager: %#v", items[0])
	}
}
