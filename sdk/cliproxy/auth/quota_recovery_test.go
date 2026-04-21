package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestClearAuthQuotaCooldown_ClearsAuthAndModelQuotaState(t *testing.T) {
	const (
		authID = "codex-plus-quota-recovery"
		modelA = "gpt-5.4"
		modelB = "gpt-5.4-mini"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	now := time.Now().UTC()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.auths[authID] = &Auth{
		ID:                authID,
		Provider:          "codex",
		Status:            StatusError,
		StatusMessage:     "quota exhausted",
		Unavailable:       true,
		FailureHTTPStatus: 429,
		NextRetryAfter:    now.Add(30 * time.Minute),
		Quota:             QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(30 * time.Minute), BackoffLevel: 2, StrikeCount: 5},
		LastError:         &Error{Message: "quota", HTTPStatus: 429},
		ModelStates: map[string]*ModelState{
			modelA: {
				Status:            StatusError,
				StatusMessage:     "quota exhausted",
				Unavailable:       true,
				FailureHTTPStatus: 429,
				NextRetryAfter:    now.Add(30 * time.Minute),
				Quota:             QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(30 * time.Minute), BackoffLevel: 1, StrikeCount: 3},
				LastError:         &Error{Message: "quota", HTTPStatus: 429},
			},
			modelB: {Status: StatusActive},
		},
	}
	reg.SetModelQuotaExceeded(authID, modelA)
	reg.SuspendClientModel(authID, modelA, "quota")

	updated, changed, err := manager.ClearAuthQuotaCooldown(context.Background(), authID)
	if err != nil {
		t.Fatalf("ClearAuthQuotaCooldown() error = %v", err)
	}
	if !changed {
		t.Fatal("ClearAuthQuotaCooldown() changed = false, want true")
	}
	if updated == nil {
		t.Fatal("ClearAuthQuotaCooldown() returned nil auth")
	}
	if updated.Status != StatusActive {
		t.Fatalf("updated.Status = %q, want %q", updated.Status, StatusActive)
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
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("updated.NextRetryAfter = %v, want zero", updated.NextRetryAfter)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("updated.Quota = %#v, want cleared quota state", updated.Quota)
	}

	state := updated.ModelStates[modelA]
	if state == nil {
		t.Fatalf("updated.ModelStates[%q] = nil", modelA)
	}
	if state.Status != StatusActive {
		t.Fatalf("modelA status = %q, want %q", state.Status, StatusActive)
	}
	if state.Unavailable {
		t.Fatal("modelA unavailable = true, want false")
	}
	if state.StatusMessage != "" {
		t.Fatalf("modelA status message = %q, want empty", state.StatusMessage)
	}
	if state.FailureHTTPStatus != 0 {
		t.Fatalf("modelA http status = %d, want 0", state.FailureHTTPStatus)
	}
	if state.LastError != nil {
		t.Fatalf("modelA last error = %#v, want nil", state.LastError)
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("modelA next retry = %v, want zero", state.NextRetryAfter)
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("modelA quota = %#v, want cleared", state.Quota)
	}

	if blocked, reason, next := isAuthBlockedForModel(updated, modelA, time.Now()); blocked {
		t.Fatalf("expected cleared auth to be selectable again, blocked=%v reason=%v next=%v", blocked, reason, next)
	}
}

func TestClearAuthQuotaCooldown_PreservesNonQuotaErrors(t *testing.T) {
	const authID = "codex-plus-unauthorized"

	manager := NewManager(nil, nil, nil)
	now := time.Now().UTC()
	manager.auths[authID] = &Auth{
		ID:                authID,
		Provider:          "codex",
		Status:            StatusError,
		StatusMessage:     "unauthorized",
		Unavailable:       true,
		FailureHTTPStatus: 401,
		NextRetryAfter:    now.Add(30 * time.Minute),
		LastError:         &Error{Message: "unauthorized", HTTPStatus: 401},
	}

	updated, changed, err := manager.ClearAuthQuotaCooldown(context.Background(), authID)
	if err != nil {
		t.Fatalf("ClearAuthQuotaCooldown() error = %v", err)
	}
	if changed {
		t.Fatal("ClearAuthQuotaCooldown() changed = true, want false")
	}
	if updated == nil {
		t.Fatal("ClearAuthQuotaCooldown() returned nil auth")
	}
	if updated.FailureHTTPStatus != 401 {
		t.Fatalf("updated.FailureHTTPStatus = %d, want 401", updated.FailureHTTPStatus)
	}
	if updated.StatusMessage != "unauthorized" {
		t.Fatalf("updated.StatusMessage = %q, want %q", updated.StatusMessage, "unauthorized")
	}
}

