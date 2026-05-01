package management

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// disableAuthsForDeletedPath 按 token 实际落盘路径禁用所有同源 auth。
// 同一个 Codex token 文件可能派生 primary/project 等多个 auth ID；
// 删除文件时必须一次性清掉这些 ID，避免 fill-first 继续命中兄弟候选。
func (h *Handler) disableAuthsForDeletedPath(ctx context.Context, targetPath string, fallbackID string) {
	ids := h.authIDsForDeletedPath(targetPath, fallbackID)
	if len(ids) == 0 {
		h.disableAuth(ctx, targetPath)
		return
	}
	for _, id := range ids {
		h.disableAuth(ctx, id)
	}
}

// authIDsForDeletedPath 返回与目标 backing path 绑定的全部 auth ID。
func (h *Handler) authIDsForDeletedPath(targetPath string, fallbackID string) []string {
	if h == nil {
		return nil
	}
	manager := h.currentAuthManager()
	if manager == nil {
		return nil
	}
	targetPath = h.normalizeAuthDeletePath(targetPath)
	ids := make([]string, 0)
	seen := make(map[string]struct{})
	ids = appendUniqueAuthID(ids, seen, fallbackID)
	for _, auth := range manager.List() {
		if h.authMatchesDeletedPath(auth, targetPath) {
			ids = appendUniqueAuthID(ids, seen, auth.ID)
		}
	}
	return ids
}

// authMatchesDeletedPath 判断某个 auth 是否来自本次删除的同一个 token 文件。
func (h *Handler) authMatchesDeletedPath(auth *coreauth.Auth, targetPath string) bool {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || targetPath == "" {
		return false
	}
	return h.normalizeAuthDeletePath(authDeleteBackingPath(auth)) == targetPath
}

// authDeleteBackingPath 提取 auth 对应的持久化文件路径。
func authDeleteBackingPath(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			return path
		}
	}
	return strings.TrimSpace(auth.FileName)
}

// normalizeAuthDeletePath 对删除匹配路径做统一清洗，避免相对路径和绝对路径不一致。
func (h *Handler) normalizeAuthDeletePath(path string) string {
	return h.normalizeAuthDeletePathForCase(path, runtime.GOOS == "windows")
}

// normalizeAuthDeletePathForCase 生成删除匹配 key；大小写不敏感平台必须折叠大小写。
func (h *Handler) normalizeAuthDeletePathForCase(path string, caseInsensitive bool) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		cfg := h.currentConfigSnapshot()
		if cfg == nil || strings.TrimSpace(cfg.AuthDir) == "" {
			return foldAuthDeletePathCase(filepath.Clean(path), caseInsensitive)
		}
		path = filepath.Join(cfg.AuthDir, filepath.Base(path))
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return foldAuthDeletePathCase(filepath.Clean(path), caseInsensitive)
}

// foldAuthDeletePathCase 保持删除路径 key 与平台大小写敏感语义一致。
func foldAuthDeletePathCase(path string, caseInsensitive bool) string {
	if caseInsensitive {
		return strings.ToLower(path)
	}
	return path
}

// appendUniqueAuthID 维持禁用列表去重，避免重复 Update 同一个 auth。
func appendUniqueAuthID(ids []string, seen map[string]struct{}, id string) []string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ids
	}
	if _, ok := seen[id]; ok {
		return ids
	}
	seen[id] = struct{}{}
	return append(ids, id)
}
