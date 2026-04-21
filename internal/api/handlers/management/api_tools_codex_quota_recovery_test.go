package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestAPICall_CodexWeeklyQuotaRecoveryClearsQuotaCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		authID = "codex-plus-api-call-recovery"
		model  = "gpt-5.4"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:                authID,
		Provider:          "codex",
		Status:            coreauth.StatusError,
		StatusMessage:     "quota exhausted",
		Unavailable:       true,
		FailureHTTPStatus: 429,
		NextRetryAfter:    now.Add(30 * time.Minute),
		Quota:             coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(30 * time.Minute), BackoffLevel: 2, StrikeCount: 4},
		LastError:         &coreauth.Error{Message: "quota", HTTPStatus: 429},
		Metadata:          map[string]any{"type": "codex", "account_id": "acct_123"},
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:            coreauth.StatusError,
				StatusMessage:     "quota exhausted",
				Unavailable:       true,
				FailureHTTPStatus: 429,
				NextRetryAfter:    now.Add(30 * time.Minute),
				Quota:             coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(30 * time.Minute), BackoffLevel: 2, StrikeCount: 4},
				LastError:         &coreauth.Error{Message: "quota", HTTPStatus: 429},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg.SetModelQuotaExceeded(authID, model)
	reg.SuspendClientModel(authID, model, "quota")

	handler := &Handler{cfg: &config.Config{SDKConfig: sdkconfig.SDKConfig{}}, authManager: manager}
	requestBody := map[string]any{
		"auth_index": auth.EnsureIndex(),
		"method":     http.MethodGet,
		"url":        "https://chatgpt.com/backend-api/wham/usage",
		"header": map[string]string{
			"Chatgpt-Account-Id": "acct_123",
		},
	}
	responseBody := `{"plan_type":"plus","rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":60}}}`
	runQuotaRecoveryCheck(t, handler, requestBody, http.StatusOK, responseBody)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("updated.Status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.Unavailable {
		t.Fatal("updated.Unavailable = true, want false")
	}
	if updated.StatusMessage != "" {
		t.Fatalf("updated.StatusMessage = %q, want empty", updated.StatusMessage)
	}
	if updated.FailureHTTPStatus != 0 {
		t.Fatalf("updated.FailureHTTPStatus = %d, want 0", updated.FailureHTTPStatus)
	}
	if updated.LastError != nil {
		t.Fatalf("updated.LastError = %#v, want nil", updated.LastError)
	}
	if updated.Quota.Exceeded || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("updated quota state still present: quota=%#v next=%v", updated.Quota, updated.NextRetryAfter)
	}

	modelState := updated.ModelStates[model]
	if modelState == nil {
		t.Fatalf("updated.ModelStates[%q] = nil", model)
	}
	if modelState.Status != coreauth.StatusActive || modelState.Unavailable || modelState.FailureHTTPStatus != 0 {
		t.Fatalf("model state not cleared: %#v", modelState)
	}

	available := reg.GetAvailableModelsByProvider("codex")
	seen := map[string]bool{}
	for _, info := range available {
		if info != nil {
			seen[info.ID] = true
		}
	}
	if !seen[model] {
		t.Fatalf("expected %s to be available again, got %#v", model, seen)
	}
}

