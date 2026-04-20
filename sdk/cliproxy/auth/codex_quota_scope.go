package auth

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// codexFree429BlocksAllModels 统一收口 Codex free 账号的 429 共享语义。
// 这类账号的额度状态属于 token 级，因此一次 429 会同时阻断该 token 的所有模型。
func codexFree429BlocksAllModels(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return AuthChatGPTPlanType(auth) == "free"
}

// codexFreeSharedQuotaRetryAfter 返回 free Codex 账号对所有模型共享的冷却截止时间。
func codexFreeSharedQuotaRetryAfter(auth *Auth, quota QuotaState, now time.Time) (time.Time, bool) {
	if !codexFree429BlocksAllModels(auth) || !quota.Exceeded {
		return time.Time{}, false
	}
	next := quota.NextRecoverAt
	if next.IsZero() || !next.After(now) {
		return time.Time{}, false
	}
	return next, true
}

// codexFreeSharedQuotaModelIDs 返回 free Codex 账号共享 quota 时需要联动的全部模型 ID。
// 这里优先读取 registry 里的真实模型目录，保证运行态冷却与 `/v1/models` 可见性一致。
func codexFreeSharedQuotaModelIDs(authID, fallbackModel string) []string {
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
