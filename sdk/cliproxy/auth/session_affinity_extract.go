package auth

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"regexp"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const sessionHashSeedMaxLen = 100

var sessionPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

type messageHashSeeds struct {
	systemPrompt      string
	firstUserMsg      string
	firstAssistantMsg string
}

// ExtractSessionID 按统一优先级提取本次请求的主会话 ID。
func ExtractSessionID(headers http.Header, payload []byte, metadata map[string]any) string {
	primaryID, _ := extractSessionIDs(headers, payload, metadata)
	return primaryID
}

func extractSessionIDs(headers http.Header, payload []byte, metadata map[string]any) (string, string) {
	if primaryID, fallbackID := sessionAffinityIDsFromMetadata(metadata); primaryID != "" {
		return primaryID, fallbackID
	}
	if executionSessionID := metadataString(metadata, cliproxyexecutor.ExecutionSessionMetadataKey); executionSessionID != "" {
		return "exec:" + executionSessionID, ""
	}
	if sessionID := extractClaudeCodeSessionID(payload); sessionID != "" {
		return "claude:" + sessionID, ""
	}
	if sessionID := extractHeaderSessionID(headers); sessionID != "" {
		return "header:" + sessionID, ""
	}
	if userID := extractPayloadUserID(payload); userID != "" {
		return "user:" + userID, ""
	}
	if conversationID := extractConversationID(payload); conversationID != "" {
		return "conv:" + conversationID, ""
	}
	return extractMessageHashIDs(payload)
}

func extractClaudeCodeSessionID(payload []byte) string {
	userID := extractPayloadMetadataUserID(payload)
	if userID == "" {
		return ""
	}
	if matches := sessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		return matches[1]
	}
	if !strings.HasPrefix(userID, "{") {
		return ""
	}
	return strings.TrimSpace(gjson.Get(userID, "session_id").String())
}

func extractPayloadMetadataUserID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
}

func extractHeaderSessionID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if sessionID := strings.TrimSpace(headers.Get("X-Session-ID")); sessionID != "" {
		return sessionID
	}
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), "X-Session-ID") || len(values) == 0 {
			continue
		}
		if sessionID := strings.TrimSpace(values[0]); sessionID != "" {
			return sessionID
		}
	}
	return ""
}

func extractPayloadUserID(payload []byte) string {
	userID := extractPayloadMetadataUserID(payload)
	if userID == "" || strings.HasPrefix(userID, "{") {
		return ""
	}
	if sessionPattern.MatchString(userID) {
		return ""
	}
	return userID
}

func extractConversationID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "conversation_id").String())
}

func extractMessageHashIDs(payload []byte) (string, string) {
	if len(payload) == 0 {
		return "", ""
	}
	seeds := &messageHashSeeds{}
	fillSeedsFromMessages(payload, seeds)
	fillSeedsFromTopLevelSystem(payload, seeds)
	fillSeedsFromGeminiFormat(payload, seeds)
	fillSeedsFromResponsesFormat(payload, seeds)
	if seeds.systemPrompt == "" && seeds.firstUserMsg == "" {
		return "", ""
	}
	shortHash := computeSessionHash(seeds.systemPrompt, seeds.firstUserMsg, "")
	if seeds.firstAssistantMsg == "" {
		return shortHash, ""
	}
	fullHash := computeSessionHash(seeds.systemPrompt, seeds.firstUserMsg, seeds.firstAssistantMsg)
	return fullHash, shortHash
}

func fillSeedsFromMessages(payload []byte, seeds *messageHashSeeds) {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return
	}
	messages.ForEach(func(_, msg gjson.Result) bool {
		role := strings.TrimSpace(msg.Get("role").String())
		content := extractMessageContent(msg.Get("content"))
		applyMessageSeed(seeds, role, content)
		return !messageHashSeedsComplete(seeds)
	})
}

func fillSeedsFromTopLevelSystem(payload []byte, seeds *messageHashSeeds) {
	if seeds == nil || seeds.systemPrompt != "" {
		return
	}
	topSystem := gjson.GetBytes(payload, "system")
	if !topSystem.Exists() {
		return
	}
	if topSystem.Type == gjson.String {
		seeds.systemPrompt = truncateSessionSeed(topSystem.String())
		return
	}
	topSystem.ForEach(func(_, part gjson.Result) bool {
		text := strings.TrimSpace(part.Get("text").String())
		if text == "" {
			return true
		}
		seeds.systemPrompt = truncateSessionSeed(text)
		return false
	})
}

