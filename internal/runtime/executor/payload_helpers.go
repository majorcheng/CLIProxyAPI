package executor

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyPayloadConfigWithRoot 按可选 root 应用 payload 规则。
// root 主要用于 Gemini CLI 这类把真实请求包在 `request` 字段下的协议；
// requestedModel 保留客户端原始模型名，让 payload 规则能精确匹配 alias。
func applyPayloadConfigWithRoot(cfg *config.Config, model, protocol, root string, payload, original []byte, requestedModel string, requestPath string) []byte {
	return applyPayloadConfigWithRequest(cfg, model, protocol, "", root, payload, original, requestedModel, requestPath, nil)
}

// applyPayloadConfigWithRequest 按目标协议、来源协议和入站 header 共同应用 payload 规则。
func applyPayloadConfigWithRequest(cfg *config.Config, model, protocol, fromProtocol, root string, payload, original []byte, requestedModel string, requestPath string, headers http.Header) []byte {
	if cfg == nil || len(payload) == 0 {
		return payload
	}
	out := payload
	rules := cfg.Payload
	hasPayloadRules := len(rules.Default) != 0 || len(rules.DefaultRaw) != 0 || len(rules.Override) != 0 || len(rules.OverrideRaw) != 0 || len(rules.Filter) != 0
	if hasPayloadRules {
		model = strings.TrimSpace(model)
		requestedModel = strings.TrimSpace(requestedModel)
		if model != "" || requestedModel != "" {
			candidates := payloadModelCandidates(model, requestedModel)
			source := original
			if len(source) == 0 {
				source = payload
			}
			out = applyPayloadRulesWithRoot(rules, protocol, fromProtocol, root, out, source, headers, candidates)
		}
	}

	if imageGenerationDisabledForRequest(cfg, requestPath) {
		out = removeToolTypeFromPayloadWithRoot(out, root, "image_generation")
		out = removeToolChoiceFromPayloadWithRoot(out, root, "image_generation")
	}
	return out
}

// imageGenerationDisabledForRequest 判断当前请求是否需要剥离 image_generation 能力。
func imageGenerationDisabledForRequest(cfg *config.Config, requestPath string) bool {
	if cfg == nil {
		return false
	}
	switch cfg.DisableImageGeneration {
	case config.DisableImageGenerationAll:
		return true
	case config.DisableImageGenerationChat:
		return !isImagesEndpointRequestPath(requestPath)
	default:
		return false
	}
}

// isImagesEndpointRequestPath 识别 OpenAI Images 入口，兼容 provider alias 的前缀路由。
func isImagesEndpointRequestPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if path == "/v1/images/generations" || path == "/v1/images/edits" {
		return true
	}
	if strings.HasSuffix(path, "/v1/images/generations") || strings.HasSuffix(path, "/v1/images/edits") {
		return true
	}
	return strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/images/edits")
}

// applyPayloadRulesWithRoot 按既有顺序应用 payload 规则，确保 default、override、filter 的优先级不变。
func applyPayloadRulesWithRoot(rules config.PayloadConfig, protocol, fromProtocol, root string, payload, source []byte, headers http.Header, candidates []string) []byte {
	out := payload
	appliedDefaults := make(map[string]struct{})
	out = applyPayloadDefaultRulesWithRoot(out, source, rules.Default, protocol, fromProtocol, root, headers, candidates, appliedDefaults)
	out = applyPayloadDefaultRawRulesWithRoot(out, source, rules.DefaultRaw, protocol, fromProtocol, root, headers, candidates, appliedDefaults)
	out = applyPayloadOverrideRulesWithRoot(out, rules.Override, protocol, fromProtocol, root, headers, candidates)
	out = applyPayloadOverrideRawRulesWithRoot(out, rules.OverrideRaw, protocol, fromProtocol, root, headers, candidates)
	return applyPayloadFilterRulesWithRoot(out, rules.Filter, protocol, fromProtocol, root, headers, candidates)
}

// applyPayloadDefaultRulesWithRoot 只在原始请求缺少字段时写入普通 JSON 值。
func applyPayloadDefaultRulesWithRoot(out, source []byte, rules []config.PayloadRule, protocol, fromProtocol, root string, headers http.Header, candidates []string, appliedDefaults map[string]struct{}) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, out, root, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			for _, resolvedPath := range resolvePayloadRulePaths(out, fullPath) {
				if gjson.GetBytes(source, resolvedPath).Exists() {
					continue
				}
				if _, ok := appliedDefaults[resolvedPath]; ok {
					continue
				}
				updated, errSet := sjson.SetBytes(out, resolvedPath, value)
				if errSet != nil {
					continue
				}
				out = updated
				appliedDefaults[resolvedPath] = struct{}{}
			}
		}
	}
	return out
}

