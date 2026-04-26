package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func newOpenAICompatTestHandler(t *testing.T, yamlContent string) (*Handler, string) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return NewHandler(cfg, configPath, nil), configPath
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, rec.Body.String())
	}
	return payload
}

func TestGetOpenAICompatIncludesRevision(t *testing.T) {
	h, _ := newOpenAICompatTestHandler(t, ""+
		"openai-compatibility:\n"+
		"  - name: \"alpha\"\n"+
		"    base-url: \"https://alpha.example.com\"\n")

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	ctx.Request = req

	h.GetOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	payload := decodeJSONBody(t, rec)
	if _, ok := payload["revision"].(string); !ok {
		t.Fatalf("expected revision string, got %T", payload["revision"])
	}
}

func TestDeleteOpenAICompatReturns404WhenNotFound(t *testing.T) {
	h, configPath := newOpenAICompatTestHandler(t, ""+
		"openai-compatibility:\n"+
		"  - name: \"alpha\"\n"+
		"    base-url: \"https://alpha.example.com\"\n")

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodDelete, "/v0/management/openai-compatibility", bytes.NewBufferString(`{"name":"missing"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.DeleteOpenAICompat(ctx)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Contains(data, []byte("name: \"alpha\"")) {
		t.Fatalf("config unexpectedly changed:\n%s", string(data))
	}
}

func TestPatchOpenAICompatAppliesLatestListAndCallback(t *testing.T) {
	h, configPath := newOpenAICompatTestHandler(t, ""+
		"openai-compatibility:\n"+
		"  - name: \"alpha\"\n"+
		"    base-url: \"https://alpha.example.com\"\n")

	recGet := httptest.NewRecorder()
	ctxGet, _ := gin.CreateTestContext(recGet)
	reqGet := httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	ctxGet.Request = reqGet
	h.GetOpenAICompat(ctxGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("initial get status = %d, body=%s", recGet.Code, recGet.Body.String())
	}
	initialPayload := decodeJSONBody(t, recGet)
	revision, _ := initialPayload["revision"].(string)
	if revision == "" {
		t.Fatal("expected initial revision")
	}

	var applied *config.Config
	h.SetConfigApplied(func(cfg *config.Config) {
		applied = cfg
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", bytes.NewBufferString(`{
	  "revision":"`+revision+`",
	  "matchName":"alpha",
	  "value":{"name":"Alpha HK","base-url":"https://alpha.example.com"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	payload := decodeJSONBody(t, rec)
	items, ok := payload["openai-compatibility"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected 1 openai item, got %#v", payload["openai-compatibility"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected item type: %T", items[0])
	}
	if got := item["name"]; got != "Alpha HK" {
		t.Fatalf("name = %#v, want %q", got, "Alpha HK")
	}
	if disabled, ok := item["disabled"].(bool); !ok || disabled {
		t.Fatalf("disabled = %#v, want false", item["disabled"])
	}
	if applied == nil {
		t.Fatal("expected configApplied callback to run")
	}
	if len(applied.OpenAICompatibility) != 1 || applied.OpenAICompatibility[0].Name != "Alpha HK" {
		t.Fatalf("applied config not updated: %+v", applied.OpenAICompatibility)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Contains(data, []byte("name: Alpha HK")) {
		t.Fatalf("persisted config not updated:\n%s", string(data))
	}
}

func TestPostOpenAICompatRevisionConflict(t *testing.T) {
	h, configPath := newOpenAICompatTestHandler(t, ""+
		"openai-compatibility:\n"+
		"  - name: \"alpha\"\n"+
		"    base-url: \"https://alpha.example.com\"\n")

	recGet := httptest.NewRecorder()
	ctxGet, _ := gin.CreateTestContext(recGet)
	reqGet := httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	ctxGet.Request = reqGet
	h.GetOpenAICompat(ctxGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("initial get status = %d, body=%s", recGet.Code, recGet.Body.String())
	}
	initialPayload := decodeJSONBody(t, recGet)
	revision, _ := initialPayload["revision"].(string)
	if revision == "" {
		t.Fatal("expected initial revision")
	}

	if err := os.WriteFile(configPath, []byte(
		"openai-compatibility:\n  - name: \"beta\"\n    base-url: \"https://beta.example.com\"\n",
	), 0o600); err != nil {
		t.Fatalf("overwrite config: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/openai-compatibility", bytes.NewBufferString(`{
	  "revision":"`+revision+`",
	  "value":{"name":"gamma","base-url":"https://gamma.example.com"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PostOpenAICompat(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestPatchOpenAICompatCanToggleDisabled(t *testing.T) {
	h, configPath := newOpenAICompatTestHandler(t, ""+
		"openai-compatibility:\n"+
		"  - name: \"alpha\"\n"+
		"    base-url: \"https://alpha.example.com\"\n")

	recGet := httptest.NewRecorder()
	ctxGet, _ := gin.CreateTestContext(recGet)
	ctxGet.Request = httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	h.GetOpenAICompat(ctxGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("initial get status = %d, body=%s", recGet.Code, recGet.Body.String())
	}
	revision, _ := decodeJSONBody(t, recGet)["revision"].(string)
	if revision == "" {
		t.Fatal("expected initial revision")
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", bytes.NewBufferString(`{
	  "revision":"`+revision+`",
	  "matchName":"alpha",
	  "value":{"disabled":true}
	}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.PatchOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	payload := decodeJSONBody(t, rec)
	items, ok := payload["openai-compatibility"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected 1 openai item, got %#v", payload["openai-compatibility"])
	}
	item := items[0].(map[string]any)
	if disabled, ok := item["disabled"].(bool); !ok || !disabled {
		t.Fatalf("disabled = %#v, want true", item["disabled"])
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Contains(data, []byte("disabled: true")) {
		t.Fatalf("persisted config missing disabled flag:\n%s", string(data))
	}
}
