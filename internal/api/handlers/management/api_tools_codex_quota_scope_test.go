package management

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestIsCodexUsageRefreshRequest_StrictScope(t *testing.T) {
	auth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct_123"}}
	goodURL, _ := url.Parse("https://chatgpt.com/backend-api/wham/usage")
	badHostURL, _ := url.Parse("https://mock.local/backend-api/wham/usage")
	badPathURL, _ := url.Parse("https://chatgpt.com/backend-api/other")

	goodReq := apiCallRequest{Method: http.MethodGet, Header: map[string]string{"Chatgpt-Account-Id": "acct_123"}}
	if !isCodexUsageRefreshRequest(auth, goodReq, goodURL) {
		t.Fatal("expected official codex usage refresh request to match")
	}
	if isCodexUsageRefreshRequest(auth, goodReq, badHostURL) {
		t.Fatal("expected non-chatgpt host to be rejected")
	}
	if isCodexUsageRefreshRequest(auth, goodReq, badPathURL) {
		t.Fatal("expected non-usage path to be rejected")
	}
	if isCodexUsageRefreshRequest(auth, apiCallRequest{Method: http.MethodGet}, goodURL) {
		t.Fatal("expected missing account header to be rejected")
	}
	if isCodexUsageRefreshRequest(auth, apiCallRequest{Method: http.MethodGet, Header: map[string]string{"Chatgpt-Account-Id": "acct_other"}}, goodURL) {
		t.Fatal("expected mismatched account header to be rejected")
	}
}

func TestAPICall_CodexWeeklyQuotaRecoveryRejectsUnofficialHost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const authID = "codex-plus-api-call-host-scope"
	manager := coreauth.NewManager(nil, nil, nil)
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:                authID,
		Provider:          "codex",
		Status:            coreauth.StatusError,
		StatusMessage:     "quota exhausted",
		Unavailable:       true,
		FailureHTTPStatus: 429,
		NextRetryAfter:    now.Add(20 * time.Minute),
		Quota:             coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(20 * time.Minute)},
		Metadata:          map[string]any{"type": "codex", "account_id": "acct_654"},
	}
	if _, err := manager.Register(nil, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	handler := &Handler{authManager: manager}
	requestBody := map[string]any{
		"auth_index": auth.EnsureIndex(),
		"method":     http.MethodGet,
		"url":        "https://mock.local/backend-api/wham/usage",
		"header": map[string]string{
			"Chatgpt-Account-Id": "acct_654",
		},
	}
	responseBody := `{"plan_type":"plus","rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":60}}}`
	runQuotaRecoveryCheck(t, handler, requestBody, http.StatusOK, responseBody)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.FailureHTTPStatus != 429 {
		t.Fatalf("updated.FailureHTTPStatus = %d, want 429 retained", updated.FailureHTTPStatus)
	}
}
