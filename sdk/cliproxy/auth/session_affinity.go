package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	sessionAffinityPrimaryMetadataKey  = "__session_affinity_primary_id"
	sessionAffinityFallbackMetadataKey = "__session_affinity_fallback_id"
	defaultSessionAffinityTTL          = time.Hour
)

// SessionAffinitySelector 在基础 selector 外增加 session->auth 绑定能力。
type SessionAffinitySelector struct {
	fallback Selector
	cache    *SessionCache
}

// SessionAffinityConfig 定义会话粘连包装层的回退 selector 与 TTL。
type SessionAffinityConfig struct {
	Fallback Selector
	TTL      time.Duration
}

// NewSessionAffinitySelector 用默认 TTL 创建会话粘连 selector。
func NewSessionAffinitySelector(fallback Selector) *SessionAffinitySelector {
	return NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: fallback, TTL: defaultSessionAffinityTTL})
}

// NewSessionAffinitySelectorWithConfig 创建带自定义 TTL 的会话粘连 selector。
func NewSessionAffinitySelectorWithConfig(cfg SessionAffinityConfig) *SessionAffinitySelector {
	if cfg.Fallback == nil {
		cfg.Fallback = &RoundRobinSelector{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultSessionAffinityTTL
	}
	return &SessionAffinitySelector{fallback: cfg.Fallback, cache: NewSessionCache(cfg.TTL)}
}

// UnwrapSelector 暴露被包装的基础 selector，便于外层做能力探测。
func (s *SessionAffinitySelector) UnwrapSelector() Selector {
	if s == nil {
		return nil
	}
	return s.fallback
}

// Pick 优先命中已成功建立的 session 绑定；未命中时交给 fallback selector 选新 auth。
func (s *SessionAffinitySelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if s == nil || s.fallback == nil {
		return nil, &Error{Code: "auth_not_found", Message: "selector not configured"}
	}
	if primaryID, _ := sessionAffinityIDsFromOptions(opts); primaryID == "" {
		selectorLogEntry(ctx).Debugf("session-affinity: no session ID, fallback selector | provider=%s model=%s", provider, model)
		return s.fallback.Pick(ctx, provider, model, opts, auths)
	}
	if auth, ok := s.pickBoundAuth(ctx, provider, model, opts, auths); ok {
		return auth, nil
	}
	return s.fallback.Pick(ctx, provider, model, opts, auths)
}

func (s *SessionAffinitySelector) pickBoundAuth(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, bool) {
	boundAuthID, ok := s.resolveBoundAuthID(ctx, provider, model, opts)
	if !ok {
		return nil, false
	}
	available, err := getAvailableAuths(auths, provider, model, time.Now())
	if err != nil {
		return nil, false
	}
	for _, auth := range available {
		if auth.ID == boundAuthID {
			return auth, true
		}
	}
	return nil, false
}

func (s *SessionAffinitySelector) resolveBoundAuthID(ctx context.Context, provider, model string, opts cliproxyexecutor.Options) (string, bool) {
	primaryID, fallbackID := sessionAffinityIDsFromOptions(opts)
	if primaryID == "" || s == nil || s.cache == nil {
		return "", false
	}
	cachedAuthID, ok := s.cache.GetAndRefresh(sessionAffinityCacheKey(provider, model, primaryID))
	if ok {
		selectorLogEntry(ctx).Infof("session-affinity: cache hit | session=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), cachedAuthID, provider, canonicalModelKey(model))
		return cachedAuthID, true
	}
	if fallbackID == "" || fallbackID == primaryID {
		return "", false
	}
	cachedAuthID, ok = s.cache.GetAndRefresh(sessionAffinityCacheKey(provider, model, fallbackID))
	if !ok {
		return "", false
	}
	selectorLogEntry(ctx).Infof("session-affinity: fallback cache hit | session=%s fallback=%s auth=%s provider=%s model=%s", truncateSessionID(primaryID), truncateSessionID(fallbackID), cachedAuthID, provider, canonicalModelKey(model))
	return cachedAuthID, true
}

// ObserveResult 透传反馈型 selector 的结果统计。
func (s *SessionAffinitySelector) ObserveResult(result Result, now time.Time) {
	if observer, ok := selectorResultObserver(s.fallback); ok {
		observer.ObserveResult(result, now)
	}
}

// BindSelectedAuth 在请求最终成功后刷新 session->auth 映射，确保切换成功后后续继续命中新 auth。
func (s *SessionAffinitySelector) BindSelectedAuth(metadata map[string]any, provider, model, authID string) {
	if s == nil || strings.TrimSpace(authID) == "" {
		return
	}
	primaryID, fallbackID := sessionAffinityIDsFromMetadata(metadata)
	if primaryID == "" {
		return
	}
	s.bindIDs(provider, model, authID, primaryID, fallbackID)
}

func (s *SessionAffinitySelector) bindIDs(provider, model, authID string, sessionIDs ...string) {
	if s == nil || s.cache == nil {
		return
	}
	for _, sessionID := range sessionIDs {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			continue
		}
		s.cache.Set(sessionAffinityCacheKey(provider, model, sessionID), authID)
	}
}

// InvalidateAuth 删除某个 auth 对应的全部会话绑定。
func (s *SessionAffinitySelector) InvalidateAuth(authID string) {
	if s == nil || s.cache == nil {
		return
	}
	s.cache.InvalidateAuth(strings.TrimSpace(authID))
}

// Stop 停止缓存清理协程，并递归释放内部 selector 资源。
func (s *SessionAffinitySelector) Stop() {
	if s == nil {
		return
	}
	if s.cache != nil {
		s.cache.Stop()
	}
	stopSelector(s.fallback)
}

func ensureSessionAffinityMetadata(opts cliproxyexecutor.Options, selector Selector) cliproxyexecutor.Options {
	if sessionAffinitySelectorOf(selector) == nil {
		return opts
	}
	if primaryID, _ := sessionAffinityIDsFromMetadata(opts.Metadata); primaryID != "" {
		return opts
	}
	primaryID, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if primaryID == "" {
		return opts
	}
	meta := cloneMetadataWithExtra(opts.Metadata, 2)
	meta[sessionAffinityPrimaryMetadataKey] = primaryID
	if fallbackID != "" {
		meta[sessionAffinityFallbackMetadataKey] = fallbackID
	}
	opts.Metadata = meta
	return opts
}

func bindSessionAffinityFromMetadata(selector Selector, metadata map[string]any, provider, model, authID string) {
	affinity := sessionAffinitySelectorOf(selector)
	if affinity == nil {
		return
	}
	affinity.BindSelectedAuth(metadata, provider, model, authID)
}

func sessionAffinityIDsFromOptions(opts cliproxyexecutor.Options) (string, string) {
	if primaryID, fallbackID := sessionAffinityIDsFromMetadata(opts.Metadata); primaryID != "" {
		return primaryID, fallbackID
	}
	return extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
}

func sessionAffinityIDsFromMetadata(metadata map[string]any) (string, string) {
	primaryID := metadataString(metadata, sessionAffinityPrimaryMetadataKey)
	if primaryID == "" {
		return "", ""
	}
	fallbackID := metadataString(metadata, sessionAffinityFallbackMetadataKey)
	return primaryID, fallbackID
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func sessionAffinityCacheKey(provider, model, sessionID string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "::" + sessionID + "::" + canonicalModelKey(model)
}

func selectorLogEntry(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if requestID := logging.GetRequestID(ctx); requestID != "" {
		return log.WithField("request_id", requestID)
	}
	return log.NewEntry(log.StandardLogger())
}

func truncateSessionID(sessionID string) string {
	if len(sessionID) <= 20 {
		return sessionID
	}
	return sessionID[:8] + "..."
}
