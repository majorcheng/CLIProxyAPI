package auth

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

type quotaCooldownClearResult struct {
	changed  bool
	modelIDs []string
}

// ClearAuthQuotaCooldown 会清理单个 auth 上由 429/quota 导致的运行态冷却。
// 这条 helper 只命中 quota 相关字段，保留 401/403/not_found 等其它失败语义。
func (m *Manager) ClearAuthQuotaCooldown(ctx context.Context, id string) (*Auth, bool, error) {
	id = strings.TrimSpace(id)
	if m == nil || id == "" {
		return nil, false, ErrAuthNotFound
	}

	auth, ok := m.GetByID(id)
	if !ok || auth == nil {
		return nil, false, ErrAuthNotFound
	}

	cleared := clearAuthQuotaCooldown(auth, time.Now())
	if !cleared.changed {
		return auth, false, nil
	}

	updated, err := m.Update(ctx, auth)
	if err != nil {
		return nil, false, err
	}
	applyClearedQuotaModelRegistryState(id, auth, cleared.modelIDs)
	if updated == nil {
		updated, _ = m.GetByID(id)
	}
	return updated, true, nil
}

// clearAuthQuotaCooldown 在单个 auth 快照上执行实际清理，并返回受影响模型集合。
func clearAuthQuotaCooldown(auth *Auth, now time.Time) quotaCooldownClearResult {
	if auth == nil {
		return quotaCooldownClearResult{}
	}

	modelIDs := clearQuotaCooldownModelStates(auth.ModelStates, now)
	authChanged := clearAuthQuotaRuntimeState(auth, now)
	if len(modelIDs) == 0 && !authChanged {
		return quotaCooldownClearResult{}
	}

	if len(auth.ModelStates) == 0 {
		normalizeSoloAuthStateAfterQuotaClear(auth, now)
	} else {
		syncAggregatedAuthStateFromModelStates(auth, now)
		projectRepresentativeModelFailure(auth)
		auth.UpdatedAt = now
	}
	return quotaCooldownClearResult{changed: true, modelIDs: modelIDs}
}

// clearQuotaCooldownModelStates 只清理当前仍属于 quota 的模型运行态，保留其它失败原因。
func clearQuotaCooldownModelStates(states map[string]*ModelState, now time.Time) []string {
	if len(states) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(states))
	modelIDs := make([]string, 0, len(states))
	for modelID, state := range states {
		if modelStateCurrentFailureKind(state, now) == runtimeFailureOther || !modelStateHasQuotaCooldown(state) {
			continue
		}
		resetModelState(state, now)
		key := canonicalModelKey(modelID)
		if key == "" {
			key = strings.TrimSpace(modelID)
		}
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		modelIDs = append(modelIDs, key)
	}
	return modelIDs
}

// clearAuthQuotaRuntimeState 清理 auth 聚合态上的 quota/429 冷却字段。
func clearAuthQuotaRuntimeState(auth *Auth, now time.Time) bool {
	if !authHasQuotaCooldown(auth) {
		return false
	}
	if authCurrentFailureKind(auth, now) == runtimeFailureOther {
		return false
	}

	changed := false
	if auth.Unavailable {
		auth.Unavailable = false
		changed = true
	}
	if !auth.NextRetryAfter.IsZero() {
		auth.NextRetryAfter = time.Time{}
		changed = true
	}
	if quotaCooldownStatePresent(auth.Quota) {
		auth.Quota = QuotaState{}
		changed = true
	}
	if currentPersistableFailureHTTPStatus(auth) == 429 {
		auth.FailureHTTPStatus = 0
		changed = true
	}
	if errorCurrentFailureKind(auth.LastError) == runtimeFailureQuota {
		auth.LastError = nil
		changed = true
	}
	if statusMessageHasQuotaCooldown(auth.StatusMessage) {
		auth.StatusMessage = ""
		changed = true
	}
	return changed
}

// normalizeSoloAuthStateAfterQuotaClear 处理“只有 auth 聚合态、没有 modelStates”的恢复收口。
func normalizeSoloAuthStateAfterQuotaClear(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		auth.Status = StatusDisabled
		auth.Unavailable = true
		auth.NextRetryAfter = time.Time{}
		auth.Quota = QuotaState{}
		auth.UpdatedAt = now
		return
	}
	if authHasResidualRuntimeSignals(auth, now) {
		auth.Status = StatusError
		auth.UpdatedAt = now
		return
	}
	clearAuthStateOnSuccess(auth, now)
}

// projectRepresentativeModelFailure 把剩余的非 quota 模型错误重新投影回 auth 聚合态。
func projectRepresentativeModelFailure(auth *Auth) {
	if auth == nil || auth.Status != StatusError || len(auth.ModelStates) == 0 {
		return
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			auth.LastError = cloneError(state.LastError)
			auth.FailureHTTPStatus = NormalizePersistableFailureHTTPStatus(state.LastError.HTTPStatus)
			if strings.TrimSpace(state.StatusMessage) != "" {
				auth.StatusMessage = state.StatusMessage
			}
			return
		}
		if statusCode := NormalizePersistableFailureHTTPStatus(state.FailureHTTPStatus); statusCode > 0 {
			auth.FailureHTTPStatus = statusCode
			if strings.TrimSpace(state.StatusMessage) != "" {
				auth.StatusMessage = state.StatusMessage
			}
			return
		}
		if strings.TrimSpace(state.StatusMessage) != "" {
			auth.StatusMessage = state.StatusMessage
			return
		}
	}
}

// applyClearedQuotaModelRegistryState 同步 registry 中的 quota/suspend 标记，恢复模型可见性。
func applyClearedQuotaModelRegistryState(authID string, auth *Auth, modelIDs []string) {
	for _, modelID := range quotaCooldownRegistryModelIDs(auth, modelIDs) {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(authID, modelID)
		registry.GetGlobalRegistry().ResumeClientModel(authID, modelID)
	}
}

// quotaCooldownRegistryModelIDs 计算清理 registry 时要联动的模型集合。
func quotaCooldownRegistryModelIDs(auth *Auth, modelIDs []string) []string {
	if codexFree429BlocksAllModels(auth) {
		fallback := ""
		if len(modelIDs) > 0 {
			fallback = modelIDs[0]
		}
		return codexFreeSharedQuotaModelIDs(strings.TrimSpace(auth.ID), fallback)
	}
	return uniqueQuotaCooldownModelIDs(modelIDs)
}

// uniqueQuotaCooldownModelIDs 归一化并去重需要恢复的模型 ID。
func uniqueQuotaCooldownModelIDs(modelIDs []string) []string {
	seen := make(map[string]struct{}, len(modelIDs))
	out := make([]string, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		modelID = canonicalModelKey(modelID)
		if modelID == "" {
			continue
		}
		if _, ok := seen[modelID]; ok {
			continue
		}
		seen[modelID] = struct{}{}
		out = append(out, modelID)
	}
	return out
}