func TestAPICall_CodexWeeklyQuotaRecoveryThresholdKeepsCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		authID = "codex-plus-api-call-threshold"
		model  = "gpt-5.4"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

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
		Metadata:          map[string]any{"type": "codex", "account_id": "acct_456"},
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:            coreauth.StatusError,
				StatusMessage:     "quota exhausted",
				Unavailable:       true,
				FailureHTTPStatus: 429,
				NextRetryAfter:    now.Add(20 * time.Minute),
				Quota:             coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(20 * time.Minute)},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	handler := &Handler{authManager: manager}
	requestBody := map[string]any{
		"auth_index": auth.EnsureIndex(),
		"method":     http.MethodGet,
		"url":        "https://chatgpt.com/backend-api/wham/usage",
		"header": map[string]string{
			"Chatgpt-Account-Id": "acct_456",
		},
	}
	responseBody := `{"plan_type":"plus","rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":70}}}`
	runQuotaRecoveryCheck(t, handler, requestBody, http.StatusOK, responseBody)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.FailureHTTPStatus != 429 {
		t.Fatalf("updated.FailureHTTPStatus = %d, want 429", updated.FailureHTTPStatus)
	}
	if updated.StatusMessage != "quota exhausted" {
		t.Fatalf("updated.StatusMessage = %q, want %q", updated.StatusMessage, "quota exhausted")
	}
	if !updated.Quota.Exceeded {
		t.Fatalf("updated.Quota.Exceeded = false, want true")
	}
}

func TestAPICall_CodexWeeklyQuotaRecoveryDoesNotClearMixedUnauthorizedState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		authID = "codex-mixed-api-call-unauthorized"
		model  = "gpt-5.4"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	manager := coreauth.NewManager(nil, nil, nil)
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:                authID,
		Provider:          "codex",
		Status:            coreauth.StatusError,
		StatusMessage:     "unauthorized",
		Unavailable:       true,
		FailureHTTPStatus: 401,
		NextRetryAfter:    now.Add(30 * time.Minute),
		Quota:             coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(25 * time.Minute), BackoffLevel: 1, StrikeCount: 2},
		LastError:         &coreauth.Error{Message: "unauthorized", HTTPStatus: 401},
		Metadata:          map[string]any{"type": "codex", "account_id": "acct_789"},
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status:            coreauth.StatusError,
				StatusMessage:     "unauthorized",
				Unavailable:       true,
				FailureHTTPStatus: 401,
				NextRetryAfter:    now.Add(30 * time.Minute),
				Quota:             coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(25 * time.Minute), BackoffLevel: 1, StrikeCount: 2},
				LastError:         &coreauth.Error{Message: "unauthorized", HTTPStatus: 401},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg.SuspendClientModel(authID, model, "unauthorized")

	handler := &Handler{authManager: manager}
	requestBody := map[string]any{
		"auth_index": auth.EnsureIndex(),
		"method":     http.MethodGet,
		"url":        "https://chatgpt.com/backend-api/wham/usage",
		"header": map[string]string{
			"Chatgpt-Account-Id": "acct_789",
		},
	}
	responseBody := `{"plan_type":"plus","rate_limit":{"secondary_window":{"limit_window_seconds":604800,"used_percent":60}}}`
	runQuotaRecoveryCheck(t, handler, requestBody, http.StatusOK, responseBody)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.FailureHTTPStatus != 401 || updated.StatusMessage != "unauthorized" {
		t.Fatalf("updated auth = %#v, want unauthorized retained", updated)
	}
	state := updated.ModelStates[model]
	if state == nil || state.FailureHTTPStatus != 401 || state.StatusMessage != "unauthorized" {
		t.Fatalf("updated model state = %#v, want unauthorized retained", state)
	}
}

func runQuotaRecoveryCheck(t *testing.T, handler *Handler, requestBody map[string]any, upstreamStatus int, upstreamBody string) {
	t.Helper()
	rawBody, errMarshal := json.Marshal(requestBody)
	if errMarshal != nil {
		t.Fatalf("json.Marshal request body: %v", errMarshal)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	var body apiCallRequest
	if err := json.Unmarshal(rawBody, &body); err != nil {
		t.Fatalf("json.Unmarshal request body: %v", err)
	}
	parsedURL, err := url.Parse(body.URL)
	if err != nil {
		t.Fatalf("url parse: %v", err)
	}
	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := handler.authByIndex(authIndex)
	handler.maybeRecoverCodexQuotaCooldown(ctx.Request.Context(), auth, body, parsedURL, upstreamStatus, []byte(upstreamBody))
}
