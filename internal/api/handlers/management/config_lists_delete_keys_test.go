package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func writeTestConfigFile(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if errWrite := os.WriteFile(path, []byte("{}\n"), 0o600); errWrite != nil {
		t.Fatalf("failed to write test config: %v", errWrite)
	}
	return path
}

func TestAPIKeysManagement_DefaultsToLegacyStringsButSupportsObjectFormatAndPatchDelete(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	configPath := writeTestConfigFile(t)
	h := &Handler{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{
				APIKeys: []string{"alpha", "beta"},
			},
		},
		configFilePath: configPath,
	}
	h.cfg.SetClientAPIKeyEntries([]config.ClientAPIKey{
		{Key: "alpha"},
		{Key: "beta", MaxPriority: intPtr(0)},
	})

	// GET legacy output
	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/api-keys", nil)
	h.GetAPIKeys(getCtx)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getRec.Code)
	}
	if !strings.Contains(getRec.Body.String(), `"api-keys":["alpha","beta"]`) {
		t.Fatalf("GET body = %s, want legacy string api-keys shape", getRec.Body.String())
	}

	// GET object output
	getObjRec := httptest.NewRecorder()
	getObjCtx, _ := gin.CreateTestContext(getObjRec)
	reqObj := httptest.NewRequest(http.MethodGet, "/v0/management/api-keys?format=object", nil)
	reqObj.Header.Set("Accept", "application/vnd.router-for-me.apikeys+json")
	getObjCtx.Request = reqObj
	h.GetAPIKeys(getObjCtx)
	if getObjRec.Code != http.StatusOK {
		t.Fatalf("GET object status = %d, want 200", getObjRec.Code)
	}
	if !strings.Contains(getObjRec.Body.String(), `"key":"beta"`) || !strings.Contains(getObjRec.Body.String(), `"max-priority":0`) {
		t.Fatalf("GET object body = %s, want object api-keys shape", getObjRec.Body.String())
	}

	// PATCH edit by index
	editRec := httptest.NewRecorder()
	editCtx, _ := gin.CreateTestContext(editRec)
	editReq := httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", bytes.NewBufferString(`{"index":1,"value":{"key":"beta","max-priority":3}}`))
	editReq.Header.Set("Content-Type", "application/json")
	editCtx.Request = editReq
	h.PatchAPIKeys(editCtx)
	if editRec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body=%s", editRec.Code, editRec.Body.String())
	}
	entries := h.cfg.ClientAPIKeyEntries()
	if entries[1].MaxPriority == nil || *entries[1].MaxPriority != 3 {
		t.Fatalf("entries[1].MaxPriority = %v, want 3", entries[1].MaxPriority)
	}

	// DELETE by key
	delRec := httptest.NewRecorder()
	delCtx, _ := gin.CreateTestContext(delRec)
	delCtx.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/api-keys?key=alpha", nil)
	h.DeleteAPIKeys(delCtx)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200; body=%s", delRec.Code, delRec.Body.String())
	}
	if len(h.cfg.APIKeys) != 1 || h.cfg.APIKeys[0] != "beta" {
		t.Fatalf("remaining api-keys = %#v, want [beta]", h.cfg.APIKeys)
	}
}

func TestAPIKeysManagement_PatchRejectsDuplicateKeyConflict(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	configPath := writeTestConfigFile(t)
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
	}
	h.cfg.SetClientAPIKeyEntries([]config.ClientAPIKey{
		{Key: "alpha"},
		{Key: "beta", MaxPriority: intPtr(0)},
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/api-keys", bytes.NewBufferString(`{"index":1,"value":{"key":"alpha","max-priority":3}}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchAPIKeys(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ClientAPIKeyEntries()) != 2 {
		t.Fatalf("entries len = %d, want 2", len(h.cfg.ClientAPIKeyEntries()))
	}
	if h.cfg.ClientAPIKeyEntries()[1].Key != "beta" {
		t.Fatalf("entries[1].Key = %q, want beta", h.cfg.ClientAPIKeyEntries()[1].Key)
	}
}

