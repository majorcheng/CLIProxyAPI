package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestUpdateAggregatedAvailability_CodexFreeUnauthorizedBlocksAllModels(t *testing.T) {
	t.Parallel()

	now := time.Now()
	next := now.Add(30 * time.Minute)
	auth := &Auth{
		ID:       "codex-free-unauthorized",
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
		},
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {
				Status:            StatusError,
				Unavailable:       true,
				NextRetryAfter:    next,
				FailureHTTPStatus: 401,
			},
			"gpt-5.3-codex": {
				Status: StatusActive,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if !auth.Unavailable {
		t.Fatal("auth.Unavailable = false, want true for codex free shared runtime state")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "gpt-5.3-codex", now); !blocked {
		t.Fatal("expected codex free unauthorized state to block sibling model")
	}
}

func TestFillFirstSelectorPick_CodexFreeUnauthorizedBlocksSiblingModel(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	next := time.Now().Add(30 * time.Minute)
	high := &Auth{
		ID:       "free-unauthorized-high",
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
			"priority":  "10",
		},
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {
				Status:            StatusError,
				Unavailable:       true,
				NextRetryAfter:    next,
				FailureHTTPStatus: 401,
			},
			"gpt-5.3-codex": {Status: StatusActive},
		},
	}
	low := &Auth{
		ID:       "free-unauthorized-low",
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
			"priority":  "0",
		},
		ModelStates: map[string]*ModelState{
			"gpt-5.3-codex": {Status: StatusActive},
		},
	}

	got, err := selector.Pick(context.Background(), "codex", "gpt-5.3-codex", cliproxyexecutor.Options{}, []*Auth{high, low})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatal("Pick() auth = nil")
	}
	if got.ID != low.ID {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, low.ID)
	}
}

func TestManagerMarkResult_CodexFreeUnauthorizedPropagatesRegistryStateToSiblingModels(t *testing.T) {
	const (
		authID   = "codex-free-unauthorized-registry"
		modelA   = "codex-free-unauthorized-model-a"
		modelB   = "codex-free-unauthorized-model-b"
		provider = "codex"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{
		{ID: modelA},
		{ID: modelB},
	})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	manager := NewManager(nil, nil, nil)
	manager.auths[authID] = &Auth{
		ID:       authID,
		Provider: provider,
		Attributes: map[string]string{
			"plan_type": "free",
		},
		ModelStates: map[string]*ModelState{
			modelA: {Status: StatusActive},
			modelB: {Status: StatusActive},
		},
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    modelA,
		Success:  false,
		Error:    &Error{HTTPStatus: 401, Message: "unauthorized"},
	})

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("expected updated auth to exist")
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, modelB, time.Now()); !blocked {
		t.Fatal("expected unauthorized state to block sibling model")
	}
	models := reg.GetAvailableModelsByProvider(provider)
	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == modelA || model.ID == modelB {
			t.Fatalf("expected shared unauthorized state to hide %s from available models, got %+v", model.ID, models)
		}
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    modelB,
		Success:  true,
	})

	updated, ok = manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("expected recovered auth to exist")
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, modelA, time.Now()); blocked {
		t.Fatalf("expected success to recover sibling model, reason=%v next=%v", reason, next)
	}
}

func TestManagerMarkResult_CodexFreeUnauthorizedInvalidatesSessionAffinityBinding(t *testing.T) {
	t.Parallel()

	const (
		authA    = "codex-free-unauthorized-affinity-a"
		authB    = "codex-free-unauthorized-affinity-b"
		model    = "codex-free-unauthorized-affinity-model"
		provider = "codex"
	)

	registerSchedulerModels(t, provider, model, authA, authB)
	selector := NewSessionAffinitySelector(&FillFirstSelector{})
	manager := NewManager(nil, selector, nil)
	manager.auths[authA] = &Auth{
		ID:       authA,
		Provider: provider,
		Attributes: map[string]string{
			"plan_type": "free",
			"priority":  "10",
		},
		ModelStates: map[string]*ModelState{
			model: {Status: StatusActive},
		},
	}
	manager.auths[authB] = &Auth{
		ID:       authB,
		Provider: provider,
		Attributes: map[string]string{
			"plan_type": "free",
			"priority":  "0",
		},
		ModelStates: map[string]*ModelState{
			model: {Status: StatusActive},
		},
	}

	opts := ensureSessionAffinityMetadata(cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-ID": []string{"codex-free-shared-unauthorized"}},
	}, selector)
	selector.BindSelectedAuth(opts.Metadata, provider, model, authA)

	manager.MarkResult(context.Background(), Result{
		AuthID:   authA,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 401, Message: "unauthorized"},
	})

	got, err := selector.Pick(context.Background(), provider, model, opts, []*Auth{manager.auths[authA], manager.auths[authB]})
	if err != nil {
		t.Fatalf("Pick() after invalidation error = %v", err)
	}
	if got == nil {
		t.Fatal("Pick() after invalidation auth = nil")
	}
	if got.ID != authB {
		t.Fatalf("Pick() after invalidation auth.ID = %q, want %q", got.ID, authB)
	}
}
