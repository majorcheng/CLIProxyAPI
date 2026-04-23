package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerMarkResult_CodexFreeQuotaPropagatesRegistryStateToSiblingModels(t *testing.T) {
	t.Parallel()

	const (
		authID   = "codex-free-registry"
		modelA   = "codex-free-registry-model-a"
		modelB   = "codex-free-registry-model-b"
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
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
	})

	models := reg.GetAvailableModelsByProvider(provider)
	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == modelA || model.ID == modelB {
			t.Fatalf("expected shared quota to hide %s from available models, got %+v", model.ID, models)
		}
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    modelA,
		Success:  true,
	})

	available := reg.GetAvailableModelsByProvider(provider)
	seen := map[string]bool{}
	for _, model := range available {
		if model != nil {
			seen[model.ID] = true
		}
	}
	if !seen[modelA] || !seen[modelB] {
		t.Fatalf("expected both sibling models to recover, got %#v", seen)
	}
}

func TestManagerMarkResult_CodexFreeQuotaInvalidatesSessionAffinityBinding(t *testing.T) {
	t.Parallel()

	const (
		authA    = "codex-free-affinity-a"
		authB    = "codex-free-affinity-b"
		model    = "codex-free-affinity-model"
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
		Headers: http.Header{"X-Session-ID": []string{"codex-free-shared-quota"}},
	}, selector)
	selector.BindSelectedAuth(opts.Metadata, provider, model, authA)

	if got, err := selector.Pick(context.Background(), provider, model, opts, []*Auth{manager.auths[authA], manager.auths[authB]}); err != nil || got == nil || got.ID != authA {
		t.Fatalf("expected initial binding to hit %s, got auth=%v err=%v", authA, got, err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   authA,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 429, Message: "quota"},
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