// applyPayloadDefaultRawRulesWithRoot 只在原始请求缺少字段时写入 raw JSON，避免字符串被二次编码。
func applyPayloadDefaultRawRulesWithRoot(out, source []byte, rules []config.PayloadRule, protocol, fromProtocol, root string, headers http.Header, candidates []string, appliedDefaults map[string]struct{}) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, out, root, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			rawValue, ok := payloadRawValue(value)
			if !ok {
				continue
			}
			for _, resolvedPath := range resolvePayloadRulePaths(out, fullPath) {
				if gjson.GetBytes(source, resolvedPath).Exists() {
					continue
				}
				if _, ok := appliedDefaults[resolvedPath]; ok {
					continue
				}
				updated, errSet := sjson.SetRawBytes(out, resolvedPath, rawValue)
				if errSet != nil {
					continue
				}
				out = updated
				appliedDefaults[resolvedPath] = struct{}{}
			}
		}
	}
	return out
}

// applyPayloadOverrideRulesWithRoot 按匹配顺序覆盖普通 JSON 值，后命中的规则自然覆盖前值。
func applyPayloadOverrideRulesWithRoot(out []byte, rules []config.PayloadRule, protocol, fromProtocol, root string, headers http.Header, candidates []string) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, out, root, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			for _, resolvedPath := range resolvePayloadRulePaths(out, fullPath) {
				updated, errSet := sjson.SetBytes(out, resolvedPath, value)
				if errSet != nil {
					continue
				}
				out = updated
			}
		}
	}
	return out
}

// applyPayloadOverrideRawRulesWithRoot 按匹配顺序覆盖 raw JSON，保证数组和对象片段保持原始类型。
func applyPayloadOverrideRawRulesWithRoot(out []byte, rules []config.PayloadRule, protocol, fromProtocol, root string, headers http.Header, candidates []string) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, out, root, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			rawValue, ok := payloadRawValue(value)
			if !ok {
				continue
			}
			for _, resolvedPath := range resolvePayloadRulePaths(out, fullPath) {
				updated, errSet := sjson.SetRawBytes(out, resolvedPath, rawValue)
				if errSet != nil {
					continue
				}
				out = updated
			}
		}
	}
	return out
}

// applyPayloadFilterRulesWithRoot 删除匹配规则指定的字段，保持 filter 在所有写入规则之后执行。
func applyPayloadFilterRulesWithRoot(out []byte, rules []config.PayloadFilterRule, protocol, fromProtocol, root string, headers http.Header, candidates []string) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, fromProtocol, headers, out, root, candidates) {
			continue
		}
		for _, path := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			resolvedPaths := resolvePayloadRulePaths(out, fullPath)
			for i := len(resolvedPaths) - 1; i >= 0; i-- {
				updated, errDel := sjson.DeleteBytes(out, resolvedPaths[i])
				if errDel != nil {
					continue
				}
				out = updated
			}
		}
	}
	return out
}

// removeToolTypeFromPayloadWithRoot 从 tools 数组移除指定工具类型，供全局能力开关覆盖 payload 规则结果。
func removeToolTypeFromPayloadWithRoot(payload []byte, root string, toolType string) []byte {
	toolType = strings.TrimSpace(toolType)
	if len(payload) == 0 || toolType == "" {
		return payload
	}
	choiceToolsPath := buildPayloadPath(root, "tool_choice.tools")
	hadChoiceTool := toolsArrayContainsType(payload, choiceToolsPath, toolType)
	out := removeToolTypeFromToolsArray(payload, buildPayloadPath(root, "tools"), toolType)
	out = removeToolTypeFromToolsArray(out, choiceToolsPath, toolType)
	if hadChoiceTool {
		out = removeEmptyAllowedToolsChoiceWithRoot(out, root)
	}
	return out
}

// removeToolChoiceFromPayloadWithRoot 删除直接指向被禁用工具的 tool_choice。
func removeToolChoiceFromPayloadWithRoot(payload []byte, root string, toolType string) []byte {
	toolType = strings.TrimSpace(toolType)
	if len(payload) == 0 || toolType == "" {
		return payload
	}
	return removeToolChoiceFromPayload(payload, buildPayloadPath(root, "tool_choice"), toolType)
}

