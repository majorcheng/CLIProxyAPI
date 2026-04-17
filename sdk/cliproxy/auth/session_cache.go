package auth

import (
	"sync"
	"time"
)

// sessionAffinityEntry 保存一次会话绑定及其过期时间。
type sessionAffinityEntry struct {
	authID    string
	expiresAt time.Time
}

// SessionCache 提供带 TTL 的 session -> auth 绑定缓存。
// 活跃会话命中时会刷新 TTL，后台协程会定期清理过期条目。
type SessionCache struct {
	mu      sync.RWMutex
	entries map[string]sessionAffinityEntry
	ttl     time.Duration
	stopCh  chan struct{}
}

// NewSessionCache 创建一个带后台清理的会话缓存。
func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	cache := &SessionCache{
		entries: make(map[string]sessionAffinityEntry),
		ttl:     ttl,
		stopCh:  make(chan struct{}),
	}
	go cache.cleanupLoop()
	return cache
}

// Get 读取绑定，不刷新 TTL。
func (c *SessionCache) Get(sessionID string) (string, bool) {
	if c == nil || sessionID == "" {
		return "", false
	}
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", false
	}
	return entry.authID, true
}

// GetAndRefresh 命中时刷新 TTL，让活跃会话继续粘在当前 auth。
func (c *SessionCache) GetAndRefresh(sessionID string) (string, bool) {
	if c == nil || sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	entry, ok := c.entries[sessionID]
	if !ok {
		c.mu.Unlock()
		return "", false
	}
	if now.After(entry.expiresAt) {
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", false
	}
	entry.expiresAt = now.Add(c.ttl)
	c.entries[sessionID] = entry
	c.mu.Unlock()
	return entry.authID, true
}

// Set 写入或刷新一个会话绑定。
func (c *SessionCache) Set(sessionID, authID string) {
	if c == nil || sessionID == "" || authID == "" {
		return
	}
	c.mu.Lock()
	c.entries[sessionID] = sessionAffinityEntry{
		authID:    authID,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// Invalidate 删除单个会话绑定。
func (c *SessionCache) Invalidate(sessionID string) {
	if c == nil || sessionID == "" {
		return
	}
	c.mu.Lock()
	delete(c.entries, sessionID)
	c.mu.Unlock()
}

// InvalidateAuth 删除某个 auth 相关的全部会话绑定。
func (c *SessionCache) InvalidateAuth(authID string) {
	if c == nil || authID == "" {
		return
	}
	c.mu.Lock()
	for sessionID, entry := range c.entries {
		if entry.authID == authID {
			delete(c.entries, sessionID)
		}
	}
	c.mu.Unlock()
}

// Stop 停止后台清理协程。
func (c *SessionCache) Stop() {
	if c == nil {
		return
	}
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
}

func (c *SessionCache) cleanupLoop() {
	interval := c.ttl / 2
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

func (c *SessionCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	for sessionID, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, sessionID)
		}
	}
	c.mu.Unlock()
}
