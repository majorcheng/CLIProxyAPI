package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const deprecatedPriorityZeroDisabledAPIKeysField = "priority-zero-disabled-api-keys"

// ClientAPIKey 描述一个入站 client api-key 配置。
// 当 MaxPriority 为空时，行为与旧版纯字符串 api-key 完全一致。
type ClientAPIKey struct {
	// Key 是客户端访问代理时使用的入站鉴权 key。
	Key string `yaml:"key" json:"key"`
	// MaxPriority 为可选的 auth 优先级上限；仅允许命中 priority <= 该值的候选。
	MaxPriority *int `yaml:"max-priority,omitempty" json:"max-priority,omitempty"`
}

// APIKeyList 保持公开 Go API 的旧字段类型语义。
// 它底层仍然是 []string，因此外部调用方可继续直接赋值 `[]string{"k"}`。
type APIKeyList []string

// UnmarshalYAML 兼容两种写法：
// 1) 纯字符串：- "client-key"
// 2) 对象：- { key: "client-key", max-priority: 0 }
func (entry *ClientAPIKey) UnmarshalYAML(node *yaml.Node) error {
	if entry == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		entry.Key = strings.TrimSpace(node.Value)
		entry.MaxPriority = nil
		return nil
	case yaml.MappingNode:
		var raw struct {
			Key         string `yaml:"key"`
			MaxPriority *int   `yaml:"max-priority"`
		}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		entry.Key = strings.TrimSpace(raw.Key)
		entry.MaxPriority = cloneOptionalInt(raw.MaxPriority)
		return nil
	default:
		return fmt.Errorf("api-keys 项必须是字符串或对象")
	}
}

// MarshalYAML 保持配置文件的最小表达：
// 未设置 MaxPriority 时仍写回纯字符串；否则写回对象。
func (entry ClientAPIKey) MarshalYAML() (any, error) {
	if entry.MaxPriority == nil {
		return strings.TrimSpace(entry.Key), nil
	}
	return struct {
		Key         string `yaml:"key"`
		MaxPriority *int   `yaml:"max-priority,omitempty"`
	}{
		Key:         strings.TrimSpace(entry.Key),
		MaxPriority: cloneOptionalInt(entry.MaxPriority),
	}, nil
}

