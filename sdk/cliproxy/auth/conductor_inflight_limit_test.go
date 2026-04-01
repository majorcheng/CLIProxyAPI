package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type inflightLimitTestExecutor struct {
	provider string

	mu sync.Mutex

	executeStarted map[string]chan struct{}
	executeHold    map[string]chan struct{}

	streamStarted map[string]chan struct{}
	streamCurrent map[string]chan cliproxyexecutor.StreamChunk
}

func (e *inflightLimitTestExecutor) Identifier() string {
	return e.provider
}

func (e *inflightLimitTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	authID := auth.ID
	e.mu.Lock()
	started := e.executeStarted[authID]
	hold := e.executeHold[authID]
	e.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if hold != nil {
		<-hold
	}
	return cliproxyexecutor.Response{Payload: []byte(authID)}, nil
}

func (e *inflightLimitTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	authID := auth.ID
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("bootstrap:" + authID)}

	e.mu.Lock()
	started := e.streamStarted[authID]
	e.streamCurrent[authID] = ch
	e.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}

	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{"X-Auth": []string{authID}},
		Chunks:  ch,
	}, nil
}

func (e *inflightLimitTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *inflightLimitTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *inflightLimitTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *inflightLimitTestExecutor) currentStream(authID string) chan cliproxyexecutor.StreamChunk {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.streamCurrent[authID]
}

func newInflightLimitTestManager(t *testing.T, selector Selector, provider, model string, maxInflight int, executor ProviderExecutor, auths ...*Auth) *Manager {
	t.Helper()

	manager := NewManager(nil, selector, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			MaxInflightPerAuth: maxInflight,
		},
	})
	manager.RegisterExecutor(executor)

	authIDs := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		authIDs = append(authIDs, auth.ID)
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}
	if model != "" && len(authIDs) > 0 {
		registerSchedulerModels(t, provider, model, authIDs...)
	}
	return manager
}

func waitForSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待 %s 超时", name)
	}
}

func waitForAuthInflight(t *testing.T, manager *Manager, authID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := manager.authInflightCount(authID); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("auth %s inflight 未收敛到 %d，当前=%d", authID, want, manager.authInflightCount(authID))
}

func TestManagerExecute_FillFirstSpillsAndReturnsToPrimaryAfterRelease(t *testing.T) {
	t.Parallel()

	const (
		provider = "gemini"
		model    = "gemini-2.5-pro"
		authA    = "auth-a-fill"
		authB    = "auth-b-fill"
	)
	executor := &inflightLimitTestExecutor{
		provider:       provider,
		executeStarted: map[string]chan struct{}{authA: make(chan struct{}, 1)},
		executeHold:    map[string]chan struct{}{authA: make(chan struct{})},
		streamStarted:  make(map[string]chan struct{}),
		streamCurrent:  make(map[string]chan cliproxyexecutor.StreamChunk),
	}
	manager := newInflightLimitTestManager(
		t,
		&FillFirstSelector{},
		provider,
		model,
		1,
		executor,
		&Auth{ID: authA, Provider: provider},
		&Auth{ID: authB, Provider: provider},
	)

	req := cliproxyexecutor.Request{Model: model}
	firstRespCh := make(chan cliproxyexecutor.Response, 1)
	firstErrCh := make(chan error, 1)
	go func() {
		resp, err := manager.Execute(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
		if err != nil {
			firstErrCh <- err
			return
		}
		firstRespCh <- resp
	}()

	waitForSignal(t, executor.executeStarted[authA], "auth-a 首次执行")
	waitForAuthInflight(t, manager, authA, 1)

	secondResp, err := manager.Execute(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("第二次 Execute() error = %v", err)
	}
	if got := string(secondResp.Payload); got != authB {
		t.Fatalf("第二次 Execute() payload = %q, want %q", got, authB)
	}

	close(executor.executeHold[authA])

	select {
	case err := <-firstErrCh:
		t.Fatalf("第一次 Execute() error = %v", err)
	case firstResp := <-firstRespCh:
		if got := string(firstResp.Payload); got != authA {
			t.Fatalf("第一次 Execute() payload = %q, want %q", got, authA)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("第一次 Execute() 未返回")
	}

	waitForAuthInflight(t, manager, authA, 0)

	thirdResp, err := manager.Execute(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("第三次 Execute() error = %v", err)
	}
	if got := string(thirdResp.Payload); got != authA {
		t.Fatalf("第三次 Execute() payload = %q, want %q", got, authA)
	}
}

func TestManagerExecute_ReturnsCapacityErrorWhenAllAuthsAreFull(t *testing.T) {
	t.Parallel()

	const (
		provider = "gemini"
		model    = "gemini-2.5-pro"
		authA    = "auth-a-capacity"
	)
	executor := &inflightLimitTestExecutor{
		provider:       provider,
		executeStarted: map[string]chan struct{}{authA: make(chan struct{}, 1)},
		executeHold:    map[string]chan struct{}{authA: make(chan struct{})},
		streamStarted:  make(map[string]chan struct{}),
		streamCurrent:  make(map[string]chan cliproxyexecutor.StreamChunk),
	}
	manager := newInflightLimitTestManager(
		t,
		&FillFirstSelector{},
		provider,
		model,
		1,
		executor,
		&Auth{ID: authA, Provider: provider},
	)

	req := cliproxyexecutor.Request{Model: model}
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
		firstDone <- err
	}()

	waitForSignal(t, executor.executeStarted[authA], "auth-a 满载保护")

	_, err := manager.Execute(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("第二次 Execute() error = nil, want capacity error")
	}
	var capacityErr *authCapacityError
	if !errors.As(err, &capacityErr) {
		t.Fatalf("第二次 Execute() error = %T, want *authCapacityError", err)
	}
	if capacityErr.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("capacityErr.StatusCode() = %d, want %d", capacityErr.StatusCode(), http.StatusServiceUnavailable)
	}
	if got := capacityErr.Headers().Get("Retry-After"); got != "1" {
		t.Fatalf("capacityErr.Headers().Get(Retry-After) = %q, want %q", got, "1")
	}

	close(executor.executeHold[authA])
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("第一次 Execute() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("第一次 Execute() 未在释放后完成")
	}
}

