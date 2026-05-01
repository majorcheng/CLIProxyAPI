package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// mergeRecentRequestBuckets 将同一 provider/api_key 下的多个 auth 趋势桶合并成一组。
func mergeRecentRequestBuckets(dst, src []coreauth.RecentRequestBucket) []coreauth.RecentRequestBucket {
	if len(dst) == 0 {
		return src
	}
	if len(src) == 0 {
		return dst
	}
	n := len(dst)
	if len(src) < n {
		n = len(src)
	}
	for i := 0; i < n; i++ {
		dst[i].Success += src[i].Success
		dst[i].Failed += src[i].Failed
	}
	return dst
}

// GetAPIKeyUsage 返回所有内存态携带 api_key 的 auth 最近请求趋势，按 provider 和原始 api_key 分组。
func (h *Handler) GetAPIKeyUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	manager := h.currentAuthManager()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	now := time.Now()
	out := make(map[string]map[string][]coreauth.RecentRequestBucket)
	for _, auth := range manager.List() {
		addAPIKeyUsageAuth(out, auth, now)
	}
	c.JSON(http.StatusOK, out)
}

// addAPIKeyUsageAuth 把单个 API-key auth 的 recent_requests 合并进响应结构。
func addAPIKeyUsageAuth(out map[string]map[string][]coreauth.RecentRequestBucket, auth *coreauth.Auth, now time.Time) {
	if out == nil || auth == nil {
		return
	}
	apiKey := apiKeyUsageAPIKey(auth)
	if apiKey == "" {
		return
	}
	provider := apiKeyUsageProvider(auth.Provider)
	providerBucket := ensureAPIKeyUsageProvider(out, provider)
	recent := auth.RecentRequestsSnapshot(now)
	providerBucket[apiKey] = mergeRecentRequestBuckets(providerBucket[apiKey], recent)
}

// apiKeyUsageAPIKey 直接读取业务请求使用的 api_key，不受 AccountInfo 展示类型影响。
func apiKeyUsageAPIKey(auth *coreauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["api_key"])
}

// ensureAPIKeyUsageProvider 确保 provider 分组存在。
func ensureAPIKeyUsageProvider(out map[string]map[string][]coreauth.RecentRequestBucket, provider string) map[string][]coreauth.RecentRequestBucket {
	bucket, ok := out[provider]
	if ok {
		return bucket
	}
	bucket = make(map[string][]coreauth.RecentRequestBucket)
	out[provider] = bucket
	return bucket
}

// apiKeyUsageProvider 归一化 provider 名称，空值统一落到 unknown。
func apiKeyUsageProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return "unknown"
	}
	return provider
}
