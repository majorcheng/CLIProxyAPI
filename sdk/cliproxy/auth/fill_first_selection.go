package auth

import "time"

// fillFirstSelection 汇总 fill-first 选择结果。
// 这里刻意只记录“稳定队列上的首个可用账号”与冷却摘要，
// 让 Codex free 在不同模型请求下共享同一条排序语义，
// 但仍允许模型级失败只在当前模型请求里被局部跳过。
type fillFirstSelection struct {
	picked        *Auth
	total         int
	cooldownCount int
	earliestRetry time.Time
}

func scanFillFirstCandidates(auths []*Auth, model string, preferWebsocket bool, now time.Time, allow func(*Auth) bool) fillFirstSelection {
	result := fillFirstSelection{}
	bestPriority := 0
	hasReady := false
	var bestAll *Auth
	var bestWebsocket *Auth

	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		if allow != nil && !allow(candidate) {
			continue
		}
		result.total++

		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if blocked {
			if reason == blockReasonCooldown {
				result.cooldownCount++
				if !next.IsZero() && (result.earliestRetry.IsZero() || next.Before(result.earliestRetry)) {
					result.earliestRetry = next
				}
			}
			continue
		}

		priority := authPriority(candidate)
		if !hasReady || priority > bestPriority {
			bestPriority = priority
			bestAll = candidate
			bestWebsocket = nil
			if authWebsocketsEnabled(candidate) {
				bestWebsocket = candidate
			}
			hasReady = true
			continue
		}
		if priority != bestPriority {
			continue
		}
		if firstRegisteredAtLess(candidate, bestAll) {
			bestAll = candidate
		}
		if authWebsocketsEnabled(candidate) && (bestWebsocket == nil || firstRegisteredAtLess(candidate, bestWebsocket)) {
			bestWebsocket = candidate
		}
	}

	if preferWebsocket && bestWebsocket != nil {
		result.picked = bestWebsocket
		return result
	}
	result.picked = bestAll
	return result
}

func fillFirstUnavailableError(provider, model string, now time.Time, result fillFirstSelection) error {
	if result.total == 0 {
		return &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if result.cooldownCount == result.total && !result.earliestRetry.IsZero() {
		providerForError := provider
		if providerForError == "mixed" {
			providerForError = ""
		}
		resetIn := result.earliestRetry.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return newModelCooldownError(model, providerForError, resetIn)
	}
	return &Error{Code: "auth_unavailable", Message: "no auth available"}
}
