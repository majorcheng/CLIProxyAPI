package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestOAuthSessionErrorWithCauseIncludesCause(t *testing.T) {
	got := oauthSessionErrorWithCause("Failed to exchange token", errors.New("invalid_grant"))
	want := "Failed to exchange token: invalid_grant"
	if got != want {
		t.Fatalf("oauthSessionErrorWithCause = %q, want %q", got, want)
	}
}

func TestPostOAuthCallbackReturnsStoredSessionStatus(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	resetOAuthSessionsForTest(t)

	const state = "state-callback-error"
	RegisterOAuthSession(state, "codex")
	SetOAuthSessionError(state, "Failed to exchange token: invalid_grant")

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	body := map[string]string{"provider": "codex", "state": state, "code": "code"}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/oauth/callback", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PostOAuthCallback(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["error"] != "Failed to exchange token: invalid_grant" {
		t.Fatalf("error = %q, want stored status", response["error"])
	}
}

func resetOAuthSessionsForTest(t *testing.T) {
	t.Helper()
	previous := oauthSessions
	oauthSessions = newOAuthSessionStore(oauthSessionTTL)
	t.Cleanup(func() {
		oauthSessions = previous
	})
}
