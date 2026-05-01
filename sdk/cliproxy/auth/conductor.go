package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/audit"
	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

const (
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval  = 5 * time.Second
	refreshMaxConcurrency = 16
	refreshPendingBackoff = time.Minute
	refreshFailureBackoff = 5 * time.Minute
	// refresh 成功后仍满足刷新条件时短暂退避，避免无效 token 更新触发空转刷新。
	refreshIneffectiveBackoff = 30 * time.Second
	quotaBackoffBase          = time.Second
	quotaBackoffMax           = 30 * time.Minute
	// 单个 auth 满载时给客户端一个很短的 Retry-After，表达“这是本地拥塞保护，不是上游长冷却”。
	authCapacityRetryAfter = time.Second
	// unlimitedRetrySafetyCap bounds legacy "retry all credentials" mode so a
	// single unhealthy request cannot fan out across thousands of auth files.
	unlimitedRetrySafetyCap = 32
	// Codex 按官方思路优先看 AT 自身 exp；本地策略只在到期前 12 小时内做主动 refresh。
	codexProactiveRefreshWindow = 12 * time.Hour
	// 当 Codex access token 缺少可解析 exp 时，回退到官方约 8 天的 stale 判定窗口。
	codexLastRefreshStaleWindow = 8 * 24 * time.Hour
	// RT 交换返回 401 后，把凭证收口到 priority=5，便于按“优先使用”原则继续消费当前 token。
	rtExchangeUnauthorizedPreferredPriority = 5
)

var quotaCooldownDisabled atomic.Bool

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	return quotaCooldownDisabled.Load()
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// Error describes the failure when Success is false.
	Error *Error
	// RequestSimHash stores the request SimHash when simhash routing is active.
	RequestSimHash uint64
	// HasRequestSimHash reports whether RequestSimHash is populated.
	HasRequestSimHash bool
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

type hookState struct {
	hook Hook
}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store           Store
	executors       map[string]ProviderExecutor
	selector        Selector
	hookValue       atomic.Value
	blockedRequests *blockedRequestLRU
	mu              sync.RWMutex
	auths           map[string]*Auth
	scheduler       *authScheduler
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets map[string]int

	// Retry controls request retry behavior.
	requestRetry             atomic.Int32
	maxRetryCredentials      atomic.Int32
	maxInvalidRequestRetries atomic.Int32
	maxRetryInterval         atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// modelPoolOffsets tracks per-auth alias pool rotation state.
	modelPoolOffsets map[string]int

	// inflightPerAuth 只记录运行时进行中的请求数，用来保护单个 token 账号。
	// 它故意不持久化，也不把 auth 标成 unavailable，避免污染管理面状态。
	inflightMu      sync.Mutex
	inflightPerAuth map[string]int

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// Auto refresh state
	refreshCancel    context.CancelFunc
	refreshLoop      *authAutoRefreshLoop
	refreshSemaphore chan struct{}
	// refreshInFlight 记录当前正在执行 refresh 的 auth，避免同一 RT 被并发使用。
	refreshInFlightMu sync.Mutex
	refreshInFlight   map[string]struct{}

	// Async persistence state for high-frequency runtime updates.
	persistStartOnce sync.Once
	persistMu        sync.Mutex
	persistPending   map[string]*Auth
	persistWake      chan struct{}
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:            store,
		executors:        make(map[string]ProviderExecutor),
		selector:         selector,
		blockedRequests:  newBlockedRequestLRU(1000),
		auths:            make(map[string]*Auth),
		providerOffsets:  make(map[string]int),
		inflightPerAuth:  make(map[string]int),
		modelPoolOffsets: make(map[string]int),
		refreshSemaphore: make(chan struct{}, refreshMaxConcurrency),
		refreshInFlight:  make(map[string]struct{}),
	}
	manager.hookValue.Store(hookState{hook: hook})
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.scheduler = newAuthScheduler(selector)
	return manager
}

func isBuiltInSelector(selector Selector) bool {
	_, ok := builtInSelectorStrategy(selector)
	return ok
}

func (m *Manager) syncSchedulerFromSnapshot(auths []*Auth) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.rebuild(auths)
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
}

// RefreshSchedulerEntry re-upserts a single auth into the scheduler so that its
// supportedModelSet is rebuilt from the current global model registry state.
// This must be called after models have been registered for a newly added auth,
// because the initial scheduler.upsertAuth during Register/Update runs before
// registerModelsForAuth and therefore snapshots an empty model set.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()
	m.scheduler.upsertAuth(snapshot)
}

// ReconcileRegistryModelStates clears stale per-model runtime failures for
// models that are currently registered for the auth in the global model registry。
// 这用于在模型目录刷新后，把历史 not_found / quota / unavailable 之类的
// 运行时状态和最新 registry 重新对齐，避免模型已重新注册但 scheduler 仍把它挡掉。
func (m *Manager) ReconcileRegistryModelStates(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}

	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(supportedModels) == 0 {
		return
	}

	supported := make(map[string]struct{}, len(supportedModels))
	for _, model := range supportedModels {
		if model == nil {
			continue
		}
		modelKey := canonicalModelKey(model.ID)
		if modelKey == "" {
			continue
		}
		supported[modelKey] = struct{}{}
	}
	if len(supported) == 0 {
		return
	}

	var snapshot *Auth
	now := time.Now()

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if ok && auth != nil && len(auth.ModelStates) > 0 {
		changed := false
		for modelKey, state := range auth.ModelStates {
			if state == nil {
				continue
			}
			baseModel := canonicalModelKey(modelKey)
			if baseModel == "" {
				baseModel = strings.TrimSpace(modelKey)
			}
			if _, supportedModel := supported[baseModel]; !supportedModel {
				continue
			}
			if !shouldReconcileRegistryModelState(state) {
				continue
			}
			resetModelState(state, now)
			changed = true
		}
		if changed {
			// 这里只回收“模型能力目录变化导致的陈旧 model 失败态”，
			// 不能顺手清掉真实运行中刚打上的 401/402/403/429 等 credential 级故障。
			// 聚合态统一复用 scheduler 的收口逻辑，避免出现状态字段已恢复、
			// 但 FailureHTTPStatus 仍残留旧值的半清理状态。
			syncAggregatedAuthStateFromModelStates(auth, now)
			auth.UpdatedAt = now
			_ = m.persist(ctx, auth)
			snapshot = auth.Clone()
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	var oldSelector Selector
	m.mu.Lock()
	oldSelector = m.selector
	m.selector = selector
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
	if oldSelector != nil && oldSelector != selector {
		stopSelector(oldSelector)
	}
}

// Hook returns the currently configured lifecycle hook.
func (m *Manager) Hook() Hook {
	if m == nil {
		return NoopHook{}
	}
	state, _ := m.hookValue.Load().(hookState)
	if state.hook == nil {
		return NoopHook{}
	}
	return state.hook
}

// SetHook replaces the lifecycle hook used for auth callbacks.
func (m *Manager) SetHook(hook Hook) {
	if m == nil {
		return
	}
	if hook == nil {
		hook = NoopHook{}
	}
	m.hookValue.Store(hookState{hook: hook})
}

// Selector returns the current selector snapshot for hot-reload style reconfiguration.
func (m *Manager) Selector() Selector {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selector
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.runtimeConfig.Store(cfg)
	if m.scheduler != nil {
		m.scheduler.setPriorityZeroStrategy(cfg.Routing.PriorityZeroStrategy)
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
}

func (m *Manager) sharedExitPriorityZeroOAuthNetworkJitterFallbackEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return sharedExitPriorityZeroOAuthNetworkJitterFallbackEnabled(cfg)
}

func (m *Manager) codexInitialRefreshOnLoadEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.CodexInitialRefreshOnLoad
}

func (m *Manager) lookupAPIKeyUpstreamModel(authID, requestedModel string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if table == nil {
		return ""
	}
	byAlias := table[authID]
	if len(byAlias) == 0 {
		return ""
	}
	key := strings.ToLower(thinking.ParseSuffix(requestedModel).ModelName)
	if key == "" {
		key = strings.ToLower(requestedModel)
	}
	resolved := strings.TrimSpace(byAlias[key])
	if resolved == "" {
		return ""
	}
	return preserveRequestedModelSuffix(requestedModel, resolved)
}

func isAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	kind, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(kind), "api_key")
}

func isOpenAICompatAPIKeyAuth(auth *Auth) bool {
	if !isAPIKeyAuth(auth) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

func openAICompatProviderKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if providerKey := strings.TrimSpace(auth.Attributes["provider_key"]); providerKey != "" {
			return strings.ToLower(providerKey)
		}
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			return strings.ToLower(compatName)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func openAICompatModelPoolKey(auth *Auth, requestedModel string) string {
	base := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if base == "" {
		base = strings.TrimSpace(requestedModel)
	}
	return strings.ToLower(strings.TrimSpace(auth.ID)) + "|" + openAICompatProviderKey(auth) + "|" + strings.ToLower(base)
}

func (m *Manager) nextModelPoolOffset(key string, size int) int {
	if m == nil || size <= 1 {
		return 0
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelPoolOffsets == nil {
		m.modelPoolOffsets = make(map[string]int)
	}
	offset := m.modelPoolOffsets[key]
	if offset >= 2_147_483_640 {
		offset = 0
	}
	m.modelPoolOffsets[key] = offset + 1
	if size <= 0 {
		return 0
	}
	return offset % size
}

func rotateStrings(values []string, offset int) []string {
	if len(values) <= 1 {
		return values
	}
	if offset <= 0 {
		out := make([]string, len(values))
		copy(out, values)
		return out
	}
	offset = offset % len(values)
	out := make([]string, 0, len(values))
	out = append(out, values[offset:]...)
	out = append(out, values[:offset]...)
	return out
}

func (m *Manager) resolveOpenAICompatUpstreamModelPool(auth *Auth, requestedModel string) []string {
	if m == nil || !isOpenAICompatAPIKeyAuth(auth) {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func preserveRequestedModelSuffix(requestedModel, resolved string) string {
	return preserveResolvedModelSuffix(resolved, thinking.ParseSuffix(requestedModel))
}

// selectionModelForAuth 返回该 auth 在调度/冷却判断时真正对应的上游模型名。
// 这里必须先处理前缀改写，再处理 OAuth alias；否则当请求走别名模型时，
// cooldown 会错误地去看 route model，导致本该等待的账号被当成可立即重试。
func (m *Manager) selectionModelForAuth(auth *Auth, routeModel string) string {
	requestedModel := rewriteModelForAuth(routeModel, auth)
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = strings.TrimSpace(routeModel)
	}
	resolvedModel := m.applyOAuthModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolvedModel) == "" {
		resolvedModel = requestedModel
	}
	return resolvedModel
}

func (m *Manager) executionModelCandidates(auth *Auth, routeModel string) []string {
	requestedModel := rewriteModelForAuth(routeModel, auth)
	requestedModel = m.applyOAuthModelAlias(auth, requestedModel)
	if pool := m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		if len(pool) == 1 {
			return pool
		}
		offset := m.nextModelPoolOffset(openAICompatModelPoolKey(auth, requestedModel), len(pool))
		return rotateStrings(pool, offset)
	}
	resolved := m.applyAPIKeyModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolved) == "" {
		resolved = requestedModel
	}
	return []string{resolved}
}

