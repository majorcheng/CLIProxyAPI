package executor

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyPayloadConfigWithRoot 按可选 root 应用 payload 规则。
// root 主要用于 Gemini CLI 这类把真实请求包在 `request` 字段下的协议；
// requestedModel 保留客户端原始模型名，让 payload 规则能精确匹配 alias。
func applyPayloadConfigWithRoot(cfg *config.Config, model, protocol, root string, payload, original []byte, requestedModel string) []byte {
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
			out = applyPayloadRulesWithRoot(rules, protocol, root, out, source, candidates)
		}
	}

	if cfg.DisableImageGeneration {
		out = removeToolTypeFromPayloadWithRoot(out, root, "image_generation")
	}
	return out
}

// applyPayloadRulesWithRoot 按既有顺序应用 payload 规则，确保 default、override、filter 的优先级不变。
func applyPayloadRulesWithRoot(rules config.PayloadConfig, protocol, root string, payload, source []byte, candidates []string) []byte {
	out := payload
	appliedDefaults := make(map[string]struct{})
	out = applyPayloadDefaultRulesWithRoot(out, source, rules.Default, protocol, root, candidates, appliedDefaults)
	out = applyPayloadDefaultRawRulesWithRoot(out, source, rules.DefaultRaw, protocol, root, candidates, appliedDefaults)
	out = applyPayloadOverrideRulesWithRoot(out, rules.Override, protocol, root, candidates)
	out = applyPayloadOverrideRawRulesWithRoot(out, rules.OverrideRaw, protocol, root, candidates)
	return applyPayloadFilterRulesWithRoot(out, rules.Filter, protocol, root, candidates)
}

// applyPayloadDefaultRulesWithRoot 只在原始请求缺少字段时写入普通 JSON 值。
func applyPayloadDefaultRulesWithRoot(out, source []byte, rules []config.PayloadRule, protocol, root string, candidates []string, appliedDefaults map[string]struct{}) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" || gjson.GetBytes(source, fullPath).Exists() {
				continue
			}
			if _, ok := appliedDefaults[fullPath]; ok {
				continue
			}
			updated, errSet := sjson.SetBytes(out, fullPath, value)
			if errSet != nil {
				continue
			}
			out = updated
			appliedDefaults[fullPath] = struct{}{}
		}
	}
	return out
}

// applyPayloadDefaultRawRulesWithRoot 只在原始请求缺少字段时写入 raw JSON，避免字符串被二次编码。
func applyPayloadDefaultRawRulesWithRoot(out, source []byte, rules []config.PayloadRule, protocol, root string, candidates []string, appliedDefaults map[string]struct{}) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" || gjson.GetBytes(source, fullPath).Exists() {
				continue
			}
			if _, ok := appliedDefaults[fullPath]; ok {
				continue
			}
			rawValue, ok := payloadRawValue(value)
			if !ok {
				continue
			}
			updated, errSet := sjson.SetRawBytes(out, fullPath, rawValue)
			if errSet != nil {
				continue
			}
			out = updated
			appliedDefaults[fullPath] = struct{}{}
		}
	}
	return out
}

// applyPayloadOverrideRulesWithRoot 按匹配顺序覆盖普通 JSON 值，后命中的规则自然覆盖前值。
func applyPayloadOverrideRulesWithRoot(out []byte, rules []config.PayloadRule, protocol, root string, candidates []string) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for path, value := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			updated, errSet := sjson.SetBytes(out, fullPath, value)
			if errSet != nil {
				continue
			}
			out = updated
		}
	}
	return out
}

// applyPayloadOverrideRawRulesWithRoot 按匹配顺序覆盖 raw JSON，保证数组和对象片段保持原始类型。
func applyPayloadOverrideRawRulesWithRoot(out []byte, rules []config.PayloadRule, protocol, root string, candidates []string) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
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
			updated, errSet := sjson.SetRawBytes(out, fullPath, rawValue)
			if errSet != nil {
				continue
			}
			out = updated
		}
	}
	return out
}

// applyPayloadFilterRulesWithRoot 删除匹配规则指定的字段，保持 filter 在所有写入规则之后执行。
func applyPayloadFilterRulesWithRoot(out []byte, rules []config.PayloadFilterRule, protocol, root string, candidates []string) []byte {
	for i := range rules {
		rule := &rules[i]
		if !payloadModelRulesMatch(rule.Models, protocol, candidates) {
			continue
		}
		for _, path := range rule.Params {
			fullPath := buildPayloadPath(root, path)
			if fullPath == "" {
				continue
			}
			updated, errDel := sjson.DeleteBytes(out, fullPath)
			if errDel != nil {
				continue
			}
			out = updated
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

func payloadModelRulesMatch(rules []config.PayloadModelRule, protocol string, models []string) bool {
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
			if matchModelPattern(name, model) {
				return true
			}
		}
	}
	return false
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
		if payloadModelRulesMatch(cfg.Payload.Default[i].Models, protocol, candidates) {
			return true
		}
	}
	for i := range cfg.Payload.DefaultRaw {
		if payloadModelRulesMatch(cfg.Payload.DefaultRaw[i].Models, protocol, candidates) {
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
