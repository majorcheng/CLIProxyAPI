package auth

import (
	"encoding/json"
	"strings"
	"time"
)

// PersistedRuntimeStateMetadataKey 是 auth JSON 中保留给运行时状态快照的字段。
// 之所以单独放在 metadata 顶层，而不是散落回各 provider 自身字段里，是为了：
// 1. 不污染上游原始 token 结构；
// 2. 让不同 store / watcher / 启动恢复都走同一套编码语义。
const PersistedRuntimeStateMetadataKey = "cli_proxy_runtime_state"

type persistedRuntimeState struct {
	Status         Status                         `json:"status,omitempty"`
	StatusMessage  string                         `json:"status_message,omitempty"`
	HTTPStatus     int                            `json:"http_status,omitempty"`
	Unavailable    bool                           `json:"unavailable,omitempty"`
	NextRetryAfter time.Time                      `json:"next_retry_after,omitempty"`
	Quota          QuotaState                     `json:"quota,omitempty"`
	ModelStates    map[string]persistedModelState `json:"model_states,omitempty"`
}

type persistedModelState struct {
	Status         Status     `json:"status,omitempty"`
	StatusMessage  string     `json:"status_message,omitempty"`
	HTTPStatus     int        `json:"http_status,omitempty"`
	Unavailable    bool       `json:"unavailable,omitempty"`
	NextRetryAfter time.Time  `json:"next_retry_after,omitempty"`
	Quota          QuotaState `json:"quota,omitempty"`
}

// NormalizePersistableFailureHTTPStatus 把状态码收口到允许跨重启恢复的白名单。
// 目前只保留用户明确要求的 401/402/403/404/429，其他 1 分钟瞬态失败不持久化。
func NormalizePersistableFailureHTTPStatus(statusCode int) int {
	switch statusCode {
	case 401, 402, 403, 404, 429:
		return statusCode
	default:
		return 0
	}
}

// MetadataWithPersistedRuntimeState 返回一份可安全落盘的 metadata 副本。
// 它会把当前 auth/runtime 冷却状态编码进保留字段，同时确保原始 auth.Metadata
// 不被原地修改，避免把内存态和磁盘态耦合到一起。
func MetadataWithPersistedRuntimeState(auth *Auth) map[string]any {
	if auth == nil {
		return nil
	}
	metadata := cloneMetadataMap(auth.Metadata)
	if metadata != nil {
		delete(metadata, PersistedRuntimeStateMetadataKey)
	}
	if state, ok := buildPersistedRuntimeState(auth); ok {
		if metadata == nil {
			metadata = make(map[string]any, 1)
		}
		metadata[PersistedRuntimeStateMetadataKey] = state
	}
	if metadata == nil && auth.Metadata != nil {
		return map[string]any{}
	}
	return metadata
}

// RestorePersistedRuntimeState 从 metadata 中恢复 runtime state。
// 这里仍然故意忽略 last_error 详情本身：我们只恢复精确状态码与 cooldown / quota /
// next_retry 等能影响调度与展示的状态，不把一次性的错误正文长期保存在磁盘里。
func RestorePersistedRuntimeState(auth *Auth, now time.Time) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	raw, ok := auth.Metadata[PersistedRuntimeStateMetadataKey]
	if ok {
		delete(auth.Metadata, PersistedRuntimeStateMetadataKey)
	}
	if !ok {
		return
	}

	var state persistedRuntimeState
	encoded, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return
	}
	if errUnmarshal := json.Unmarshal(encoded, &state); errUnmarshal != nil {
		return
	}

	auth.Status = state.Status
	auth.StatusMessage = strings.TrimSpace(state.StatusMessage)
	auth.FailureHTTPStatus = NormalizePersistableFailureHTTPStatus(state.HTTPStatus)
	auth.Unavailable = state.Unavailable
	auth.NextRetryAfter = state.NextRetryAfter
	auth.Quota = state.Quota
	auth.LastError = nil
	auth.ModelStates = hydratePersistedModelStates(state.ModelStates)

	normalizeRestoredRuntimeState(auth, now)
}

func buildPersistedRuntimeState(auth *Auth) (persistedRuntimeState, bool) {
	if auth == nil {
		return persistedRuntimeState{}, false
	}

	state := persistedRuntimeState{}
	hasState := false
	statusCode := currentPersistableFailureHTTPStatus(auth)
	shouldPersistAuth := shouldPersistFailureRuntimeState(statusCode, auth.NextRetryAfter, auth.Quota, time.Now())

	if shouldPersistAuth && auth.Status != "" && auth.Status != StatusActive {
		state.Status = auth.Status
		hasState = true
	}
	if shouldPersistAuth {
		state.HTTPStatus = statusCode
		hasState = true
	}
	if shouldPersistAuth {
		if message := strings.TrimSpace(auth.StatusMessage); message != "" {
			state.StatusMessage = message
			hasState = true
		}
	}
	if shouldPersistAuth && auth.Unavailable {
		state.Unavailable = true
		hasState = true
	}
	if shouldPersistAuth && !auth.NextRetryAfter.IsZero() {
		state.NextRetryAfter = auth.NextRetryAfter
		hasState = true
	}
	if shouldPersistAuth && statusCode == 429 && quotaHasPersistedState(auth.Quota) {
		state.Quota = auth.Quota
		hasState = true
	}

	if len(auth.ModelStates) > 0 {
		modelStates := make(map[string]persistedModelState, len(auth.ModelStates))
		for modelName, modelState := range auth.ModelStates {
			if strings.TrimSpace(modelName) == "" || modelState == nil {
				continue
			}
			persisted, ok := buildPersistedModelState(modelState)
			if !ok {
				continue
			}
			modelStates[modelName] = persisted
		}
		if len(modelStates) > 0 {
			state.ModelStates = modelStates
			hasState = true
		}
	}

	return state, hasState
}

