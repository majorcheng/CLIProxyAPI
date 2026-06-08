package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type autoRefreshStaggerExecutor struct {
	schedulerProviderTestExecutor
	delay time.Duration
	done  chan struct{}
	want  int

	mu          sync.Mutex
	starts      []time.Time
	inFlight    int
	maxInFlight int
}

// Refresh 记录每次后台刷新启动时间，并用短延迟暴露并发启动问题。
func (e *autoRefreshStaggerExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.recordStart()
	defer e.recordDone()
	if e.delay <= 0 {
		return auth, nil
	}
	select {
	case <-ctx.Done():
		return auth, ctx.Err()
	case <-time.After(e.delay):
		return auth, nil
	}
}

// recordStart 记录刷新启动点和当前并发数。
func (e *autoRefreshStaggerExecutor) recordStart() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.starts = append(e.starts, time.Now())
	e.inFlight++
	if e.inFlight > e.maxInFlight {
		e.maxInFlight = e.inFlight
	}
}

// recordDone 在达到期望刷新次数后通知测试继续断言。
func (e *autoRefreshStaggerExecutor) recordDone() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inFlight--
	if len(e.starts) < e.want {
		return
	}
	select {
	case <-e.done:
	default:
		close(e.done)
	}
}

// snapshot 返回刷新启动时间和最大并发数的稳定快照。
func (e *autoRefreshStaggerExecutor) snapshot() ([]time.Time, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	starts := append([]time.Time(nil), e.starts...)
	return starts, e.maxInFlight
}

// TestAutoRefreshLoop_StaggersDueRefreshJobs 验证同一批到期 RT 会串行且错峰启动。
func TestAutoRefreshLoop_StaggersDueRefreshJobs(t *testing.T) {
	const authCount = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	provider := "stagger-refresh"
	exec := &autoRefreshStaggerExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: provider},
		delay:                         40 * time.Millisecond,
		done:                          make(chan struct{}),
		want:                          authCount,
	}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	for i := 0; i < authCount; i++ {
		registerStaggerRefreshAuth(t, ctx, manager, provider, i)
	}

	loop := newAuthAutoRefreshLoop(manager, 15*time.Minute, authCount)
	loop.dispatchInterval = 20 * time.Millisecond
	loop.rebuild(time.Now())
	go loop.run(ctx)

	select {
	case <-exec.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for staggered refresh jobs")
	}

	assertStaggeredRefreshStarts(t, exec, authCount, loop.dispatchInterval)
}

// TestAutoRefreshLoop_DoesNotRequeueDispatchingAuth 验证待发车 job 不会因 pending backoff 过期重复入队。
func TestAutoRefreshLoop_DoesNotRequeueDispatchingAuth(t *testing.T) {
	ctx := context.Background()
	provider := "pending-dispatch"
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: provider})
	registerStaggerRefreshAuth(t, ctx, manager, provider, 0)

	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	loop := newAuthAutoRefreshLoop(manager, 15*time.Minute, 1)
	loop.rebuild(now)
	loop.handleDue(ctx, now)
	if got := len(loop.jobs); got != 1 {
		t.Fatalf("initial queued refresh jobs = %d, want 1", got)
	}

	loop.applyDirty(now)
	loop.handleDue(ctx, now.Add(refreshPendingBackoff+time.Second))
	if got := len(loop.jobs); got != 1 {
		t.Fatalf("queued refresh jobs after pending backoff = %d, want 1", got)
	}
	if !loop.isDispatching("stagger-refresh-0") {
		t.Fatal("expected auth to remain dispatching while queued job has not run")
	}
}

// registerStaggerRefreshAuth 注册一个已经满足刷新条件的测试凭证。
func registerStaggerRefreshAuth(t *testing.T, ctx context.Context, manager *Manager, provider string, index int) {
	t.Helper()
	auth := &Auth{
		ID:              fmt.Sprintf("stagger-refresh-%d", index),
		Provider:        provider,
		LastRefreshedAt: time.Now().Add(-time.Minute),
		Metadata: map[string]any{
			"email":                    fmt.Sprintf("stagger-%d@example.com", index),
			"refresh_interval_seconds": 1,
		},
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth %d: %v", index, err)
	}
}

// assertStaggeredRefreshStarts 断言刷新没有并发，且相邻启动点满足调度间隔。
func assertStaggeredRefreshStarts(t *testing.T, exec *autoRefreshStaggerExecutor, wantCount int, interval time.Duration) {
	t.Helper()
	starts, maxInFlight := exec.snapshot()
	if maxInFlight != 1 {
		t.Fatalf("max in-flight refresh jobs = %d, want 1", maxInFlight)
	}
	if len(starts) != wantCount {
		t.Fatalf("refresh start count = %d, want %d", len(starts), wantCount)
	}
	for i := 1; i < len(starts); i++ {
		if delta := starts[i].Sub(starts[i-1]); delta < interval {
			t.Fatalf("refresh start delta[%d] = %s, want at least %s", i, delta, interval)
		}
	}
}
