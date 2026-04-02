package auth

import (
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// priorityZeroDisallowedByClientPolicy 判断本次请求是否被标记为
// “禁止命中所有 priority=0 auth”。
func priorityZeroDisallowedByClientPolicy(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.DisallowPriorityZeroAuthMetadataKey]
	if !ok {
		return false
	}
	switch typed := raw.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

// shouldSkipPriorityZeroAuth 统一收口 priority=0 相关的请求期过滤：
// 1) 指定 client api-key 明确禁止命中 priority=0；
// 2) 已有的 priority=0 OAuth 网络抖动回退逻辑。
func shouldSkipPriorityZeroAuth(meta map[string]any, auth *Auth) bool {
	if auth == nil {
		return false
	}
	if priorityZeroDisallowedByClientPolicy(meta) && authPriority(auth) == 0 {
		return true
	}
	return shouldSkipPriorityZeroOAuthAuth(meta, auth)
}
