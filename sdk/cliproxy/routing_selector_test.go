package cliproxy

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestBuildRoutingSelector_SessionAffinityEnabledReturnsWrapper(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Routing.Strategy = "round-robin"
	cfg.Routing.SessionAffinity = true
	cfg.Routing.PriorityZeroStrategy = "fill-first"

	selector := buildRoutingSelector(cfg)
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok || affinity == nil {
		t.Fatalf("buildRoutingSelector() type = %T, want *SessionAffinitySelector", selector)
	}
	if _, ok := affinity.UnwrapSelector().(*coreauth.PriorityZeroOverrideSelector); !ok {
		t.Fatalf("wrapped selector type = %T, want *PriorityZeroOverrideSelector", affinity.UnwrapSelector())
	}
}

func TestRoutingSelectorNeedsRebuild_DetectsSessionAffinityToggleOrTTLChange(t *testing.T) {
	t.Parallel()

	previous := &config.Config{}
	next := &config.Config{}
	if routingSelectorNeedsRebuild(previous, next) {
		t.Fatal("expected identical default config to skip rebuild")
	}

	next.Routing.SessionAffinity = true
	if !routingSelectorNeedsRebuild(previous, next) {
		t.Fatal("expected enabling session affinity to trigger rebuild")
	}

	next.Routing.SessionAffinityTTL = "2h"
	previous.Routing.SessionAffinity = true
	previous.Routing.SessionAffinityTTL = "1h"
	if !routingSelectorNeedsRebuild(previous, next) {
		t.Fatal("expected TTL change to trigger rebuild")
	}
}

func TestRoutingSessionAffinityTTL_UsesDefaultOnInvalidConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Routing.SessionAffinityTTL = "bad-duration"
	if got := routingSessionAffinityTTL(cfg); got != time.Hour {
		t.Fatalf("routingSessionAffinityTTL() = %v, want %v", got, time.Hour)
	}
}
