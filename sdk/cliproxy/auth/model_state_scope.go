package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// modelStateFamilyKeys 返回当前请求模型家族对应的所有运行态 key。
// 这里同时覆盖原始请求名、canonical base model，以及当前 auth 上
// 已存在的同家族 suffix key，避免 success/failure 只更新单个 key
// 导致 suffix/base 状态长期分裂。
func modelStateFamilyKeys(auth *Auth, model string) []string {
	rawModel := strings.TrimSpace(model)
	baseModel := canonicalModelKey(rawModel)
	if rawModel == "" && baseModel == "" {
		return nil
	}
	seen := make(map[string]struct{})
	keys := make([]string, 0, len(authModelStates(auth))+2)
	add := func(modelKey string) {
		modelKey = strings.TrimSpace(modelKey)
		if modelKey == "" {
			return
		}
		if _, ok := seen[modelKey]; ok {
			return
		}
		seen[modelKey] = struct{}{}
		keys = append(keys, modelKey)
	}
	add(rawModel)
	add(baseModel)
	for modelKey := range authModelStates(auth) {
		if canonicalModelKey(modelKey) == baseModel {
			add(modelKey)
		}
	}
	return keys
}

// registryModelFamilyIDs 返回当前请求模型家族对应的 registry model IDs。
// registry 只认识真实注册模型，所以这里按 canonical base model 聚合，
// 让带 thinking suffix 的请求也能正确 Suspend/Resume base model。
func registryModelFamilyIDs(authID, model string) []string {
	rawModel := strings.TrimSpace(model)
	baseModel := canonicalModelKey(rawModel)
	if rawModel == "" && baseModel == "" {
		return nil
	}
	seen := make(map[string]struct{})
	modelIDs := make([]string, 0, 4)
	add := func(modelID string) {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			return
		}
		if _, ok := seen[modelID]; ok {
			return
		}
		seen[modelID] = struct{}{}
		modelIDs = append(modelIDs, modelID)
	}
	for _, info := range registry.GetGlobalRegistry().GetModelsForClient(strings.TrimSpace(authID)) {
		if info == nil {
			continue
		}
		if canonicalModelKey(info.ID) == baseModel {
			add(info.ID)
		}
	}
	if len(modelIDs) == 0 {
		if baseModel != "" {
			add(baseModel)
		} else {
			add(rawModel)
		}
	}
	return modelIDs
}

// codexFreeTokenScopedRuntimeStateKeys 返回当前 auth 上仍然有效的 token 级共享阻断 key。
// 这里保留 exact key，不做 canonical 去重，避免 success 只能清 base key，
// 把原始 suffix key 永久留在 blocked 状态。
func codexFreeTokenScopedRuntimeStateKeys(auth *Auth, now time.Time) []string {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	keys := make([]string, 0, len(auth.ModelStates))
	for modelKey, state := range auth.ModelStates {
		if blocked, _, _ := codexFreeSharedBlockFromModelState(state, now); blocked {
			keys = append(keys, strings.TrimSpace(modelKey))
		}
	}
	return keys
}

// registryActionsForBlockedModelStates 根据当前剩余 model states 重建 registry 应有状态。
// 共享恢复会先批量 Resume 全模型，再调用这里把仍应保留的模型级失败重新挂回去，
// 避免某个 sibling model 的 404 / unsupported 被 success 顺手放行。
func registryActionsForBlockedModelStates(auth *Auth, now time.Time) ([]string, map[string]string) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil, nil
	}
	quotaIDs := make([]string, 0)
	suspendReasons := make(map[string]string)
	for modelKey, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		modelIDs := registryModelFamilyIDs(auth.ID, modelKey)
		if modelStateNeedsQuotaFlag(state, now) {
			quotaIDs = appendUniqueModelIDs(quotaIDs, modelIDs...)
		}
		reason, shouldSuspend := modelStateSuspendReason(state, now)
		if !shouldSuspend {
			continue
		}
		for _, modelID := range modelIDs {
			existing, ok := suspendReasons[modelID]
			if !ok || registrySuspendReasonRank(reason) > registrySuspendReasonRank(existing) {
				suspendReasons[modelID] = reason
			}
		}
	}
	if len(suspendReasons) == 0 {
		suspendReasons = nil
	}
	return quotaIDs, suspendReasons
}

func modelStateNeedsQuotaFlag(state *ModelState, now time.Time) bool {
	if state == nil || !state.Quota.Exceeded {
		return false
	}
	if !state.Quota.NextRecoverAt.IsZero() {
		return state.Quota.NextRecoverAt.After(now)
	}
	return !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now)
}

func modelStateSuspendReason(state *ModelState, now time.Time) (string, bool) {
	if state == nil {
		return "", false
	}
	if state.Status == StatusDisabled {
		return "disabled", true
	}
	if !state.Unavailable || state.NextRetryAfter.IsZero() || !state.NextRetryAfter.After(now) {
		return "", false
	}
	if state.Quota.Exceeded {
		return "quota", true
	}
	if isModelSupportResultError(state.LastError) || isModelSupportErrorMessage(strings.TrimSpace(state.StatusMessage)) {
		return "model_not_supported", true
	}
	switch currentPersistableModelFailureHTTPStatus(state) {
	case http.StatusUnauthorized:
		return "unauthorized", true
	case http.StatusPaymentRequired, http.StatusForbidden:
		return "payment_required", true
	case http.StatusNotFound:
		return "not_found", true
	default:
		return "", false
	}
}

func registrySuspendReasonRank(reason string) int {
	switch strings.TrimSpace(strings.ToLower(reason)) {
	case "disabled":
		return 4
	case "quota":
		return 3
	case "payment_required":
		return 2
	case "unauthorized", "not_found", "model_not_supported":
		return 1
	default:
		return 0
	}
}

func appendUniqueModelIDs(target []string, modelIDs ...string) []string {
	if len(modelIDs) == 0 {
		return target
	}
	seen := make(map[string]struct{}, len(target)+len(modelIDs))
	for _, modelID := range target {
		modelID = strings.TrimSpace(modelID)
		if modelID != "" {
			seen[modelID] = struct{}{}
		}
	}
	for _, modelID := range modelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		if _, ok := seen[modelID]; ok {
			continue
		}
		seen[modelID] = struct{}{}
		target = append(target, modelID)
	}
	return target
}

func authModelStates(auth *Auth) map[string]*ModelState {
	if auth == nil {
		return nil
	}
	return auth.ModelStates
}
