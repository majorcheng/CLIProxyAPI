package watcher

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	log "github.com/sirupsen/logrus"
)

func matchProvider(provider string, targets []string) (string, bool) {
	p := strings.ToLower(strings.TrimSpace(provider))
	for _, t := range targets {
		if strings.EqualFold(p, strings.TrimSpace(t)) {
			return p, true
		}
	}
	return p, false
}

// addWatchTargets 统一注册配置目录与鉴权目录监听，并显式保持缺失配置文件时启动失败的旧语义。
func (w *Watcher) addWatchTargets() error {
	if _, err := os.Stat(w.configPath); err != nil {
		log.Errorf("failed to access config file %s: %v", w.configPath, err)
		return err
	}

	configDir := filepath.Dir(w.configPath)
	if err := w.addWatchPath(configDir, "config directory"); err != nil {
		return err
	}

	if w.normalizeAuthPath(configDir) == w.normalizeAuthPath(w.authDir) {
		log.Debugf("config directory and auth directory share one watcher: %s", configDir)
		return nil
	}

	return w.addWatchPath(w.authDir, "auth directory")
}

// addWatchPath 封装底层 watcher 注册，统一日志格式，避免重复拼接错误信息。
func (w *Watcher) addWatchPath(path, label string) error {
	if err := w.watcher.Add(path); err != nil {
		log.Errorf("failed to watch %s %s: %v", label, path, err)
		return err
	}
	log.Debugf("watching %s: %s", label, path)
	return nil
}

func (w *Watcher) authFileUnchanged(path string) (bool, error) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return false, errRead
	}
	if len(data) == 0 {
		return false, nil
	}
	sum := sha256.Sum256(data)
	curHash := hex.EncodeToString(sum[:])

	normalized := w.normalizeAuthPath(path)
	w.clientsMutex.RLock()
	prevHash, ok := w.lastAuthHashes[normalized]
	w.clientsMutex.RUnlock()
	if ok && prevHash == curHash {
		return true, nil
	}
	return false, nil
}

func (w *Watcher) isKnownAuthFile(path string) bool {
	normalized := w.normalizeAuthPath(path)
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	_, ok := w.lastAuthHashes[normalized]
	return ok
}

func (w *Watcher) normalizeAuthPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.Clean(trimmed)
	if runtime.GOOS == "windows" {
		cleaned = strings.TrimPrefix(cleaned, `\\?\`)
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}
