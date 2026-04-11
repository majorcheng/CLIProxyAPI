package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type refreshFailureTestExecutor struct {
	provider string
	err      error
	after    func(*Auth)
}

func (e refreshFailureTestExecutor) Identifier() string { return e.provider }

func (e refreshFailureTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e refreshFailureTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e refreshFailureTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	if e.after != nil {
		e.after(auth)
	}
	return auth, e.err
}

func (e refreshFailureTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e refreshFailureTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type terminalRefreshTestError struct {
	status int
	code   string
	msg    string
}

func (e terminalRefreshTestError) Error() string   { return e.msg }
func (e terminalRefreshTestError) StatusCode() int { return e.status }
func (e terminalRefreshTestError) Terminal() bool  { return true }
func (e terminalRefreshTestError) ErrorCode() string {
	return e.code
}

type transientRefreshTestError struct {
	status int
	msg    string
}

func (e transientRefreshTestError) Error() string   { return e.msg }
func (e transientRefreshTestError) StatusCode() int { return e.status }
func (e transientRefreshTestError) Terminal() bool  { return false }

type codexRefreshRecordingStore struct {
	mu    sync.Mutex
	saves []*Auth
}

func (s *codexRefreshRecordingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *codexRefreshRecordingStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, auth.Clone())
	return auth.ID, nil
}

func (s *codexRefreshRecordingStore) Delete(context.Context, string) error { return nil }

func (s *codexRefreshRecordingStore) snapshot() []*Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Auth, len(s.saves))
	copy(out, s.saves)
	return out
}

func TestManagerRefreshAuth_PersistsTerminalRefresh401ForMaintenance(t *testing.T) {
	ctx := context.Background()
	store := &codexRefreshRecordingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			CodexInitialRefreshOnLoad: true,
		},
	})
	manager.RegisterExecutor(refreshFailureTestExecutor{
		provider: "codex",
		err: terminalRefreshTestError{
			status: http.StatusUnauthorized,
			code:   codexauth.RefreshUnauthorizedErrorCode,
			msg:    "token refresh failed with status 401: unauthorized",
		},
	})

	auth := &Auth{
		ID:       "refresh-401",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"email":         "user@example.com",
			"refresh_token": "refresh-token",
		},
	}
	MarkCodexInitialRefreshPendingForNewFile(auth)
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	started := time.Now()
	manager.refreshAuth(ctx, auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %q to remain registered", auth.ID)
	}
	if updated.LastError == nil {
		t.Fatal("expected refresh failure to persist LastError")
	}
	if updated.LastError.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("expected LastError.HTTPStatus = 401, got %d", updated.LastError.HTTPStatus)
	}
	if updated.LastError.Code != codexauth.RefreshUnauthorizedErrorCode {
		t.Fatalf("expected LastError.Code = %q, got %q", codexauth.RefreshUnauthorizedErrorCode, updated.LastError.Code)
	}
	if !strings.Contains(updated.LastError.Message, "status 401") {
		t.Fatalf("expected LastError.Message to preserve refresh failure details, got %q", updated.LastError.Message)
	}
	if updated.Status != StatusActive {
		t.Fatalf("expected auth status to remain active, got %q", updated.Status)
	}
	if updated.Unavailable {
		t.Fatal("expected auth to remain schedulable until maintenance handles deletion")
	}
	if updated.NextRefreshAfter.IsZero() || !updated.NextRefreshAfter.After(started) {
		t.Fatalf("expected NextRefreshAfter to be scheduled after refresh failure, got %v", updated.NextRefreshAfter)
	}
	if CodexInitialRefreshPending(updated) {
		t.Fatal("expected terminal initial refresh failure to clear pending flag")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		saves := store.snapshot()
		if len(saves) >= 2 {
			last := saves[len(saves)-1]
			if CodexInitialRefreshPending(last) {
				t.Fatalf("expected persisted auth to clear pending flag after terminal failure")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected async persistence after terminal initial refresh failure, got %d saves", len(saves))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagerRefreshAuth_DoesNotMarkTransientRefreshStatusForMaintenance(t *testing.T) {
	ctx := context.Background()
	store := &codexRefreshRecordingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			CodexInitialRefreshOnLoad: true,
		},
	})
	manager.RegisterExecutor(refreshFailureTestExecutor{
		provider: "codex",
		err: transientRefreshTestError{
			status: http.StatusTooManyRequests,
			msg:    "token refresh failed with status 429: rate limited",
		},
	})

	auth := &Auth{
		ID:       "refresh-429",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"email":         "user@example.com",
			"refresh_token": "refresh-token",
		},
	}
	MarkCodexInitialRefreshPendingForNewFile(auth)
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.refreshAuth(ctx, auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %q to remain registered", auth.ID)
	}
	if updated.LastError == nil {
		t.Fatal("expected refresh failure to persist LastError")
	}
	if updated.LastError.HTTPStatus != 0 {
		t.Fatalf("expected transient refresh error to avoid maintenance status code, got %d", updated.LastError.HTTPStatus)
	}
	if updated.Status != StatusActive {
		t.Fatalf("expected auth status to remain active, got %q", updated.Status)
	}
	if updated.Unavailable {
		t.Fatal("expected transient refresh failure to avoid blocking scheduler")
	}
	if !CodexInitialRefreshPending(updated) {
		t.Fatal("expected transient initial refresh failure to keep pending flag for retry")
	}
	if got := len(store.snapshot()); got != 1 {
		t.Fatalf("expected transient failure not to enqueue extra persistence, got %d saves", got)
	}
}