func fillSeedsFromGeminiFormat(payload []byte, seeds *messageHashSeeds) {
	if seeds == nil || messageHashSeedsComplete(seeds) {
		return
	}
	fillGeminiSystemPrompt(payload, seeds)
	contents := gjson.GetBytes(payload, "contents")
	if !contents.Exists() || !contents.IsArray() {
		return
	}
	contents.ForEach(func(_, msg gjson.Result) bool {
		role := strings.TrimSpace(msg.Get("role").String())
		text := extractTextParts(msg.Get("parts"), "text")
		if role == "model" {
			role = "assistant"
		}
		applyMessageSeed(seeds, role, text)
		return !messageHashSeedsComplete(seeds)
	})
}

func fillGeminiSystemPrompt(payload []byte, seeds *messageHashSeeds) {
	if seeds == nil || seeds.systemPrompt != "" {
		return
	}
	parts := gjson.GetBytes(payload, "systemInstruction.parts")
	if !parts.Exists() || !parts.IsArray() {
		return
	}
	seeds.systemPrompt = extractTextParts(parts, "text")
}

func fillSeedsFromResponsesFormat(payload []byte, seeds *messageHashSeeds) {
	if seeds == nil || messageHashSeedsComplete(seeds) {
		return
	}
	if seeds.systemPrompt == "" {
		if instructions := strings.TrimSpace(gjson.GetBytes(payload, "instructions").String()); instructions != "" {
			seeds.systemPrompt = truncateSessionSeed(instructions)
		}
	}
	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return
	}
	input.ForEach(func(_, item gjson.Result) bool {
		role := responseItemRole(item)
		content := extractResponsesItemContent(item)
		applyMessageSeed(seeds, role, content)
		return !messageHashSeedsComplete(seeds)
	})
}

func responseItemRole(item gjson.Result) string {
	itemType := strings.TrimSpace(item.Get("type").String())
	if itemType == "reasoning" || (itemType != "" && itemType != "message") {
		return ""
	}
	return strings.TrimSpace(item.Get("role").String())
}

func extractResponsesItemContent(item gjson.Result) string {
	content := item.Get("content")
	if content.Type == gjson.String {
		return truncateSessionSeed(content.String())
	}
	return extractResponsesAPIContent(content)
}

func applyMessageSeed(seeds *messageHashSeeds, role string, content string) {
	if seeds == nil || strings.TrimSpace(content) == "" {
		return
	}
	content = truncateSessionSeed(content)
	switch role {
	case "system", "developer":
		if seeds.systemPrompt == "" {
			seeds.systemPrompt = content
		}
	case "user":
		if seeds.firstUserMsg == "" {
			seeds.firstUserMsg = content
		}
	case "assistant":
		if seeds.firstAssistantMsg == "" {
			seeds.firstAssistantMsg = content
		}
	}
}

func messageHashSeedsComplete(seeds *messageHashSeeds) bool {
	if seeds == nil {
		return false
	}
	return seeds.systemPrompt != "" && seeds.firstUserMsg != "" && seeds.firstAssistantMsg != ""
}

func truncateSessionSeed(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= sessionHashSeedMaxLen {
		return value
	}
	return value[:sessionHashSeedMaxLen]
}

func computeSessionHash(systemPrompt, userMsg, assistantMsg string) string {
	h := fnv.New64a()
	if systemPrompt != "" {
		_, _ = h.Write([]byte("sys:" + systemPrompt + "\n"))
	}
	if userMsg != "" {
		_, _ = h.Write([]byte("usr:" + userMsg + "\n"))
	}
	if assistantMsg != "" {
		_, _ = h.Write([]byte("ast:" + assistantMsg + "\n"))
	}
	return fmt.Sprintf("msg:%016x", h.Sum64())
}

func extractMessageContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return truncateSessionSeed(content.String())
	}
	return extractTextParts(content, "text")
}

func extractResponsesAPIContent(content gjson.Result) string {
	return extractTextParts(content, "text", "input_text", "output_text")
}

func extractTextParts(parts gjson.Result, allowedTypes ...string) string {
	if !parts.Exists() || !parts.IsArray() {
		return ""
	}
	allowed := make(map[string]struct{}, len(allowedTypes))
	for _, itemType := range allowedTypes {
		allowed[itemType] = struct{}{}
	}
	texts := make([]string, 0, 2)
	parts.ForEach(func(_, part gjson.Result) bool {
		partType := strings.TrimSpace(part.Get("type").String())
		if len(allowed) > 0 && partType != "" {
			if _, ok := allowed[partType]; !ok {
				return true
			}
		}
		text := strings.TrimSpace(part.Get("text").String())
		if text == "" {
			return true
		}
		texts = append(texts, text)
		return true
	})
	return truncateSessionSeed(strings.Join(texts, " "))
}

// extractSessionID 为旧调用点保留兼容入口。
func extractSessionID(payload []byte) string {
	return ExtractSessionID(nil, payload, nil)
}
