package management

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const codexWeeklyQuotaRecoveryThresholdPercent = 30.0
const codexUsageWeeklyWindowSeconds = 7 * 24 * 60 * 60
const codexUsageHost = "chatgpt.com"
const codexUsagePath = "/backend-api/wham/usage"

// maybeRecoverCodexQuotaCooldown 复用现有 management api-call -> wham/usage 查询闭环：
// 当刚刷出的 Codex 主 code 周额度剩余超过阈值时，顺手清理当前 auth 残留的 429 冷却。
func (h *Handler) maybeRecoverCodexQuotaCooldown(ctx context.Context, auth *coreauth.Auth, req apiCallRequest, parsedURL *url.URL, statusCode int, body []byte) {
	if h == nil || auth == nil || parsedURL == nil || statusCode < 200 || statusCode >= 300 {
		return
	}
	if !isCodexUsageRefreshRequest(auth, req, parsedURL) {
		return
	}

	remainingPercent, ok := codexWeeklyRemainingPercent(body)
	if !ok || remainingPercent <= codexWeeklyQuotaRecoveryThresholdPercent {
		return
	}
	manager := h.currentAuthManager()
	if manager == nil {
		return
	}
	_, _, _ = manager.ClearAuthQuotaCooldown(ctx, auth.ID)
}

// isCodexUsageRefreshRequest 只识别管理面现有“单凭证刷新 Codex 配额”这类 usage 查询请求。
func isCodexUsageRefreshRequest(auth *coreauth.Auth, req apiCallRequest, parsedURL *url.URL) bool {
	if auth == nil || parsedURL == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(parsedURL.Hostname()), codexUsageHost) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(parsedURL.Path), codexUsagePath) {
		return false
	}
	return codexUsageRequestMatchesAuth(auth, req.Header)
}

// codexUsageRequestMatchesAuth 进一步校验当前 usage 查询确实对应这把 auth 的账户标识。
func codexUsageRequestMatchesAuth(auth *coreauth.Auth, headers map[string]string) bool {
	if auth == nil {
		return false
	}
	accountID := strings.TrimSpace(stringValue(auth.Metadata, "account_id"))
	if accountID == "" {
		return false
	}
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "Chatgpt-Account-Id") {
			return strings.EqualFold(strings.TrimSpace(value), accountID)
		}
	}
	return false
}

// codexWeeklyRemainingPercent 返回主 code 周窗口的剩余额度百分比。
func codexWeeklyRemainingPercent(body []byte) (float64, bool) {
	usedPercent, ok := codexWeeklyUsedPercent(body)
	if !ok {
		return 0, false
	}
	if usedPercent < 0 {
		usedPercent = 0
	}
	if usedPercent > 100 {
		usedPercent = 100
	}
	return 100 - usedPercent, true
}

// codexWeeklyUsedPercent 按管理中心现有口径读取主 code 周窗口的 used_percent。
func codexWeeklyUsedPercent(body []byte) (float64, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0, false
	}
	weeklyWindow := codexWeeklyWindowNode(gjson.ParseBytes(body))
	if !weeklyWindow.Exists() {
		return 0, false
	}
	if value := codexWindowNumberField(weeklyWindow, "used_percent", "usedPercent"); value != nil {
		return *value, true
	}
	if allowed := weeklyWindow.Get("allowed"); allowed.Exists() && !allowed.Bool() {
		return 100, true
	}
	limitReached := weeklyWindow.Get("limit_reached")
	if !limitReached.Exists() {
		limitReached = weeklyWindow.Get("limitReached")
	}
	if limitReached.Exists() && limitReached.Bool() {
		return 100, true
	}
	return 0, false
}

// codexWeeklyWindowNode 优先按 limit_window_seconds=604800 识别周窗口。
func codexWeeklyWindowNode(payload gjson.Result) gjson.Result {
	limitInfo := payload.Get("rate_limit")
	if !limitInfo.Exists() {
		limitInfo = payload.Get("rateLimit")
	}
	if !limitInfo.Exists() {
		return gjson.Result{}
	}
	primaryWindow := limitInfo.Get("primary_window")
	if !primaryWindow.Exists() {
		primaryWindow = limitInfo.Get("primaryWindow")
	}
	secondaryWindow := limitInfo.Get("secondary_window")
	if !secondaryWindow.Exists() {
		secondaryWindow = limitInfo.Get("secondaryWindow")
	}
	if codexWindowSeconds(primaryWindow) == codexUsageWeeklyWindowSeconds {
		return primaryWindow
	}
	if codexWindowSeconds(secondaryWindow) == codexUsageWeeklyWindowSeconds {
		return secondaryWindow
	}
	if secondaryWindow.Exists() {
		return secondaryWindow
	}
	if primaryWindow.Exists() {
		return primaryWindow
	}
	return gjson.Result{}
}

// codexWindowSeconds 统一读取 usage window 的时长字段。
func codexWindowSeconds(window gjson.Result) int64 {
	if !window.Exists() {
		return 0
	}
	if value := codexWindowNumberField(window, "limit_window_seconds", "limitWindowSeconds"); value != nil {
		return int64(*value)
	}
	return 0
}

// codexWindowNumberField 兼容 snake/camel 两套字段名以及字符串数字。
func codexWindowNumberField(window gjson.Result, snake, camel string) *float64 {
	if !window.Exists() {
		return nil
	}
	raw := window.Get(snake)
	if !raw.Exists() {
		raw = window.Get(camel)
	}
	if !raw.Exists() {
		return nil
	}
	switch raw.Type {
	case gjson.Number:
		value := raw.Float()
		return &value
	case gjson.String:
		trimmed := strings.TrimSpace(raw.String())
		if trimmed == "" {
			return nil
		}
		parsed := gjson.Parse(trimmed)
		if parsed.Type != gjson.Number {
			return nil
		}
		value := parsed.Float()
		return &value
	default:
		return nil
	}
}
