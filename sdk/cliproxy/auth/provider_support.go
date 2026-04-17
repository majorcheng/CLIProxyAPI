package auth

import (
	"errors"
	"fmt"
	"strings"
)

var ErrUnsupportedPersistedAuthProvider = errors.New("unsupported persisted auth provider")

const removedQwenSupportVersion = "v6.9.26"

// ValidatePersistedAuthProvider 显式拒绝已经下线、但旧 auth 文件里仍可能出现的 provider。
// 这样升级后会直接暴露不再支持的历史凭证，而不是继续把它们读进内存后静默失效。
func ValidatePersistedAuthProvider(provider string) error {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "":
		return nil
	case "qwen":
		return fmt.Errorf("%w: qwen provider 已在 %s 起移除支持", ErrUnsupportedPersistedAuthProvider, removedQwenSupportVersion)
	default:
		return nil
	}
}
