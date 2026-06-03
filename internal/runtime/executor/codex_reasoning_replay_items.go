package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func codexInputHasValidReasoningEncryptedContent(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			continue
		}
		encryptedContent := item.Get("encrypted_content")
		if encryptedContent.Type != gjson.String {
			continue
		}
		if _, err := signature.InspectGPTReasoningSignature(encryptedContent.String()); err == nil {
			return true
		}
	}
	return false
}

func filterCodexReasoningReplayItemsForInput(body []byte, items [][]byte) [][]byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil
	}

	hasInputReasoning := codexInputHasValidReasoningEncryptedContent(body)
	existingCalls, existingOutputs := codexReplayExistingCallState(input)

	filtered := make([][]byte, 0, len(items))
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "reasoning":
			if hasInputReasoning {
				continue
			}
		case "function_call", "custom_tool_call":
			keys := codexReplayToolCallKeys(itemResult)
			if len(keys) == 0 || codexReplayAnyToolCallKeyExists(existingCalls, keys) {
				continue
			}
			// 只回放本轮已有输出引用的 tool call，避免把孤儿 tool call 注入给上游。
			if !codexReplayItemHasMatchingOutput(itemResult, existingOutputs) {
				continue
			}
			for _, key := range keys {
				existingCalls[key] = true
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// codexReplayExistingCallState 收集当前请求已有 tool call 和 tool output 的可比较 ID。
func codexReplayExistingCallState(input gjson.Result) (map[string]bool, map[string]bool) {
	existingCalls := make(map[string]bool)
	existingOutputs := make(map[string]bool)
	for _, inputItem := range input.Array() {
		itemType := strings.TrimSpace(inputItem.Get("type").String())
		if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			for _, callID := range codexReplayComparableCallIDs(inputItem.Get("call_id").String()) {
				existingOutputs[callID] = true
			}
		}
		for _, key := range codexReplayToolCallKeys(inputItem) {
			existingCalls[key] = true
		}
	}
	return existingCalls, existingOutputs
}

// codexReplayItemHasMatchingOutput 判断缓存 tool call 是否被当前请求的 output 引用。
func codexReplayItemHasMatchingOutput(item gjson.Result, existingOutputs map[string]bool) bool {
	for _, callID := range codexReplayComparableCallIDs(item.Get("call_id").String()) {
		if existingOutputs[callID] {
			return true
		}
	}
	return false
}

func insertCodexReasoningReplayItems(body []byte, replayItems [][]byte) ([]byte, bool) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() || len(replayItems) == 0 {
		return body, false
	}
	inputItems := input.Array()
	insertIndex := codexReasoningReplayInsertIndex(inputItems, replayItems)
	replayItems = codexAlignReasoningReplayToolCallIDs(inputItems, replayItems)
	items := make([]string, 0, len(inputItems)+len(replayItems))
	for index, inputItem := range inputItems {
		if index == insertIndex {
			for _, replayItem := range replayItems {
				items = append(items, string(replayItem))
			}
		}
		items = append(items, inputItem.Raw)
	}
	if insertIndex == len(inputItems) {
		for _, replayItem := range replayItems {
			items = append(items, string(replayItem))
		}
	}
	updated, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(items, ",")+"]"))
	if err != nil {
		return body, false
	}
	return updated, true
}

func codexReasoningReplayInsertIndex(inputItems []gjson.Result, replayItems [][]byte) int {
	replayCallIDs := make(map[string]bool)
	for _, replayItem := range replayItems {
		itemResult := gjson.ParseBytes(replayItem)
		itemType := strings.TrimSpace(itemResult.Get("type").String())
		if itemType != "function_call" && itemType != "custom_tool_call" {
			continue
		}
		for _, callID := range codexReplayComparableCallIDs(itemResult.Get("call_id").String()) {
			replayCallIDs[callID] = true
		}
	}
	if len(replayCallIDs) > 0 {
		for index, inputItem := range inputItems {
			itemType := strings.TrimSpace(inputItem.Get("type").String())
			if itemType != "function_call_output" && itemType != "custom_tool_call_output" {
				continue
			}
			callID := strings.TrimSpace(inputItem.Get("call_id").String())
			if callID == "" || replayCallIDs[callID] {
				return index
			}
		}
	}
	for index := len(inputItems) - 1; index >= 0; index-- {
		inputItem := inputItems[index]
		if strings.TrimSpace(inputItem.Get("type").String()) == "message" && strings.TrimSpace(inputItem.Get("role").String()) == "assistant" {
			return index
		}
	}
	for index, inputItem := range inputItems {
		if shouldInsertCodexReasoningReplayBefore(inputItem) {
			return index
		}
	}
	return len(inputItems)
}

