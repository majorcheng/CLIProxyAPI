package auth

import (
	"strconv"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// maxAuthPriorityFromClientPolicy 解析请求级别的 auth priority 上限。
func maxAuthPriorityFromClientPolicy(meta map[string]any) (int, bool) {
	if len(meta) == 0 {
		return 0, false
	}
	raw, ok := meta[cliproxyexecutor.MaxAuthPriorityMetadataKey]
	if !ok {
		return 0, false
	}
	switch typed := raw.(type) {
	case int:
		return typed, true
	case int8:
		return int(typed), true
	case int16:
		return int(typed), true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float32:
		return int(typed), true
	case float64:
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// maxAuthPriorityBoundByClientPolicy 返回本次请求是否显式带了 auth priority 上限。
func maxAuthPriorityBoundByClientPolicy(meta map[string]any) bool {
	_, ok := maxAuthPriorityFromClientPolicy(meta)
	return ok
}

// shouldSkipAuthByClientPolicy 统一收口 client api-key 触发的请求期过滤：
// 1) 若设置了 max-priority，则禁止命中 priority > max-priority 的 auth；
// 2) 保留已有的 priority=0 OAuth 网络抖动回退逻辑。
func shouldSkipAuthByClientPolicy(meta map[string]any, auth *Auth) bool {
	if auth == nil {
		return false
	}
	if limit, ok := maxAuthPriorityFromClientPolicy(meta); ok && authPriority(auth) > limit {
		return true
	}
	return shouldSkipPriorityZeroOAuthAuth(meta, auth)
}
