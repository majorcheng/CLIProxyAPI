package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestManagerShouldRefresh_CodexUsesConservativeTokenJSONGate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	staleWindow := 8 * 24 * time.Hour

	tests := []struct {
		name string
		auth *Auth
		want bool
	}{
		{
			name: "codex without refresh token stays idle even when expired",
			auth: &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"access_token": testJWTWithExp(now.Add(-time.Minute)),
				},
			},
			want: false,
		},
		{
			name: "codex refreshes when JWT exp already expired",
			auth: &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"access_token":  testJWTWithExp(now.Add(-time.Minute)),
				},
			},
			want: true,
		},
		{
			name: "codex refreshes when JWT exp enters 12 hour proactive window",
			auth: &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"access_token":  testJWTWithExp(now.Add(11*time.Hour + 30*time.Minute)),
				},
			},
			want: true,
		},
		{
			name: "codex does not refresh when JWT exp is still beyond 12 hour window",
			auth: &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"access_token":  testJWTWithExp(now.Add(13 * time.Hour)),
				},
			},
			want: false,
		},
		{
			name: "codex falls back to metadata expiry when access token lacks JWT exp",
			auth: &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"expired":       now.Add(2 * time.Hour).Format(time.RFC3339),
				},
			},
			want: true,
		},
		{
			name: "codex does not refresh when metadata expiry is still beyond 12 hour window",
			auth: &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"expired":       now.Add(13 * time.Hour).Format(time.RFC3339),
				},
			},
			want: false,
		},
		{
			name: "codex refreshes when last refresh is older than 8 day fallback",
			auth: &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"last_refresh":  now.Add(-staleWindow - time.Hour).Format(time.RFC3339),
				},
			},
			want: true,
		},
		{
			name: "codex does not refresh when last refresh is still within 8 days",
			auth: &Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"last_refresh":  now.Add(-staleWindow + time.Hour).Format(time.RFC3339),
				},
			},
			want: false,
		},
		{
			name: "codex with missing expired and last_refresh does not probe upstream",
			auth: &Auth{
				Provider: "codex",
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
			manager := NewManager(nil, nil, nil)
			if got := manager.shouldRefresh(tt.auth, now); got != tt.want {
				t.Fatalf("shouldRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagerShouldRefresh_CodexInitialRefreshPendingForcesFreshLookingToken(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			CodexInitialRefreshOnLoad: true,
		},
	})
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:       "codex-initial-refresh-once",
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
			"last_refresh":  now.Add(-time.Minute).Format(time.RFC3339),
			"expired":       now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		},
	}
	MarkCodexInitialRefreshPendingForNewFile(auth)

	if got := manager.shouldRefresh(auth, now); !got {
		t.Fatal("shouldRefresh() = false, want pending initial refresh to force one refresh")
	}
	ClearCodexInitialRefreshPending(auth)
	if got := manager.shouldRefresh(auth, now.Add(time.Hour)); got {
		t.Fatal("shouldRefresh() = true after pending flag cleared, want fresh-looking token to stay idle")
	}
}

func TestManagerShouldRefresh_DisabledAuthStillEvaluatesRefresh(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:              "disabled-refresh",
		Provider:        "claude",
		Disabled:        true,
		Status:          StatusDisabled,
		Runtime:         staticRefreshLeadRuntime(time.Hour),
		LastRefreshedAt: now.Add(-2 * time.Hour),
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
			"email":         "disabled@example.com",
		},
	}

	if got := manager.shouldRefresh(auth, now); !got {
		t.Fatal("shouldRefresh() = false, want disabled auth to remain refresh-eligible")
	}
}

func TestManagerShouldRefresh_UnauthorizedFailureStopsRefresh(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		auth *Auth
	}{
		{
			name: "last error",
			auth: &Auth{
				ID:              "unauthorized-refresh",
				Provider:        "claude",
				Runtime:         staticRefreshLeadRuntime(time.Hour),
				LastRefreshedAt: now.Add(-2 * time.Hour),
				LastError: &Error{
					Code:       "unauthorized",
					Message:    "token refresh failed with status 401",
					HTTPStatus: http.StatusUnauthorized,
				},
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"email":         "unauthorized@example.com",
				},
			},
		},
		{
			name: "restored runtime state",
			auth: &Auth{
				ID:                "restored-unauthorized-refresh",
				Provider:          "claude",
				Runtime:           staticRefreshLeadRuntime(time.Hour),
				LastRefreshedAt:   now.Add(-2 * time.Hour),
				Status:            StatusError,
				StatusMessage:     "unauthorized",
				Unavailable:       true,
				FailureHTTPStatus: http.StatusUnauthorized,
				Metadata: map[string]any{
					"refresh_token": "refresh-token",
					"email":         "unauthorized@example.com",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := manager.shouldRefresh(tt.auth, now); got {
				t.Fatal("shouldRefresh() = true, want unauthorized auth to stop refresh attempts")
			}
		})
	}
}

