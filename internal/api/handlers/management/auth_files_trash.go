package management

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const managementDeleteTrashBucket = "management"

// archiveDeletedAuthFile 会把 management 主动删除的 token 归档到 logs/delete 下，
// 让文件先离开活跃 auth 池，同时保留一份可追溯的本地副本。
func (h *Handler) archiveDeletedAuthFile(path string) (string, bool, error) {
	path = cleanManagementTrashPath(path)
	if path == "" {
		return "", false, fmt.Errorf("auth path is empty")
	}
	trashDir := filepath.Join(h.deletedAuthTrashRoot(), managementDeleteTrashBucket)
	archivedPath, err := moveDeletedAuthFileToTrash(path, trashDir)
	if err != nil {
		return "", false, err
	}
	if archivedPath == "" {
		return "", true, nil
	}
	return archivedPath, false, nil
}

// deletedAuthTrashRoot 统一解析 management 删除归档的根目录。
// 当 logs 目录落在 authDir 内部时，归档会改放到 authDir 同级 logs，
// 避免被 auth 文件扫描重新纳入活跃池。
func (h *Handler) deletedAuthTrashRoot() string {
	logDir := strings.TrimSpace(h.logDirectory())
	if logDir == "" {
		logDir = "logs"
	}
	authDir := ""
	if cfg := h.currentConfigSnapshot(); cfg != nil {
		authDir = strings.TrimSpace(cfg.AuthDir)
	}
	return resolveDeletedAuthTrashRoot(logDir, authDir)
}

func resolveDeletedAuthTrashRoot(logDir string, authDir string) string {
	logDir = cleanManagementTrashPath(logDir)
	if logDir == "" {
		logDir = cleanManagementTrashPath("logs")
	}
	authDir = cleanManagementTrashAuthDir(authDir)
	if authDir != "" && pathWithinManagementTrashBase(logDir, authDir) {
		logDir = filepath.Join(filepath.Dir(authDir), "logs")
	}
	return filepath.Join(logDir, "delete")
}

// moveDeletedAuthFileToTrash 负责把单个 auth 文件移入归档目录。
// 这里先做目录创建与目标路径分配，再交给底层移动逻辑处理跨设备场景。
func moveDeletedAuthFileToTrash(src string, trashDir string) (string, error) {
	src = cleanManagementTrashPath(src)
	trashDir = cleanManagementTrashPath(trashDir)
	if src == "" {
		return "", fmt.Errorf("auth path is empty")
	}
	if trashDir == "" {
		return "", fmt.Errorf("trash directory is empty")
	}
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat auth file: %w", err)
	}
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return "", fmt.Errorf("create trash directory: %w", err)
	}
	dst, err := nextDeletedAuthTrashPath(trashDir, filepath.Base(src))
	if err != nil {
		return "", err
	}
	if err := moveDeletedAuthFile(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// moveDeletedAuthFile 优先使用 rename 保持原子移动语义。
// 当源目录与目标目录跨设备时，会自动切到复制再删除源文件的保守路径。
func moveDeletedAuthFile(src string, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if os.IsNotExist(err) {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("archive auth file to trash: %w", err)
	}
	return copyDeletedAuthFileToTrash(src, dst)
}

// copyDeletedAuthFileToTrash 处理 EXDEV 场景下的跨设备归档。
// 先写临时文件再 rename 到最终目标，保证归档目录里始终只有完整文件。
func copyDeletedAuthFileToTrash(src string, dst string) (err error) {
	srcFile, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open auth file for trash copy: %w", err)
	}
	defer func() {
		if closeErr := srcFile.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close auth source file: %w", closeErr)
		}
	}()

	info, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("stat auth file for trash copy: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".management-delete-trash-*.tmp")
	if err != nil {
		return fmt.Errorf("create trash temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = io.Copy(tmp, srcFile); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy auth file to trash temp file: %w", err)
	}
	if err = tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod trash temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close trash temp file: %w", err)
	}
	if err = os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("finalize trash temp file: %w", err)
	}
	if err = os.Remove(src); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove original auth file after trash copy: %w", err)
	}
	return nil
}

// nextDeletedAuthTrashPath 为同名文件生成稳定的归档落点。
// 首次直接复用原文件名，冲突时追加 UTC 时间戳避免覆盖历史归档。
func nextDeletedAuthTrashPath(dir string, base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", fmt.Errorf("trash filename is empty")
	}
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := filepath.Join(dir, base)
		if attempt > 0 {
			suffix := time.Now().UTC().Format("20060102T150405.000000000Z")
			if attempt > 1 {
				suffix = fmt.Sprintf("%s-%d", suffix, attempt)
			}
			candidate = filepath.Join(dir, fmt.Sprintf("%s-%s%s", name, suffix, ext))
		}
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("stat trash target: %w", err)
		}
	}
	return "", fmt.Errorf("trash target collision overflow for %s", base)
}

// cleanManagementTrashAuthDir 会先按 authDir 语义解析，再转成绝对规范路径。
func cleanManagementTrashAuthDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if resolved, err := util.ResolveAuthDir(path); err == nil {
		path = resolved
	}
	return cleanManagementTrashPath(path)
}

// cleanManagementTrashPath 统一把输入收敛成绝对规范路径。
func cleanManagementTrashPath(path string) string {
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

// pathWithinManagementTrashBase 用于判断日志目录是否落在 authDir 内部。
func pathWithinManagementTrashBase(path string, base string) bool {
	path = cleanManagementTrashPath(path)
	base = cleanManagementTrashPath(base)
	if path == "" || base == "" {
		return false
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..")
}
