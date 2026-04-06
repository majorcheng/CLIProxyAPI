package auth

import "context"

type skipPersistContextKey struct{}
type forcePersistContextKey struct{}

// WithSkipPersist returns a derived context that disables persistence for Manager Update/Register calls.
// It is intended for code paths that are reacting to file watcher events, where the file on disk is
// already the source of truth and persisting again would create a write-back loop.
func WithSkipPersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, skipPersistContextKey{}, true)
}

// WithoutSkipPersist returns a derived context that re-enables persistence even when the
// parent context came from a watcher path using WithSkipPersist.
//
// 这主要用于“由 watcher 触发、但后续又必须把新凭证落盘”的后台任务，例如 Codex 初始 refresh。
// 如果 refresh 成功后仍沿用 skipPersist，上游轮转出的新 refresh_token 只会留在内存里，
// 重启后又会从磁盘读回旧 RT，进而再次触发 refresh_token_reused。
func WithoutSkipPersist(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, forcePersistContextKey{}, true)
}

func shouldSkipPersist(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v := ctx.Value(forcePersistContextKey{}); v != nil {
		if forced, ok := v.(bool); ok && forced {
			return false
		}
	}
	v := ctx.Value(skipPersistContextKey{})
	enabled, ok := v.(bool)
	return ok && enabled
}