// removeToolChoiceFromPayload 只删除明确选择 image_generation 的 tool_choice。
func removeToolChoiceFromPayload(payload []byte, toolChoicePath string, toolType string) []byte {
	choice := gjson.GetBytes(payload, toolChoicePath)
	if !choice.Exists() {
		return payload
	}
	if choice.Type == gjson.String {
		return deleteMatchingStringToolChoice(payload, toolChoicePath, choice, toolType)
	}
	if !choice.IsObject() {
		return payload
	}
	return deleteMatchingObjectToolChoice(payload, toolChoicePath, choice, toolType)
}

func deleteMatchingStringToolChoice(payload []byte, path string, choice gjson.Result, toolType string) []byte {
	if !strings.EqualFold(strings.TrimSpace(choice.String()), toolType) {
		return payload
	}
	updated, errDel := sjson.DeleteBytes(payload, path)
	if errDel != nil {
		return payload
	}
	return updated
}

func deleteMatchingObjectToolChoice(payload []byte, path string, choice gjson.Result, toolType string) []byte {
	choiceType := strings.TrimSpace(choice.Get("type").String())
	if strings.EqualFold(choiceType, toolType) || toolChoiceToolNameMatches(choice, choiceType, toolType) {
		updated, errDel := sjson.DeleteBytes(payload, path)
		if errDel == nil {
			return updated
		}
	}
	return payload
}

func toolChoiceToolNameMatches(choice gjson.Result, choiceType string, toolType string) bool {
	if !strings.EqualFold(choiceType, "tool") {
		return false
	}
	name := strings.TrimSpace(choice.Get("name").String())
	return strings.EqualFold(name, toolType)
}

// removeToolTypeFromToolsArray 过滤数组内被禁用的工具项；若全部移除则删除该 tools 字段。
func removeToolTypeFromToolsArray(payload []byte, toolsPath string, toolType string) []byte {
	tools := gjson.GetBytes(payload, toolsPath)
	if !tools.IsArray() {
		return payload
	}
	items := tools.Array()
	rawItems := make([]string, 0, len(items))
	removed := false
	for _, item := range items {
		if strings.TrimSpace(item.Get("type").String()) == toolType {
			removed = true
			continue
		}
		rawItems = append(rawItems, payloadResultRaw(item))
	}
	if !removed {
		return payload
	}
	if len(rawItems) == 0 {
		updated, errDel := sjson.DeleteBytes(payload, toolsPath)
		if errDel != nil {
			return payload
		}
		return updated
	}
	updated, errSet := sjson.SetRawBytes(payload, toolsPath, []byte("["+strings.Join(rawItems, ",")+"]"))
	if errSet != nil {
		return payload
	}
	return updated
}

// toolsArrayContainsType 判断指定 tools 数组里是否包含目标工具类型，用于限制后续清理范围。
func toolsArrayContainsType(payload []byte, toolsPath string, toolType string) bool {
	tools := gjson.GetBytes(payload, toolsPath)
	if !tools.IsArray() {
		return false
	}
	for _, item := range tools.Array() {
		if strings.TrimSpace(item.Get("type").String()) == toolType {
			return true
		}
	}
	return false
}

// removeEmptyAllowedToolsChoiceWithRoot 删除已经没有 allowed tools 的限制对象，避免发送无效 tool_choice。
func removeEmptyAllowedToolsChoiceWithRoot(payload []byte, root string) []byte {
	choicePath := buildPayloadPath(root, "tool_choice")
	choice := gjson.GetBytes(payload, choicePath)
	if !choice.IsObject() || strings.TrimSpace(choice.Get("type").String()) != "allowed_tools" {
		return payload
	}
	tools := choice.Get("tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		return payload
	}
	updated, errDel := sjson.DeleteBytes(payload, choicePath)
	if errDel != nil {
		return payload
	}
	return updated
}

// payloadResultRaw 返回 gjson 节点的原始 JSON，用于重建数组时避免改变工具对象内容。
func payloadResultRaw(item gjson.Result) string {
	if item.Raw != "" {
		return item.Raw
	}
	raw, errMarshal := json.Marshal(item.Value())
	if errMarshal != nil {
		return "null"
	}
	return string(raw)
}

