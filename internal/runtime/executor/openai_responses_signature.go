package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// sanitizeOpenAIResponsesReasoningEncryptedContent 删除非法 reasoning.encrypted_content，避免 GPT/Codex Responses 上游拒绝整请求。
func sanitizeOpenAIResponsesReasoningEncryptedContent(ctx context.Context, provider string, body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai responses upstream"
	}

	updated := body
	for index, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}

		encryptedContentPath := fmt.Sprintf("input.%d.encrypted_content", index)
		encryptedContent := gjson.GetBytes(updated, encryptedContentPath)
		if !encryptedContent.Exists() {
			continue
		}

		reason := invalidGPTReasoningEncryptedContentReason(encryptedContent)
		if reason == "" {
			continue
		}

		next, err := sjson.DeleteBytes(updated, encryptedContentPath)
		if err != nil {
			logWithRequestID(ctx).Debugf("%s: failed to drop invalid reasoning encrypted_content at input[%d]: %v", provider, index, err)
			continue
		}
		updated = next

		itemID := strings.TrimSpace(gjson.GetBytes(updated, fmt.Sprintf("input.%d.id", index)).String())
		if itemID == "" {
			itemID = fmt.Sprintf("input[%d]", index)
		}
		logWithRequestID(ctx).Debugf("%s: dropped invalid reasoning encrypted_content at input[%d] item_id=%q reason=%s", provider, index, itemID, reason)
	}
	return updated
}

// invalidGPTReasoningEncryptedContentReason 返回 encrypted_content 非法原因；空字符串代表可保留。
func invalidGPTReasoningEncryptedContentReason(encryptedContent gjson.Result) string {
	switch encryptedContent.Type {
	case gjson.String:
		rawSignature := encryptedContent.String()
		if rawSignature != strings.TrimSpace(rawSignature) {
			return "encrypted_content has leading or trailing whitespace"
		}
		if _, err := signature.InspectGPTReasoningSignature(rawSignature); err != nil {
			return err.Error()
		}
		return ""
	case gjson.Null:
		return "encrypted_content is null"
	default:
		return fmt.Sprintf("encrypted_content must be a string, got %s", encryptedContent.Type.String())
	}
}