func executionResultModel(routeModel, upstreamModel string, pooled bool) string {
	if pooled {
		if resolved := strings.TrimSpace(upstreamModel); resolved != "" {
			return resolved
		}
	}
	if requested := strings.TrimSpace(routeModel); requested != "" {
		return requested
	}
	return strings.TrimSpace(upstreamModel)
}

func filterExecutionModels(auth *Auth, routeModel string, candidates []string, pooled bool) []string {
	if len(candidates) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]string, 0, len(candidates))
	for _, upstreamModel := range candidates {
		stateModel := executionResultModel(routeModel, upstreamModel, pooled)
		blocked, _, _ := isAuthBlockedForModel(auth, stateModel, now)
		if blocked {
			continue
		}
		out = append(out, upstreamModel)
	}
	return out
}

func (m *Manager) preparedExecutionModels(auth *Auth, routeModel string) ([]string, bool) {
	candidates := m.executionModelCandidates(auth, routeModel)
	pooled := len(candidates) > 1
	return filterExecutionModels(auth, routeModel, candidates, pooled), pooled
}

func (m *Manager) executionModelsForRequest(auth *Auth, routeModel, execModel string) ([]string, bool) {
	if strings.TrimSpace(routeModel) != "" && !strings.EqualFold(strings.TrimSpace(routeModel), strings.TrimSpace(execModel)) {
		return []string{strings.TrimSpace(execModel)}, false
	}
	return m.preparedExecutionModels(auth, execModel)
}

func (m *Manager) prepareExecutionModels(auth *Auth, routeModel string) []string {
	models, _ := m.preparedExecutionModels(auth, routeModel)
	return models
}

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func (m *Manager) withAuditContext(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) context.Context {
	if audit.FromContext(ctx) != nil {
		return ctx
	}
	maxBodyBytes := 0
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.RequestAudit.Enable || strings.TrimSpace(cfg.RequestAudit.Endpoint) == "" {
		return ctx
	}
	if cfg != nil {
		maxBodyBytes = cfg.RequestAudit.MaxBodyBytes
	}
	return audit.WithRequest(ctx, opts, req, maxBodyBytes)
}

func auditAuthPath(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["path"])
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, authID, provider, resultModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, release func()) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		if release != nil {
			defer release()
		}
		var failed bool
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := &Error{Message: chunk.Err.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](chunk.Err); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				m.MarkResult(ctx, Result{AuthID: authID, Provider: provider, Model: resultModel, Success: false, Error: rerr})
			}
			if len(chunk.Payload) > 0 {
				audit.AppendClientResponse(ctx, chunk.Payload)
			}
			if !forward {
				return false
			}
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if !failed {
			m.MarkResult(ctx, Result{AuthID: authID, Provider: provider, Model: resultModel, Success: true})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel string, execModels []string, pooled bool, release func()) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		if release != nil {
			release()
		}
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	releaseOnReturn := release
	releaseNow := func() {
		if releaseOnReturn != nil {
			releaseOnReturn()
			releaseOnReturn = nil
		}
	}
	var lastErr error
	for idx, execModel := range execModels {
		resultModel := executionResultModel(routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		audit.SetAttempt(ctx, provider, execModel, auth.ID, auth.Label, auth.FileName, auditAuthPath(auth))
		updatedAuth, streamResult, errStream := executeWithCodex401Recovery(m, ctx, auth, provider, execReq, opts, func(runCtx context.Context, runAuth *Auth) (*cliproxyexecutor.StreamResult, error) {
			return executor.ExecuteStream(runCtx, runAuth, execReq, opts)
		})
		auth = updatedAuth
		m.syncExecutionAuth(ctx, auth)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				releaseNow()
				return nil, errCtx
			}
			rerr := resultErrorFromExecError(errStream)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
			result.RetryAfter = retryAfterFromError(errStream)
			if isRequestInvalidError(errStream) {
				m.Hook().OnResult(ctx, result)
				releaseNow()
				return nil, errStream
			}
			m.MarkResult(ctx, result)
			lastErr = errStream
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discardStreamChunks(streamResult.Chunks)
				releaseNow()
				return nil, errCtx
			}
			if isRequestInvalidError(bootstrapErr) {
				rerr := &Error{Message: bootstrapErr.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.Hook().OnResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				releaseNow()
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := &Error{Message: bootstrapErr.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			rerr := &Error{Message: bootstrapErr.Error()}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
				rerr.HTTPStatus = se.StatusCode()
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			m.MarkResult(ctx, result)
			discardStreamChunks(streamResult.Chunks)
			releaseNow()
			return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr}
			m.MarkResult(ctx, result)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			releaseNow()
			return nil, newStreamBootstrapError(emptyErr, streamResult.Headers)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		currentRelease := releaseOnReturn
		releaseOnReturn = nil
		return m.wrapStreamResult(ctx, auth.ID, provider, resultModel, streamResult.Headers, buffered, remaining, currentRelease), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	releaseNow()
	return nil, lastErr
}

// isCodex401RecoveryCandidate 只在 Codex 请求返回 401 且 auth 仍持有 refresh_token 时触发恢复链。
func isCodex401RecoveryCandidate(provider string, auth *Auth, err error) bool {
	if err == nil || auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return false
	}
	if !authHasRefreshToken(auth) {
		return false
	}
	return statusCodeFromError(err) == http.StatusUnauthorized
}

// latestAuthSnapshot 在 refresh 或 reload 后优先切到最新 auth 快照继续请求。
func latestAuthSnapshot(current, updated *Auth) *Auth {
	if updated != nil {
		return updated
	}
	return current
}

// codexTerminalRefreshRecoveryError 把 refresh 阶段已经确认不可恢复的 401 归一成稳定错误码。
func codexTerminalRefreshRecoveryError(err error) *Error {
	if err == nil || statusCodeFromError(err) != http.StatusUnauthorized {
		return nil
	}
	code := errorCodeFromError(err)
	if code == "" {
		code = codexauth.RefreshUnauthorizedErrorCode
	}
	return &Error{
		Code:       code,
		Message:    err.Error(),
		HTTPStatus: http.StatusUnauthorized,
	}
}

// executeWithCodex401Recovery 为 Codex 请求链补上一层官方风格的恢复语义：
// 首次请求命中 401 时同步 refresh 一次，并仅重试一次；只有终态 401 才继续往上冒泡。
func executeWithCodex401Recovery[T any](m *Manager, ctx context.Context, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, run func(context.Context, *Auth) (T, error)) (*Auth, T, error) {
	var zero T
	current := auth
	resp, err := run(ctx, current)
	if err == nil || !isCodex401RecoveryCandidate(provider, current, err) {
		return current, resp, err
	}

	entry := logEntryWithRequestID(ctx)
	authID := strings.TrimSpace(current.ID)
	entry.Warnf("codex 请求命中 401，开始 refresh-retry 恢复: auth=%s model=%s", authID, strings.TrimSpace(req.Model))

	refreshed, refreshErr := m.RefreshAuthNow(ctx, authID)
	current = latestAuthSnapshot(current, refreshed)
	if refreshErr != nil {
		if terminalErr := codexTerminalRefreshRecoveryError(refreshErr); terminalErr != nil {
			entry.Warnf("codex 401 恢复中的 refresh 终态失败: auth=%s code=%s err=%v", authID, terminalErr.Code, refreshErr)
			return current, zero, terminalErr
		}
		entry.Warnf("codex 401 恢复中的 refresh 未完成，保留原始 401: auth=%s err=%v", authID, refreshErr)
		return current, zero, err
	}

	if latest := m.cloneAuthByID(authID); latest != nil {
		current = latest
	}
	entry.Infof("codex 401 恢复 refresh 成功，开始重试请求: auth=%s model=%s", authID, strings.TrimSpace(req.Model))

	resp, retryErr := run(ctx, current)
	if retryErr == nil {
		return current, resp, nil
	}
	if statusCodeFromError(retryErr) == http.StatusUnauthorized {
		entry.Warnf("codex 401 恢复耗尽，重试后仍返回 401: auth=%s model=%s", authID, strings.TrimSpace(req.Model))
		return current, zero, &Error{
			Code:       codexauth.UnauthorizedAfterRecoveryErrorCode,
			Message:    retryErr.Error(),
			HTTPStatus: http.StatusUnauthorized,
		}
	}
	return current, zero, retryErr
}

