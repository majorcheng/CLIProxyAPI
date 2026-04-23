package auth

import (
	"context"
	"testing"
	"time"

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
