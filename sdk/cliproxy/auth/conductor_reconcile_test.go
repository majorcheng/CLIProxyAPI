package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestReconcileRegistryModelStates_DoesNotClearRuntimeQuota429(t *testing.T) {
	authID := "quota-auth-reconcile"
	modelID := "gpt-5.4"
	now := time.Now()
	nextRetry := now.Add(30 * time.Minute)

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(authID)
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:                authID,
		Provider:          "codex",
		Status:            StatusError,
		StatusMessage:     "quota exhausted",
		Unavailable:       true,
		FailureHTTPStatus: http.StatusTooManyRequests,
		NextRetryAfter:    nextRetry,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: nextRetry,
			StrikeCount:   2,
		},
		ModelStates: map[string]*ModelState{
			modelID: {
				Status:            StatusError,
				StatusMessage:     "quota exhausted",
				Unavailable:       true,
				FailureHTTPStatus: http.StatusTooManyRequests,
				NextRetryAfter:    nextRetry,
				LastError: &Error{
					HTTPStatus: http.StatusTooManyRequests,
					Message:    `{"error":{"type":"usage_limit_reached","message":"quota"}}`,
				},
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: nextRetry,
					StrikeCount:   2,
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelID}})

	manager.ReconcileRegistryModelStates(context.Background(), authID)

	got, ok := manager.GetByID(authID)
	if !ok || got == nil {
		t.Fatalf("expected auth %q to remain registered", authID)
	}
	if got.Status != StatusError {
		t.Fatalf("auth status = %q, want %q", got.Status, StatusError)
	}
	if got.StatusMessage != "quota exhausted" {
		t.Fatalf("auth status_message = %q, want %q", got.StatusMessage, "quota exhausted")
	}
	if !got.Unavailable {
		t.Fatal("expected auth unavailable to remain true after reconcile")
	}
	if got.FailureHTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("auth FailureHTTPStatus = %d, want %d", got.FailureHTTPStatus, http.StatusTooManyRequests)
	}
	if !got.Quota.Exceeded {
		t.Fatal("expected auth quota exceeded to remain true after reconcile")
	}
	state := got.ModelStates[modelID]
	if state == nil {
		t.Fatalf("expected model state %q to remain present", modelID)
	}
	if state.Status != StatusError {
		t.Fatalf("model status = %q, want %q", state.Status, StatusError)
	}
	if state.StatusMessage != "quota exhausted" {
		t.Fatalf("model status_message = %q, want %q", state.StatusMessage, "quota exhausted")
	}
	if !state.Unavailable {
		t.Fatal("expected model unavailable to remain true after reconcile")
	}
	if state.FailureHTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("model FailureHTTPStatus = %d, want %d", state.FailureHTTPStatus, http.StatusTooManyRequests)
	}
	if !state.Quota.Exceeded {
		t.Fatal("expected model quota exceeded to remain true after reconcile")
	}
}

func TestReconcileRegistryModelStates_ClearsStaleModelSupportState(t *testing.T) {
	authID := "support-auth-reconcile"
	modelID := "gpt-5.4"
	now := time.Now()

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(authID)
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:            authID,
		Provider:      "codex",
		Status:        StatusError,
		StatusMessage: "requested model is not supported",
		Unavailable:   true,
		ModelStates: map[string]*ModelState{
			modelID: {
				Status:         StatusError,
				StatusMessage:  "requested model is not supported",
				Unavailable:    true,
				NextRetryAfter: now.Add(12 * time.Hour),
				LastError: &Error{
					HTTPStatus: http.StatusBadRequest,
					Message:    "requested model is not supported",
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelID}})

	manager.ReconcileRegistryModelStates(context.Background(), authID)

	got, ok := manager.GetByID(authID)
	if !ok || got == nil {
		t.Fatalf("expected auth %q to remain registered", authID)
	}
	if got.Status != StatusActive {
		t.Fatalf("auth status = %q, want %q", got.Status, StatusActive)
	}
	if got.StatusMessage != "" {
		t.Fatalf("auth status_message = %q, want empty", got.StatusMessage)
	}
	if got.Unavailable {
		t.Fatal("expected auth unavailable to be cleared after stale support state reconcile")
	}
	if got.FailureHTTPStatus != 0 {
		t.Fatalf("auth FailureHTTPStatus = %d, want 0", got.FailureHTTPStatus)
	}
	state := got.ModelStates[modelID]
	if state == nil {
		t.Fatalf("expected model state %q to remain present", modelID)
	}
	if state.Status != StatusActive {
		t.Fatalf("model status = %q, want %q", state.Status, StatusActive)
	}
	if state.StatusMessage != "" {
		t.Fatalf("model status_message = %q, want empty", state.StatusMessage)
	}
	if state.Unavailable {
		t.Fatal("expected model unavailable to be cleared after reconcile")
	}
	if state.FailureHTTPStatus != 0 {
		t.Fatalf("model FailureHTTPStatus = %d, want 0", state.FailureHTTPStatus)
	}
}