func (m *Manager) syncExecutionAuth(ctx context.Context, auth *Auth) {
	if m == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	nextCLIUA := ""
	if auth.Metadata != nil {
		if raw, ok := auth.Metadata["cli_ua"].(string); ok {
			nextCLIUA = strings.TrimSpace(raw)
		}
	}

	m.mu.RLock()
	current := m.auths[auth.ID]
	currentCLIUA := ""
	if current != nil && current.Metadata != nil {
		if raw, ok := current.Metadata["cli_ua"].(string); ok {
			currentCLIUA = strings.TrimSpace(raw)
		}
	}
	m.mu.RUnlock()

	if nextCLIUA == "" || nextCLIUA == currentCLIUA {
		return
	}
	auth.UpdatedAt = time.Now()
	_, _ = m.Update(ctx, auth)
}

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}

		byAlias := make(map[string]string)
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "gemini":
			if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "claude":
			if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "codex":
			if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "vertex":
			if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		default:
			// OpenAI-compat uses config selection from auth.Attributes.
			providerKey := ""
			compatName := ""
			if auth.Attributes != nil {
				providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			}
			if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
				if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
					compileAPIKeyModelAliasForModels(byAlias, entry.Models)
				}
			}
		}

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	m.apiKeyModelAlias.Store(out)
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts, credential retry limit, caller-error retry
// limit, and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int, maxInvalidRequestRetries int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxInvalidRequestRetries < 0 {
		maxInvalidRequestRetries = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxInvalidRequestRetries.Store(int32(maxInvalidRequestRetries))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	EnsureFirstRegisteredAt(auth, auth.CreatedAt)
	auth.EnsureIndex()
	authClone := auth.Clone()
	schedulerSnapshot := authClone.Clone()
	m.mu.Lock()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	m.queueRefreshReschedule(auth.ID)
	if errPersist := m.persist(ctx, auth); errPersist != nil {
		log.WithError(errPersist).Warnf("auth manager: 持久化认证 %s 失败", strings.TrimSpace(auth.ID))
	}
	hook := m.Hook()
	hook.OnAuthRegistered(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	m.mu.Lock()
	if existing, ok := m.auths[auth.ID]; ok && existing != nil {
		// 请求统计是纯运行态数据；同 ID 配置更新不能把它清零。
		auth.Success = existing.Success
		auth.Failed = existing.Failed
		auth.recentRequests = existing.recentRequests
		if !auth.indexAssigned && auth.Index == "" {
			auth.Index = existing.Index
			auth.indexAssigned = existing.indexAssigned
		}
		if registeredAt, okRegisteredAt := FirstRegisteredAt(existing); okRegisteredAt {
			auth.CreatedAt = registeredAt
			if auth.Metadata != nil {
				auth.Metadata[FirstRegisteredAtMetadataKey] = registeredAt.Format(time.RFC3339Nano)
			}
		} else if auth.CreatedAt.IsZero() {
			auth.CreatedAt = existing.CreatedAt
		}
		if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
		}
	}
	EnsureFirstRegisteredAt(auth, auth.CreatedAt)
	auth.EnsureIndex()
	authClone := auth.Clone()
	schedulerSnapshot := authClone.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(schedulerSnapshot)
	}
	m.queueRefreshReschedule(auth.ID)
	if errPersist := m.persist(ctx, auth); errPersist != nil {
		log.WithError(errPersist).Warnf("auth manager: 持久化认证 %s 失败", strings.TrimSpace(auth.ID))
	}
	hook := m.Hook()
	hook.OnAuthUpdated(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	pendingPersist := make([]*Auth, 0)
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		if _, changed := ensureFirstRegisteredAtWithChanged(auth, auth.CreatedAt); changed {
			pendingPersist = append(pendingPersist, auth.Clone())
		}
		auth.EnsureIndex()
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	for _, auth := range pendingPersist {
		m.enqueuePersist(auth)
	}
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = m.withAuditContext(ctx, req, opts)
	var err error
	opts, err = m.rejectBlockedRequest(opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait, maxInvalidRequestRetries := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := m.executeMixedOnce(ctx, normalized, req, opts, maxRetryCredentials, maxInvalidRequestRetries)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteCount performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = m.withAuditContext(ctx, req, opts)
	var err error
	opts, err = m.rejectBlockedRequest(opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait, maxInvalidRequestRetries := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := m.executeCountMixedOnce(ctx, normalized, req, opts, maxRetryCredentials, maxInvalidRequestRetries)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ctx = m.withAuditContext(ctx, req, opts)
	var err error
	opts, err = m.rejectBlockedRequest(opts)
	if err != nil {
		return nil, err
	}
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait, maxInvalidRequestRetries := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		result, errStream := m.executeStreamMixedOnce(ctx, normalized, req, opts, maxRetryCredentials, maxInvalidRequestRetries)
		if errStream == nil {
			return result, nil
		}
		lastErr = errStream
		wait, shouldRetry := m.shouldRetryAfterError(errStream, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return nil, errWait
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
}

func (m *Manager) pickNextExecutableMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, func(), error) {
	// 这里显式做“选中后再占位”的两阶段控制：
	// 先沿现有路由策略挑候选 auth，再原子申请该 auth 的 inflight 配额。
	// 这样才能保证上限是硬限制，而不是“看起来尽量不超过”。
	var lastCapacityErr error
	for {
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, model, opts, tried)
		if errPick != nil {
			if lastCapacityErr != nil {
				var authErr *Error
				if errors.As(errPick, &authErr) && authErr != nil {
					switch authErr.Code {
					case "auth_not_found", "auth_unavailable":
						return nil, nil, "", nil, lastCapacityErr
					}
				}
			}
			return nil, nil, "", nil, errPick
		}
		release, ok := m.tryAcquireAuthSlot(auth.ID)
		if ok {
			return auth, executor, provider, release, nil
		}
		tried[auth.ID] = struct{}{}
		lastCapacityErr = newAuthCapacityError(authCapacityRetryAfter)
	}
}

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, maxInvalidRequestRetries int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := selectionModelFromMetadata(opts.Metadata, req.Model)
	opts = ensureRequestedModelMetadata(opts, req.Model)
	opts = ensureSessionAffinityMetadata(opts, m.selector)
	opts = ensureRequestSimHashMetadata(opts, m.selector)
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	credentialRetryLimit := effectiveCredentialRetryLimit(maxRetryCredentials)
	var lastErr error
	var requestInvalidErr error
	invalidRetryAttempts := 0
	for {
		if requestInvalidErr != nil && invalidRetryAttempts >= maxInvalidRequestRetries {
			return cliproxyexecutor.Response{}, requestInvalidErr
		}
		if credentialRetryLimit > 0 && len(attempted) >= credentialRetryLimit {
			if requestInvalidErr != nil {
				return cliproxyexecutor.Response{}, requestInvalidErr
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, release, errPick := m.pickNextExecutableMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if requestInvalidErr != nil {
				return cliproxyexecutor.Response{}, requestInvalidErr
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}
		if requestInvalidErr != nil {
			invalidRetryAttempts++
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = withRequestSimHash(execCtx, opts.Metadata)

		models, pooled := m.executionModelsForRequest(auth, routeModel, req.Model)
		if len(models) == 0 {
			release()
			continue
		}
		attempted[auth.ID] = struct{}{}
		var authErr error
		for _, upstreamModel := range models {
			resultModel := executionResultModel(routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			audit.SetAttempt(execCtx, provider, upstreamModel, auth.ID, auth.Label, auth.FileName, auditAuthPath(auth))
			updatedAuth, resp, errExec := executeWithCodex401Recovery(m, execCtx, auth, provider, execReq, opts, func(runCtx context.Context, runAuth *Auth) (cliproxyexecutor.Response, error) {
				return executor.Execute(runCtx, runAuth, execReq, opts)
			})
			auth = updatedAuth
			m.syncExecutionAuth(execCtx, auth)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil}
			attachRequestSimHashResult(&result, opts.Metadata)
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					release()
					return cliproxyexecutor.Response{}, errCtx
				}
				result.Error = resultErrorFromExecError(errExec)
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.recordBlockedRequest(opts, errExec)
				if isRequestInvalidError(errExec) {
					m.Hook().OnResult(execCtx, result)
					authErr = errExec
					break
				}
				m.MarkResult(execCtx, result)
				authErr = errExec
				continue
			}
			audit.SetClientResponse(execCtx, resp.Payload)
			m.MarkResult(execCtx, result)
			bindSessionAffinityFromMetadata(m.selector, opts.Metadata, sessionAffinityProviderScope(providers, provider), routeModel, auth.ID)
			release()
			return resp, nil
		}
		release()
		if authErr != nil {
			if isRequestInvalidError(authErr) {
				requestInvalidErr = authErr
				if maxInvalidRequestRetries == 0 || invalidRetryAttempts >= maxInvalidRequestRetries {
					return cliproxyexecutor.Response{}, authErr
				}
				continue
			}
			if m.sharedExitPriorityZeroOAuthNetworkJitterFallbackEnabled() {
				markPriorityZeroOAuthSkippedOnNetworkJitter(opts.Metadata, auth, authErr)
			}
			lastErr = authErr
			continue
		}
	}
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, maxInvalidRequestRetries int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := selectionModelFromMetadata(opts.Metadata, req.Model)
	opts = ensureRequestedModelMetadata(opts, req.Model)
	opts = ensureSessionAffinityMetadata(opts, m.selector)
	opts = ensureRequestSimHashMetadata(opts, m.selector)
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	credentialRetryLimit := effectiveCredentialRetryLimit(maxRetryCredentials)
	var lastErr error
	var requestInvalidErr error
	invalidRetryAttempts := 0
	for {
		if requestInvalidErr != nil && invalidRetryAttempts >= maxInvalidRequestRetries {
			return cliproxyexecutor.Response{}, requestInvalidErr
		}
		if credentialRetryLimit > 0 && len(attempted) >= credentialRetryLimit {
			if requestInvalidErr != nil {
				return cliproxyexecutor.Response{}, requestInvalidErr
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, release, errPick := m.pickNextExecutableMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if requestInvalidErr != nil {
				return cliproxyexecutor.Response{}, requestInvalidErr
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}
		if requestInvalidErr != nil {
			invalidRetryAttempts++
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = withRequestSimHash(execCtx, opts.Metadata)

		models, pooled := m.executionModelsForRequest(auth, routeModel, req.Model)
		if len(models) == 0 {
			release()
			continue
		}
		attempted[auth.ID] = struct{}{}
		var authErr error
		for _, upstreamModel := range models {
			resultModel := executionResultModel(routeModel, upstreamModel, pooled)
			execReq := req
			execReq.Model = upstreamModel
			audit.SetAttempt(execCtx, provider, upstreamModel, auth.ID, auth.Label, auth.FileName, auditAuthPath(auth))
			resp, errExec := executor.CountTokens(execCtx, auth, execReq, opts)
			m.syncExecutionAuth(execCtx, auth)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil}
			attachRequestSimHashResult(&result, opts.Metadata)
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					release()
					return cliproxyexecutor.Response{}, errCtx
				}
				result.Error = &Error{Message: errExec.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](errExec); ok && se != nil {
					result.Error.HTTPStatus = se.StatusCode()
				}
				if ra := retryAfterFromError(errExec); ra != nil {
					result.RetryAfter = ra
				}
				m.recordBlockedRequest(opts, errExec)
				if isRequestInvalidError(errExec) {
					m.Hook().OnResult(execCtx, result)
					authErr = errExec
					break
				}
				m.MarkResult(execCtx, result)
				authErr = errExec
				continue
			}
			audit.SetClientResponse(execCtx, resp.Payload)
			m.MarkResult(execCtx, result)
			bindSessionAffinityFromMetadata(m.selector, opts.Metadata, sessionAffinityProviderScope(providers, provider), routeModel, auth.ID)
			release()
			return resp, nil
		}
		release()
		if authErr != nil {
			if isRequestInvalidError(authErr) {
				requestInvalidErr = authErr
				if maxInvalidRequestRetries == 0 || invalidRetryAttempts >= maxInvalidRequestRetries {
					return cliproxyexecutor.Response{}, authErr
				}
				continue
			}
			if m.sharedExitPriorityZeroOAuthNetworkJitterFallbackEnabled() {
				markPriorityZeroOAuthSkippedOnNetworkJitter(opts.Metadata, auth, authErr)
			}
			lastErr = authErr
			continue
		}
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int, maxInvalidRequestRetries int) (*cliproxyexecutor.StreamResult, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := selectionModelFromMetadata(opts.Metadata, req.Model)
	opts = ensureRequestedModelMetadata(opts, req.Model)
	opts = ensureSessionAffinityMetadata(opts, m.selector)
	opts = ensureRequestSimHashMetadata(opts, m.selector)
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	credentialRetryLimit := effectiveCredentialRetryLimit(maxRetryCredentials)
	var lastErr error
	var requestInvalidErr error
	invalidRetryAttempts := 0
	for {
		if requestInvalidErr != nil && invalidRetryAttempts >= maxInvalidRequestRetries {
			return nil, requestInvalidErr
		}
		if credentialRetryLimit > 0 && len(attempted) >= credentialRetryLimit {
			if requestInvalidErr != nil {
				return nil, requestInvalidErr
			}
			if lastErr != nil {
				var bootstrapErr *streamBootstrapError
				if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
					return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
				}
				return nil, lastErr
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, release, errPick := m.pickNextExecutableMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if requestInvalidErr != nil {
				return nil, requestInvalidErr
			}
			if lastErr != nil {
				var bootstrapErr *streamBootstrapError
				if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
					return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
				}
				return nil, lastErr
			}
			return nil, errPick
		}
		if requestInvalidErr != nil {
			invalidRetryAttempts++
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx = withRequestSimHash(execCtx, opts.Metadata)
		models, pooled := m.executionModelsForRequest(auth, routeModel, req.Model)
		if len(models) == 0 {
			release()
			continue
		}
		attempted[auth.ID] = struct{}{}
		streamResult, errStream := m.executeStreamWithModelPool(execCtx, executor, auth, provider, req, opts, routeModel, models, pooled, release)
		if errStream != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return nil, errCtx
			}
			m.recordBlockedRequest(opts, errStream)
			if isRequestInvalidError(errStream) {
				requestInvalidErr = errStream
				if maxInvalidRequestRetries == 0 || invalidRetryAttempts >= maxInvalidRequestRetries {
					return nil, errStream
				}
				continue
			}
			if m.sharedExitPriorityZeroOAuthNetworkJitterFallbackEnabled() {
				markPriorityZeroOAuthSkippedOnNetworkJitter(opts.Metadata, auth, errStream)
			}
			lastErr = errStream
			continue
		}
		bindSessionAffinityFromMetadata(m.selector, opts.Metadata, sessionAffinityProviderScope(providers, provider), routeModel, auth.ID)
		return streamResult, nil
	}
}