func TestAPIKeysManagement_PutAcceptsObjectInputButKeepsLegacyStorageView(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	configPath := writeTestConfigFile(t)
	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configPath,
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPut, "/v0/management/api-keys", bytes.NewBufferString(`[{"key":"alpha"},{"key":"beta","max-priority":2}]`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PutAPIKeys(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := []string(h.cfg.APIKeys); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("legacy APIKeys view = %#v, want [alpha beta]", got)
	}
	entries := h.cfg.ClientAPIKeyEntries()
	if len(entries) != 2 || entries[1].MaxPriority == nil || *entries[1].MaxPriority != 2 {
		t.Fatalf("client api key entries = %#v, want beta max-priority=2", entries)
	}

}

func intPtr(v int) *int { return &v }

func TestDeleteGeminiKey_RequiresBaseURLWhenAPIKeyDuplicated(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg: &config.Config{
			GeminiKey: []config.GeminiKey{
				{APIKey: "shared-key", BaseURL: "https://a.example.com"},
				{APIKey: "shared-key", BaseURL: "https://b.example.com"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/gemini-api-key?api-key=shared-key", nil)

	h.DeleteGeminiKey(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := len(h.cfg.GeminiKey); got != 2 {
		t.Fatalf("gemini keys len = %d, want 2", got)
	}
}

func TestDeleteGeminiKey_DeletesOnlyMatchingBaseURL(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg: &config.Config{
			GeminiKey: []config.GeminiKey{
				{APIKey: "shared-key", BaseURL: "https://a.example.com"},
				{APIKey: "shared-key", BaseURL: "https://b.example.com"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/gemini-api-key?api-key=shared-key&base-url=https://a.example.com", nil)

	h.DeleteGeminiKey(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := len(h.cfg.GeminiKey); got != 1 {
		t.Fatalf("gemini keys len = %d, want 1", got)
	}
	if got := h.cfg.GeminiKey[0].BaseURL; got != "https://b.example.com" {
		t.Fatalf("remaining base-url = %q, want %q", got, "https://b.example.com")
	}
}

func TestDeleteClaudeKey_DeletesEmptyBaseURLWhenExplicitlyProvided(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "shared-key", BaseURL: ""},
				{APIKey: "shared-key", BaseURL: "https://claude.example.com"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/claude-api-key?api-key=shared-key&base-url=", nil)

	h.DeleteClaudeKey(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := len(h.cfg.ClaudeKey); got != 1 {
		t.Fatalf("claude keys len = %d, want 1", got)
	}
	if got := h.cfg.ClaudeKey[0].BaseURL; got != "https://claude.example.com" {
		t.Fatalf("remaining base-url = %q, want %q", got, "https://claude.example.com")
	}
}

func TestDeleteVertexCompatKey_DeletesOnlyMatchingBaseURL(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "shared-key", BaseURL: "https://a.example.com"},
				{APIKey: "shared-key", BaseURL: "https://b.example.com"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/vertex-api-key?api-key=shared-key&base-url=https://b.example.com", nil)

	h.DeleteVertexCompatKey(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := len(h.cfg.VertexCompatAPIKey); got != 1 {
		t.Fatalf("vertex keys len = %d, want 1", got)
	}
	if got := h.cfg.VertexCompatAPIKey[0].BaseURL; got != "https://a.example.com" {
		t.Fatalf("remaining base-url = %q, want %q", got, "https://a.example.com")
	}
}

func TestDeleteCodexKey_RequiresBaseURLWhenAPIKeyDuplicated(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	h := &Handler{
		cfg: &config.Config{
			CodexKey: []config.CodexKey{
				{APIKey: "shared-key", BaseURL: "https://a.example.com"},
				{APIKey: "shared-key", BaseURL: "https://b.example.com"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/codex-api-key?api-key=shared-key", nil)

	h.DeleteCodexKey(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := len(h.cfg.CodexKey); got != 2 {
		t.Fatalf("codex keys len = %d, want 2", got)
	}
}