func codexAlignReasoningReplayToolCallIDs(inputItems []gjson.Result, replayItems [][]byte) [][]byte {
	outputCallIDs := codexReplayOutputCallIDs(inputItems)
	if len(outputCallIDs) == 0 {
		return replayItems
	}
	aligned := make([][]byte, 0, len(replayItems))
	for _, replayItem := range replayItems {
		aligned = append(aligned, alignCodexReplayToolCallID(replayItem, outputCallIDs))
	}
	return aligned
}

func alignCodexReplayToolCallID(replayItem []byte, outputCallIDs map[string]string) []byte {
	itemResult := gjson.ParseBytes(replayItem)
	itemType := strings.TrimSpace(itemResult.Get("type").String())
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return replayItem
	}
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	outputCallID := ""
	for _, candidate := range codexReplayComparableCallIDs(callID) {
		if value := outputCallIDs[candidate]; value != "" {
			outputCallID = value
			break
		}
	}
	if outputCallID == "" || outputCallID == callID {
		return replayItem
	}
	updated, err := sjson.SetBytes(replayItem, "call_id", outputCallID)
	if err != nil {
		return replayItem
	}
	return updated
}

func codexReplayOutputCallIDs(inputItems []gjson.Result) map[string]string {
	outputCallIDs := make(map[string]string)
	for _, inputItem := range inputItems {
		itemType := strings.TrimSpace(inputItem.Get("type").String())
		if itemType != "function_call_output" && itemType != "custom_tool_call_output" {
			continue
		}
		callID := strings.TrimSpace(inputItem.Get("call_id").String())
		if callID == "" {
			continue
		}
		for _, candidate := range codexReplayComparableCallIDs(callID) {
			outputCallIDs[candidate] = callID
		}
	}
	return outputCallIDs
}

func shouldInsertCodexReasoningReplayBefore(item gjson.Result) bool {
	if strings.TrimSpace(item.Get("type").String()) != "message" {
		return true
	}
	switch strings.TrimSpace(item.Get("role").String()) {
	case "developer", "system":
		return false
	default:
		return true
	}
}

func codexReplayToolCallKeys(item gjson.Result) []string {
	itemType := strings.TrimSpace(item.Get("type").String())
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return nil
	}
	callIDs := codexReplayComparableCallIDs(item.Get("call_id").String())
	if len(callIDs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(callIDs))
	for _, callID := range callIDs {
		keys = append(keys, itemType+":"+callID)
	}
	return keys
}

func codexReplayAnyToolCallKeyExists(existing map[string]bool, keys []string) bool {
	for _, key := range keys {
		if existing[key] {
			return true
		}
	}
	return false
}

func codexReplayComparableCallIDs(callID string) []string {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return nil
	}
	claudeVisibleCallID := shortenCodexReplayCallIDIfNeeded(util.SanitizeClaudeToolID(callID))
	if claudeVisibleCallID == "" || claudeVisibleCallID == callID {
		return []string{callID}
	}
	return []string{callID, claudeVisibleCallID}
}

func shortenCodexReplayCallIDIfNeeded(id string) string {
	if len(id) <= codexReplayToolIDLimit {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLen := codexReplayToolIDLimit - len(suffix)
	if prefixLen <= 0 {
		return suffix[len(suffix)-codexReplayToolIDLimit:]
	}
	return id[:prefixLen] + suffix
}
