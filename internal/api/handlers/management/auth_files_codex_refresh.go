package management

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var (
	errCodexRefreshTargetMissing  = errors.New("name 或 auth_index 必填")
	errCodexRefreshTargetMismatch = errors.New("name 与 auth_index 指向的凭证不一致")
)

type codexAuthRefreshRequest struct {
	AuthIndexSnake  *string `json:"auth_index"`
	AuthIndexCamel  *string `json:"authIndex"`
	AuthIndexPascal *string `json:"AuthIndex"`
	Name            *string `json:"name"`
}

// RefreshCodexAuthFile 手动触发一次指定 Codex 凭证的 RT 刷新，并同步返回最新状态。
func (h *Handler) RefreshCodexAuthFile(c *gin.Context) {
	manager := h.requireAuthManager(c)
	if manager == nil {
		return
	}

	var req codexAuthRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体无效"})
		return
	}

	authIndex := firstNonEmptyString(req.AuthIndexSnake, req.AuthIndexCamel, req.AuthIndexPascal)
	name := firstNonEmptyString(req.Name)
	target, errResolve := h.resolveCodexRefreshTarget(authIndex, name)
	if errResolve != nil {
		switch {
		case errors.Is(errResolve, errCodexRefreshTargetMissing), errors.Is(errResolve, errCodexRefreshTargetMismatch):
			c.JSON(http.StatusBadRequest, gin.H{"error": errResolve.Error()})
		case errors.Is(errResolve, coreauth.ErrAuthNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "未找到认证文件"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解析目标凭证失败"})
		}
		return
	}

	if !strings.EqualFold(strings.TrimSpace(target.Provider), "codex") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持 Codex 凭证"})
		return
	}
	if !authMetadataHasRefreshToken(target.Metadata) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前 Codex 凭证缺少 refresh_token"})
		return
	}

	refreshed, errRefresh := manager.RefreshAuthNow(c.Request.Context(), target.ID)
	if errRefresh != nil {
		response := gin.H{"error": "Codex RT 刷新失败: " + errRefresh.Error()}
		if entry := h.codexRefreshResponseEntry(refreshed); entry != nil {
			response["file"] = entry
		}
		switch {
		case errors.Is(errRefresh, coreauth.ErrAuthNotFound):
			response["error"] = "未找到认证文件"
			c.JSON(http.StatusNotFound, response)
		case errors.Is(errRefresh, coreauth.ErrAuthRefreshInFlight):
			response["error"] = "该凭证正在刷新中"
			c.JSON(http.StatusConflict, response)
		case errors.Is(errRefresh, coreauth.ErrAuthRefreshExecutorUnavailable):
			response["error"] = "Codex 刷新执行器不可用"
			c.JSON(http.StatusServiceUnavailable, response)
		default:
			c.JSON(http.StatusBadGateway, response)
		}
		return
	}

	entry := h.codexRefreshResponseEntry(refreshed)
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"file":   entry,
	})
}

func (h *Handler) resolveCodexRefreshTarget(authIndex string, name string) (*coreauth.Auth, error) {
	authIndex = strings.TrimSpace(authIndex)
	name = strings.TrimSpace(name)
	if authIndex == "" && name == "" {
		return nil, errCodexRefreshTargetMissing
	}

	var targetByIndex *coreauth.Auth
	if authIndex != "" {
		targetByIndex = h.authByIndex(authIndex)
		if targetByIndex == nil {
			return nil, coreauth.ErrAuthNotFound
		}
	}

	var targetByName *coreauth.Auth
	if name != "" {
		targetByName = h.authByName(name)
		if authIndex == "" && targetByName == nil {
			return nil, coreauth.ErrAuthNotFound
		}
		if targetByIndex != nil && targetByName != nil && targetByIndex.ID != targetByName.ID {
			return nil, errCodexRefreshTargetMismatch
		}
	}

	if targetByIndex != nil {
		return targetByIndex, nil
	}
	return targetByName, nil
}

func (h *Handler) authByName(name string) *coreauth.Auth {
	name = strings.TrimSpace(name)
	manager := h.currentAuthManager()
	if name == "" || h == nil || manager == nil {
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

func (h *Handler) codexRefreshResponseEntry(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}
	if entry := h.buildAuthFileEntry(auth); entry != nil {
		return entry
	}
	auth.EnsureIndex()
	entry := gin.H{
		"id":                auth.ID,
		"auth_index":        auth.Index,
		"name":              auth.FileName,
		"type":              strings.TrimSpace(auth.Provider),
		"provider":          strings.TrimSpace(auth.Provider),
		"status":            auth.Status,
		"status_message":    auth.StatusMessage,
		"disabled":          auth.Disabled,
		"unavailable":       auth.Unavailable,
		"has_refresh_token": authMetadataHasRefreshToken(auth.Metadata),
	}
	if entry["name"] == "" {
		entry["name"] = auth.ID
	}
	if !auth.LastRefreshedAt.IsZero() {
		entry["last_refresh"] = auth.LastRefreshedAt
	}
	if email := authEmail(auth); email != "" {
		entry["email"] = email
	}
	return entry
}
