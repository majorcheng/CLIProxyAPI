package auth

import (
	"context"
	"testing"
	"time"
)

func TestManagerShouldRefresh_CodexUsesConservativeTokenJSONGate(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	lead := 5 * 24 * time.Hour

	tests := []struct {
		name string
		auth *Auth
		want bool
	}{
		{
			name: "codex without refresh token stays idle even when expired",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"expired": now.Add(-time.Minute).Format(time.RFC3339),
				},
			},
			want: false,
		},
		{
			name: "codex refreshes when already expired",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"expired":       now.Add(-time.Minute).Format(time.RFC3339),
				},
			},
			want: true,
		},
		{
			name: "codex refreshes when expiry is within refresh lead",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"expired":       now.Add(lead - time.Hour).Format(time.RFC3339),
				},
			},
			want: true,
		},
		{
			name: "codex does not refresh when expiry is still far away",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"expired":       now.Add(lead + time.Hour).Format(time.RFC3339),
				},
			},
			want: false,
		},
		{
			name: "codex refreshes when last refresh is older than lead",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"last_refresh":  now.Add(-lead - time.Hour).Format(time.RFC3339),
				},
			},
			want: true,
		},
		{
			name: "codex does not refresh when last refresh is still recent",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"last_refresh":  now.Add(-lead + time.Hour).Format(time.RFC3339),
				},
			},
			want: false,
		},
		{
			name: "codex with missing expired and last_refresh does not probe upstream",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
				},
			},
			want: false,
		},
		{
			name: "codex honors explicit refresh interval when last refresh is old enough",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"refresh_token":            "refresh-token",
					"last_refresh":             now.Add(-2 * time.Hour).Format(time.RFC3339),
					"refresh_interval_seconds": 3600,
				},
			},
			want: true,
		},
		{
			name: "codex explicit refresh interval still needs concrete timing fields",
			auth: &Auth{
				Provider: "codex",
				Runtime:  staticRefreshLeadRuntime(lead),
				Metadata: map[string]any{
					"refresh_token":            "refresh-token",
					"refresh_interval_seconds": 3600,
				},
			},
			want: false,
		},
		{
			name: "non codex retains legacy optimistic refresh behavior",
			auth: &Auth{
				Provider: "claude",
				Runtime:  staticRefreshLeadRuntime(time.Hour),
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := manager.shouldRefresh(tt.auth, now); got != tt.want {
				t.Fatalf("shouldRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagerCollectRefreshTargets_SkipsCodexWithUnknownTokenTiming(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(refreshFailureTestExecutor{provider: "codex"})
	manager.RegisterExecutor(refreshFailureTestExecutor{provider: "claude"})

	ctx := context.Background()
	if _, err := manager.Register(ctx, &Auth{
		ID:       "codex-unknown",
		Provider: "codex",
		Runtime:  staticRefreshLeadRuntime(5 * 24 * time.Hour),
		Status:   StatusActive,
		Metadata: map[string]any{
			"email":         "codex@example.com",
			"refresh_token": "refresh-token",
		},
	}); err != nil {
		t.Fatalf("register codex auth: %v", err)
	}
	if _, err := manager.Register(ctx, &Auth{
		ID:       "claude-unknown",
		Provider: "claude",
		Runtime:  staticRefreshLeadRuntime(time.Hour),
		Status:   StatusActive,
		Metadata: map[string]any{
			"email":         "claude@example.com",
			"refresh_token": "refresh-token",
		},
	}); err != nil {
		t.Fatalf("register claude auth: %v", err)
	}

	targets := manager.collectRefreshTargets(time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))
	if containsRefreshTarget(targets, "codex-unknown") {
		t.Fatalf("collectRefreshTargets() unexpectedly scheduled codex account with unknown token timing: %v", targets)
	}
	if !containsRefreshTarget(targets, "claude-unknown") {
		t.Fatalf("collectRefreshTargets() = %v, want claude account to retain legacy behavior", targets)
	}
}

func containsRefreshTarget(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

type staticRefreshLeadRuntime time.Duration

func (r staticRefreshLeadRuntime) RefreshLead() *time.Duration {
	d := time.Duration(r)
	return &d
}
