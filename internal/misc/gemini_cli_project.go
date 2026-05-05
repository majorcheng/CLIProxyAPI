package misc

import "strings"

const (
	geminiCLIFreeTierID       = "FREE"
	geminiCLILegacyTierID     = "LEGACY"
	geminiCLIBackendProjectID = "gen-lang-client-"
)

// GeminiCLIProjectSelection 描述 Gemini CLI onboarding 后选择落盘项目所需的上下文。
type GeminiCLIProjectSelection struct {
	RequestedProjectID string
	ResponseProjectID  string
	TierID             string
	ExplicitProject    bool
}

// ResolveGeminiCLIProjectID 统一选择 Gemini CLI onboarding 最终落盘项目。
func ResolveGeminiCLIProjectID(selection GeminiCLIProjectSelection) string {
	requestedProjectID := strings.TrimSpace(selection.RequestedProjectID)
	responseProjectID := strings.TrimSpace(selection.ResponseProjectID)
	if responseProjectID == "" {
		return requestedProjectID
	}
	if !selection.ExplicitProject || strings.EqualFold(responseProjectID, requestedProjectID) {
		return responseProjectID
	}
	if prefersGeminiCLIBackendProject(requestedProjectID, selection.TierID) {
		return responseProjectID
	}
	return requestedProjectID
}

// prefersGeminiCLIBackendProject 判断显式项目是否应切换到 Google 返回的后端项目。
func prefersGeminiCLIBackendProject(requestedProjectID, tierID string) bool {
	if strings.HasPrefix(strings.TrimSpace(requestedProjectID), geminiCLIBackendProjectID) {
		return true
	}
	trimmedTierID := strings.TrimSpace(tierID)
	return strings.EqualFold(trimmedTierID, geminiCLIFreeTierID) ||
		strings.EqualFold(trimmedTierID, geminiCLILegacyTierID)
}