func TestClearAuthQuotaCooldown_DoesNotClearMixedUnauthorizedState(t *testing.T) {
	const (
		authID = "codex-mixed-unauthorized"
		model  = "gpt-5.4"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	now := time.Now().UTC()
	manager := NewManager(nil, nil, nil)
	manager.auths[authID] = &Auth{
		ID:                authID,
		Provider:          "codex",
		Status:            StatusError,
		StatusMessage:     "unauthorized",
		Unavailable:       true,
		FailureHTTPStatus: 401,
		NextRetryAfter:    now.Add(30 * time.Minute),
		Quota:             QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(25 * time.Minute), BackoffLevel: 2, StrikeCount: 4},
		LastError:         &Error{Message: "unauthorized", HTTPStatus: 401},
		ModelStates: map[string]*ModelState{
			model: {
				Status:            StatusError,
				StatusMessage:     "unauthorized",
				Unavailable:       true,
				FailureHTTPStatus: 401,
				NextRetryAfter:    now.Add(30 * time.Minute),
				Quota:             QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(25 * time.Minute), BackoffLevel: 1, StrikeCount: 2},
				LastError:         &Error{Message: "unauthorized", HTTPStatus: 401},
			},
		},
	}
	reg.SetModelQuotaExceeded(authID, model)
	reg.SuspendClientModel(authID, model, "unauthorized")

	updated, changed, err := manager.ClearAuthQuotaCooldown(context.Background(), authID)
	if err != nil {
		t.Fatalf("ClearAuthQuotaCooldown() error = %v", err)
	}
	if changed {
		t.Fatal("ClearAuthQuotaCooldown() changed = true, want false")
	}
	if updated == nil {
		t.Fatal("ClearAuthQuotaCooldown() returned nil auth")
	}
	if updated.FailureHTTPStatus != 401 || updated.StatusMessage != "unauthorized" {
		t.Fatalf("updated auth state = %#v, want 401 unauthorized retained", updated)
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("updated.ModelStates[%q] = nil", model)
	}
	if state.FailureHTTPStatus != 401 || state.StatusMessage != "unauthorized" {
		t.Fatalf("state after recovery = %#v, want 401 unauthorized retained", state)
	}
	if blocked, reason, _ := isAuthBlockedForModel(updated, model, time.Now()); !blocked {
		t.Fatalf("expected auth to stay blocked by unauthorized state, blocked=%v reason=%v", blocked, reason)
	}
}

func TestClearAuthQuotaCooldown_ClearsCodexFreeSharedQuotaAcrossModels(t *testing.T) {
	const (
		authID = "codex-free-shared-quota-recovery"
		modelA = "gpt-5.4"
		modelB = "gpt-5.3-codex"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	now := time.Now().UTC()
	manager := NewManager(nil, nil, nil)
	manager.auths[authID] = &Auth{
		ID:                authID,
		Provider:          "codex",
		Status:            StatusError,
		StatusMessage:     "quota exhausted",
		Unavailable:       true,
		FailureHTTPStatus: 429,
		NextRetryAfter:    now.Add(45 * time.Minute),
		Attributes:        map[string]string{"plan_type": "free"},
		Quota:             QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(45 * time.Minute)},
		ModelStates: map[string]*ModelState{
			modelA: {
				Status:            StatusError,
				StatusMessage:     "quota exhausted",
				Unavailable:       true,
				FailureHTTPStatus: 429,
				NextRetryAfter:    now.Add(45 * time.Minute),
				Quota:             QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(45 * time.Minute)},
			},
		},
	}
	reg.SetModelQuotaExceeded(authID, modelA)
	reg.SetModelQuotaExceeded(authID, modelB)
	reg.SuspendClientModel(authID, modelA, "shared_quota")
	reg.SuspendClientModel(authID, modelB, "shared_quota")

	updated, changed, err := manager.ClearAuthQuotaCooldown(context.Background(), authID)
	if err != nil {
		t.Fatalf("ClearAuthQuotaCooldown() error = %v", err)
	}
	if !changed {
		t.Fatal("ClearAuthQuotaCooldown() changed = false, want true")
	}
	if updated == nil {
		t.Fatal("ClearAuthQuotaCooldown() returned nil auth")
	}

	available := reg.GetAvailableModelsByProvider("codex")
	seen := map[string]bool{}
	for _, model := range available {
		if model != nil {
			seen[model.ID] = true
		}
	}
	if !seen[modelA] || !seen[modelB] {
		t.Fatalf("expected both free-plan sibling models to recover, got %#v", seen)
	}
}
