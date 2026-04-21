package auth

import (
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type runtimeFailureKind uint8

const (
	runtimeFailureNone runtimeFailureKind = iota
	runtimeFailureQuota
	runtimeFailureOther
)

// authHasQuotaCooldown 判断 auth 聚合态是否仍有 quota/429 冷却痕迹。
func authHasQuotaCooldown(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if quotaCooldownStatePresent(auth.Quota) {
		return true
	}
	if NormalizePersistableFailureHTTPStatus(auth.FailureHTTPStatus) == 429 {
		return true
	}
	if errorCurrentFailureKind(auth.LastError) == runtimeFailureQuota {
		return true
	}
	return statusMessageHasQuotaCooldown(auth.StatusMessage)
}

// modelStateHasQuotaCooldown 判断单个 model state 是否带 quota/429 冷却痕迹。
func modelStateHasQuotaCooldown(state *ModelState) bool {
	if state == nil {
		return false
	}
	if quotaCooldownStatePresent(state.Quota) {
		return true
	}
	if NormalizePersistableFailureHTTPStatus(state.FailureHTTPStatus) == 429 {
		return true
	}
	if errorCurrentFailureKind(state.LastError) == runtimeFailureQuota {
		return true
	}
	return statusMessageHasQuotaCooldown(state.StatusMessage)
}

// authCurrentFailureKind 给当前 auth 聚合态做故障类型分类。
func authCurrentFailureKind(auth *Auth, now time.Time) runtimeFailureKind {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return runtimeFailureOther
	}
	if kind := errorCurrentFailureKind(auth.LastError); kind != runtimeFailureNone {
		return kind
	}
	return failureKindFromStateSignals(
		NormalizePersistableFailureHTTPStatus(auth.FailureHTTPStatus),
		auth.StatusMessage,
		auth.Unavailable,
		auth.NextRetryAfter,
		auth.Quota,
		now,
	)
}

// modelStateCurrentFailureKind 给单模型运行态做故障类型分类。
func modelStateCurrentFailureKind(state *ModelState, now time.Time) runtimeFailureKind {
	if state == nil || state.Status == StatusDisabled {
		return runtimeFailureOther
	}
	if kind := errorCurrentFailureKind(state.LastError); kind != runtimeFailureNone {
		return kind
	}
	return failureKindFromStateSignals(
		NormalizePersistableFailureHTTPStatus(state.FailureHTTPStatus),
		state.StatusMessage,
		state.Unavailable,
		state.NextRetryAfter,
		state.Quota,
		now,
	)
}

// errorCurrentFailureKind 根据当前错误对象判断活跃失败是否属于 quota。
func errorCurrentFailureKind(err *Error) runtimeFailureKind {
	if err == nil {
		return runtimeFailureNone
	}
	statusCode := statusCodeFromResult(err)
	switch statusCode {
	case 429:
		return runtimeFailureQuota
	case 0:
	default:
		return runtimeFailureOther
	}
	message := strings.TrimSpace(err.Message)
	if statusMessageHasQuotaCooldown(message) {
		return runtimeFailureQuota
	}
	if message != "" || err.Code != "" {
		return runtimeFailureOther
	}
	return runtimeFailureNone
}

// failureKindFromStateSignals 只从当前运行态字段判断故障类型。
func failureKindFromStateSignals(statusCode int, message string, unavailable bool, nextRetry time.Time, quota QuotaState, now time.Time) runtimeFailureKind {
	switch statusCode {
	case 429:
		return runtimeFailureQuota
	case 0:
	default:
		return runtimeFailureOther
	}

	message = strings.TrimSpace(message)
	if message != "" {
		if statusMessageHasQuotaCooldown(message) {
			return runtimeFailureQuota
		}
		return runtimeFailureOther
	}
	if unavailable && !nextRetry.IsZero() && nextRetry.After(now) {
		if quotaCooldownStatePresent(quota) {
			return runtimeFailureQuota
		}
		return runtimeFailureOther
	}
	if quotaCooldownStatePresent(quota) {
		return runtimeFailureQuota
	}
	return runtimeFailureNone
}

// authHasResidualRuntimeSignals 用于判断清理 429 后 auth 是否仍保留其它运行态失败信号。
func authHasResidualRuntimeSignals(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	if authCurrentFailureKind(auth, now) == runtimeFailureOther {
		return true
	}
	return auth.Unavailable && !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now)
}

// quotaCooldownStatePresent 判断 quota 结构体是否仍带运行态内容。
func quotaCooldownStatePresent(quota QuotaState) bool {
	return quota.Exceeded ||
		strings.TrimSpace(quota.Reason) != "" ||
		!quota.NextRecoverAt.IsZero() ||
		quota.BackoffLevel != 0 ||
		quota.StrikeCount != 0
}

// statusMessageHasQuotaCooldown 兼容固定文本与 JSON 错误体两种 quota 文案形态。
func statusMessageHasQuotaCooldown(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		return false
	}
	if strings.EqualFold(message, "quota exhausted") {
		return true
	}
	if NormalizePersistableFailureHTTPStatus(int(gjson.Get(message, "status").Int())) == 429 {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(gjson.Get(message, "error.type").String()), "usage_limit_reached")
}
