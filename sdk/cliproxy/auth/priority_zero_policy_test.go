package auth

import (
	"context"
	"errors"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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

func TestManagerBuiltInScheduler_PriorityZeroRoutingStrategyUsesFillFirstWithinZeroBucket(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			PriorityZeroStrategy: "fill-first",
		},
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "zero-old", Provider: "gemini", Attributes: map[string]string{"priority": "0"}}); errRegister != nil {
		t.Fatalf("Register(zero-old) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "zero-new", Provider: "gemini", Attributes: map[string]string{"priority": "0"}}); errRegister != nil {
		t.Fatalf("Register(zero-new) error = %v", errRegister)
	}

	for index := 0; index < 2; index++ {
		got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNext() #%d auth = nil", index)
		}
		if got.ID != "zero-old" {
			t.Fatalf("pickNext() #%d auth.ID = %q, want %q", index, got.ID, "zero-old")
		}
	}
}

func TestManagerBuiltInScheduler_PriorityZeroRoutingStrategyDoesNotAffectNonZeroBucket(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			PriorityZeroStrategy: "fill-first",
		},
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "high-b", Provider: "gemini", Attributes: map[string]string{"priority": "1"}}); errRegister != nil {
		t.Fatalf("Register(high-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "high-a", Provider: "gemini", Attributes: map[string]string{"priority": "1"}}); errRegister != nil {
		t.Fatalf("Register(high-a) error = %v", errRegister)
	}

	want := []string{"high-a", "high-b"}
	for index, wantID := range want {
		got, _, errPick := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNext() #%d auth = nil", index)
		}
		if got.ID != wantID {
			t.Fatalf("pickNext() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}

func TestManagerBuiltInScheduler_PriorityZeroRoutingStrategyAppliesToMixedProviderSelection(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			PriorityZeroStrategy: "fill-first",
		},
	})
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-zero", Provider: "gemini", Attributes: map[string]string{"priority": "0"}}); errRegister != nil {
		t.Fatalf("Register(gemini-zero) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-zero", Provider: "claude", Attributes: map[string]string{"priority": "0"}}); errRegister != nil {
		t.Fatalf("Register(claude-zero) error = %v", errRegister)
	}

	for index := 0; index < 2; index++ {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if provider != "gemini" {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, "gemini")
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if got.ID != "gemini-zero" {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, "gemini-zero")
		}
	}
}