func sessionAffinityProviderScope(providers []string, provider string) string {
	scope := sessionAffinityScopeForProviders(providers)
	if scope != "" {
		return scope
	}
	return provider
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func selectionModelFromMetadata(meta map[string]any, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(meta) == 0 {
		return fallback
	}
	raw, ok := meta[cliproxyexecutor.SelectionModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch v := raw.(type) {
	case string:
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	case []byte:
		if trimmed := strings.TrimSpace(string(v)); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

func attachRequestSimHashResult(result *Result, meta map[string]any) {
	if result == nil {
		return
	}
	hash, ok := requestSimHashFromMetadata(meta)
	if !ok {
		return
	}
	result.RequestSimHash = hash
	result.HasRequestSimHash = true
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func disallowFreeAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.DisallowFreeAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(val))
		return err == nil && parsed
	case []byte:
		parsed, err := strconv.ParseBool(strings.TrimSpace(string(val)))
		return err == nil && parsed
	default:
		return false
	}
}

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return AuthChatGPTPlanType(auth) == "free"
}

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return resolved
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return upstreamModel
	}
	return requestedModel
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(cfg *internalconfig.Config, providerKey, compatName, authProvider string) *internalconfig.OpenAICompatibility {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if internalconfig.MatchOpenAICompatIdentity(candidate, candidate, candidate, compat) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

func (m *Manager) retrySettings() (int, int, time.Duration, int) {
	if m == nil {
		return 0, 0, 0, 0
	}
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load()), int(m.maxInvalidRequestRetries.Load())
}

func (m *Manager) maxInflightPerAuth() int {
	if m == nil {
		return 0
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return 0
	}
	if cfg.Routing.MaxInflightPerAuth < 0 {
		return 0
	}
	return cfg.Routing.MaxInflightPerAuth
}

func (m *Manager) tryAcquireAuthSlot(authID string) (func(), bool) {
	if m == nil {
		return nil, false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, false
	}
	limit := m.maxInflightPerAuth()
	if limit <= 0 {
		return func() {}, true
	}
	m.inflightMu.Lock()
	if current := m.inflightPerAuth[authID]; current >= limit {
		m.inflightMu.Unlock()
		return nil, false
	}
	m.inflightPerAuth[authID]++
	m.inflightMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.inflightMu.Lock()
			defer m.inflightMu.Unlock()
			current := m.inflightPerAuth[authID]
			if current <= 1 {
				delete(m.inflightPerAuth, authID)
				return
			}
			m.inflightPerAuth[authID] = current - 1
		})
	}, true
}

func (m *Manager) authInflightCount(authID string) int {
	if m == nil {
		return 0
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	return m.inflightPerAuth[authID]
}

func (m *Manager) tryAcquireRefreshSlot(authID string) (func(), bool) {
	if m == nil {
		return nil, false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, false
	}
	m.refreshInFlightMu.Lock()
	if _, exists := m.refreshInFlight[authID]; exists {
		m.refreshInFlightMu.Unlock()
		return nil, false
	}
	m.refreshInFlight[authID] = struct{}{}
	m.refreshInFlightMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.refreshInFlightMu.Lock()
			delete(m.refreshInFlight, authID)
			m.refreshInFlightMu.Unlock()
		})
	}, true
}

