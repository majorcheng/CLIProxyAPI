package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestUpdateAggregatedAvailability_CodexFreeQuotaBlocksAllModels(t *testing.T) {
	t.Parallel()

	now := time.Now()
	next := now.Add(5 * time.Minute)
	auth := &Auth{
		ID:       "codex-free",
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
		},
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
					BackoffLevel:  2,
					StrikeCount:   3,
				},
			},
			"gpt-5.3-codex": {
				Status: StatusActive,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if !auth.Unavailable {
		t.Fatal("auth.Unavailable = false, want true for codex free shared quota")
	}
	if auth.NextRetryAfter.Sub(next) > time.Second || next.Sub(auth.NextRetryAfter) > time.Second {
		t.Fatalf("auth.NextRetryAfter = %v, want %v", auth.NextRetryAfter, next)
	}
	if !auth.Quota.Exceeded {
		t.Fatal("auth.Quota.Exceeded = false, want true")
	}
}

func TestProjectAggregatedAuthState_CodexFreeQuotaBlocksAllModels(t *testing.T) {
	t.Parallel()

	now := time.Now()
	next := now.Add(5 * time.Minute)
	quota := QuotaState{
		Exceeded:      true,
		Reason:        "quota",
		NextRecoverAt: next,
		BackoffLevel:  2,
		StrikeCount:   3,
	}
	state := &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          quota,
	}
	auth := &Auth{
		ID:       "codex-free",
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
		},
		ModelStates: map[string]*ModelState{
			"gpt-5.4":       state,
			"gpt-5.3-codex": {Status: StatusActive},
		},
	}

	projected := projectAggregatedAuthState(auth, "gpt-5.4", state, now)

	if !projected.unavailable {
		t.Fatal("projected.unavailable = false, want true for codex free shared quota")
	}
	if projected.nextRetryAfter.Sub(next) > time.Second || next.Sub(projected.nextRetryAfter) > time.Second {
		t.Fatalf("projected.nextRetryAfter = %v, want %v", projected.nextRetryAfter, next)
	}
	if !projected.quota.Exceeded {
		t.Fatal("projected.quota.Exceeded = false, want true")
	}
}

func TestFillFirstSelectorPick_CodexFreeQuotaBlocksSiblingModel(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	now := time.Now()
	next := now.Add(5 * time.Minute)
	quota := QuotaState{
		Exceeded:      true,
		Reason:        "quota",
		NextRecoverAt: next,
		BackoffLevel:  1,
		StrikeCount:   1,
	}
	high := &Auth{
		ID:       "free-high",
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "free",
			"priority":  "10",
		},
		Quota: quota,
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota:          quota,
			},
			"gpt-5.3-codex": {
				Status: StatusActive,
			},
		},
	}
	low := &Auth{
		ID:       "free-low",
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
	if blocked, _, _ := isAuthBlockedForModel(high, "gpt-5.3-codex", now); !blocked {
		t.Fatal("expected codex free quota to block sibling model")
	}
}

func TestFillFirstSelectorPick_CodexPlusQuotaKeepsSiblingModelAvailable(t *testing.T) {
	t.Parallel()

	selector := &FillFirstSelector{}
	next := time.Now().Add(5 * time.Minute)
	quota := QuotaState{
		Exceeded:      true,
		Reason:        "quota",
		NextRecoverAt: next,
		BackoffLevel:  1,
		StrikeCount:   1,
	}
	high := &Auth{
		ID:       "plus-high",
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "plus",
			"priority":  "10",
		},
		Quota: quota,
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota:          quota,
			},
			"gpt-5.3-codex": {
				Status: StatusActive,
			},
		},
	}
	low := &Auth{
		ID:       "plus-low",
		Provider: "codex",
		Attributes: map[string]string{
			"plan_type": "plus",
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
	if got.ID != high.ID {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, high.ID)
	}
}

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
