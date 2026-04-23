package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCodexFreeSharedBlockFromModelState_OnlyAccountScopedStatusesShare(t *testing.T) {
	t.Parallel()

	now := time.Now()
	next := now.Add(5 * time.Minute)
	cases := []struct {
		name  string
		state *ModelState
		want  bool
	}{
		{name: "401", state: &ModelState{Unavailable: true, NextRetryAfter: next, FailureHTTPStatus: http.StatusUnauthorized}, want: true},
		{name: "403", state: &ModelState{Unavailable: true, NextRetryAfter: next, FailureHTTPStatus: http.StatusForbidden}, want: true},
		{name: "429 quota", state: &ModelState{Unavailable: true, NextRetryAfter: next, Quota: QuotaState{Exceeded: true, NextRecoverAt: next}}, want: true},
		{name: "404", state: &ModelState{Unavailable: true, NextRetryAfter: next, FailureHTTPStatus: http.StatusNotFound}, want: false},
		{name: "500", state: &ModelState{Unavailable: true, NextRetryAfter: next, FailureHTTPStatus: http.StatusInternalServerError}, want: false},
		{name: "model_not_supported", state: &ModelState{Unavailable: true, NextRetryAfter: next, FailureHTTPStatus: http.StatusBadRequest, LastError: &Error{HTTPStatus: http.StatusBadRequest, Message: "requested model is not supported"}}, want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			blocked, _, _ := codexFreeSharedBlockFromModelState(tc.state, now)
			if blocked != tc.want {
				t.Fatalf("blocked = %v, want %v", blocked, tc.want)
			}
		})
	}
}

func TestManagerMarkResult_CodexFreeModelScopedFailuresDoNotHideSiblingModels(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
	}{
		{name: "model_support", err: &Error{HTTPStatus: http.StatusBadRequest, Message: "requested model is not supported"}},
		{name: "not_found", err: &Error{HTTPStatus: http.StatusNotFound, Message: "not found"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const provider = "codex"
			modelA, modelB := "codex-free-boundary-model-a", "codex-free-boundary-model-b"
			authID := "codex-free-boundary-" + strings.ReplaceAll(tc.name, "_", "-")
			manager, reg := newCodexFreeBoundaryManager(t, authID, provider, modelA, modelB)

			manager.MarkResult(context.Background(), Result{AuthID: authID, Provider: provider, Model: modelA, Success: false, Error: tc.err})

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil {
				t.Fatal("expected updated auth")
			}
			if blocked, reason, next := isAuthBlockedForModel(updated, modelB, time.Now()); blocked {
				t.Fatalf("expected sibling model to stay available, reason=%v next=%v", reason, next)
			}
			visible := visibleModelsByProvider(reg, provider)
			if !visible[modelB] {
				t.Fatalf("expected sibling model %q to remain visible, got %#v", modelB, visible)
			}
			if visible[modelA] {
				t.Fatalf("expected failed model %q to stay hidden, got %#v", modelA, visible)
			}
		})
	}
}

func TestManagerMarkResult_CodexFreeHTTP503DoesNotBlockSiblingModel(t *testing.T) {
	const provider = "codex"
	modelA, modelB := "codex-free-503-model-a", "codex-free-503-model-b"
	authID := "codex-free-503-boundary"
	manager, reg := newCodexFreeBoundaryManager(t, authID, provider, modelA, modelB)

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    modelA,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"},
	})

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, modelB, time.Now()); blocked {
		t.Fatalf("expected sibling model to stay selectable, reason=%v next=%v", reason, next)
	}
	selector := &FillFirstSelector{}
	got, err := selector.Pick(context.Background(), provider, modelB, cliproxyexecutor.Options{}, []*Auth{updated})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil || got.ID != authID {
		t.Fatalf("Pick() auth = %#v, want %q", got, authID)
	}
	visible := visibleModelsByProvider(reg, provider)
	if !visible[modelA] || !visible[modelB] {
		t.Fatalf("expected transient 503 not to hide models, got %#v", visible)
	}
}

func newCodexFreeBoundaryManager(t *testing.T, authID, provider string, modelIDs ...string) (*Manager, *registry.ModelRegistry) {
	t.Helper()

	reg := registry.GetGlobalRegistry()
	models := make([]*registry.ModelInfo, 0, len(modelIDs))
	states := make(map[string]*ModelState, len(modelIDs))
	for _, modelID := range modelIDs {
		models = append(models, &registry.ModelInfo{ID: modelID})
		states[modelID] = &ModelState{Status: StatusActive}
	}
	reg.RegisterClient(authID, provider, models)
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	manager := NewManager(nil, nil, nil)
	manager.auths[authID] = &Auth{
		ID:          authID,
		Provider:    provider,
		Attributes:  map[string]string{"plan_type": "free"},
		ModelStates: states,
	}
	return manager, reg
}

func visibleModelsByProvider(reg *registry.ModelRegistry, provider string) map[string]bool {
	visible := make(map[string]bool)
	for _, model := range reg.GetAvailableModelsByProvider(provider) {
		if model != nil {
			visible[strings.TrimSpace(model.ID)] = true
		}
	}
	return visible
}
