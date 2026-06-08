package auth

import (
	"context"
	"time"
)

const (
	// 后台 auto-refresh 对 RT 交换做串行发车，避免同一批到期凭证突发并发请求上游。
	refreshDispatchInterval = time.Second
)

// runRefreshJob 串行发起后台 RT 交换，并保证两次启动至少间隔 refreshDispatchInterval。
func (l *authAutoRefreshLoop) runRefreshJob(ctx context.Context, authID string) bool {
	if l == nil || l.manager == nil || authID == "" {
		return false
	}
	l.dispatchMu.Lock()
	defer l.dispatchMu.Unlock()
	if !l.waitDispatchTurnLocked(ctx) {
		return false
	}
	l.manager.refreshAuth(ctx, authID)
	return true
}

// beginDispatching 标记 auth 已进入后台发车队列或执行阶段，返回 false 表示已有待发车任务。
func (l *authAutoRefreshLoop) beginDispatching(authID string) bool {
	if l == nil || authID == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.dispatching[authID]; ok {
		return false
	}
	l.dispatching[authID] = struct{}{}
	return true
}

// finishDispatching 在后台 refresh job 结束或取消后释放待发车标记。
func (l *authAutoRefreshLoop) finishDispatching(authID string) {
	if l == nil || authID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.dispatching, authID)
}

// isDispatching 判断 auth 是否已在后台 jobs 队列中等待或正在执行 refresh。
func (l *authAutoRefreshLoop) isDispatching(authID string) bool {
	if l == nil || authID == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.dispatching[authID]
	return ok
}

// waitDispatchTurnLocked 只在持有 dispatchMu 时调用，确保等待和刷新启动点不可并发穿透。
func (l *authAutoRefreshLoop) waitDispatchTurnLocked(ctx context.Context) bool {
	interval := l.dispatchInterval
	if interval <= 0 {
		interval = refreshDispatchInterval
	}
	if !l.lastDispatchAt.IsZero() {
		wait := time.Until(l.lastDispatchAt.Add(interval))
		if wait > 0 && !sleepWithContext(ctx, wait) {
			return false
		}
	}
	l.lastDispatchAt = time.Now()
	return true
}

// sleepWithContext 让调度等待能被 StopAutoRefresh 的 context 取消及时打断。
func sleepWithContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
