package management

import (
	"encoding/json"
	"strconv"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// syncAuthFileMetadataFields 将 metadata 里的管理字段同步到 Auth 运行态字段与 attributes。
func syncAuthFileMetadataFields(auth *coreauth.Auth, touchedRoots map[string]struct{}) {
	if auth == nil || len(touchedRoots) == 0 {
		return
	}
	if _, ok := touchedRoots["prefix"]; ok {
		syncAuthFilePrefix(auth)
	}
	if _, ok := touchedRoots["proxy_url"]; ok {
		syncAuthFileProxyURL(auth)
	}
	if _, ok := touchedRoots["headers"]; ok {
		syncAuthFileHeaderAttributes(auth)
	}
	if _, ok := touchedRoots["priority"]; ok {
		syncAuthFilePriorityAttribute(auth)
	}
	if _, ok := touchedRoots["note"]; ok {
		syncAuthFileNoteAttribute(auth)
	}
	if _, ok := touchedRoots["websockets"]; ok {
		syncAuthFileWebsocketsAttribute(auth)
	}
	if _, ok := touchedRoots["disabled"]; ok {
		syncAuthFileDisabledState(auth)
	}
}

// syncAuthFilePrefix 同步 prefix；空字符串保持旧语义，表示删除配置。
func syncAuthFilePrefix(auth *coreauth.Auth) {
	prefix, ok := auth.Metadata["prefix"].(string)
	if !ok {
		auth.Prefix = ""
		return
	}
	auth.Prefix = strings.TrimSpace(prefix)
	if auth.Prefix == "" {
		delete(auth.Metadata, "prefix")
	}
}

// syncAuthFileProxyURL 同步 proxy_url；空字符串保持旧语义，表示删除配置。
func syncAuthFileProxyURL(auth *coreauth.Auth) {
	proxyURL, ok := auth.Metadata["proxy_url"].(string)
	if !ok {
		auth.ProxyURL = ""
		return
	}
	auth.ProxyURL = strings.TrimSpace(proxyURL)
	if auth.ProxyURL == "" {
		delete(auth.Metadata, "proxy_url")
	}
}

// syncAuthFileHeaderAttributes 将 metadata.headers 镜像成 executor 可读取的 header:* attributes。
func syncAuthFileHeaderAttributes(auth *coreauth.Auth) {
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for key := range auth.Attributes {
		if strings.HasPrefix(key, "header:") {
			delete(auth.Attributes, key)
		}
	}
	for name, value := range authFileCustomHeadersFromMetadata(auth.Metadata) {
		if strings.TrimSpace(value) != "" {
			auth.Attributes["header:"+name] = value
		}
	}
}

// syncAuthFilePriorityAttribute 同步 priority；0 沿用本地旧语义，表示移除优先级。
func syncAuthFilePriorityAttribute(auth *coreauth.Auth) {
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	priority, ok := authFileIntValue(auth.Metadata["priority"])
	if !ok || priority == 0 {
		delete(auth.Metadata, "priority")
		delete(auth.Attributes, "priority")
		return
	}
	auth.Attributes["priority"] = strconv.Itoa(priority)
}

// authFileIntValue 兼容 JSON 数字、字符串与测试里常见的原生整数类型。
func authFileIntValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		if i, err := typed.Int64(); err == nil {
			return int(i), true
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// syncAuthFileNoteAttribute 同步 note；空字符串保持旧语义，表示移除备注。
func syncAuthFileNoteAttribute(auth *coreauth.Auth) {
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	note, ok := auth.Metadata["note"].(string)
	if !ok || strings.TrimSpace(note) == "" {
		delete(auth.Metadata, "note")
		delete(auth.Attributes, "note")
		return
	}
	auth.Attributes["note"] = strings.TrimSpace(note)
}

// syncAuthFileWebsocketsAttribute 同步 websockets，false 也是有效配置，不能被当成空值删除。
func syncAuthFileWebsocketsAttribute(auth *coreauth.Auth) {
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	websockets, ok := authFileBoolValue(auth.Metadata["websockets"])
	if !ok {
		delete(auth.Attributes, "websockets")
		return
	}
	auth.Attributes["websockets"] = strconv.FormatBool(websockets)
}

// authFileBoolValue 解析 auth 文件中的布尔字段，兼容字符串形式的历史配置。
func authFileBoolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(typed))
		if errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

// syncAuthFileDisabledState 让字段 PATCH 的 disabled 与专用状态接口保持一致。
func syncAuthFileDisabledState(auth *coreauth.Auth) {
	disabled, ok := authFileBoolValue(auth.Metadata["disabled"])
	if !ok {
		return
	}
	auth.Disabled = disabled
	if disabled {
		auth.Status = coreauth.StatusDisabled
		if strings.TrimSpace(auth.StatusMessage) == "" {
			auth.StatusMessage = "disabled via management API"
		}
		return
	}
	auth.Status = coreauth.StatusActive
	auth.StatusMessage = ""
}

// authWebsocketsValue 读取列表页使用的 websockets 值，attributes 优先代表当前运行态。
func authWebsocketsValue(auth *coreauth.Auth) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if raw := strings.TrimSpace(authAttribute(auth, "websockets")); raw != "" {
		if parsed, errParse := strconv.ParseBool(raw); errParse == nil {
			return parsed, true
		}
	}
	if auth.Metadata == nil {
		return false, false
	}
	return authFileBoolValue(auth.Metadata["websockets"])
}
