package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// DisableImageGenerationMode 表示 disable-image-generation 的三态配置。
//
// 支持值：
//   - false：允许所有图片生成能力。
//   - true：全局禁用 image_generation，并让图片入口返回 404。
//   - "chat"：仅在非 Images 入口禁用 image_generation，保留图片入口。
type DisableImageGenerationMode int

const (
	DisableImageGenerationOff DisableImageGenerationMode = iota
	DisableImageGenerationAll
	DisableImageGenerationChat
)

// String 返回用于日志和配置 diff 的稳定展示值。
func (m DisableImageGenerationMode) String() string {
	switch m {
	case DisableImageGenerationAll:
		return "true"
	case DisableImageGenerationChat:
		return "chat"
	default:
		return "false"
	}
}

// MarshalYAML 保持 false/true 使用布尔值，chat 使用字符串，兼容既有配置。
func (m DisableImageGenerationMode) MarshalYAML() (any, error) {
	switch m {
	case DisableImageGenerationAll:
		return true, nil
	case DisableImageGenerationChat:
		return "chat", nil
	default:
		return false, nil
	}
}

// UnmarshalYAML 同时接受布尔值和字符串，便于旧配置无损升级。
func (m *DisableImageGenerationMode) UnmarshalYAML(value *yaml.Node) error {
	mode, err := parseDisableImageGenerationNode(value)
	if err != nil {
		return err
	}
	*m = mode
	return nil
}

// MarshalJSON 与 YAML 语义一致，保持 API 输出稳定。
func (m DisableImageGenerationMode) MarshalJSON() ([]byte, error) {
	switch m {
	case DisableImageGenerationAll:
		return []byte("true"), nil
	case DisableImageGenerationChat:
		return json.Marshal("chat")
	default:
		return []byte("false"), nil
	}
}

// UnmarshalJSON 同时兼容布尔值、null 和字符串。
func (m *DisableImageGenerationMode) UnmarshalJSON(data []byte) error {
	mode, err := parseDisableImageGenerationJSON(data)
	if err != nil {
		return err
	}
	*m = mode
	return nil
}

func parseDisableImageGenerationNode(value *yaml.Node) (DisableImageGenerationMode, error) {
	if value == nil {
		return DisableImageGenerationOff, nil
	}
	var b bool
	if err := value.Decode(&b); err == nil && value.Kind == yaml.ScalarNode && value.ShortTag() == "!!bool" {
		return boolDisableImageGenerationMode(b), nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return DisableImageGenerationOff, fmt.Errorf("disable-image-generation 值无效")
	}
	return parseDisableImageGenerationString(s)
}

func parseDisableImageGenerationJSON(data []byte) (DisableImageGenerationMode, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return DisableImageGenerationOff, nil
	}
	var b bool
	if err := json.Unmarshal(trimmed, &b); err == nil {
		return boolDisableImageGenerationMode(b), nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return DisableImageGenerationOff, fmt.Errorf("disable-image-generation 值无效")
	}
	return parseDisableImageGenerationString(s)
}

func boolDisableImageGenerationMode(enabled bool) DisableImageGenerationMode {
	if enabled {
		return DisableImageGenerationAll
	}
	return DisableImageGenerationOff
}

func parseDisableImageGenerationString(s string) (DisableImageGenerationMode, error) {
	normalized := strings.TrimSpace(strings.ToLower(s))
	switch normalized {
	case "", "false", "0", "off", "no":
		return DisableImageGenerationOff, nil
	case "true", "1", "on", "yes":
		return DisableImageGenerationAll, nil
	case "chat":
		return DisableImageGenerationChat, nil
	default:
		return DisableImageGenerationOff, fmt.Errorf("disable-image-generation 值 %q 无效，只允许 true、false 或 chat", s)
	}
}