func effectiveCredentialRetryLimit(maxRetryCredentials int) int {
	if maxRetryCredentials > 0 {
		return maxRetryCredentials
	}
	return unlimitedRetrySafetyCap
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		blocked, reason, next := isAuthBlockedForModel(auth, checkModel, now)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

// retryAllowed 用于判断当前 providers 里是否仍存在“值得继续重试”的 auth。
// 只有至少一个 auth 的 request_retry 上限尚未耗尽时，才会消费上游 Retry-After。
func (m *Manager) retryAllowed(attempt int, providers []string) bool {
	if m == nil || attempt < 0 || len(providers) == 0 {
		return false
	}
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt < effectiveRetry {
			return true
		}
	}
	return false
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	if isAuthCapacityError(err) {
		return 0, false
	}
	status := statusCodeFromError(err)
	if status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	wait, found := m.closestCooldownWait(providers, model, attempt)
	if found {
		if wait > maxWait {
			return 0, false
		}
		return wait, true
	}
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if !m.retryAllowed(attempt, providers) {
		return 0, false
	}
	retryAfter := retryAfterFromError(err)
	if retryAfter == nil || *retryAfter <= 0 || *retryAfter > maxWait {
		return 0, false
	}
	return *retryAfter, true
}

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	if !result.HasRequestSimHash {
		if hash, ok := requestSimHashFromContext(ctx); ok {
			result.RequestSimHash = hash
			result.HasRequestSimHash = true
		}
	}

	now := time.Now()

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	sharedQuotaCooldown := false
	clearModelQuotaIDs := []string(nil)
	setModelQuotaIDs := []string(nil)
	resumeModelIDs := []string(nil)
	suspendModelIDs := []string(nil)
	reapplyModelQuotaIDs := []string(nil)
	reapplySuspendReasons := map[string]string(nil)
	forceSchedulerUpsert := false
	invalidateAffinity := false
	var authSnapshot *Auth
	var modelStateSnapshot *ModelState
	var persistSnapshot *Auth
	useSchedulerFastPath := result.Model != "" && m.useSchedulerFastPath()

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}
		if result.HasRequestSimHash {
			auth.LastRequestSimHash = result.RequestSimHash
			auth.HasLastRequestSimHash = true
		}
		if result.Success {
			if result.Model != "" {
				runtimeKeys := modelStateFamilyKeys(auth, result.Model)
				sharedKeys := []string(nil)
				if codexFreeSharesModelState(auth) {
					sharedKeys = codexFreeTokenScopedRuntimeStateKeys(auth, now)
				}
				resetKeys := append([]string(nil), runtimeKeys...)
				resetKeys = appendUniqueModelIDs(resetKeys, sharedKeys...)
				if len(resetKeys) == 0 {
					resetKeys = []string{strings.TrimSpace(result.Model)}
				}
				for _, modelKey := range resetKeys {
					resetModelState(ensureModelState(auth, modelKey), now)
				}
				if len(sharedKeys) > 0 {
					ids := codexFreeSharedModelIDs(result.AuthID, result.Model)
					clearModelQuotaIDs = append([]string(nil), ids...)
					resumeModelIDs = append([]string(nil), ids...)
					reapplyModelQuotaIDs, reapplySuspendReasons = registryActionsForBlockedModelStates(auth, now)
					forceSchedulerUpsert = true
				} else {
					ids := registryModelFamilyIDs(result.AuthID, result.Model)
					clearModelQuotaIDs = append([]string(nil), ids...)
					resumeModelIDs = append([]string(nil), ids...)
					if len(runtimeKeys) > 1 {
						forceSchedulerUpsert = true
					}
				}
				updateAggregatedAvailability(auth, now)
				if !hasModelError(auth, now) {
					auth.LastError = nil
					auth.FailureHTTPStatus = 0
					auth.StatusMessage = ""
					auth.Status = StatusActive
				}
				auth.UpdatedAt = now
				shouldResumeModel = true
				clearModelQuota = true
			} else {
				clearAuthStateOnSuccess(auth, now)
			}
		} else {
			if result.Model != "" {
				if !isRequestScopedNotFoundResultError(result.Error) {
					disableCooling := quotaCooldownDisabledForAuth(auth)
					stateKeys := modelStateFamilyKeys(auth, result.Model)
					if len(stateKeys) == 0 {
						stateKeys = []string{strings.TrimSpace(result.Model)}
					}
					state := ensureModelState(auth, stateKeys[0])
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
						auth.LastError = cloneError(result.Error)
						auth.StatusMessage = result.Error.Message
					}

					statusCode := statusCodeFromResult(result.Error)
					state.FailureHTTPStatus = NormalizePersistableFailureHTTPStatus(statusCode)
					auth.FailureHTTPStatus = NormalizePersistableFailureHTTPStatus(statusCode)
					if isModelSupportResultError(result.Error) {
						next := now.Add(12 * time.Hour)
						state.NextRetryAfter = next
						suspendReason = "model_not_supported"
						shouldSuspendModel = true
					} else {
						switch statusCode {
						case 401:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "unauthorized"
								shouldSuspendModel = true
							}
						case 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "payment_required"
								shouldSuspendModel = true
							}
						case 404:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(12 * time.Hour)
								state.NextRetryAfter = next
								suspendReason = "not_found"
								shouldSuspendModel = true
							}
						case 429:
							var next time.Time
							backoffLevel := state.Quota.BackoffLevel
							strikeCount := state.Quota.StrikeCount + 1
							if !disableCooling {
								if result.RetryAfter != nil {
									next = now.Add(*result.RetryAfter)
								} else {
									cooldown, nextLevel := nextQuotaCooldown(backoffLevel, disableCooling)
									if cooldown > 0 {
										next = now.Add(cooldown)
									}
									backoffLevel = nextLevel
								}
							}
							state.NextRetryAfter = next
							state.Quota = QuotaState{
								Exceeded:      true,
								Reason:        "quota",
								NextRecoverAt: next,
								BackoffLevel:  backoffLevel,
								StrikeCount:   strikeCount,
							}
							if !disableCooling {
								suspendReason = "quota"
								shouldSuspendModel = true
								setModelQuota = true
								if codexFree429BlocksAllModels(auth) {
									sharedQuotaCooldown = true
									suspendReason = "shared_quota"
									setModelQuotaIDs = codexFreeSharedQuotaModelIDs(result.AuthID, result.Model)
									suspendModelIDs = append([]string(nil), setModelQuotaIDs...)
								}
							}
						case 408, 500, 502, 503, 504:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(1 * time.Minute)
								state.NextRetryAfter = next
							}
						default:
							state.NextRetryAfter = time.Time{}
						}
					}
					for _, modelKey := range stateKeys[1:] {
						auth.ModelStates[modelKey] = state.Clone()
					}
					modelRegistryIDs := registryModelFamilyIDs(result.AuthID, result.Model)
					if codexFreeSharesModelState(auth) {
						if blocked, _, _ := codexFreeSharedBlockFromModelState(state, now); blocked {
							forceSchedulerUpsert = true
							invalidateAffinity = true
							if setModelQuota {
								setModelQuotaIDs = append([]string(nil), codexFreeSharedQuotaModelIDs(result.AuthID, result.Model)...)
							}
							if shouldSuspendModel {
								suspendModelIDs = append([]string(nil), codexFreeSharedModelIDs(result.AuthID, result.Model)...)
							}
						}
					} else if len(stateKeys) > 1 {
						forceSchedulerUpsert = true
					}
					if setModelQuota && len(setModelQuotaIDs) == 0 {
						setModelQuotaIDs = append([]string(nil), modelRegistryIDs...)
					}
					if shouldSuspendModel && len(suspendModelIDs) == 0 {
						suspendModelIDs = append([]string(nil), modelRegistryIDs...)
					}

					auth.Status = StatusError
					auth.UpdatedAt = now
					updateAggregatedAvailability(auth, now)
				}
			} else {
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
			}
		}

		if m.store != nil && !shouldSkipPersist(ctx) {
			persistSnapshot = auth.Clone()
		}
		if useSchedulerFastPath && !forceSchedulerUpsert {
			if state := auth.ModelStates[result.Model]; state != nil {
				modelStateSnapshot = state.Clone()
			}
		} else {
			if persistSnapshot != nil {
				authSnapshot = persistSnapshot.Clone()
			} else {
				authSnapshot = auth.Clone()
			}
		}
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		appliedFast := false
		if useSchedulerFastPath && modelStateSnapshot != nil {
			appliedFast = m.scheduler.applyModelStateUpdate(result.AuthID, result.Provider, result.Model, modelStateSnapshot)
		}
		if !appliedFast && authSnapshot == nil {
			authSnapshot = m.cloneAuthByID(result.AuthID)
		}
		if !appliedFast && authSnapshot != nil {
			m.scheduler.upsertAuth(authSnapshot)
		}
	}
	if persistSnapshot != nil {
		m.enqueuePersist(persistSnapshot)
	}

	if clearModelQuota && result.Model != "" {
		clearIDs := clearModelQuotaIDs
		if len(clearIDs) == 0 {
			clearIDs = registryModelFamilyIDs(result.AuthID, result.Model)
		}
		for _, modelID := range clearIDs {
			registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, modelID)
		}
	}
	if setModelQuota && result.Model != "" {
		setIDs := setModelQuotaIDs
		if len(setIDs) == 0 {
			setIDs = registryModelFamilyIDs(result.AuthID, result.Model)
		}
		for _, modelID := range setIDs {
			registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, modelID)
		}
	}
	if shouldResumeModel {
		resumeIDs := resumeModelIDs
		if len(resumeIDs) == 0 {
			resumeIDs = registryModelFamilyIDs(result.AuthID, result.Model)
		}
		for _, modelID := range resumeIDs {
			registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, modelID)
		}
		for _, modelID := range reapplyModelQuotaIDs {
			registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, modelID)
		}
		for modelID, reason := range reapplySuspendReasons {
			registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, modelID, reason)
		}
	} else if shouldSuspendModel {
		suspendIDs := suspendModelIDs
		if len(suspendIDs) == 0 {
			suspendIDs = registryModelFamilyIDs(result.AuthID, result.Model)
		}
		for _, modelID := range suspendIDs {
			registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, modelID, suspendReason)
		}
		if sharedQuotaCooldown {
			invalidateAffinity = true
		}
	}
	if invalidateAffinity {
		invalidateSessionAffinityAuth(m.selector, result.AuthID)
	}

	// 反馈型 selector 只在进程内维护评分，不参与持久化，因此在结果落库后再喂回运行态统计。
	if observer, ok := m.selector.(ResultObserver); ok && observer != nil {
		observer.ObserveResult(result, now)
	}

	hook := m.Hook()
	hook.OnResult(ctx, result)
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.FailureHTTPStatus = 0
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	maxStrikeCount := 0
	sharedBlocked := false
	sharedReason := blockReasonNone
	sharedNext := time.Time{}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
			if state.Quota.StrikeCount > maxStrikeCount {
				maxStrikeCount = state.Quota.StrikeCount
			}
		}
		if codexFreeSharesModelState(auth) {
			blocked, reason, next := codexFreeSharedBlockFromModelState(state, now)
			if blocked {
				sharedBlocked, sharedReason, sharedNext = mergeCodexFreeSharedBlock(sharedBlocked, sharedReason, sharedNext, reason, next)
			}
		}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
		auth.Quota.StrikeCount = maxStrikeCount
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
		auth.Quota.StrikeCount = 0
	}
	if next, ok := codexFreeSharedQuotaRetryAfter(auth, auth.Quota, now); ok {
		auth.Unavailable = true
		auth.NextRetryAfter = next
		return
	}
	if sharedBlocked {
		auth.Unavailable = true
		auth.NextRetryAfter = sharedNext
		if sharedReason == blockReasonDisabled {
			auth.Status = StatusDisabled
		}
		return
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
		return
	}
	auth.NextRetryAfter = time.Time{}
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.Quota.StrikeCount = 0
	auth.LastError = nil
	auth.FailureHTTPStatus = 0
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

