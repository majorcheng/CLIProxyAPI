package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFilesIncludesProjectIDFromLiveAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	path := filepath.Join(authDir, "gemini.json")
	if err := os.WriteFile(path, []byte(`{"type":"gemini","project_id":"file-project"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "gemini.json",
		FileName: "gemini.json",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":                   path,
			"gemini_virtual_project": "virtual-project",
		},
		Metadata: map[string]any{"type": "gemini"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	file := listSingleAuthFileForTest(t, NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager))
	if file["project_id"] != "virtual-project" {
		t.Fatalf("project_id = %#v, want %q", file["project_id"], "virtual-project")
	}
}

func TestListAuthFilesFromDiskIncludesProjectID(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	path := filepath.Join(authDir, "gemini.json")
	if err := os.WriteFile(path, []byte(`{"type":"gemini","project_id":"disk-project"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	file := listSingleAuthFileForTest(t, NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil))
	if file["project_id"] != "disk-project" {
		t.Fatalf("project_id = %#v, want %q", file["project_id"], "disk-project")
	}
}

func listSingleAuthFileForTest(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files len = %d, want 1; body=%s", len(payload.Files), rec.Body.String())
	}
	return payload.Files[0]
}