func payloadModelRulesMatch(rules []config.PayloadModelRule, protocol string, fromProtocol string, headers http.Header, payload []byte, root string, models []string) bool {
	if len(rules) == 0 || len(models) == 0 {
		return false
	}
	for _, model := range models {
		for _, entry := range rules {
			name := strings.TrimSpace(entry.Name)
			if name == "" {
				continue
			}
			if ep := strings.TrimSpace(entry.Protocol); ep != "" && protocol != "" && !strings.EqualFold(ep, protocol) {
				continue
			}
			if !payloadFromProtocolMatches(entry.FromProtocol, fromProtocol) {
				continue
			}
			if !payloadHeadersMatch(headers, entry.Headers) {
				continue
			}
			if !matchModelPattern(name, model) {
				continue
			}
			if payloadModelRuleConditionsMatch(payload, root, entry) {
				return true
			}
		}
	}
	return false
}

func payloadModelRuleConditionsMatch(payload []byte, root string, rule config.PayloadModelRule) bool {
	if !payloadMatchConditionsMatch(payload, root, rule.Match) {
		return false
	}
	if !payloadNotMatchConditionsMatch(payload, root, rule.NotMatch) {
		return false
	}
	if !payloadExistConditionsMatch(payload, root, rule.Exist) {
		return false
	}
	if !payloadNotExistConditionsMatch(payload, root, rule.NotExist) {
		return false
	}
	return true
}

func payloadMatchConditionsMatch(payload []byte, root string, conditions []map[string]any) bool {
	for _, condition := range conditions {
		for path, value := range condition {
			if strings.TrimSpace(path) == "" {
				continue
			}
			if !payloadPathMatchesValue(payload, buildPayloadPath(root, path), value) {
				return false
			}
		}
	}
	return true
}

func payloadNotMatchConditionsMatch(payload []byte, root string, conditions []map[string]any) bool {
	for _, condition := range conditions {
		for path, value := range condition {
			if strings.TrimSpace(path) == "" {
				continue
			}
			if payloadPathMatchesValue(payload, buildPayloadPath(root, path), value) {
				return false
			}
		}
	}
	return true
}

func payloadExistConditionsMatch(payload []byte, root string, paths []string) bool {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if !payloadPathExists(payload, buildPayloadPath(root, path)) {
			return false
		}
	}
	return true
}

func payloadNotExistConditionsMatch(payload []byte, root string, paths []string) bool {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if payloadPathExists(payload, buildPayloadPath(root, path)) {
			return false
		}
	}
	return true
}

func payloadPathMatchesValue(payload []byte, path string, value any) bool {
	for _, resolvedPath := range resolvePayloadRulePaths(payload, path) {
		result := gjson.GetBytes(payload, resolvedPath)
		if !result.Exists() {
			continue
		}
		if payloadResultEquals(result, value) {
			return true
		}
	}
	return false
}

func payloadPathExists(payload []byte, path string) bool {
	for _, resolvedPath := range resolvePayloadRulePaths(payload, path) {
		result := gjson.GetBytes(payload, resolvedPath)
		if result.Exists() && result.Type != gjson.Null {
			return true
		}
	}
	return false
}

func payloadResultEquals(result gjson.Result, value any) bool {
	actual, ok := normalizedPayloadResult(result)
	if !ok {
		return false
	}
	expected, ok := normalizedPayloadValue(value)
	if !ok {
		return false
	}
	return reflect.DeepEqual(actual, expected)
}

func normalizedPayloadResult(result gjson.Result) (any, bool) {
	if !result.Exists() {
		return nil, false
	}
	raw := strings.TrimSpace(result.Raw)
	if raw == "" {
		encoded, errMarshal := json.Marshal(result.Value())
		if errMarshal != nil {
			return nil, false
		}
		raw = string(encoded)
	}
	return normalizedPayloadJSON([]byte(raw))
}

func normalizedPayloadValue(value any) (any, bool) {
	encoded, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, false
	}
	return normalizedPayloadJSON(encoded)
}

func normalizedPayloadJSON(data []byte) (any, bool) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, false
	}
	var out any
	if errUnmarshal := json.Unmarshal(data, &out); errUnmarshal != nil {
		return nil, false
	}
	return out, true
}

func payloadFromProtocolMatches(pattern, fromProtocol string) bool {
	pattern = normalizePayloadFromProtocol(pattern)
	if pattern == "" {
		return true
	}
	fromProtocol = normalizePayloadFromProtocol(fromProtocol)
	if fromProtocol == "" {
		return false
	}
	return strings.EqualFold(pattern, fromProtocol)
}

func normalizePayloadFromProtocol(protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	switch protocol {
	case "openai-response", "openai-responses", "response":
		return "responses"
	case "gemini-cli":
		return "gemini"
	default:
		return protocol
	}
}