// errorCodeFromError 从 provider 原始错误里提取机器可读错误码，供 result/maintenance 共享。
func errorCodeFromError(err error) string {
	if err == nil {
		return ""
	}
	type errorCoder interface {
		ErrorCode() string
	}
	var coder errorCoder
	if errors.As(err, &coder) && coder != nil {
		if code := strings.TrimSpace(coder.ErrorCode()); code != "" {
			return code
		}
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return strings.TrimSpace(authErr.Code)
	}
	return ""
}

// resultErrorFromExecError 把 executor error 统一适配成 manager 内部的 Error 结构。
func resultErrorFromExecError(err error) *Error {
	if err == nil {
		return nil
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return cloneError(authErr)
	}
	out := &Error{
		Code:    errorCodeFromError(err),
		Message: err.Error(),
	}
	if se, ok := errors.AsType[cliproxyexecutor.StatusError](err); ok && se != nil {
		out.HTTPStatus = se.StatusCode()
	}
	return out
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func terminalStatusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type terminalStatusCoder interface {
		StatusCode() int
		Terminal() bool
	}
	var sc terminalStatusCoder
	if errors.As(err, &sc) && sc != nil && sc.Terminal() {
		return sc.StatusCode()
	}
	return 0
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	rap, ok := err.(retryAfterProvider)
	if !ok || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	return new(*retryAfter)
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isModelSupportError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Message)
}

func isRequestScopedNotFoundMessage(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return isRequestScopedNotFoundMessage(err.Message)
}

func isTransientProviderError(err error) bool {
	if err == nil {
		return false
	}
	if authErr, ok := err.(*Error); ok && authErr != nil && authErr.Retryable {
		return true
	}
	switch statusCodeFromError(err) {
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	return strings.Contains(message, "context deadline exceeded") ||
		strings.Contains(message, "deadline exceeded") ||
		strings.Contains(message, "timeout") ||
		strings.Contains(message, "temporary") ||
		strings.Contains(message, "connection refused") ||
		strings.Contains(message, "connection reset") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "eof")
}

// isRequestInvalidError returns true if the error represents a caller-side
// request-shape failure that should not keep poisoning auth state. Model-support
// failures remain eligible for auth/upstream fallback, but 422 responses and
// caller-side 400 validation signals such as invalid_request_error,
// invalid_json_schema, or unsupported parameters are treated as request-invalid.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromError(err) == http.StatusNotFound {
		return isRequestScopedNotFoundMessage(err.Error())
	}
	return isBlockableInvalidRequestError(err)
}

func isBlockableInvalidRequestError(err error) bool {
	if err == nil {
		return false
	}
	if isModelSupportError(err) {
		return false
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusUnauthorized, http.StatusTooManyRequests:
		return false
	}
	if isTransientProviderError(err) {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	if strings.Contains(message, "invalid_function_parameters") {
		return true
	}
	if status == http.StatusBadRequest {
		return isRequestInvalidBody(err.Error()) ||
			strings.Contains(message, "invalid_request_error") ||
			strings.Contains(message, "invalid response format") ||
			strings.Contains(message, "invalid_response_format") ||
			strings.Contains(message, "response_format")
	}
	if status == http.StatusUnprocessableEntity {
		return true
	}
	return false
}

func (m *Manager) rejectBlockedRequest(opts cliproxyexecutor.Options) (cliproxyexecutor.Options, error) {
	if m == nil || m.blockedRequests == nil {
		return opts, nil
	}
	updated, hash, ok := requestBodyHashFromOptions(opts)
	if !ok {
		return updated, nil
	}
	if !m.blockedRequests.Contains(hash) {
		return updated, nil
	}
	return updated, &Error{
		Code:       "blocked_invalid_request",
		Message:    "request body matches a previously blocked invalid request",
		Retryable:  false,
		HTTPStatus: http.StatusBadRequest,
	}
}

func (m *Manager) recordBlockedRequest(opts cliproxyexecutor.Options, err error) {
	if m == nil || m.blockedRequests == nil || !isBlockableInvalidRequestError(err) {
		return
	}
	_, hash, ok := requestBodyHashFromOptions(opts)
	if !ok {
		return
	}
	m.blockedRequests.Add(hash)
}

type requestInvalidBody struct {
	Detail  string `json:"detail"`
	Message string `json:"message"`
	Type    string `json:"type"`
	Error   struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
		Type    string `json:"type"`
	} `json:"error"`
}

func isRequestInvalidBody(raw string) bool {
	text := strings.TrimSpace(raw)
	if text == "" {
		return false
	}
	if hasRequestInvalidSignal(text) {
		return true
	}

	var payload requestInvalidBody
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(payload.Type), "invalid_request_error") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(payload.Error.Type), "invalid_request_error") {
		return true
	}
	if hasRequestInvalidCode(payload.Error.Code) {
		return true
	}
	if hasRequestInvalidSignal(payload.Detail) || hasRequestInvalidSignal(payload.Message) || hasRequestInvalidSignal(payload.Error.Message) {
		return true
	}
	return strings.TrimSpace(payload.Error.Param) != "" && (hasRequestInvalidCode(payload.Error.Code) || hasRequestInvalidSignal(payload.Error.Message))
}

func hasRequestInvalidCode(code string) bool {
	code = strings.TrimSpace(strings.ToLower(code))
	if code == "" {
		return false
	}
	return strings.HasPrefix(code, "invalid_") || strings.HasPrefix(code, "unsupported_")
}