func TestManagerTriggerCodexInitialRefreshOnLoadIfNeeded_RequiresPendingMarker(t *testing.T) {
	ctx := context.Background()
	calls := make(chan string, 2)
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			CodexInitialRefreshOnLoad: true,
		},
	})
	manager.RegisterExecutor(refreshFailureTestExecutor{
		provider: "codex",
		after: func(auth *Auth) {
			if auth != nil {
				calls <- auth.ID
			}
		},
	})

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:       "initial-force-refresh",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
			"last_refresh":  now.Add(-time.Minute).Format(time.RFC3339),
			"expired":       now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.TriggerCodexInitialRefreshOnLoadIfNeeded(ctx, auth.ID)
	select {
	case got := <-calls:
		t.Fatalf("unexpected initial refresh without pending marker for %q", got)
	case <-time.After(150 * time.Millisecond):
	}

	current, ok := manager.GetByID(auth.ID)
	if !ok || current == nil {
		t.Fatalf("expected auth %q to remain registered", auth.ID)
	}
	MarkCodexInitialRefreshPendingForNewFile(current)
	if _, err := manager.Update(ctx, current); err != nil {
		t.Fatalf("update auth with pending marker: %v", err)
	}

	manager.TriggerCodexInitialRefreshOnLoadIfNeeded(ctx, auth.ID)
	select {
	case got := <-calls:
		if got != auth.ID {
			t.Fatalf("initial refresh auth id = %q, want %q", got, auth.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected pending initial refresh trigger to call executor")
	}

	manager.TriggerCodexInitialRefreshOnLoadIfNeeded(ctx, auth.ID)
	select {
	case got := <-calls:
		t.Fatalf("unexpected second initial refresh within backoff window for %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestManagerRefreshAuth_ClearsCodexInitialRefreshPendingAfterSuccess(t *testing.T) {
	ctx := context.Background()
	store := &codexRefreshRecordingStore{}
	manager := NewManager(store, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			CodexInitialRefreshOnLoad: true,
		},
	})
	manager.RegisterExecutor(refreshFailureTestExecutor{
		provider: "codex",
		after: func(auth *Auth) {
			if auth == nil {
				return
			}
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["refresh_token"] = "rotated-refresh-token"
			auth.Metadata["access_token"] = "new-access-token"
			auth.Metadata["expired"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
		},
	})

	auth := &Auth{
		ID:       "refresh-success",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"refresh_token": "refresh-token",
		},
	}
	MarkCodexInitialRefreshPendingForNewFile(auth)
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.refreshAuth(ctx, auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %q to remain registered", auth.ID)
	}
	if CodexInitialRefreshPending(updated) {
		t.Fatal("expected successful initial refresh to clear pending flag")
	}
	if got, _ := updated.Metadata["refresh_token"].(string); got != "rotated-refresh-token" {
		t.Fatalf("updated refresh_token = %q, want %q", got, "rotated-refresh-token")
	}
	saves := store.snapshot()
	if len(saves) == 0 {
		t.Fatal("expected successful initial refresh to persist updated auth")
	}
	last := saves[len(saves)-1]
	if CodexInitialRefreshPending(last) {
		t.Fatal("expected persisted auth after successful initial refresh to clear pending flag")
	}
}

func TestManagerRefreshAuthNow_ReEnablesPersistenceAndReturnsUpdatedSnapshot(t *testing.T) {
	ctx := WithSkipPersist(context.Background())
	store := &codexRefreshRecordingStore{}
	manager := NewManager(store, nil, nil)
	manager.RegisterExecutor(refreshFailureTestExecutor{
		provider: "codex",
		after: func(auth *Auth) {
			if auth == nil {
				return
			}
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["refresh_token"] = "rotated-refresh-token"
			auth.Metadata["access_token"] = "new-access-token"
			auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
			auth.Metadata["expired"] = time.Now().Add(2 * time.Hour).Format(time.RFC3339)
		},
	})

	auth := &Auth{
		ID:       "manual-refresh.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"refresh_token": "original-refresh-token",
			"access_token":  "original-access-token",
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("注册测试 auth 失败: %v", err)
	}
	if got := len(store.snapshot()); got != 0 {
		t.Fatalf("WithSkipPersist 注册后不应写盘，实际保存次数 = %d", got)
	}

	updated, err := manager.RefreshAuthNow(ctx, auth.ID)
	if err != nil {
		t.Fatalf("RefreshAuthNow 返回错误: %v", err)
	}
	if updated == nil {
		t.Fatal("RefreshAuthNow 应返回更新后的快照")
	}
	if got, _ := updated.Metadata["refresh_token"].(string); got != "rotated-refresh-token" {
		t.Fatalf("refresh_token = %q，期望 %q", got, "rotated-refresh-token")
	}
	if got, _ := updated.Metadata["access_token"].(string); got != "new-access-token" {
		t.Fatalf("access_token = %q，期望 %q", got, "new-access-token")
	}
	if updated.LastRefreshedAt.IsZero() {
		t.Fatal("手动刷新成功后应写入 LastRefreshedAt")
	}

	saves := store.snapshot()
	if len(saves) != 1 {
		t.Fatalf("RefreshAuthNow 应覆盖 skipPersist 并写盘一次，实际 = %d", len(saves))
	}
	last := saves[len(saves)-1]
	if got, _ := last.Metadata["refresh_token"].(string); got != "rotated-refresh-token" {
		t.Fatalf("持久化后的 refresh_token = %q，期望 %q", got, "rotated-refresh-token")
	}
}

func TestManagerRefreshAuthNow_ReturnsBusyWhenAutoRefreshInFlight(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(refreshFailureTestExecutor{
		provider: "codex",
		after: func(auth *Auth) {
			if auth == nil {
				return
			}
			started <- struct{}{}
			<-release
			if auth.Metadata == nil {
				auth.Metadata = make(map[string]any)
			}
			auth.Metadata["refresh_token"] = "rotated-refresh-token"
		},
	})

	auth := &Auth{
		ID:       "busy-refresh.json",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"refresh_token": "original-refresh-token",
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("注册测试 auth 失败: %v", err)
	}

	done := make(chan struct{})
	go func() {
		manager.refreshAuthWithLimit(ctx, auth.ID)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("预期后台 refresh 已进入执行状态")
	}

	current, err := manager.RefreshAuthNow(ctx, auth.ID)
	if !errors.Is(err, ErrAuthRefreshInFlight) {
		t.Fatalf("并发手动刷新错误 = %v，期望 %v", err, ErrAuthRefreshInFlight)
	}
	if current == nil || current.ID != auth.ID {
		t.Fatalf("忙碌态应返回当前 auth 快照，实际 = %#v", current)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("释放阻塞后后台 refresh 仍未退出")
	}
}

type codexRecoveryTestExecutor struct {
	mu                    sync.Mutex
	refreshed             bool
	refreshErr            error
	retryUnauthorized     bool
	executeCalls          int
	streamCalls           int
	refreshCalls          int
	lastAccessTokenByCall []string
}

func (e *codexRecoveryTestExecutor) Identifier() string { return "codex" }

func (e *codexRecoveryTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.executeCalls++
	e.lastAccessTokenByCall = append(e.lastAccessTokenByCall, authMetadataString(auth, "access_token"))
	if !e.refreshed {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized before refresh"}
	}
	if e.retryUnauthorized {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized after refresh"}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok-after-refresh")}, nil
}