func TestManagerExecute_PinnedAuthAtCapacityDoesNotFallback(t *testing.T) {
	t.Parallel()

	const (
		provider = "gemini"
		model    = "gemini-2.5-pro"
		authA    = "auth-a-pinned"
		authB    = "auth-b-pinned"
	)
	executor := &inflightLimitTestExecutor{
		provider:       provider,
		executeStarted: map[string]chan struct{}{authA: make(chan struct{}, 1)},
		executeHold:    map[string]chan struct{}{authA: make(chan struct{})},
		streamStarted:  make(map[string]chan struct{}),
		streamCurrent:  make(map[string]chan cliproxyexecutor.StreamChunk),
	}
	manager := newInflightLimitTestManager(
		t,
		&FillFirstSelector{},
		provider,
		model,
		1,
		executor,
		&Auth{ID: authA, Provider: provider},
		&Auth{ID: authB, Provider: provider},
	)

	req := cliproxyexecutor.Request{Model: model}
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Execute(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
		firstDone <- err
	}()

	waitForSignal(t, executor.executeStarted[authA], "pinned auth 首次执行")

	_, err := manager.Execute(context.Background(), []string{provider}, req, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey: authA,
		},
	})
	if err == nil {
		t.Fatal("pinned Execute() error = nil, want capacity error")
	}
	var capacityErr *authCapacityError
	if !errors.As(err, &capacityErr) {
		t.Fatalf("pinned Execute() error = %T, want *authCapacityError", err)
	}

	close(executor.executeHold[authA])
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("第一次 Execute() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("第一次 Execute() 未在释放后完成")
	}
}

func TestManagerExecuteStream_ReleasesAuthSlotAfterStreamEnds(t *testing.T) {
	t.Parallel()

	const (
		provider = "gemini"
		model    = "gemini-2.5-pro"
		authA    = "auth-a-stream"
	)
	executor := &inflightLimitTestExecutor{
		provider:       provider,
		executeStarted: make(map[string]chan struct{}),
		executeHold:    make(map[string]chan struct{}),
		streamStarted: map[string]chan struct{}{
			authA: make(chan struct{}, 1),
		},
		streamCurrent: make(map[string]chan cliproxyexecutor.StreamChunk),
	}
	manager := newInflightLimitTestManager(
		t,
		&FillFirstSelector{},
		provider,
		model,
		1,
		executor,
		&Auth{ID: authA, Provider: provider},
	)

	req := cliproxyexecutor.Request{Model: model}
	streamResult, err := manager.ExecuteStream(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("第一次 ExecuteStream() error = %v", err)
	}
	waitForSignal(t, executor.streamStarted[authA], "auth-a 流式执行")
	waitForAuthInflight(t, manager, authA, 1)

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range streamResult.Chunks {
		}
	}()

	_, err = manager.ExecuteStream(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("第二次 ExecuteStream() error = nil, want capacity error")
	}
	var capacityErr *authCapacityError
	if !errors.As(err, &capacityErr) {
		t.Fatalf("第二次 ExecuteStream() error = %T, want *authCapacityError", err)
	}

	current := executor.currentStream(authA)
	if current == nil {
		t.Fatal("当前 stream channel = nil")
	}
	close(current)

	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("第一次 stream 未在关闭后结束")
	}
	waitForAuthInflight(t, manager, authA, 0)

	nextResult, err := manager.ExecuteStream(context.Background(), []string{provider}, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("第三次 ExecuteStream() error = %v", err)
	}
	nextCurrent := executor.currentStream(authA)
	if nextCurrent == nil {
		t.Fatal("第三次 stream channel = nil")
	}
	close(nextCurrent)
	for range nextResult.Chunks {
	}
}
