package cliproxy

import (
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

const defaultSessionAffinityTTL = time.Hour

type routingSelectorConfigSnapshot struct {
	strategy             string
	priorityZeroStrategy string
	sessionAffinity      bool
	sessionAffinityTTL   time.Duration
}

func buildRoutingSelector(cfg *config.Config) coreauth.Selector {
	base := buildRoutingBaseSelector(cfg)
	if !routingSessionAffinityEnabled(cfg) {
		return base
	}
	wrappedBase := wrapRoutingSelectorWithSessionAffinity(cfg, base)
	return coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: wrappedBase,
		TTL:      routingSessionAffinityTTL(cfg),
	})
}

func buildRoutingBaseSelector(cfg *config.Config) coreauth.Selector {
	if cfg == nil {
		return &coreauth.RoundRobinSelector{}
	}
	switch normalizeRoutingStrategy(cfg.Routing.Strategy) {
	case "fill-first":
		return &coreauth.FillFirstSelector{}
	case "success-rate":
		return coreauth.NewSuccessRateSelector(
			cfg.Routing.SuccessRate.HalfLifeSeconds,
			cfg.Routing.SuccessRate.ExploreRate,
		)
	case "simhash":
		return coreauth.NewSimHashSelector(cfg.Routing.SimHash)
	default:
		return &coreauth.RoundRobinSelector{}
	}
}

func wrapRoutingSelectorWithSessionAffinity(cfg *config.Config, base coreauth.Selector) coreauth.Selector {
	if cfg == nil {
		return base
	}
	strategy := normalizeRoutingStrategy(cfg.Routing.Strategy)
	priorityZeroStrategy := normalizePriorityZeroStrategy(cfg.Routing.PriorityZeroStrategy)
	if priorityZeroStrategy == "" {
		return base
	}
	if strategy != "round-robin" && strategy != "fill-first" {
		return base
	}
	return coreauth.NewPriorityZeroOverrideSelector(base, routingPriorityZeroSelector(priorityZeroStrategy))
}

func routingPriorityZeroSelector(strategy string) coreauth.Selector {
	switch normalizePriorityZeroStrategy(strategy) {
	case "fill-first":
		return &coreauth.FillFirstSelector{}
	default:
		return &coreauth.RoundRobinSelector{}
	}
}

func routingSelectorSnapshot(cfg *config.Config) routingSelectorConfigSnapshot {
	snapshot := routingSelectorConfigSnapshot{strategy: normalizeRoutingStrategy("")}
	if cfg == nil {
		return snapshot
	}
	snapshot.strategy = normalizeRoutingStrategy(cfg.Routing.Strategy)
	snapshot.sessionAffinity = routingSessionAffinityEnabled(cfg)
	if snapshot.sessionAffinity {
		snapshot.sessionAffinityTTL = routingSessionAffinityTTL(cfg)
		if snapshot.strategy == "round-robin" || snapshot.strategy == "fill-first" {
			snapshot.priorityZeroStrategy = normalizePriorityZeroStrategy(cfg.Routing.PriorityZeroStrategy)
		}
	}
	return snapshot
}

func routingSelectorNeedsRebuild(previous, next *config.Config) bool {
	return routingSelectorSnapshot(previous) != routingSelectorSnapshot(next)
}

func routingSessionAffinityEnabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return cfg.Routing.SessionAffinity || cfg.Routing.ClaudeCodeSessionAffinity
}

func routingSessionAffinityTTL(cfg *config.Config) time.Duration {
	if cfg == nil {
		return defaultSessionAffinityTTL
	}
	raw := strings.TrimSpace(cfg.Routing.SessionAffinityTTL)
	if raw == "" {
		return defaultSessionAffinityTTL
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl <= 0 {
		return defaultSessionAffinityTTL
	}
	return ttl
}

func normalizeRoutingStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	case "success-rate", "successrate", "sr":
		return "success-rate"
	case "simhash", "sh":
		return "simhash"
	default:
		return "round-robin"
	}
}

func normalizePriorityZeroStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	case "round-robin", "roundrobin", "rr":
		return "round-robin"
	default:
		return ""
	}
}
