package auth

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// codexFreeSharesModelState 统一收口 Codex free 账号的 token 级模型状态语义。
// free 套餐不再把同一个 token 的不同模型视作独立额度/可用性单元。
func codexFreeSharesModelState(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return AuthChatGPTPlanType(auth) == "free"
}

// codexFree429BlocksAllModels 保留旧 helper 名称，避免 quota 调用点重复判断套餐。
func codexFree429BlocksAllModels(auth *Auth) bool {
	return codexFreeSharesModelState(auth)
}

// codexFreeSharedQuotaRetryAfter 返回 free Codex 账号对所有模型共享的冷却截止时间。
func codexFreeSharedQuotaRetryAfter(auth *Auth, quota QuotaState, now time.Time) (time.Time, bool) {
	if !codexFreeSharesModelState(auth) || !quota.Exceeded {
		return time.Time{}, false
	}
	next := quota.NextRecoverAt
	if next.IsZero() || !next.After(now) {
		return time.Time{}, false
	}
	return next, true
}

// codexFreeSharedModelIDs 返回 free Codex 账号 token 级状态需要联动的全部模型 ID。
// 这里优先读取 registry 里的真实模型目录，保证运行态冷却与 `/v1/models` 可见性一致。
func codexFreeSharedModelIDs(authID, fallbackModel string) []string {
	seen := make(map[string]struct{})
	models := registry.GetGlobalRegistry().GetModelsForClient(strings.TrimSpace(authID))
	out := make([]string, 0, len(models)+1)
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		if _, ok := seen[modelID]; ok {
			continue
		}
		seen[modelID] = struct{}{}
		out = append(out, modelID)
	}
	fallbackModel = strings.TrimSpace(fallbackModel)
	if fallbackModel != "" {
		if _, ok := seen[fallbackModel]; !ok {
			out = append(out, fallbackModel)
		}
	}
	return out
}

// codexFreeSharedQuotaModelIDs 保留 quota 语义调用点的可读性。
func codexFreeSharedQuotaModelIDs(authID, fallbackModel string) []string {
	return codexFreeSharedModelIDs(authID, fallbackModel)
}

// codexFreeSharedBlockForAuth 判断 free token 是否已被任意模型状态整体阻断。
// 这里先看 auth 聚合态，再扫历史 model state，避免旧快照只记录单模型时漏选。
func codexFreeSharedBlockForAuth(auth *Auth, now time.Time) (bool, blockReason, time.Time) {
	if !codexFreeSharesModelState(auth) {
		return false, blockReasonNone, time.Time{}
	}
	if blocked, reason, next := codexFreeSharedBlockFromAuthState(auth, now); blocked {
		return true, reason, next
	}
	return codexFreeSharedBlockFromModelStates(auth.ModelStates, now)
}

// codexFreeSharedBlockFromAuthState 从 auth 聚合态读取 token 级阻断信号。
func codexFreeSharedBlockFromAuthState(auth *Auth, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return false, blockReasonNone, time.Time{}
	}
	if next, ok := codexFreeSharedQuotaRetryAfter(auth, auth.Quota, now); ok {
		return true, blockReasonCooldown, next
	}
	if !codexFreeTokenScopedFailureHTTPStatus(currentPersistableFailureHTTPStatus(auth)) {
		return false, blockReasonNone, time.Time{}
	}
	if !auth.Unavailable || auth.NextRetryAfter.IsZero() || !auth.NextRetryAfter.After(now) {
		return false, blockReasonNone, time.Time{}
	}
	next := auth.NextRetryAfter
	if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
		next = auth.Quota.NextRecoverAt
	}
	if auth.Quota.Exceeded {
		return true, blockReasonCooldown, next
	}
	return true, blockReasonOther, next
}

// codexFreeSharedBlockFromModelStates 汇总所有单模型状态里的 token 级阻断信号。
func codexFreeSharedBlockFromModelStates(states map[string]*ModelState, now time.Time) (bool, blockReason, time.Time) {
	blocked := false
	reason := blockReasonNone
	next := time.Time{}
	for _, state := range states {
		stateBlocked, stateReason, stateNext := codexFreeSharedBlockFromModelState(state, now)
		if !stateBlocked {
			continue
		}
		blocked, reason, next = mergeCodexFreeSharedBlock(blocked, reason, next, stateReason, stateNext)
	}
	return blocked, reason, next
}

// codexFreeSharedBlockFromModelState 判断单个 model state 是否应扩散为 free token 阻断。
func codexFreeSharedBlockFromModelState(state *ModelState, now time.Time) (bool, blockReason, time.Time) {
	if state == nil {
		return false, blockReasonNone, time.Time{}
	}
	if next := codexFreeSharedQuotaNext(state.Quota, now); !next.IsZero() {
		return true, blockReasonCooldown, next
	}
	if !codexFreeTokenScopedFailureHTTPStatus(currentPersistableModelFailureHTTPStatus(state)) {
		return false, blockReasonNone, time.Time{}
	}
	if !state.Unavailable || state.NextRetryAfter.IsZero() || !state.NextRetryAfter.After(now) {
		return false, blockReasonNone, time.Time{}
	}
	next := state.NextRetryAfter
	if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
		next = state.Quota.NextRecoverAt
	}
	if state.Quota.Exceeded {
		return true, blockReasonCooldown, next
	}
	return true, blockReasonOther, next
}

// codexFreeTokenScopedFailureHTTPStatus 只认会影响整个 free token 的账号级错误。
func codexFreeTokenScopedFailureHTTPStatus(statusCode int) bool {
	switch NormalizePersistableFailureHTTPStatus(statusCode) {
	case 401, 402, 403, 429:
		return true
	default:
		return false
	}
}

// codexFreeSharedQuotaNext 返回仍有效的 quota 恢复时间。
func codexFreeSharedQuotaNext(quota QuotaState, now time.Time) time.Time {
	if !quota.Exceeded || quota.NextRecoverAt.IsZero() || !quota.NextRecoverAt.After(now) {
		return time.Time{}
	}
	return quota.NextRecoverAt
}

// mergeCodexFreeSharedBlock 合并多模型阻断信号，优先保留最早可恢复时间。
func mergeCodexFreeSharedBlock(blocked bool, reason blockReason, next time.Time, incomingReason blockReason, incomingNext time.Time) (bool, blockReason, time.Time) {
	if !blocked {
		return true, incomingReason, incomingNext
	}
	if codexFreeBlockReasonRank(incomingReason) > codexFreeBlockReasonRank(reason) {
		reason = incomingReason
	}
	if reason == blockReasonDisabled {
		return true, reason, time.Time{}
	}
	if next.IsZero() || (!incomingNext.IsZero() && incomingNext.Before(next)) {
		next = incomingNext
	}
	return true, reason, next
}

// codexFreeBlockReasonRank 定义共享阻断原因优先级，永久禁用高于临时冷却。
func codexFreeBlockReasonRank(reason blockReason) int {
	switch reason {
	case blockReasonDisabled:
		return 3
	case blockReasonCooldown:
		return 2
	case blockReasonOther:
		return 1
	default:
		return 0
	}
}