func TestManagerMarkRefreshPending_AllowsDisabledAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	manager.mu.Lock()
	manager.auths["disabled-refresh"] = &Auth{
		ID:       "disabled-refresh",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
		},
	}
	manager.mu.Unlock()

	if ok := manager.markRefreshPending("disabled-refresh", now); !ok {
		t.Fatal("markRefreshPending() = false, want disabled auth to be schedulable")
	}
	manager.mu.RLock()
	updated := manager.auths["disabled-refresh"]
	manager.mu.RUnlock()
	if updated == nil || updated.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter was not set for disabled auth: %#v", updated)
	}
}

func TestManagerCollectRefreshTargets_IncludesDisabledRefreshEligibleAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(refreshFailureTestExecutor{provider: "claude"})
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:              "disabled-refresh",
		Provider:        "claude",
		Disabled:        true,
		Status:          StatusDisabled,
		Runtime:         staticRefreshLeadRuntime(time.Hour),
		LastRefreshedAt: now.Add(-2 * time.Hour),
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
			"email":         "disabled@example.com",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register disabled auth: %v", err)
	}

	targets := manager.collectRefreshTargets(now)
	found := false
	for _, target := range targets {
		if target == auth.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("collectRefreshTargets() = %v, want disabled auth %q", targets, auth.ID)
	}
}

func TestManagerCollectRefreshTargets_SkipsPendingDeleteDisabledAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(refreshFailureTestExecutor{provider: "claude"})
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:              "pending-delete-refresh",
		Provider:        "claude",
		Disabled:        true,
		Status:          StatusDisabled,
		Runtime:         staticRefreshLeadRuntime(time.Hour),
		LastRefreshedAt: now.Add(-2 * time.Hour),
		Metadata: map[string]any{
			"refresh_token":                         "refresh-token",
			"email":                                 "disabled@example.com",
			authMaintenancePendingDeleteMetadataKey: true,
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register pending-delete auth: %v", err)
	}

	targets := manager.collectRefreshTargets(now)
	for _, target := range targets {
		if target == auth.ID {
			t.Fatalf("collectRefreshTargets() = %v, want pending-delete auth skipped", targets)
		}
	}
}

func TestManagerMarkRefreshPending_SkipsPendingDeleteAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	manager.mu.Lock()
	manager.auths["pending-delete-refresh"] = &Auth{
		ID:       "pending-delete-refresh",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{
			"refresh_token":                         "refresh-token",
			authMaintenancePendingDeleteMetadataKey: true,
		},
	}
	manager.mu.Unlock()

	if ok := manager.markRefreshPending("pending-delete-refresh", now); ok {
		t.Fatal("markRefreshPending() = true, want pending-delete auth skipped")
	}
}

func TestManagerMarkRefreshPending_SkipsUnauthorizedAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	manager.mu.Lock()
	manager.auths["unauthorized-refresh"] = &Auth{
		ID:       "unauthorized-refresh",
		Provider: "claude",
		LastError: &Error{
			Code:       "unauthorized",
			Message:    "token refresh failed with status 401",
			HTTPStatus: http.StatusUnauthorized,
		},
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
		},
	}
	manager.mu.Unlock()

	if ok := manager.markRefreshPending("unauthorized-refresh", now); ok {
		t.Fatal("markRefreshPending() = true, want unauthorized auth skipped")
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

func TestManagerCollectRefreshTargets_CodexInitialRefreshPendingSchedulesFreshLookingToken(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			CodexInitialRefreshOnLoad: true,
		},
	})
	manager.RegisterExecutor(refreshFailureTestExecutor{provider: "codex"})
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	ctx := context.Background()
	if _, err := manager.Register(ctx, &Auth{
		ID:       "codex-initial-unknown",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"email":         "codex@example.com",
			"refresh_token": "refresh-token",
			"last_refresh":  now.Add(-time.Minute).Format(time.RFC3339),
			"expired":       now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("register codex auth: %v", err)
	}
	auth, ok := manager.GetByID("codex-initial-unknown")
	if !ok || auth == nil {
		t.Fatalf("expected codex auth to be present")
	}
	MarkCodexInitialRefreshPendingForNewFile(auth)
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatalf("update codex auth with pending initial refresh: %v", err)
	}

	first := manager.collectRefreshTargets(now)
	if !containsRefreshTarget(first, "codex-initial-unknown") {
		t.Fatalf("collectRefreshTargets() = %v, want pending initial refresh target", first)
	}

	auth, ok = manager.GetByID("codex-initial-unknown")
	if !ok || auth == nil {
		t.Fatalf("expected codex auth after pending refresh scheduling")
	}
	ClearCodexInitialRefreshPending(auth)
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatalf("update codex auth after clearing pending flag: %v", err)
	}

	second := manager.collectRefreshTargets(now.Add(time.Hour))
	if containsRefreshTarget(second, "codex-initial-unknown") {
		t.Fatalf("collectRefreshTargets() = %v, want fresh-looking codex auth to stop scheduling after pending flag is cleared", second)
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

func testJWTWithExp(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return header + "." + payload + ".signature"
}
