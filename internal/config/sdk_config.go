// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DisableImageGeneration 控制内建 image_generation 图片生成能力。
	// false 保持默认行为；true 全局禁用并让图片入口返回 404；chat 仅禁用非 Images 入口。
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// EnableGeminiCLIEndpoint 控制 Gemini CLI 内部端点（/v1internal:*）是否可用。
	// 默认 false，避免未预期地暴露仅供本机 CLI 使用的内部入口。
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// CodexInitialRefreshOnLoad 控制 Codex 文件型/OAuth auth 在当前服务
	// 首次读到该 token 时，是否只要 refresh_token 非空就先强制 refresh 一次，
	// 以优先校准 access_token / expired / last_refresh 等运行字段。
	// 默认 false，保持现有保守门控不变。
	CodexInitialRefreshOnLoad bool `yaml:"codex-initial-refresh-on-load" json:"codex-initial-refresh-on-load"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// RequestAudit emits per-attempt request/response audit events to an external hook.
	RequestAudit RequestAuditConfig `yaml:"request-audit" json:"request-audit"`

	// APIKeys 保持对外公开 Go API 的旧形状：仍然是纯字符串列表。
	// 若需要携带 max-priority 等对象化策略，请通过 ClientAPIKeyEntries /
	// SetClientAPIKeyEntries 这组 helper 读写；YAML 仍兼容字符串或对象混写。
	APIKeys APIKeyList `yaml:"api-keys,omitempty" json:"api-keys"`

	// clientAPIKeyEntries 存放对象化后的内部策略视图，避免破坏公开 SDK 的
	// `APIKeys: []string{...}` 结构体初始化方式。
	clientAPIKeyEntries []ClientAPIKey `yaml:"-" json:"-"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// RequestAuditConfig controls asynchronous request audit emission for external analysis.
type RequestAuditConfig struct {
	// Enable toggles audit event emission.
	Enable bool `yaml:"enable" json:"enable"`

	// Endpoint is the destination for JSON POST events.
	// Supported values:
	//   - http://host/path
	//   - https://host/path
	//   - unix:///absolute/path.sock
	Endpoint string `yaml:"endpoint" json:"endpoint"`

	// Providers limits audit emission to the listed provider identifiers.
	// Empty means all providers.
	Providers []string `yaml:"providers,omitempty" json:"providers,omitempty"`

	// QueueSize is the async in-memory queue length. Default: 256.
	QueueSize int `yaml:"queue-size,omitempty" json:"queue-size,omitempty"`

	// TimeoutSeconds is the per-delivery timeout. Default: 5.
	TimeoutSeconds int `yaml:"timeout-seconds,omitempty" json:"timeout-seconds,omitempty"`

	// MaxBodyBytes truncates request/response bodies captured in audit events after JSON compaction.
	// Default: 262144 (256 KiB).
	MaxBodyBytes int `yaml:"max-body-bytes,omitempty" json:"max-body-bytes,omitempty"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