func (e *codexRecoveryTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.streamCalls++
	e.lastAccessTokenByCall = append(e.lastAccessTokenByCall, authMetadataString(auth, "access_token"))
	if !e.refreshed {
		return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "stream unauthorized before refresh"}
	}
	if e.retryUnauthorized {
		return nil, &Error{HTTPStatus: http.StatusUnauthorized, Message: "stream unauthorized after refresh"}
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("stream-after-refresh")}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *codexRecoveryTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refreshCalls++
	if e.refreshErr != nil {
		return auth, e.refreshErr
	}
	e.refreshed = true
	cloned := auth.Clone()
	if cloned.Metadata == nil {
		cloned.Metadata = make(map[string]any)
	}
	cloned.Metadata["access_token"] = "rotated-access-token"
	cloned.Metadata["refresh_token"] = "rotated-refresh-token"
	cloned.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	cloned.Metadata["expired"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	return cloned, nil
}

func (e *codexRecoveryTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *codexRecoveryTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func authMetadataString(auth *Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	raw, _ := auth.Metadata[key].(string)
	return raw
}

func registerCodexRecoveryTestAuth(t *testing.T, manager *Manager, id string) {
	t.Helper()
	auth := &Auth{
		ID:       id,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "stale-access-token",
			"refresh_token": "refresh-token",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})
}