func hasRequestInvalidSignal(text string) bool {
	lower := strings.TrimSpace(strings.ToLower(text))
	if lower == "" {
		return false
	}
	signals := []string{
		"invalid_request_error",
		"invalid_json_schema",
		"invalid schema",
		"unsupported parameter",
		"unsupported field",
		"unknown parameter",
		"unknown field",
		"invalid value",
		"invalid_value",
		"does not represent a valid image",
		"supported image formats",
		"malformed payload",
		"unrecognized request argument",
	}
	for _, signal := range signals {
		if strings.Contains(lower, signal) {
			return true
		}
	}
	return false
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	if isRequestScopedNotFoundResultError(resultErr) {
		return
	}
	disableCooling := quotaCooldownDisabledForAuth(auth)
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	auth.FailureHTTPStatus = NormalizePersistableFailureHTTPStatus(statusCode)
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 402, 403:
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 404:
		auth.StatusMessage = "not_found"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.StrikeCount++
		var next time.Time
		if !disableCooling {
			if retryAfter != nil {
				next = now.Add(*retryAfter)
			} else {
				cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, disableCooling)
				if cooldown > 0 {
					next = now.Add(cooldown)
				}
				auth.Quota.BackoffLevel = nextLevel
			}
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.StatusMessage = "transient upstream error"
		if quotaCooldownDisabledForAuth(auth) {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

func modelStateIsClean(state *ModelState) bool {
	if state == nil {
		return true
	}
	if state.Status != StatusActive {
		return false
	}
	if state.Unavailable || state.StatusMessage != "" || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		return false
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 || state.Quota.StrikeCount != 0 {
		return false
	}
	return true
}

// shouldReconcileRegistryModelState 只允许回收“模型能力目录变化导致的陈旧失败态”。
// 真实运行时故障（尤其 401/402/403/429）必须保留，避免刚打上的 credential 冷却/周限
// 被一次 registry 对齐顺手清掉，导致调度与 management 又把 auth 看成正常。
func shouldReconcileRegistryModelState(state *ModelState) bool {
	if state == nil || modelStateIsClean(state) {
		return false
	}
	if state.Status == StatusDisabled {
		return false
	}

	if state.LastError != nil {
		switch statusCodeFromResult(state.LastError) {
		case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
			return false
		case http.StatusNotFound:
			return true
		}
		if isModelSupportResultError(state.LastError) {
			return true
		}
	}

	switch NormalizePersistableFailureHTTPStatus(state.FailureHTTPStatus) {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return false
	case http.StatusNotFound:
		return true
	}

	message := strings.TrimSpace(state.StatusMessage)
	if strings.EqualFold(message, "not_found") {
		return true
	}
	return isModelSupportErrorMessage(message)
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// FindByFileName retrieves an auth entry by persisted filename or backing path basename
// without cloning the entire auth set.
func (m *Manager) FindByFileName(name string) (*Auth, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.FileName) == name {
			return auth.Clone(), true
		}
		if auth.Attributes != nil {
			if filepath.Base(strings.TrimSpace(auth.Attributes["path"])) == name {
				return auth.Clone(), true
			}
		}
	}
	return nil, false
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	m.mu.RUnlock()

	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

func executorLookupKeys(provider string, auth *Auth) []string {
	keys := make([]string, 0, 4)
	appendKey := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range keys {
			if strings.EqualFold(existing, value) {
				return
			}
		}
		keys = append(keys, value)
	}

	appendKey(provider)
	appendKey(executorKeyFromAuth(auth))
	if auth != nil {
		appendKey(auth.Provider)
		if auth.Attributes != nil {
			appendKey(auth.Attributes["compat_name"])
			appendKey(auth.Attributes["provider_key"])
		}
	}
	return keys
}

func (m *Manager) executorForAuth(provider string, auth *Auth) (ProviderExecutor, string, bool) {
	for _, key := range executorLookupKeys(provider, auth) {
		if executor, ok := m.Executor(key); ok {
			return executor, strings.ToLower(strings.TrimSpace(key)), true
		}
	}
	return nil, "", false
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.RLock()
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.RUnlock()

	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

func (m *Manager) useSchedulerFastPath() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	return isBuiltInSelector(m.selector)
}

func shouldRetrySchedulerPick(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable"
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate.Provider != provider || candidate.Disabled {
			continue
		}
		if shouldSkipAuthByClientPolicy(opts.Metadata, candidate) {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	selected, errPick := m.selector.Pick(ctx, provider, model, opts, candidates)
	if errPick != nil {
		m.mu.RUnlock()
		return nil, nil, errPick
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	return m.finalizePickedAuth(authCopy), executor, nil
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if !m.useSchedulerFastPath() {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	if selected, executor, ok := m.pickBoundSingleFastPath(ctx, provider, model, opts, tried); ok {
		return selected, executor, nil
	}
	executor, okExecutor := m.Executor(provider)
	if !okExecutor {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, errPick := m.scheduler.pickSingle(ctx, provider, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, errPick = m.scheduler.pickSingle(ctx, provider, model, opts, tried)
		}
		if errPick != nil {
			return nil, nil, errPick
		}
		if selected == nil {
			return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		return m.finalizePickedAuth(selected), executor, nil
	}
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if shouldSkipAuthByClientPolicy(opts.Metadata, candidate) {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	selected, errPick := m.selector.Pick(ctx, "mixed", model, opts, candidates)
	if errPick != nil {
		m.mu.RUnlock()
		return nil, nil, "", errPick
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	executor, resolvedProvider, okExecutor := m.executorForAuth(providerKey, selected)
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	return m.finalizePickedAuth(authCopy), executor, resolvedProvider, nil
}

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if !m.useSchedulerFastPath() {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}

	eligibleProviders := make([]string, 0, len(providers))
	seenProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		if _, seen := seenProviders[providerKey]; seen {
			continue
		}
		if _, okExecutor := m.Executor(providerKey); !okExecutor {
			continue
		}
		seenProviders[providerKey] = struct{}{}
		eligibleProviders = append(eligibleProviders, providerKey)
	}
	if len(eligibleProviders) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if selected, executor, resolvedProvider, ok := m.pickBoundMixedFastPath(ctx, eligibleProviders, model, opts, tried); ok {
		return selected, executor, resolvedProvider, nil
	}
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	for {
		selected, providerKey, errPick := m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
		if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
			m.syncScheduler()
			selected, providerKey, errPick = m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
		}
		if errPick != nil {
			return nil, nil, "", errPick
		}
		if selected == nil {
			return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		if disallowFreeAuth && isFreeCodexAuth(selected) {
			if tried == nil {
				tried = make(map[string]struct{})
			}
			tried[selected.ID] = struct{}{}
			continue
		}
		executor, resolvedProvider, okExecutor := m.executorForAuth(providerKey, selected)
		if !okExecutor {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		return m.finalizePickedAuth(selected), executor, resolvedProvider, nil
	}
}

func (m *Manager) pickBoundSingleFastPath(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, bool) {
	affinity := sessionAffinitySelectorOf(m.selector)
	if affinity == nil || pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return nil, nil, false
	}
	candidates, executor, _, ok := m.collectAffinityCandidates([]string{provider}, model, opts, tried)
	if !ok {
		return nil, nil, false
	}
	selected, hit := affinity.pickBoundAuth(ctx, provider, model, opts, candidates)
	if !hit || selected == nil {
		return nil, nil, false
	}
	return m.finalizePickedAuth(selected.Clone()), executor, true
}

func (m *Manager) pickBoundMixedFastPath(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, bool) {
	affinity := sessionAffinitySelectorOf(m.selector)
	if affinity == nil || pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return nil, nil, "", false
	}
	candidates, _, executors, ok := m.collectAffinityCandidates(providers, model, opts, tried)
	if !ok {
		return nil, nil, "", false
	}
	scope := sessionAffinityScopeForProviders(providers)
	selected, hit := affinity.pickBoundAuth(ctx, scope, model, opts, candidates)
	if !hit || selected == nil {
		return nil, nil, "", false
	}
	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	executor, resolvedProvider, ok := m.executorForAuth(providerKey, selected)
	if !ok {
		return nil, nil, "", false
	}
	if _, allowed := executors[providerKey]; !allowed {
		return nil, nil, "", false
	}
	return m.finalizePickedAuth(selected.Clone()), executor, resolvedProvider, true
}

func (m *Manager) collectAffinityCandidates(providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) ([]*Auth, ProviderExecutor, map[string]ProviderExecutor, bool) {
	if m == nil {
		return nil, nil, nil, false
	}
	disallowFreeAuth := disallowFreeAuthFromMetadata(opts.Metadata)
	modelKey := strings.TrimSpace(model)
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		providerSet[providerKey] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, nil, false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	registryRef := registry.GetGlobalRegistry()
	candidates := make([]*Auth, 0, len(m.auths))
	executors := make(map[string]ProviderExecutor, len(providerSet))
	var singleExecutor ProviderExecutor
	for providerKey := range providerSet {
		executor, ok := m.executors[providerKey]
		if !ok {
			continue
		}
		executors[providerKey] = executor
		singleExecutor = executor
	}
	if len(executors) == 0 {
		return nil, nil, nil, false
	}
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if shouldSkipAuthByClientPolicy(opts.Metadata, candidate) {
			continue
		}
		if disallowFreeAuth && isFreeCodexAuth(candidate) {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if _, ok := executors[providerKey]; !ok {
			continue
		}
		if len(tried) > 0 {
			if _, used := tried[candidate.ID]; used {
				continue
			}
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, nil, nil, false
	}
	return candidates, singleExecutor, executors, true
}

func sessionAffinityScopeForProviders(providers []string) string {
	if len(providers) > 1 {
		return "mixed"
	}
	if len(providers) == 1 {
		return providers[0]
	}
	return ""
}

func (m *Manager) finalizePickedAuth(authCopy *Auth) *Auth {
	if m == nil || authCopy == nil || authCopy.indexAssigned {
		return authCopy
	}
	m.mu.Lock()
	if current := m.auths[authCopy.ID]; current != nil {
		if !current.indexAssigned {
			current.EnsureIndex()
		}
		authCopy.Index = current.Index
		authCopy.indexAssigned = current.indexAssigned
	}
	m.mu.Unlock()
	return authCopy
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = refreshCheckInterval
	}

	m.mu.Lock()
	cancelPrev := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	m.mu.Unlock()
	if cancelPrev != nil {
		cancelPrev()
	}

	ctx, cancelCtx := context.WithCancel(parent)
	workers := refreshMaxConcurrency
	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
		workers = cfg.AuthAutoRefreshWorkers
	}
	loop := newAuthAutoRefreshLoop(m, interval, workers)

	m.mu.Lock()
	m.refreshCancel = cancelCtx
	m.refreshLoop = loop
	m.mu.Unlock()

	loop.rebuild(time.Now())
	go loop.run(ctx)
}

// TriggerCodexInitialRefreshOnLoadIfNeeded 只会在 auth metadata 仍带有
// “新文件初始 refresh 待处理”标记时，按配置决定是否立即发起一次后台 refresh。
// 这样可以把 codex-initial-refresh-on-load 收口成“新文件首次入池的一次性初始交换”，
// 避免 RT 轮转后又被当成“首次读取”重复触发。
func (m *Manager) TriggerCodexInitialRefreshOnLoadIfNeeded(ctx context.Context, id string) {
	if m == nil || strings.TrimSpace(id) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 这里显式去掉 watcher 路径带来的 skipPersist。
	// “文件变更 -> 注册/更新 auth”本身不该回写磁盘，否则会形成 watcher 写回环；
	// 但其后派生出的 Codex 初始 refresh 一旦成功，会拿到新的 RT/AT，并且必须落盘。
	// 否则重启后仍会从磁盘读到旧 RT，再次触发 refresh_token_reused。
	ctx = WithoutSkipPersist(ctx)
	now := time.Now()
	if !m.markCodexInitialRefreshPending(id, now) {
		return
	}
	log.Debugf("codex 新文件初始 refresh 已调度: %s", strings.TrimSpace(id))
	go m.refreshAuthWithLimit(ctx, id)
}

// RefreshAuthNow 立即同步刷新指定 auth，并返回刷新后的最新快照。
// 该入口主要给 management 等“手动操作某个凭证”的场景使用。
func (m *Manager) RefreshAuthNow(ctx context.Context, id string) (*Auth, error) {
	if m == nil || strings.TrimSpace(id) == "" {
		return nil, ErrAuthNotFound
	}
	ctx = WithoutSkipPersist(ctx)
	return m.runRefreshAuth(ctx, id, true)
}

// StopAutoRefresh cancels the background refresh loop, if running.
func (m *Manager) StopAutoRefresh() {
	var currentSelector Selector
	m.mu.Lock()
	cancel := m.refreshCancel
	m.refreshCancel = nil
	m.refreshLoop = nil
	currentSelector = m.selector
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	stopSelector(currentSelector)
}

func (m *Manager) queueRefreshReschedule(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	if loop == nil {
		return
	}
	loop.queueReschedule(authID)
}

func (m *Manager) checkRefreshes(ctx context.Context) {
	// log.Debugf("checking refreshes")
	now := time.Now()
	for _, authID := range m.collectRefreshTargets(now) {
		if !m.markRefreshPending(authID, now) {
			continue
		}
		go m.refreshAuthWithLimit(ctx, authID)
	}
}

func (m *Manager) collectRefreshTargets(now time.Time) []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if m.executors[auth.Provider] == nil {
			continue
		}
		if auth.Disabled {
			continue
		}
		if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
			continue
		}
		candidates = append(candidates, auth.Clone())
	}
	m.mu.RUnlock()

	targets := make([]string, 0, len(candidates))
	for _, auth := range candidates {
		if auth == nil {
			continue
		}
		typ, _ := auth.AccountInfo()
		if typ == "api_key" || !m.shouldRefresh(auth, now) {
			continue
		}
		log.Debugf("checking refresh for %s, %s, %s", auth.Provider, auth.ID, typ)
		targets = append(targets, auth.ID)
	}
	return targets
}

