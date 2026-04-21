package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type sessionAffinityExecutor struct {
	failures map[string]int
}

func (e *sessionAffinityExecutor) Identifier() string { return "gemini" }

func (e *sessionAffinityExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth != nil && e.failures != nil && e.failures[auth.ID] > 0 {
		e.failures[auth.ID]--
		return cliproxyexecutor.Response{}, &Error{Code: "upstream_failed", Message: auth.ID + " temporary failure", Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
	}
	payload := []byte("")
	if auth != nil {
		payload = []byte(auth.ID)
	}
	return cliproxyexecutor.Response{Payload: payload}, nil
}

func (e *sessionAffinityExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *sessionAffinityExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *sessionAffinityExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *sessionAffinityExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestSessionAffinitySelector_SameSessionSticksToSameAuth(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	auths := []*Auth{{ID: "auth-a", Provider: "gemini"}, {ID: "auth-b", Provider: "gemini"}}
	opts := ensureSessionAffinityMetadata(cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-ID": []string{"session-1"}},
	}, selector)

	first, errPick := selector.Pick(context.Background(), "gemini", "gemini-2.5-pro", opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() first error = %v", errPick)
	}
	selector.BindSelectedAuth(opts.Metadata, "gemini", "gemini-2.5-pro", first.ID)
	second, errPick := selector.Pick(context.Background(), "gemini", "gemini-2.5-pro", opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() second error = %v", errPick)
	}
	if first == nil || second == nil {
		t.Fatal("Pick() returned nil auth")
	}
	if second.ID != first.ID {
		t.Fatalf("second auth = %q, want %q", second.ID, first.ID)
	}
}

func TestSessionAffinitySelector_FallbackHashPromotesSecondTurn(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	auths := []*Auth{{ID: "auth-a", Provider: "openai"}, {ID: "auth-b", Provider: "openai"}}
	firstTurn := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`)
	secondTurn := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"},{"role":"assistant","content":"world"}]}`)

	first, errPick := selector.Pick(context.Background(), "openai", "gpt-5", cliproxyexecutor.Options{OriginalRequest: firstTurn}, auths)
	if errPick != nil {
		t.Fatalf("Pick() first error = %v", errPick)
	}
	firstOpts := ensureSessionAffinityMetadata(cliproxyexecutor.Options{OriginalRequest: firstTurn}, selector)
	selector.BindSelectedAuth(firstOpts.Metadata, "openai", "gpt-5", first.ID)
	second, errPick := selector.Pick(context.Background(), "openai", "gpt-5", cliproxyexecutor.Options{OriginalRequest: secondTurn}, auths)
	if errPick != nil {
		t.Fatalf("Pick() second error = %v", errPick)
	}
	if first == nil || second == nil {
		t.Fatal("Pick() returned nil auth")
	}
	if second.ID != first.ID {
		t.Fatalf("second auth = %q, want %q", second.ID, first.ID)
	}
}