func buildPersistedModelState(state *ModelState) (persistedModelState, bool) {
	if state == nil {
		return persistedModelState{}, false
	}

	result := persistedModelState{}
	hasState := false
	statusCode := currentPersistableModelFailureHTTPStatus(state)
	shouldPersistState := shouldPersistFailureRuntimeState(statusCode, state.NextRetryAfter, state.Quota, time.Now())
	if shouldPersistState && state.Status != "" && state.Status != StatusActive {
		result.Status = state.Status
		hasState = true
	}
	if shouldPersistState {
		result.HTTPStatus = statusCode
		hasState = true
	}
	if shouldPersistState {
		if message := strings.TrimSpace(state.StatusMessage); message != "" {
			result.StatusMessage = message
			hasState = true
		}
	}
	if shouldPersistState && state.Unavailable {
		result.Unavailable = true
		hasState = true
	}
	if shouldPersistState && !state.NextRetryAfter.IsZero() {
		result.NextRetryAfter = state.NextRetryAfter
		hasState = true
	}
	if shouldPersistState && statusCode == 429 && quotaHasPersistedState(state.Quota) {
		result.Quota = state.Quota
		hasState = true
	}
	return result, hasState
}

func hydratePersistedModelStates(states map[string]persistedModelState) map[string]*ModelState {
	if len(states) == 0 {
		return nil
	}
	result := make(map[string]*ModelState, len(states))
	for modelName, state := range states {
		if strings.TrimSpace(modelName) == "" {
			continue
		}
		result[modelName] = &ModelState{
			Status:            state.Status,
			StatusMessage:     strings.TrimSpace(state.StatusMessage),
			FailureHTTPStatus: NormalizePersistableFailureHTTPStatus(state.HTTPStatus),
			Unavailable:       state.Unavailable,
			NextRetryAfter:    state.NextRetryAfter,
			Quota:             state.Quota,
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func cloneMetadataMap(metadata map[string]any) map[string]any {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func currentPersistableFailureHTTPStatus(auth *Auth) int {
	if auth == nil {
		return 0
	}
	if statusCode := NormalizePersistableFailureHTTPStatus(auth.FailureHTTPStatus); statusCode > 0 {
		return statusCode
	}
	if auth.LastError != nil {
		return NormalizePersistableFailureHTTPStatus(auth.LastError.HTTPStatus)
	}
	return 0
}

func currentPersistableModelFailureHTTPStatus(state *ModelState) int {
	if state == nil {
		return 0
	}
	if statusCode := NormalizePersistableFailureHTTPStatus(state.FailureHTTPStatus); statusCode > 0 {
		return statusCode
	}
	if state.LastError != nil {
		return NormalizePersistableFailureHTTPStatus(state.LastError.HTTPStatus)
	}
	return 0
}

func quotaHasPersistedState(quota QuotaState) bool {
	return quota.Exceeded ||
		strings.TrimSpace(quota.Reason) != "" ||
		!quota.NextRecoverAt.IsZero() ||
		quota.BackoffLevel != 0 ||
		quota.StrikeCount != 0
}

func shouldPersistFailureRuntimeState(statusCode int, nextRetryAfter time.Time, quota QuotaState, now time.Time) bool {
	statusCode = NormalizePersistableFailureHTTPStatus(statusCode)
	if statusCode == 0 {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	switch statusCode {
	case 429:
		if !nextRetryAfter.IsZero() {
			return nextRetryAfter.After(now)
		}
		if !quota.NextRecoverAt.IsZero() {
			return quota.NextRecoverAt.After(now)
		}
		return quotaHasPersistedState(quota)
	default:
		return !nextRetryAfter.IsZero() && nextRetryAfter.After(now)
	}
}

func normalizeRestoredRuntimeState(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	auth.LastError = nil
	if len(auth.ModelStates) > 0 {
		for _, state := range auth.ModelStates {
			if state == nil {
				continue
			}
			state.LastError = nil
			state.UpdatedAt = time.Time{}
			if !state.NextRetryAfter.IsZero() && !state.NextRetryAfter.After(now) {
				resetModelState(state, now)
			}
		}
		if auth.Disabled || auth.Status == StatusDisabled {
			auth.Status = StatusDisabled
			auth.FailureHTTPStatus = 0
			return
		}
		updateAggregatedAvailability(auth, now)
		syncAggregatedAuthStateFromModelStates(auth, now)
		auth.LastError = nil
		return
	}

	if !auth.NextRetryAfter.IsZero() && !auth.NextRetryAfter.After(now) {
		clearAuthStateOnSuccess(auth, now)
	}
	auth.LastError = nil
	if auth.Disabled {
		auth.Status = StatusDisabled
		auth.FailureHTTPStatus = 0
	}
}