func TestManagerExecute_Codex401RefreshesAndRetriesOnce(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	executor := &codexRecoveryTestExecutor{}
	manager.RegisterExecutor(executor)
	registerCodexRecoveryTestAuth(t, manager, "codex-recovery")

	resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := string(resp.Payload); got != "ok-after-refresh" {
		t.Fatalf("payload = %q, want %q", got, "ok-after-refresh")
	}
	if executor.refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", executor.refreshCalls)
	}
	if executor.executeCalls != 2 {
		t.Fatalf("executeCalls = %d, want 2", executor.executeCalls)
	}
	if len(executor.lastAccessTokenByCall) < 2 {
		t.Fatalf("expected two execute attempts, got %v", executor.lastAccessTokenByCall)
	}
	if executor.lastAccessTokenByCall[0] != "stale-access-token" || executor.lastAccessTokenByCall[1] != "rotated-access-token" {
		t.Fatalf("access token sequence = %v, want stale -> rotated", executor.lastAccessTokenByCall)
	}
}

func TestManagerExecute_CodexRefreshTerminalFailureBecomesSemantic401(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	executor := &codexRecoveryTestExecutor{
		refreshErr: terminalRefreshTestError{
			status: http.StatusUnauthorized,
			code:   codexauth.RefreshTokenReusedErrorCode,
			msg:    "token refresh failed with status 401: refresh_token_reused",
		},
	}
	manager.RegisterExecutor(executor)
	registerCodexRecoveryTestAuth(t, manager, "codex-recovery-terminal")

	_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected terminal recovery error")
	}
	authErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if authErr.Code != codexauth.RefreshTokenReusedErrorCode {
		t.Fatalf("error code = %q, want %q", authErr.Code, codexauth.RefreshTokenReusedErrorCode)
	}
	if authErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("error status = %d, want 401", authErr.HTTPStatus)
	}
}

func TestManagerExecute_CodexRetryStill401MarksUnauthorizedAfterRecovery(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	executor := &codexRecoveryTestExecutor{retryUnauthorized: true}
	manager.RegisterExecutor(executor)
	registerCodexRecoveryTestAuth(t, manager, "codex-recovery-after-refresh")

	_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected unauthorized-after-recovery error")
	}
	authErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if authErr.Code != codexauth.UnauthorizedAfterRecoveryErrorCode {
		t.Fatalf("error code = %q, want %q", authErr.Code, codexauth.UnauthorizedAfterRecoveryErrorCode)
	}
	if authErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("error status = %d, want 401", authErr.HTTPStatus)
	}
}

func TestManagerExecuteStream_Codex401RefreshesBeforeFirstByte(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	executor := &codexRecoveryTestExecutor{}
	manager.RegisterExecutor(executor)
	registerCodexRecoveryTestAuth(t, manager, "codex-stream-recovery")

	result, err := manager.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if result == nil {
		t.Fatal("expected stream result")
	}
	chunk, ok := <-result.Chunks
	if !ok {
		t.Fatal("expected first stream chunk")
	}
	if string(chunk.Payload) != "stream-after-refresh" {
		t.Fatalf("stream payload = %q, want %q", string(chunk.Payload), "stream-after-refresh")
	}
	if executor.refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", executor.refreshCalls)
	}
	if executor.streamCalls != 2 {
		t.Fatalf("streamCalls = %d, want 2", executor.streamCalls)
	}
}
