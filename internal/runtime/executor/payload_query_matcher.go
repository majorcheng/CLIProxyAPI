package executor

import (
	"strings"

	"github.com/tidwall/gjson"
)

var payloadComparisonOperators = []string{"==", "!=", ">=", "<=", ">", "<", "!%", "%"}

// payloadQueryMatches 支持查询表达式中的 `||`，与 gjson 单项查询语义保持一致。
func payloadQueryMatches(item gjson.Result, query string) bool {
	for _, orPart := range splitPayloadLogical(query, "||") {
		if payloadQueryAndMatches(item, orPart) {
			return true
		}
	}
	return false
}

// payloadQueryAndMatches 支持查询表达式中的 `&&`，所有条件命中才算匹配。
func payloadQueryAndMatches(item gjson.Result, query string) bool {
	parts := splitPayloadLogical(query, "&&")
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !payloadQueryTermMatches(item, part) {
			return false
		}
	}
	return true
}

// splitPayloadLogical 拆分逻辑表达式，跳过字符串字面量内部的操作符。
func splitPayloadLogical(query string, operator string) []string {
	var parts []string
	start := 0
	depth := payloadTopLevelQueryDepth
	var quote byte
	escaped := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '(' {
			depth++
			continue
		}
		if ch == ')' {
			if depth > payloadTopLevelQueryDepth {
				depth--
			}
			continue
		}
		if depth == payloadTopLevelQueryDepth && strings.HasPrefix(query[i:], operator) {
			parts = append(parts, strings.TrimSpace(query[start:i]))
			i += len(operator) - 1
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(query[start:]))
}

// payloadQueryTermMatches 复用 gjson 查询引擎判断单个数组元素是否满足条件。
func payloadQueryTermMatches(item gjson.Result, term string) bool {
	term = strings.TrimSpace(term)
	if term == "" || item.Raw == "" {
		return false
	}
	wrapped := make([]byte, 0, len(item.Raw)+payloadJSONWrapperExtraBytes)
	wrapped = append(wrapped, '[')
	wrapped = append(wrapped, item.Raw...)
	wrapped = append(wrapped, ']')
	if gjson.GetBytes(wrapped, payloadQueryPrefix+term+")").Exists() {
		return true
	}
	return payloadNestedQueryPathExists(item, term)
}

// payloadNestedQueryPathExists 递归解析嵌套查询路径，补足 gjson 单项查询不支持内层逻辑符的场景。
func payloadNestedQueryPathExists(item gjson.Result, term string) bool {
	if !strings.Contains(term, payloadQueryPrefix) || payloadHasTopLevelComparator(term) {
		return false
	}
	return len(resolvePayloadRulePaths([]byte(item.Raw), term)) > 0
}

// payloadHasTopLevelComparator 判断 term 顶层是否有比较运算符，避免把比较表达式误当成存在性路径。
func payloadHasTopLevelComparator(term string) bool {
	depth := payloadTopLevelQueryDepth
	var quote byte
	escaped := false
	for i := 0; i < len(term); i++ {
		ch := term[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if ch == '(' {
			depth++
			continue
		}
		if ch == ')' {
			if depth > payloadTopLevelQueryDepth {
				depth--
			}
			continue
		}
		if depth == payloadTopLevelQueryDepth && payloadHasOperatorAt(term[i:]) {
			return true
		}
	}
	return false
}

// payloadHasOperatorAt 判断当前位置是否是支持的 gjson 比较运算符。
func payloadHasOperatorAt(value string) bool {
	for _, operator := range payloadComparisonOperators {
		if strings.HasPrefix(value, operator) {
			return true
		}
	}
	return false
}
