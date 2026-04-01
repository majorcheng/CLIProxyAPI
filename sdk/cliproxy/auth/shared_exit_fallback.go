package auth

import (
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const skipPriorityZeroOAuthMetadataKey = "__skip_priority_zero_oauth_after_network_jitter"

// priorityZeroOAuthSkipped reports whether the current request has already
// observed a shared-exit network jitter on the special priority=0 OAuth layer.
// 这个标记只存在于单次请求的执行 metadata 中，用来阻止同层 token 继续横向重试。
func priorityZeroOAuthSkipped(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[skipPriorityZeroOAuthMetadataKey]
	if !ok {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

// markPriorityZeroOAuthSkipped records that the current request should skip the
// special priority=0 OAuth/token layer from now on.
func markPriorityZeroOAuthSkipped(meta map[string]any) {
	if meta == nil {
		return
	}
	meta[skipPriorityZeroOAuthMetadataKey] = true
}

// isPriorityZeroOAuthSharedExitAuth reports whether the auth belongs to the
// special shared-exit token layer: priority=0 + OAuth account.
func isPriorityZeroOAuthSharedExitAuth(auth *Auth) bool {
	if auth == nil || authPriority(auth) != 0 {
		return false
	}
	accountType, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(accountType), "oauth")
}

// shouldSkipPriorityZeroOAuthAuth filters out the special priority=0 OAuth
// layer once the current request has already seen a shared-exit network jitter.
func shouldSkipPriorityZeroOAuthAuth(meta map[string]any, auth *Auth) bool {
	if !priorityZeroOAuthSkipped(meta) {
		return false
	}
	return isPriorityZeroOAuthSharedExitAuth(auth)
}

// isSharedExitNetworkJitterError matches only transport / connect-layer errors.
// 这里故意不把 408 / 5xx / 429 算进来，因为这些仍应保持现有重试与冷却语义。
func isSharedExitNetworkJitterError(err error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromError(err) != 0 {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}

	signals := []string{
		"dial tcp",
		"connect: connection timed out",
		"i/o timeout",
		"context deadline exceeded",
		"deadline exceeded",
		"timeout",
		"connection refused",
		"connection reset",
		"unexpected eof",
		"eof",
	}
	for _, signal := range signals {
		if strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

// markPriorityZeroOAuthSkippedOnNetworkJitter activates the request-scoped
// fallback once a priority=0 OAuth/token auth fails with shared-exit network jitter.
func markPriorityZeroOAuthSkippedOnNetworkJitter(meta map[string]any, auth *Auth, err error) bool {
	if meta == nil || !isPriorityZeroOAuthSharedExitAuth(auth) || !isSharedExitNetworkJitterError(err) {
		return false
	}
	markPriorityZeroOAuthSkipped(meta)
	return true
}

func sharedExitPriorityZeroOAuthNetworkJitterFallbackEnabled(cfg *internalconfig.Config) bool {
	if cfg == nil {
		return false
	}
	return cfg.SharedExitPriorityZeroOAuthNetworkJitterFallback
}
