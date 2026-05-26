package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type patchAuthFileFieldRequest struct {
	name   string
	fields map[string]json.RawMessage
}

// PatchAuthFileFields 更新 auth 文件的 metadata 字段，并同步影响运行时的派生属性。
func (h *Handler) PatchAuthFileFields(c *gin.Context) {
	manager := h.requireAuthManager(c)
	if manager == nil {
		return
	}
	req, ok := readPatchAuthFileFieldRequest(c)
	if !ok {
		return
	}
	targetAuth := authFileByName(manager, req.name)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	changed, err := applyAuthFileMetadataPatch(targetAuth, req.fields)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !changed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}
	targetAuth.UpdatedAt = time.Now()
	if _, err = manager.Update(c.Request.Context(), targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readPatchAuthFileFieldRequest 读取原始 JSON，保留 json.Number 以避免整数精度被 float64 化。
func readPatchAuthFileFieldRequest(c *gin.Context) (patchAuthFileFieldRequest, bool) {
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return patchAuthFileFieldRequest{}, false
	}
	nameRaw, ok := raw["name"]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return patchAuthFileFieldRequest{}, false
	}
	var nameValue string
	if err := json.Unmarshal(nameRaw, &nameValue); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return patchAuthFileFieldRequest{}, false
	}
	name := strings.TrimSpace(nameValue)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return patchAuthFileFieldRequest{}, false
	}
	delete(raw, "name")
	return patchAuthFileFieldRequest{name: name, fields: raw}, true
}

// authFileByName 兼容按 auth ID 或文件名定位，避免管理页传参依赖内部 ID。
func authFileByName(manager *coreauth.Manager, name string) *coreauth.Auth {
	if manager == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if auth, ok := manager.GetByID(name); ok {
		return auth
	}
	if auth, ok := manager.FindByFileName(name); ok {
		return auth
	}
	return nil
}

// applyAuthFileMetadataPatch 把请求字段写入 metadata，并收集需要同步的根字段。
func applyAuthFileMetadataPatch(auth *coreauth.Auth, fields map[string]json.RawMessage) (bool, error) {
	if auth == nil {
		return false, fmt.Errorf("auth file not found")
	}
	if len(fields) == 0 {
		return false, nil
	}
	auth.Metadata = cloneAuthFileMetadata(auth.Metadata)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	touchedRoots := make(map[string]struct{}, len(fields))
	for key, rawValue := range fields {
		fieldPath := strings.TrimSpace(key)
		if fieldPath == "" {
			return false, fmt.Errorf("invalid field path")
		}
		value, err := decodeAuthFileFieldValue(rawValue)
		if err != nil {
			return false, fmt.Errorf("invalid field %s", fieldPath)
		}
		if fieldPath == "headers" {
			applyAuthFileHeadersPatch(auth, value)
		} else if err = setAuthFileMetadataValue(auth.Metadata, fieldPath, value); err != nil {
			return false, err
		}
		touchedRoots[rootAuthFileField(fieldPath)] = struct{}{}
	}
	syncAuthFileMetadataFields(auth, touchedRoots)
	return true, nil
}

// decodeAuthFileFieldValue 按单字段解码，确保数字在后续同步前仍保留原始数值语义。
func decodeAuthFileFieldValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// rootAuthFileField 返回点号路径的根字段，用于判断需要同步哪些运行时派生状态。
func rootAuthFileField(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if idx := strings.Index(path, "."); idx >= 0 {
		return strings.TrimSpace(path[:idx])
	}
	return path
}

// setAuthFileMetadataValue 支持 a.b.c 形式的嵌套 metadata 更新。
func setAuthFileMetadataValue(metadata map[string]any, path string, value any) error {
	parts := strings.Split(path, ".")
	current := metadata
	for i, rawPart := range parts {
		part := strings.TrimSpace(rawPart)
		if part == "" {
			return fmt.Errorf("invalid field path: %s", path)
		}
		if i == len(parts)-1 {
			current[part] = value
			return nil
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			next = make(map[string]any)
			current[part] = next
		}
		current = next
	}
	return nil
}

// applyAuthFileHeadersPatch 合并 headers 字段，空字符串表示删除单个自定义 header。
func applyAuthFileHeadersPatch(auth *coreauth.Auth, value any) {
	if auth == nil {
		return
	}
	headersPatch, ok := authFileHeadersStringMap(value)
	if !ok {
		auth.Metadata["headers"] = value
		return
	}
	nextHeaders := authFileCustomHeadersFromMetadata(auth.Metadata)
	for key, value := range headersPatch {
		name := strings.TrimSpace(key)
		val := strings.TrimSpace(value)
		if name == "" {
			continue
		}
		if val == "" {
			delete(nextHeaders, name)
			continue
		}
		nextHeaders[name] = val
	}
	if len(nextHeaders) == 0 {
		delete(auth.Metadata, "headers")
		return
	}
	auth.Metadata["headers"] = stringMapToAnyMap(nextHeaders)
}

// authFileHeadersStringMap 只接受字符串值，避免把非 header 类型误同步到请求头。
func authFileHeadersStringMap(value any) (map[string]string, bool) {
	switch typed := value.(type) {
	case map[string]string:
		return typed, true
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, rawValue := range typed {
			value, ok := rawValue.(string)
			if !ok {
				return nil, false
			}
			out[key] = value
		}
		return out, true
	default:
		return nil, false
	}
}

// authFileCustomHeadersFromMetadata 提取已持久化的 headers，供增量 PATCH 合并使用。
func authFileCustomHeadersFromMetadata(metadata map[string]any) map[string]string {
	out := make(map[string]string)
	raw, ok := metadata["headers"]
	if !ok {
		return out
	}
	headers, ok := authFileHeadersStringMap(raw)
	if !ok {
		return out
	}
	for key, value := range headers {
		if name := strings.TrimSpace(key); name != "" {
			out[name] = strings.TrimSpace(value)
		}
	}
	return out
}

// stringMapToAnyMap 转成可 JSON 落盘的通用 map，保持 metadata 结构一致。
func stringMapToAnyMap(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// cloneAuthFileMetadata 递归复制 metadata，避免 PATCH 写穿 Auth.Clone() 共享的嵌套 map。
func cloneAuthFileMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = cloneAuthFileMetadataValue(value)
	}
	return cloned
}

// cloneAuthFileMetadataValue 只复制可变容器；标量、time/json.Number 等不可变值直接复用。
func cloneAuthFileMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAuthFileMetadata(typed)
	case map[string]string:
		cloned := make(map[string]string, len(typed))
		for key, item := range typed {
			cloned[key] = item
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, item := range typed {
			cloned[i] = cloneAuthFileMetadataValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}
