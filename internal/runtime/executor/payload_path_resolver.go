package executor

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	payloadQueryPrefix           = "#("
	payloadAllMatchesSuffix      = "#"
	payloadTopLevelQueryDepth    = 0
	payloadInitialQueryDepth     = 1
	payloadJSONWrapperExtraBytes = len("[]")
)

type payloadQueryPathContext struct {
	payload    []byte
	bases      []string
	query      string
	allMatches bool
}

// resolvePayloadRulePaths 将带 gjson 查询的规则路径解析成 sjson 可写入的实际数组下标路径。
func resolvePayloadRulePaths(payload []byte, path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if !strings.Contains(path, payloadQueryPrefix) {
		return []string{path}
	}
	parts := splitPayloadRulePath(path)
	if len(parts) == 0 {
		return nil
	}
	paths := []string{""}
	for _, part := range parts {
		query, allMatches, ok := parsePayloadQueryPathPart(part)
		if !ok {
			for i := range paths {
				paths[i] = appendPayloadPathPart(paths[i], part)
			}
			continue
		}
		paths = resolvePayloadQueryPart(payloadQueryPathContext{
			payload:    payload,
			bases:      paths,
			query:      query,
			allMatches: allMatches,
		})
		if len(paths) == 0 {
			return nil
		}
	}
	return paths
}

// resolvePayloadQueryPart 在当前候选路径下执行数组查询，返回命中项的真实下标路径。
func resolvePayloadQueryPart(ctx payloadQueryPathContext) []string {
	nextPaths := make([]string, 0, len(ctx.bases))
	for _, basePath := range ctx.bases {
		array := payloadValueAtPath(ctx.payload, basePath)
		if !array.Exists() || !array.IsArray() {
			continue
		}
		for index, item := range array.Array() {
			if !payloadQueryMatches(item, ctx.query) {
				continue
			}
			nextPaths = append(nextPaths, appendPayloadPathPart(basePath, strconv.Itoa(index)))
			if !ctx.allMatches {
				break
			}
		}
	}
	return nextPaths
}

// splitPayloadRulePath 按点号拆分规则路径，同时保留查询表达式和引号内的点号。
func splitPayloadRulePath(path string) []string {
	var parts []string
	start := 0
	depth := 0
	var quote byte
	escaped := false
	for i := 0; i < len(path); i++ {
		ch := path[i]
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
			if depth > 0 {
				depth--
			}
			continue
		}
		if ch == '.' && depth == 0 {
			parts = append(parts, path[start:i])
			start = i + 1
		}
	}
	return append(parts, path[start:])
}

// parsePayloadQueryPathPart 识别 `#(...)` 和 `#(...)#` 两种查询路径片段。
func parsePayloadQueryPathPart(part string) (string, bool, bool) {
	if !strings.HasPrefix(part, payloadQueryPrefix) {
		return "", false, false
	}
	closeIndex := findPayloadQueryClose(part)
	if closeIndex < 0 {
		return "", false, false
	}
	suffix := part[closeIndex+1:]
	if suffix != "" && suffix != payloadAllMatchesSuffix {
		return "", false, false
	}
	return strings.TrimSpace(part[len(payloadQueryPrefix):closeIndex]), suffix == payloadAllMatchesSuffix, true
}

// findPayloadQueryClose 找到查询片段的右括号，避免被引号或嵌套括号误判。
func findPayloadQueryClose(part string) int {
	var quote byte
	escaped := false
	depth := payloadInitialQueryDepth
	for i := len(payloadQueryPrefix); i < len(part); i++ {
		ch := part[i]
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
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// appendPayloadPathPart 拼接 gjson/sjson 路径片段，避免生成多余点号。
func appendPayloadPathPart(path string, part string) string {
	if path == "" {
		return part
	}
	if part == "" {
		return path
	}
	return path + "." + part
}

// payloadValueAtPath 读取当前路径下的 JSON 节点；空路径表示根节点。
func payloadValueAtPath(payload []byte, path string) gjson.Result {
	if path == "" {
		return gjson.ParseBytes(payload)
	}
	return gjson.GetBytes(payload, path)
}