func TestExtractSessionID_ClaudeCodeJSONUserID(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"sess-json\"}"}}`)
	got := ExtractSessionID(nil, payload, nil)
	if got != "claude:sess-json" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "claude:sess-json")
	}
}

func TestExtractSessionID_UsesExecutionSessionMetadata(t *testing.T) {
	t.Parallel()

	metadata := map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "exec-1"}
	got := ExtractSessionID(nil, nil, metadata)
	if got != "exec:exec-1" {
		t.Fatalf("ExtractSessionID() = %q, want %q", got, "exec:exec-1")
	}
}

func TestEnsureRequestSimHashMetadata_WrappedSimHashSelector(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(NewSimHashSelector(internalconfig.RoutingSimHashConfig{}))
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
	updated := ensureRequestSimHashMetadata(opts, selector)
	if _, ok := updated.Metadata[cliproxyexecutor.RequestSimHashMetadataKey]; !ok {
		t.Fatal("expected wrapped simhash selector to inject request_simhash")
	}
}

func TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth(t *testing.T) {
	t.Parallel()

	const (
		authA = "session-affinity-rebind-a"
		authB = "session-affinity-rebind-b"
	)
	registerSchedulerModels(t, "gemini", "gemini-2.5-pro", authA, authB)
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	manager := NewManager(nil, selector, nil)
	manager.executors["gemini"] = &sessionAffinityExecutor{failures: map[string]int{authA: 1}}
	manager.auths[authA] = &Auth{ID: authA, Provider: "gemini", Metadata: map[string]any{"disable_cooling": true}}
	manager.auths[authB] = &Auth{ID: authB, Provider: "gemini"}

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{"session-sticky"}}}
	resp, errExec := manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, opts)
	if errExec != nil {
		t.Fatalf("Execute() first error = %v", errExec)
	}
	if got := string(resp.Payload); got != authB {
		t.Fatalf("Execute() first payload = %q, want %q", got, authB)
	}

	resp, errExec = manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{Model: "gemini-2.5-pro"}, opts)
	if errExec != nil {
		t.Fatalf("Execute() second error = %v", errExec)
	}
	if got := string(resp.Payload); got != authB {
		t.Fatalf("Execute() second payload = %q, want %q", got, authB)
	}
}

func TestSessionAffinitySelector_FailedPickDoesNotCreateBinding(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	auths := []*Auth{{ID: "auth-a", Provider: "gemini"}, {ID: "auth-b", Provider: "gemini"}}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": []string{"session-fail"}}}

	first, errPick := selector.Pick(context.Background(), "gemini", "gemini-2.5-pro", opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() first error = %v", errPick)
	}
	if first == nil {
		t.Fatal("Pick() first auth = nil")
	}
	second, errPick := selector.Pick(context.Background(), "gemini", "gemini-2.5-pro", opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() second error = %v", errPick)
	}
	if second == nil {
		t.Fatal("Pick() second auth = nil")
	}
	if second.ID == first.ID {
		t.Fatalf("second auth = %q, want different auth before success binding", second.ID)
	}
}

func TestManager_UseSchedulerFastPath_WithWrappedBuiltInSelector(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, NewSessionAffinitySelector(&FillFirstSelector{}), nil)
	if !manager.useSchedulerFastPath() {
		t.Fatal("useSchedulerFastPath() = false, want true for wrapped built-in selector")
	}
}

func TestManager_PickNextMixed_WrappedFillFirstPreservesProviderOrder(t *testing.T) {
	t.Parallel()

	model := "gemini-2.5-pro"
	registerSchedulerModels(t, "claude", model, "claude-a")
	registerSchedulerModels(t, "gemini", model, "gemini-a")

	manager := NewManager(nil, NewSessionAffinitySelector(&FillFirstSelector{}), nil)
	manager.executors["claude"] = schedulerTestExecutor{}
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.auths["gemini-a"] = &Auth{ID: "gemini-a", Provider: "gemini"}
	manager.auths["claude-a"] = &Auth{ID: "claude-a", Provider: "claude"}
	manager.syncScheduler()

	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"claude", "gemini"}, model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickNextMixed() auth = nil")
	}
	if provider != "claude" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "claude")
	}
	if got.ID != "claude-a" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "claude-a")
	}
}

func TestManager_PickNextMixed_WrappedFillFirstPreservesCodexWebsocketPreference(t *testing.T) {
	t.Parallel()

	model := "gpt-5-codex"
	registerSchedulerModels(t, "codex", model, "codex-http", "codex-ws")
	manager := NewManager(nil, NewSessionAffinitySelector(&FillFirstSelector{}), nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	manager.auths["codex-http"] = &Auth{ID: "codex-http", Provider: "codex"}
	manager.auths["codex-ws"] = &Auth{ID: "codex-ws", Provider: "codex", Attributes: map[string]string{"websockets": "true"}}
	manager.syncScheduler()

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	got, _, provider, errPick := manager.pickNextMixed(ctx, []string{"codex"}, model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil {
		t.Fatal("pickNextMixed() auth = nil")
	}
	if provider != "codex" {
		t.Fatalf("pickNextMixed() provider = %q, want %q", provider, "codex")
	}
	if got.ID != "codex-ws" {
		t.Fatalf("pickNextMixed() auth.ID = %q, want %q", got.ID, "codex-ws")
	}
}

func TestSessionAffinitySelector_BindSelectedAuthPromotesFallbackHash(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	firstTurn := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`)
	secondTurn := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"},{"role":"assistant","content":"world"}]}`)
	firstOpts := ensureSessionAffinityMetadata(cliproxyexecutor.Options{OriginalRequest: firstTurn}, selector)
	firstPrimaryID, fallbackID := sessionAffinityIDsFromMetadata(firstOpts.Metadata)
	if firstPrimaryID == "" || fallbackID != "" {
		t.Fatalf("first turn ids = (%q,%q), want primary only", firstPrimaryID, fallbackID)
	}
	secondOpts := ensureSessionAffinityMetadata(cliproxyexecutor.Options{OriginalRequest: secondTurn}, selector)
	secondPrimaryID, fallbackID := sessionAffinityIDsFromMetadata(secondOpts.Metadata)
	if secondPrimaryID == "" || fallbackID == "" {
		t.Fatalf("second turn ids = (%q,%q), want primary and fallback", secondPrimaryID, fallbackID)
	}

	selector.BindSelectedAuth(firstOpts.Metadata, "openai", "gpt-5", "auth-a")
	if got, ok := selector.cache.Get(sessionAffinityCacheKey("openai", "gpt-5", firstPrimaryID)); !ok || got != "auth-a" {
		t.Fatalf("cache primary = (%q,%v), want auth-a", got, ok)
	}

	selector.BindSelectedAuth(secondOpts.Metadata, "openai", "gpt-5", "auth-b")
	if got, ok := selector.cache.Get(sessionAffinityCacheKey("openai", "gpt-5", secondPrimaryID)); !ok || got != "auth-b" {
		t.Fatalf("cache full hash = (%q,%v), want auth-b", got, ok)
	}
	if got, ok := selector.cache.Get(sessionAffinityCacheKey("openai", "gpt-5", fallbackID)); !ok || got != "auth-b" {
		t.Fatalf("cache short hash = (%q,%v), want auth-b", got, ok)
	}
}

func TestSessionAffinitySelector_CacheHitRefreshesTTL(t *testing.T) {
	t.Parallel()

	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      200 * time.Millisecond,
	})
	auths := []*Auth{{ID: "auth-a", Provider: "gemini"}, {ID: "auth-b", Provider: "gemini"}}
	opts := ensureSessionAffinityMetadata(cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-ID": []string{"ttl-session"}},
	}, selector)
	selector.BindSelectedAuth(opts.Metadata, "gemini", "gemini-2.5-pro", "auth-a")

	time.Sleep(120 * time.Millisecond)
	got, errPick := selector.Pick(context.Background(), "gemini", "gemini-2.5-pro", opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() error = %v", errPick)
	}
	if got == nil || got.ID != "auth-a" {
		t.Fatalf("Pick() auth = %v, want auth-a", got)
	}

	time.Sleep(120 * time.Millisecond)
	got, errPick = selector.Pick(context.Background(), "gemini", "gemini-2.5-pro", opts, auths)
	if errPick != nil {
		t.Fatalf("Pick() second error = %v", errPick)
	}
	if got == nil || got.ID != "auth-a" {
		t.Fatalf("Pick() second auth = %v, want auth-a after TTL refresh", got)
	}
}
