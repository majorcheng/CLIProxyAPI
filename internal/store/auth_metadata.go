package store

import cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

// applyDisabledMetadata 把持久化 metadata 里的禁用标记恢复到运行态 Auth 字段。
func applyDisabledMetadata(auth *cliproxyauth.Auth, metadata map[string]any) {
	if auth == nil || len(metadata) == 0 {
		return
	}
	disabled, ok := metadata["disabled"].(bool)
	if !ok || !disabled {
		return
	}
	auth.Disabled = true
	auth.Status = cliproxyauth.StatusDisabled
	auth.FailureHTTPStatus = 0
}