func (m *Manager) refreshAuthWithLimit(ctx context.Context, id string) {
	_, _ = m.runRefreshAuth(ctx, id, false)
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil || a.Disabled {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()
	provider := strings.ToLower(strings.TrimSpace(a.Provider))

	// Codex 在启动首轮/周期 auto-refresh 下改成“保守门控”：
	// 只有 token JSON 明确表明需要刷新（已过期、临近过期、last_refresh 足够旧）才触发。
	// 若缺少 refresh_token 或缺少关键时间字段，则默认不主动打上游，避免刚启动/刚入池就做探测式 refresh。
	if provider == "codex" {
		return m.shouldRefreshCodexFromTokenJSON(a, now, lastRefresh, expiry, hasExpiry)
	}

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func (m *Manager) shouldRefreshCodexFromTokenJSON(a *Auth, now, lastRefresh, expiry time.Time, hasExpiry bool) bool {
	if !authHasRefreshToken(a) {
		return false
	}
	if m.codexInitialRefreshOnLoadEnabled() && CodexInitialRefreshPending(a) {
		return true
	}

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if !lastRefresh.IsZero() {
			return now.Sub(lastRefresh) >= interval
		}
		return false
	}

	// Codex 默认不再使用 provider 级固定 lead：
	// 1. 优先解析 access token 自身的 exp，并只在到期前 12 小时内主动 refresh；
	// 2. 若拿不到 exp，再退回到官方客户端近似的 last_refresh 8 天 stale 规则；
	// 3. 若两类时间信号都没有，则保持静默，不做探测式 refresh。
	if tokenExpiry, ok := authCodexAccessTokenExpiry(a); ok && !tokenExpiry.IsZero() {
		return !tokenExpiry.After(now.Add(codexProactiveRefreshWindow))
	}
	if hasExpiry && !expiry.IsZero() {
		return !expiry.After(now.Add(codexProactiveRefreshWindow))
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= codexLastRefreshStaleWindow
	}
	return false
}

func authHasRefreshToken(a *Auth) bool {
	if a == nil || len(a.Metadata) == 0 {
		return false
	}
	refreshToken, _ := a.Metadata["refresh_token"].(string)
	return strings.TrimSpace(refreshToken) != ""
}

// authRefreshTokenValue 返回 auth metadata 中当前 refresh_token 的规范化字符串。
// 这里只用于日志审计时判断 RT 是否发生轮转，不会把 token 内容写入日志。
func authRefreshTokenValue(a *Auth) string {
	if a == nil || len(a.Metadata) == 0 {
		return ""
	}
	refreshToken, _ := a.Metadata["refresh_token"].(string)
	return strings.TrimSpace(refreshToken)
}

// authRefreshTokenRotated 判断一次 refresh 前后 refresh_token 是否真的发生了轮转。
// 只有新旧 RT 都非空且值不同才认为轮转成功，避免把空值或缺失误判成“已轮转”。
func authRefreshTokenRotated(before string, after string) bool {
	before = strings.TrimSpace(before)
	after = strings.TrimSpace(after)
	if before == "" || after == "" {
		return false
	}
	return before != after
}

// setPreferredPriorityAfterRTUnauthorized 在 RT 交换返回 401 后把 auth 收口到 priority=5。
// 这里同时写 Attributes 与 Metadata，保证当前调度与管理面读取到同一优先级。
func setPreferredPriorityAfterRTUnauthorized(a *Auth) bool {
	if a == nil {
		return false
	}
	if a.Attributes == nil {
		a.Attributes = make(map[string]string, 1)
	}
	if a.Metadata == nil {
		a.Metadata = make(map[string]any, 1)
	}
	changed := false
	priorityText := strconv.Itoa(rtExchangeUnauthorizedPreferredPriority)
	if strings.TrimSpace(a.Attributes["priority"]) != priorityText {
		a.Attributes["priority"] = priorityText
		changed = true
	}
	if !authMetadataPriorityEquals(a.Metadata, rtExchangeUnauthorizedPreferredPriority) {
		a.Metadata["priority"] = rtExchangeUnauthorizedPreferredPriority
		changed = true
	}
	return changed
}

// authMetadataPriorityEquals 判断 metadata 中的 priority 是否已经等于目标值。
func authMetadataPriorityEquals(metadata map[string]any, expected int) bool {
	switch value := metadata["priority"].(type) {
	case int:
		return value == expected
	case float64:
		return int(value) == expected
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		return err == nil && parsed == expected
	default:
		return false
	}
}

// authCodexAccessTokenExpiry 尝试直接从 Codex access token 的 JWT `exp` 读取真实过期时间。
// 解析失败时返回 false，让上层回退到 metadata 中的 `expired` / `last_refresh` 判定。
func authCodexAccessTokenExpiry(a *Auth) (time.Time, bool) {
	if a == nil || len(a.Metadata) == 0 {
		return time.Time{}, false
	}
	raw, _ := a.Metadata["access_token"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	claims, err := codexauth.ParseJWTToken(raw)
	if err != nil || claims == nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0).UTC(), true
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok || auth == nil || auth.Disabled {
		m.mu.Unlock()
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		m.mu.Unlock()
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	m.mu.Unlock()
	m.queueRefreshReschedule(id)
	return true
}

func (m *Manager) markCodexInitialRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok || auth == nil || auth.Disabled {
		m.mu.Unlock()
		return false
	}
	if !m.codexInitialRefreshOnLoadEnabled() || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		m.mu.Unlock()
		return false
	}
	if !CodexInitialRefreshPending(auth) {
		m.mu.Unlock()
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		m.mu.Unlock()
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	m.mu.Unlock()
	m.queueRefreshReschedule(id)
	return true
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	_, _ = m.runRefreshAuth(ctx, id, false)
}

func (m *Manager) runRefreshAuth(ctx context.Context, id string, failIfBusy bool) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrAuthNotFound
	}
	if m.refreshSemaphore != nil {
		select {
		case m.refreshSemaphore <- struct{}{}:
			defer func() { <-m.refreshSemaphore }()
		case <-ctx.Done():
			return m.cloneAuthByID(id), ctx.Err()
		}
	}
	return m.runRefreshAuthLocked(ctx, id, failIfBusy)
}

func (m *Manager) runRefreshAuthLocked(ctx context.Context, id string, failIfBusy bool) (*Auth, error) {
	release, ok := m.tryAcquireRefreshSlot(id)
	if !ok {
		if failIfBusy {
			return m.cloneAuthByID(id), ErrAuthRefreshInFlight
		}
		return nil, nil
	}
	defer release()
	return m.executeRefreshAuth(ctx, id)
}

func (m *Manager) executeRefreshAuth(ctx context.Context, id string) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	if auth != nil {
		exec = m.executors[auth.Provider]
	}
	m.mu.RUnlock()
	if auth == nil {
		return nil, ErrAuthNotFound
	}
	if exec == nil {
		return auth.Clone(), ErrAuthRefreshExecutorUnavailable
	}
	cloned := auth.Clone()
	initialRefreshPending := m.codexInitialRefreshOnLoadEnabled() && CodexInitialRefreshPending(cloned)
	rtExchangeLogged := authHasRefreshToken(cloned)
	beforeRefreshToken := authRefreshTokenValue(cloned)
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return m.cloneAuthByID(id), err
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		if rtExchangeLogged {
			log.Warnf("auth manager: rt 交换失败: provider=%s auth=%s err=%v", strings.TrimSpace(auth.Provider), strings.TrimSpace(auth.ID), err)
		}
		refreshErr := &Error{Message: err.Error(), Code: errorCodeFromError(err)}
		terminalStatus := terminalStatusCodeFromError(err)
		if terminalStatus > 0 {
			refreshErr.HTTPStatus = terminalStatus
		}
		var persistSnapshot *Auth
		var currentSnapshot *Auth
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			persistNeeded := false
			if rtExchangeLogged && terminalStatus == http.StatusUnauthorized && setPreferredPriorityAfterRTUnauthorized(current) {
				persistNeeded = true
			}
			current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			current.LastError = refreshErr
			current.UpdatedAt = now
			if initialRefreshPending && terminalStatus > 0 && ClearCodexInitialRefreshPending(current) {
				persistNeeded = true
			}
			if persistNeeded {
				persistSnapshot = current.Clone()
			}
			currentSnapshot = current.Clone()
			m.auths[id] = current
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		m.queueRefreshReschedule(id)
		if persistSnapshot != nil {
			m.enqueuePersist(persistSnapshot)
		}
		if initialRefreshPending {
			if terminalStatus > 0 {
				log.Warnf("codex 新文件初始 refresh 终态失败，停止继续初始 refresh: %s, %v", strings.TrimSpace(id), err)
			} else {
				log.Warnf("codex 新文件初始 refresh 失败，将按退避继续重试: %s, %v", strings.TrimSpace(id), err)
			}
		}
		if currentSnapshot == nil {
			currentSnapshot = m.cloneAuthByID(id)
		}
		return currentSnapshot, err
	}
	if updated == nil {
		updated = cloned
	}
	if rtExchangeLogged {
		log.Infof(
			"auth manager: rt 交换完成: provider=%s auth=%s rt_rotated=%t",
			strings.TrimSpace(auth.Provider),
			strings.TrimSpace(auth.ID),
			authRefreshTokenRotated(beforeRefreshToken, authRefreshTokenValue(updated)),
		)
	}
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	if initialRefreshPending {
		ClearCodexInitialRefreshPending(updated)
		log.Infof("codex 新文件初始 refresh 完成: %s", strings.TrimSpace(id))
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	persisted, _ := m.Update(ctx, updated)
	if persisted != nil {
		return persisted, nil
	}
	return m.cloneAuthByID(id), nil
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[provider]
}

func (m *Manager) cloneAuthByID(id string) *Auth {
	if m == nil || strings.TrimSpace(id) == "" {
		return nil
	}
	m.mu.RLock()
	auth := m.auths[id]
	m.mu.RUnlock()
	if auth == nil {
		return nil
	}
	return auth.Clone()
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec = m.executors[executorKeyFromAuth(a)]
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(ctx context.Context, auth *Auth, method, targetURL string, body []byte, headers http.Header) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	return exec.HttpRequest(ctx, auth, req)
}
