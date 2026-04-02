package auth

import (
	"context"
	"errors"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestSchedulerPickSingle_PriorityZeroPolicySkipsZeroBucket(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "priority-zero", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
		&Auth{ID: "fallback", Provider: "gemini", Attributes: map[string]string{"priority": "-1"}},
	)

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.DisallowPriorityZeroAuthMetadataKey: true},
	}
	got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", opts, nil)
	if errPick != nil {
		t.Fatalf("pickSingle() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickSingle() auth = nil")
	}
	if got.ID != "fallback" {
		t.Fatalf("pickSingle() auth.ID = %q, want %q", got.ID, "fallback")
	}
}

func TestSchedulerPickSingle_PriorityZeroPolicyDoesNotSpillPinnedAuth(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "priority-zero", Provider: "gemini", Attributes: map[string]string{"priority": "0"}},
		&Auth{ID: "fallback", Provider: "gemini", Attributes: map[string]string{"priority": "-1"}},
	)

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.DisallowPriorityZeroAuthMetadataKey: true,
			cliproxyexecutor.PinnedAuthMetadataKey:               "priority-zero",
		},
	}
	got, errPick := scheduler.pickSingle(context.Background(), "gemini", "", opts, nil)
	if errPick == nil {
		t.Fatal("pickSingle() error = nil, want auth_not_found")
	}
	if got != nil {
		t.Fatalf("pickSingle() auth = %#v, want nil", got)
	}
	var authErr *Error
	if !errors.As(errPick, &authErr) || authErr == nil {
		t.Fatalf("pickSingle() error = %v, want *Error", errPick)
	}
	if authErr.Code != "auth_not_found" {
		t.Fatalf("pickSingle() error code = %q, want %q", authErr.Code, "auth_not_found")
	}
}

func TestSchedulerPickMixed_PriorityZeroPolicyFallsThroughToLowerPriorityProvider(t *testing.T) {
	t.Parallel()

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "codex-zero", Provider: "codex", Attributes: map[string]string{"priority": "0"}},
		&Auth{ID: "compat-fallback", Provider: "openai-compat", Attributes: map[string]string{"priority": "-1"}},
	)

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.DisallowPriorityZeroAuthMetadataKey: true},
	}
	got, provider, errPick := scheduler.pickMixed(context.Background(), []string{"codex", "openai-compat"}, "", opts, nil)
	if errPick != nil {
		t.Fatalf("pickMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickMixed() auth = nil")
	}
	if provider != "openai-compat" {
		t.Fatalf("pickMixed() provider = %q, want %q", provider, "openai-compat")
	}
	if got.ID != "compat-fallback" {
		t.Fatalf("pickMixed() auth.ID = %q, want %q", got.ID, "compat-fallback")
	}
}

func TestManagerCustomSelector_PriorityZeroPolicyFiltersLegacyCandidates(t *testing.T) {
	t.Parallel()

	selector := &trackingSelector{}
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.auths["priority-zero"] = &Auth{
		ID:         "priority-zero",
		Provider:   "gemini",
		Attributes: map[string]string{"priority": "0"},
	}
	manager.auths["fallback"] = &Auth{
		ID:         "fallback",
		Provider:   "gemini",
		Attributes: map[string]string{"priority": "-1"},
	}

	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.DisallowPriorityZeroAuthMetadataKey: true},
	}
	got, _, errPick := manager.pickNext(context.Background(), "gemini", "", opts, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickNext() auth = nil")
	}
	if selector.calls != 1 {
		t.Fatalf("selector.calls = %d, want %d", selector.calls, 1)
	}
	if len(selector.lastAuthID) != 1 || selector.lastAuthID[0] != "fallback" {
		t.Fatalf("selector.lastAuthID = %v, want [fallback]", selector.lastAuthID)
	}
	if got.ID != "fallback" {
		t.Fatalf("pickNext() auth.ID = %q, want %q", got.ID, "fallback")
	}
}
