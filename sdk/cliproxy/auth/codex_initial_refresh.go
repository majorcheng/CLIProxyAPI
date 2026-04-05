package auth

import "strings"

const codexInitialRefreshPendingMetadataKey = "cli_proxy_codex_initial_refresh_pending"

// MarkCodexInitialRefreshPendingForNewFile 仅在“新的 Codex 文件型 auth 首次入池”时
// 给 metadata 打上一次性初始 refresh 待处理标记。
// 该标记会随 auth JSON 一起落盘，用来保证：
// 1. 新文件首次提交后可以立即触发一次 RT 交换；
// 2. 若第一次交换因网络/5xx 等瞬态问题失败，重启后仍能继续重试；
// 3. refresh 成功或终态失败后清除标记，避免后续普通 update / 重启再次触发。
func MarkCodexInitialRefreshPendingForNewFile(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || !authHasRefreshToken(auth) {
		return false
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any, 1)
	}
	if pending, _ := auth.Metadata[codexInitialRefreshPendingMetadataKey].(bool); pending {
		return false
	}
	auth.Metadata[codexInitialRefreshPendingMetadataKey] = true
	return true
}

// CodexInitialRefreshPending 返回当前 auth 是否仍带有“新文件初始 refresh 待处理”标记。
func CodexInitialRefreshPending(auth *Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	pending, _ := auth.Metadata[codexInitialRefreshPendingMetadataKey].(bool)
	return pending
}

// ClearCodexInitialRefreshPending 在初始 refresh 成功或命中终态失败后清理待处理标记。
func ClearCodexInitialRefreshPending(auth *Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	if !CodexInitialRefreshPending(auth) {
		return false
	}
	delete(auth.Metadata, codexInitialRefreshPendingMetadataKey)
	return true
}