func payloadHeadersMatch(headers http.Header, rules map[string]string) bool {
	if len(rules) == 0 {
		return true
	}
	for key, pattern := range rules {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values := payloadHeaderValues(headers, key)
		if len(values) == 0 {
			return false
		}
		matched := false
		for _, value := range values {
			if matchModelPattern(pattern, value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func payloadHeaderValues(headers http.Header, key string) []string {
	if headers == nil {
		return nil
	}
	var values []string
	for headerKey, headerValues := range headers {
		if strings.EqualFold(headerKey, key) {
			values = append(values, headerValues...)
		}
	}
	return values
}

func payloadModelCandidates(model, requestedModel string) []string {
	model = strings.TrimSpace(model)
	requestedModel = strings.TrimSpace(requestedModel)
	if model == "" && requestedModel == "" {
		return nil
	}
	candidates := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	addCandidate := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, value)
	}
	if model != "" {
		addCandidate(model)
	}
	if requestedModel != "" {
		parsed := thinking.ParseSuffix(requestedModel)
		base := strings.TrimSpace(parsed.ModelName)
		if base != "" {
			addCandidate(base)
		}
		if parsed.HasSuffix {
			addCandidate(requestedModel)
		}
	}
	return candidates
}

// buildPayloadPath combines an optional root path with a relative parameter path.
// When root is empty, the parameter path is used as-is. When root is non-empty,
// the parameter path is treated as relative to root.
func buildPayloadPath(root, path string) string {
	r := strings.TrimSpace(root)
	p := strings.TrimSpace(path)
	if r == "" {
		return p
	}
	if p == "" {
		return r
	}
	if strings.HasPrefix(p, ".") {
		p = p[1:]
	}
	return r + "." + p
}

func payloadRawValue(value any) ([]byte, bool) {
	if value == nil {
		return nil, false
	}
	switch typed := value.(type) {
	case string:
		return []byte(typed), true
	case []byte:
		return typed, true
	default:
		raw, errMarshal := json.Marshal(typed)
		if errMarshal != nil {
			return nil, false
		}
		return raw, true
	}
}

func payloadRequestedModel(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		return strings.TrimSpace(v)
	case []byte:
		if len(v) == 0 {
			return fallback
		}
		trimmed := strings.TrimSpace(string(v))
		if trimmed == "" {
			return fallback
		}
		return trimmed
	default:
		return fallback
	}
}

// payloadRequestPath 从 executor metadata 读取下游请求路径，用于区分 Images 专用入口。
func payloadRequestPath(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func payloadConfigNeedsOriginal(cfg *config.Config, model, protocol, requestedModel string) bool {
	if cfg == nil {
		return false
	}
	if len(cfg.Payload.Default) == 0 && len(cfg.Payload.DefaultRaw) == 0 {
		return false
	}
	candidates := payloadModelCandidates(model, requestedModel)
	if len(candidates) == 0 {
		return false
	}
	for i := range cfg.Payload.Default {
		if payloadModelRulesMayMatch(cfg.Payload.Default[i].Models, protocol, candidates) {
			return true
		}
	}
	for i := range cfg.Payload.DefaultRaw {
		if payloadModelRulesMayMatch(cfg.Payload.DefaultRaw[i].Models, protocol, candidates) {
			return true
		}
	}
	return false
}

// payloadModelRulesMayMatch 只用模型和目标协议判断 default 是否可能命中，用于决定是否保留 original source。
func payloadModelRulesMayMatch(rules []config.PayloadModelRule, protocol string, models []string) bool {
	for _, model := range models {
		for _, entry := range rules {
			name := strings.TrimSpace(entry.Name)
			if name == "" || !matchModelPattern(name, model) {
				continue
			}
			if ep := strings.TrimSpace(entry.Protocol); ep != "" && protocol != "" && !strings.EqualFold(ep, protocol) {
				continue
			}
			return true
		}
	}
	return false
}

// matchModelPattern performs simple wildcard matching where '*' matches zero or more characters.
// Examples:
//
//	"*-5" matches "gpt-5"
//	"gpt-*" matches "gpt-5" and "gpt-4"
//	"gemini-*-pro" matches "gemini-2.5-pro" and "gemini-3-pro".
func matchModelPattern(pattern, model string) bool {
	pattern = strings.TrimSpace(pattern)
	model = strings.TrimSpace(model)
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	// Iterative glob-style matcher supporting only '*' wildcard.
	pi, si := 0, 0
	starIdx := -1
	matchIdx := 0
	for si < len(model) {
		if pi < len(pattern) && (pattern[pi] == model[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			matchIdx = si
			pi++
			continue
		}
		if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