// UnmarshalJSON 兼容 management API 的旧字符串写法与新对象写法。
func (entry *ClientAPIKey) UnmarshalJSON(data []byte) error {
	if entry == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*entry = ClientAPIKey{}
		return nil
	}

	var rawString string
	if err := json.Unmarshal(trimmed, &rawString); err == nil {
		entry.Key = strings.TrimSpace(rawString)
		entry.MaxPriority = nil
		return nil
	}

	var raw struct {
		Key         string `json:"key"`
		MaxPriority *int   `json:"max-priority"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return err
	}
	entry.Key = strings.TrimSpace(raw.Key)
	entry.MaxPriority = cloneOptionalInt(raw.MaxPriority)
	return nil
}

// ClientAPIKeyEntries 返回对象化后的 client key 视图。
func (cfg *SDKConfig) ClientAPIKeyEntries() []ClientAPIKey {
	if cfg == nil {
		return nil
	}
	if len(cfg.clientAPIKeyEntries) == 0 {
		return ClientAPIKeysFromStrings(cfg.APIKeys)
	}
	return cloneClientAPIKeys(cfg.clientAPIKeyEntries)
}

// SetClientAPIKeyEntries 统一更新公开字符串视图与内部对象视图。
func (cfg *SDKConfig) SetClientAPIKeyEntries(entries []ClientAPIKey) {
	if cfg == nil {
		return
	}
	cfg.syncClientAPIKeyViews(NormalizeClientAPIKeys(entries))
}

// UnmarshalYAML 让公开 `APIKeys` 仍可接收混合写法的 YAML，并只暴露纯字符串视图。
func (keys *APIKeyList) UnmarshalYAML(node *yaml.Node) error {
	if keys == nil {
		return nil
	}
	var entries []ClientAPIKey
	if err := node.Decode(&entries); err != nil {
		return err
	}
	*keys = clientAPIKeyListFromEntries(entries)
	return nil
}

// UnmarshalJSON 让公开 `APIKeys` 在 JSON 侧也能兼容对象数组输入。
func (keys *APIKeyList) UnmarshalJSON(data []byte) error {
	if keys == nil {
		return nil
	}
	var entries []ClientAPIKey
	if err := json.Unmarshal(data, &entries); err == nil {
		*keys = clientAPIKeyListFromEntries(entries)
		return nil
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	*keys = APIKeyList(normalizeUniqueTrimmedStrings(values))
	return nil
}

// NormalizeClientAPIKeys 统一做 trim、去空与按 key 去重。
// 若同一个 key 重复出现，则保留首次出现的配置，后续项忽略。
func NormalizeClientAPIKeys(entries []ClientAPIKey) []ClientAPIKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]ClientAPIKey, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ClientAPIKey{
			Key:         key,
			MaxPriority: cloneOptionalInt(entry.MaxPriority),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ClientAPIKeysFromStrings 将旧版纯字符串 key 列表提升为对象视图。
func ClientAPIKeysFromStrings(keys []string) []ClientAPIKey {
	if len(keys) == 0 {
		return nil
	}
	entries := make([]ClientAPIKey, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, ClientAPIKey{Key: strings.TrimSpace(key)})
	}
	return NormalizeClientAPIKeys(entries)
}

// FindClientAPIKey 在规范化后的配置中按 key 查找首个匹配项。
func FindClientAPIKey(entries []ClientAPIKey, key string) (ClientAPIKey, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return ClientAPIKey{}, false
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Key) != key {
			continue
		}
		return ClientAPIKey{
			Key:         strings.TrimSpace(entry.Key),
			MaxPriority: cloneOptionalInt(entry.MaxPriority),
		}, true
	}
	return ClientAPIKey{}, false
}

// FindClientAPIKeyInConfig 按当前 SDK 配置里的对象策略视图查找 key。
func FindClientAPIKeyInConfig(cfg *SDKConfig, key string) (ClientAPIKey, bool) {
	if cfg == nil {
		return ClientAPIKey{}, false
	}
	return FindClientAPIKey(cfg.ClientAPIKeyEntries(), key)
}

// ClientAPIKeyValues 仅提取入站鉴权所需的纯 key 文本。
func ClientAPIKeyValues(entries []ClientAPIKey) []string {
	if len(entries) == 0 {
		return nil
	}
	values := make([]string, 0, len(entries))
	for _, entry := range NormalizeClientAPIKeys(entries) {
		values = append(values, entry.Key)
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

// ClientAPIKeyValuesFromConfig 仅从配置中提取入站鉴权所需的纯 key 文本。
func ClientAPIKeyValuesFromConfig(cfg *SDKConfig) []string {
	if cfg == nil {
		return nil
	}
	if len(cfg.APIKeys) > 0 {
		return normalizeUniqueTrimmedStrings([]string(cfg.APIKeys))
	}
	return ClientAPIKeyValues(cfg.ClientAPIKeyEntries())
}

// ValidateNoDeprecatedAPIKeyRoutingField 在启动时拒绝已废弃的旧字段，
// 防止配置被静默忽略后失去原有限制。
func ValidateNoDeprecatedAPIKeyRoutingField(data []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0] == nil {
		return nil
	}
	if findMapKeyIndex(root.Content[0], deprecatedPriorityZeroDisabledAPIKeysField) >= 0 {
		return fmt.Errorf("无效配置：%s 已废弃，请改用 api-keys[].max-priority", deprecatedPriorityZeroDisabledAPIKeysField)
	}
	return nil
}

// DecodeClientAPIKeyEntriesFromYAML 从原始配置文本中提取对象化的 api-keys 视图。
func DecodeClientAPIKeyEntriesFromYAML(data []byte) ([]ClientAPIKey, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0] == nil {
		return nil, nil
	}
	apiKeysNode, ok := findRootKeyNode(root.Content[0], "api-keys")
	if !ok {
		return nil, nil
	}
	var entries []ClientAPIKey
	if err := apiKeysNode.Decode(&entries); err != nil {
		return nil, err
	}
	return NormalizeClientAPIKeys(entries), nil
}

// MarshalConfigForPersistence 在不破坏公开 SDK 字段兼容性的前提下，
// 为配置落盘生成对象化的 api-keys YAML。
func MarshalConfigForPersistence(cfg *Config) ([]byte, error) {
	if cfg == nil {
		return yaml.Marshal(cfg)
	}
	return yaml.Marshal(persistenceConfigFromConfig(*cfg))
}

func cloneOptionalInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneClientAPIKeys(entries []ClientAPIKey) []ClientAPIKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]ClientAPIKey, 0, len(entries))
	for _, entry := range entries {
		out = append(out, ClientAPIKey{
			Key:         strings.TrimSpace(entry.Key),
			MaxPriority: cloneOptionalInt(entry.MaxPriority),
		})
	}
	return out
}

func clientAPIKeyListFromEntries(entries []ClientAPIKey) APIKeyList {
	if len(entries) == 0 {
		return nil
	}
	values := ClientAPIKeyValues(entries)
	if len(values) == 0 {
		return nil
	}
	out := make(APIKeyList, 0, len(values))
	out = append(out, values...)
	return out
}

func (cfg *SDKConfig) syncClientAPIKeyViews(entries []ClientAPIKey) {
	if cfg == nil {
		return
	}
	normalized := NormalizeClientAPIKeys(entries)
	cfg.clientAPIKeyEntries = cloneClientAPIKeys(normalized)
	cfg.APIKeys = clientAPIKeyListFromEntries(normalized)
}

func findRootKeyNode(mapNode *yaml.Node, key string) (*yaml.Node, bool) {
	index := findMapKeyIndex(mapNode, key)
	if index < 0 || index+1 >= len(mapNode.Content) {
		return nil, false
	}
	return mapNode.Content[index+1], true
}

type persistenceSDKConfig struct {
	ProxyURL                   string             `yaml:"proxy-url" json:"proxy-url"`
	EnableGeminiCLIEndpoint    bool               `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`
	CodexInitialRefreshOnLoad  bool               `yaml:"codex-initial-refresh-on-load" json:"codex-initial-refresh-on-load"`
	ForceModelPrefix           bool               `yaml:"force-model-prefix" json:"force-model-prefix"`
	RequestLog                 bool               `yaml:"request-log" json:"request-log"`
	RequestAudit               RequestAuditConfig `yaml:"request-audit" json:"request-audit"`
	APIKeys                    []ClientAPIKey     `yaml:"api-keys,omitempty" json:"api-keys"`
	PassthroughHeaders         bool               `yaml:"passthrough-headers" json:"passthrough-headers"`
	Streaming                  StreamingConfig    `yaml:"streaming" json:"streaming"`
	NonStreamKeepAliveInterval int                `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

type persistenceConfig struct {
	persistenceSDKConfig                             `yaml:",inline"`
	Host                                             string                       `yaml:"host" json:"-"`
	Port                                             int                          `yaml:"port" json:"-"`
	TLS                                              TLSConfig                    `yaml:"tls" json:"tls"`
	RemoteManagement                                 RemoteManagement             `yaml:"remote-management" json:"-"`
	AuthDir                                          string                       `yaml:"auth-dir" json:"-"`
	Debug                                            bool                         `yaml:"debug" json:"debug"`
	Pprof                                            PprofConfig                  `yaml:"pprof" json:"pprof"`
	CommercialMode                                   bool                         `yaml:"commercial-mode" json:"commercial-mode"`
	LoggingToFile                                    bool                         `yaml:"logging-to-file" json:"logging-to-file"`
	LogsMaxTotalSizeMB                               int                          `yaml:"logs-max-total-size-mb" json:"logs-max-total-size-mb"`
	ErrorLogsMaxFiles                                int                          `yaml:"error-logs-max-files" json:"error-logs-max-files"`
	UsageStatisticsEnabled                           bool                         `yaml:"usage-statistics-enabled" json:"usage-statistics-enabled"`
	UsageStatisticsPersistIntervalSeconds            int                          `yaml:"usage-statistics-persist-interval-seconds" json:"usage-statistics-persist-interval-seconds"`
	UsageStatisticsRetentionDays                     int                          `yaml:"usage-statistics-retention-days" json:"usage-statistics-retention-days"`
	DisableCooling                                   bool                         `yaml:"disable-cooling" json:"disable-cooling"`
	AuthAutoRefreshWorkers                           int                          `yaml:"auth-auto-refresh-workers" json:"auth-auto-refresh-workers"`
	RequestRetry                                     int                          `yaml:"request-retry" json:"request-retry"`
	MaxRetryCredentials                              int                          `yaml:"max-retry-credentials" json:"max-retry-credentials"`
	MaxInvalidRequestRetries                         int                          `yaml:"max-invalid-request-retries" json:"max-invalid-request-retries"`
	MaxRetryInterval                                 int                          `yaml:"max-retry-interval" json:"max-retry-interval"`
	SharedExitPriorityZeroOAuthNetworkJitterFallback bool                         `yaml:"shared-exit-priority-zero-oauth-network-jitter-fallback" json:"shared-exit-priority-zero-oauth-network-jitter-fallback"`
	QuotaExceeded                                    QuotaExceeded                `yaml:"quota-exceeded" json:"quota-exceeded"`
	AuthMaintenance                                  AuthMaintenanceConfig        `yaml:"auth-maintenance" json:"auth-maintenance"`
	Routing                                          RoutingConfig                `yaml:"routing" json:"routing"`
	WebsocketAuth                                    bool                         `yaml:"ws-auth" json:"ws-auth"`
	GeminiKey                                        []GeminiKey                  `yaml:"gemini-api-key" json:"gemini-api-key"`
	CodexKey                                         []CodexKey                   `yaml:"codex-api-key" json:"codex-api-key"`
	CodexHeaderDefaults                              CodexHeaderDefaults          `yaml:"codex-header-defaults" json:"codex-header-defaults"`
	ClaudeKey                                        []ClaudeKey                  `yaml:"claude-api-key" json:"claude-api-key"`
	ClaudeHeaderDefaults                             ClaudeHeaderDefaults         `yaml:"claude-header-defaults" json:"claude-header-defaults"`
	OpenAICompatibility                              []OpenAICompatibility        `yaml:"openai-compatibility" json:"openai-compatibility"`
	VertexCompatAPIKey                               []VertexCompatKey            `yaml:"vertex-api-key" json:"vertex-api-key"`
	AmpCode                                          AmpCode                      `yaml:"ampcode" json:"ampcode"`
	OAuthExcludedModels                              map[string][]string          `yaml:"oauth-excluded-models,omitempty" json:"oauth-excluded-models,omitempty"`
	OAuthModelAlias                                  map[string][]OAuthModelAlias `yaml:"oauth-model-alias,omitempty" json:"oauth-model-alias,omitempty"`
	Payload                                          PayloadConfig                `yaml:"payload" json:"payload"`
}

func persistenceConfigFromConfig(cfg Config) persistenceConfig {
	return persistenceConfig{
		persistenceSDKConfig: persistenceSDKConfig{
			ProxyURL:                   cfg.ProxyURL,
			EnableGeminiCLIEndpoint:    cfg.EnableGeminiCLIEndpoint,
			CodexInitialRefreshOnLoad:  cfg.CodexInitialRefreshOnLoad,
			ForceModelPrefix:           cfg.ForceModelPrefix,
			RequestLog:                 cfg.RequestLog,
			RequestAudit:               cfg.RequestAudit,
			APIKeys:                    cfg.ClientAPIKeyEntries(),
			PassthroughHeaders:         cfg.PassthroughHeaders,
			Streaming:                  cfg.Streaming,
			NonStreamKeepAliveInterval: cfg.NonStreamKeepAliveInterval,
		},
		Host:                                  cfg.Host,
		Port:                                  cfg.Port,
		TLS:                                   cfg.TLS,
		RemoteManagement:                      cfg.RemoteManagement,
		AuthDir:                               cfg.AuthDir,
		Debug:                                 cfg.Debug,
		Pprof:                                 cfg.Pprof,
		CommercialMode:                        cfg.CommercialMode,
		LoggingToFile:                         cfg.LoggingToFile,
		LogsMaxTotalSizeMB:                    cfg.LogsMaxTotalSizeMB,
		ErrorLogsMaxFiles:                     cfg.ErrorLogsMaxFiles,
		UsageStatisticsEnabled:                cfg.UsageStatisticsEnabled,
		UsageStatisticsPersistIntervalSeconds: cfg.UsageStatisticsPersistIntervalSeconds,
		UsageStatisticsRetentionDays:          cfg.UsageStatisticsRetentionDays,
		DisableCooling:                        cfg.DisableCooling,
		AuthAutoRefreshWorkers:                cfg.AuthAutoRefreshWorkers,
		RequestRetry:                          cfg.RequestRetry,
		MaxRetryCredentials:                   cfg.MaxRetryCredentials,
		MaxInvalidRequestRetries:              cfg.MaxInvalidRequestRetries,
		MaxRetryInterval:                      cfg.MaxRetryInterval,
		SharedExitPriorityZeroOAuthNetworkJitterFallback: cfg.SharedExitPriorityZeroOAuthNetworkJitterFallback,
		QuotaExceeded:        cfg.QuotaExceeded,
		AuthMaintenance:      cfg.AuthMaintenance,
		Routing:              cfg.Routing,
		WebsocketAuth:        cfg.WebsocketAuth,
		GeminiKey:            cfg.GeminiKey,
		CodexKey:             cfg.CodexKey,
		CodexHeaderDefaults:  cfg.CodexHeaderDefaults,
		ClaudeKey:            cfg.ClaudeKey,
		ClaudeHeaderDefaults: cfg.ClaudeHeaderDefaults,
		OpenAICompatibility:  cfg.OpenAICompatibility,
		VertexCompatAPIKey:   cfg.VertexCompatAPIKey,
		AmpCode:              cfg.AmpCode,
		OAuthExcludedModels:  cfg.OAuthExcludedModels,
		OAuthModelAlias:      cfg.OAuthModelAlias,
		Payload:              cfg.Payload,
	}
}
