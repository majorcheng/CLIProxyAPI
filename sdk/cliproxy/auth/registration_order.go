package auth

import (
	"sort"
	"strings"
	"time"
)

// FirstRegisteredAtMetadataKey 是 auth JSON 顶层保留字段，
// 用于记录 credential 首次进入池子的稳定时间。
// 之所以单独使用一个 CLIProxyAPI 自己的键，而不是直接复用 created_at，
// 是为了避免和上游/provider 自带字段或运行时时间语义混淆。
const FirstRegisteredAtMetadataKey = "cli_proxy_first_registered_at"

// ParseFirstRegisteredAtValue 解析首次入池时间字段。
// 当前统一使用 RFC3339Nano 字符串；这里额外兼容 time.Time，
// 便于测试或内存态直接传值。
func ParseFirstRegisteredAtValue(raw any) (time.Time, bool) {
	switch value := raw.(type) {
	case time.Time:
		if value.IsZero() {
			return time.Time{}, false
		}
		return value.UTC(), true
	case string:
		if value == "" {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.IsZero() {
			return time.Time{}, false
		}
		return parsed.UTC(), true
	default:
		return time.Time{}, false
	}
}

// FirstRegisteredAtFromMetadata 从 metadata 中读取稳定首次入池时间。
func FirstRegisteredAtFromMetadata(metadata map[string]any) (time.Time, bool) {
	if metadata == nil {
		return time.Time{}, false
	}
	return ParseFirstRegisteredAtValue(metadata[FirstRegisteredAtMetadataKey])
}

// FirstRegisteredAt 返回 auth 当前可用的首次入池时间：
// 先看稳定 metadata 字段，没有则退回运行时 CreatedAt。
func FirstRegisteredAt(auth *Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	if registeredAt, ok := FirstRegisteredAtFromMetadata(auth.Metadata); ok {
		return registeredAt, true
	}
	if auth.CreatedAt.IsZero() {
		return time.Time{}, false
	}
	return auth.CreatedAt.UTC(), true
}

// EnsureFirstRegisteredAt 确保 auth 带有稳定首次入池时间。
// 当 metadata 已存在该字段时，只做 CreatedAt 对齐；
// 当 metadata 不为空但字段缺失时，会用 fallback 回填；
// 当 metadata 为空时，仅对齐 CreatedAt，不额外创建 metadata，
// 以免把原本不需要落盘的 config/runtime-only auth 意外变成可持久化对象。
func EnsureFirstRegisteredAt(auth *Auth, fallback time.Time) time.Time {
	registeredAt, _ := ensureFirstRegisteredAtWithChanged(auth, fallback)
	return registeredAt
}

func ensureFirstRegisteredAtWithChanged(auth *Auth, fallback time.Time) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	if registeredAt, ok := FirstRegisteredAtFromMetadata(auth.Metadata); ok {
		if auth.CreatedAt.IsZero() || !auth.CreatedAt.Equal(registeredAt) {
			auth.CreatedAt = registeredAt
		}
		return registeredAt, false
	}

	if fallback.IsZero() {
		fallback = auth.CreatedAt
	}
	if fallback.IsZero() {
		fallback = time.Now().UTC()
	} else {
		fallback = fallback.UTC()
	}
	auth.CreatedAt = fallback

	if auth.Metadata == nil {
		return fallback, false
	}
	auth.Metadata[FirstRegisteredAtMetadataKey] = fallback.Format(time.RFC3339Nano)
	return fallback, true
}

// NormalizeChatGPTPlanType 统一 Codex/ChatGPT 账号计划类型写法，
// 便于在 fill-first 中做稳定排序。
// 目前将 business/go 视为 team 同一档位，和现有模型放行逻辑保持一致。
func NormalizeChatGPTPlanType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pro":
		return "pro"
	case "plus":
		return "plus"
	case "team", "business", "go":
		return "team"
	case "free":
		return "free"
	default:
		return ""
	}
}

// ChatGPTPlanTypeSortRank 返回计划类型的排序权重：
// pro -> plus -> team -> free。
// 未识别的类型返回 ok=false，由调用方决定是否回退到其它排序键。
func ChatGPTPlanTypeSortRank(raw string) (rank int, ok bool) {
	switch NormalizeChatGPTPlanType(raw) {
	case "pro":
		return 0, true
	case "plus":
		return 1, true
	case "team":
		return 2, true
	case "free":
		return 3, true
	default:
		return 0, false
	}
}

// AuthChatGPTPlanType 读取 auth 上可用的计划类型。
// 优先使用 Attributes 中已整理好的 plan_type；若没有，再回退到 metadata。
func AuthChatGPTPlanType(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if normalized := NormalizeChatGPTPlanType(auth.Attributes["plan_type"]); normalized != "" {
			return normalized
		}
	}
	if auth.Metadata != nil {
		if raw, ok := auth.Metadata["plan_type"].(string); ok {
			return NormalizeChatGPTPlanType(raw)
		}
	}
	return ""
}

func authProviderKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func firstRegisteredAtLess(left, right *Auth) bool {
	// 仅在同一 provider 内比较计划档位，避免 mixed-provider 场景被
	// ChatGPT plan_type 扩散成跨 provider 的全局优先级。
	if authProviderKey(left) == authProviderKey(right) {
		leftRank, leftRankOK := ChatGPTPlanTypeSortRank(AuthChatGPTPlanType(left))
		rightRank, rightRankOK := ChatGPTPlanTypeSortRank(AuthChatGPTPlanType(right))
		switch {
		case leftRankOK != rightRankOK:
			return leftRankOK
		case leftRankOK && rightRankOK && leftRank != rightRank:
			return leftRank < rightRank
		}
	}

	leftTime, leftOK := FirstRegisteredAt(left)
	rightTime, rightOK := FirstRegisteredAt(right)

	switch {
	case leftOK && rightOK && !leftTime.Equal(rightTime):
		return leftTime.Before(rightTime)
	case leftOK != rightOK:
		return leftOK
	}

	leftID := ""
	if left != nil {
		leftID = left.ID
	}
	rightID := ""
	if right != nil {
		rightID = right.ID
	}
	return leftID < rightID
}

func sortAuthsByFirstRegisteredAt(auths []*Auth) {
	if len(auths) <= 1 {
		return
	}
	sort.Slice(auths, func(i, j int) bool {
		return firstRegisteredAtLess(auths[i], auths[j])
	})
}
