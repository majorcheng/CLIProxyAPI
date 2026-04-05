package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
)

var (
	authMaintenanceRenameFile = os.Rename
	authMaintenanceNow        = time.Now

	authMaintenanceReasonBucketSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
)

type authMaintenanceStorePersister interface {
	PersistAuthFiles(ctx context.Context, message string, paths ...string) error
}

// archiveAuthMaintenanceCandidate 会把命中 auth-maintenance 的源文件移到垃圾站。
// 这里不能继续直接删除：用户明确要求先保留“缓冲区”，方便后续人工核查和手工恢复。
func (s *Service) archiveAuthMaintenanceCandidate(path string, reason string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("auth maintenance path is empty")
	}
	trashDir := filepath.Join(s.authMaintenanceTrashRoot(), resolveAuthMaintenanceReasonBucket(reason))
	return moveFileToAuthMaintenanceTrash(path, trashDir)
}

// authMaintenanceTrashRoot 返回垃圾站根目录。
// 这里优先复用现有 logs 目录解析，但如果 logs 被解析到 authDir 内部，
// 会退回到 authDir 同级的 logs，避免文件型 token store 把垃圾站 JSON 再次扫回 auth 池。
func (s *Service) authMaintenanceTrashRoot() string {
	if s == nil {
		return filepath.Join("logs", "delete")
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	logDir := strings.TrimSpace(internallogging.ResolveLogDirectory(cfg))
	if logDir == "" {
		logDir = "logs"
	}
	authDir := ""
	if cfg != nil {
		authDir = strings.TrimSpace(cfg.AuthDir)
	}
	return resolveAuthMaintenanceTrashRoot(logDir, authDir)
}

func resolveAuthMaintenanceTrashRoot(logDir string, authDir string) string {
	cleanLogDir := cleanAbsolutePath(strings.TrimSpace(logDir))
	if cleanLogDir == "" {
		cleanLogDir = cleanAbsolutePath("logs")
	}
	cleanAuthDir := cleanAbsoluteAuthDir(authDir)
	if cleanAuthDir != "" && pathWithinBase(cleanLogDir, cleanAuthDir) {
		cleanLogDir = filepath.Join(filepath.Dir(cleanAuthDir), "logs")
	}
	return filepath.Join(cleanLogDir, "delete")
}

func resolveAuthMaintenanceReasonBucket(reason string) string {
	reason = strings.TrimSpace(reason)
	if strings.HasPrefix(reason, "http_") {
		status := strings.TrimSpace(strings.TrimPrefix(reason, "http_"))
		if status != "" {
			return status
		}
	}
	if reason == "" {
		return "unknown"
	}
	sanitized := authMaintenanceReasonBucketSanitizer.ReplaceAllString(reason, "_")
	sanitized = strings.Trim(sanitized, "._-")
	if sanitized == "" {
		return "unknown"
	}
	return sanitized
}

func moveFileToAuthMaintenanceTrash(src string, trashDir string) (string, error) {
	src = cleanAbsolutePath(strings.TrimSpace(src))
	if src == "" {
		return "", fmt.Errorf("auth maintenance source path is empty")
	}
	trashDir = cleanAbsolutePath(strings.TrimSpace(trashDir))
	if trashDir == "" {
		return "", fmt.Errorf("auth maintenance trash directory is empty")
	}
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat auth file: %w", err)
	}
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return "", fmt.Errorf("create auth maintenance trash directory: %w", err)
	}

	dst, err := nextAuthMaintenanceTrashPath(trashDir, filepath.Base(src))
	if err != nil {
		return "", err
	}
	if err := moveFileWithCrossDeviceFallback(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func nextAuthMaintenanceTrashPath(dir string, base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == string(os.PathSeparator) {
		return "", fmt.Errorf("auth maintenance trash filename is empty")
	}
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := filepath.Join(dir, base)
		if attempt > 0 {
			suffix := authMaintenanceNow().UTC().Format("20060102T150405.000000000Z")
			if attempt > 1 {
				suffix = fmt.Sprintf("%s-%d", suffix, attempt)
			}
			candidate = filepath.Join(dir, fmt.Sprintf("%s-%s%s", name, suffix, ext))
		}
		if _, err := os.Stat(candidate); err == nil {
			continue
		} else if os.IsNotExist(err) {
			return candidate, nil
		} else {
			return "", fmt.Errorf("stat auth maintenance trash target: %w", err)
		}
	}
	return "", fmt.Errorf("auth maintenance trash target collision overflow for %s", base)
}

func moveFileWithCrossDeviceFallback(src string, dst string) error {
	if err := authMaintenanceRenameFile(src, dst); err == nil {
		return nil
	} else if os.IsNotExist(err) {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("move auth file to trash: %w", err)
	}
	return copyFileToTrashAndRemoveSource(src, dst)
}

func copyFileToTrashAndRemoveSource(src string, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open auth file for trash copy: %w", err)
	}
	defer func() {
		if closeErr := in.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close auth source file: %w", closeErr)
		}
	}()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat auth file for trash copy: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".auth-maintenance-trash-*.tmp")
	if err != nil {
		return fmt.Errorf("create auth maintenance trash temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy auth file to trash temp file: %w", err)
	}
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod auth maintenance trash temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close auth maintenance trash temp file: %w", err)
	}
	if err = authMaintenanceRenameFile(tmpName, dst); err != nil {
		return fmt.Errorf("finalize auth maintenance trash temp file: %w", err)
	}
	if err = os.Remove(src); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove original auth file after trash copy: %w", err)
	}
	return nil
}

// deleteAuthMaintenanceTokenRecord 在文件已移入垃圾站后继续同步持久化后端。
// 优先走 PersistAuthFiles，因为它能处理“源路径已不存在”的场景（git/object/postgres）。
// 不支持该接口的本地文件 store 再退回 Delete，此时只会得到一个幂等 no-op。
func (s *Service) deleteAuthMaintenanceTokenRecord(ctx context.Context, path string, reason string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	store := sdkAuth.GetTokenStore()
	if store == nil {
		return fmt.Errorf("token store unavailable")
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg != nil {
		if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(cfg.AuthDir)
		}
		if proxySetter, ok := store.(interface{ SetGlobalProxyURL(string) }); ok {
			proxySetter.SetGlobalProxyURL(cfg.ProxyURL)
		}
	}
	if persister, ok := store.(authMaintenanceStorePersister); ok {
		return persister.PersistAuthFiles(ctx, authMaintenanceDeleteMessage(path, reason), path)
	}
	return store.Delete(ctx, path)
}

func authMaintenanceDeleteMessage(path string, reason string) string {
	name := filepath.Base(strings.TrimSpace(path))
	if name == "" {
		name = "unknown"
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Sprintf("auth-maintenance 归档移除 %s", name)
	}
	return fmt.Sprintf("auth-maintenance 归档移除 %s (%s)", name, reason)
}

func cleanAbsoluteAuthDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if resolved, err := util.ResolveAuthDir(path); err == nil {
		path = resolved
	}
	return cleanAbsolutePath(path)
}

func cleanAbsolutePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return path
}

func pathWithinBase(path string, base string) bool {
	path = cleanAbsolutePath(path)
	base = cleanAbsolutePath(base)
	if path == "" || base == "" {
		return false
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")
}
