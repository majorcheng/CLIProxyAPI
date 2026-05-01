package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// apiKeyUsageEntry 是 management API 返回的单个 API-key 分组统计。
type apiKeyUsageEntry struct {
	Success        int64                          `json:"success"`
	Failed         int64                          `json:"failed"`
	RecentRequests []coreauth.RecentRequestBucket `json:"recent_requests"`
}

// recentRequestBucketTotals 汇总最近请求时间桶里的成功与失败计数。
// management 对外暴露时要和 recent_requests 保持同一窗口口径，避免累计总数与窗口趋势混用。
func recentRequestBucketTotals(buckets []coreauth.RecentRequestBucket) (int64, int64) {
	var success int64
	var failed int64
	for _, bucket := range buckets {
		success += bucket.Success
		failed += bucket.Failed
	}
	return success, failed
}

// mergeRecentRequestBuckets 将同一 provider/base_url|api_key 下的多个 auth 趋势桶合并成一组。
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

// GetAPIKeyUsage 返回携带 api_key 的 auth 最近请求趋势，按 provider 和 base_url|api_key 分组。
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
	out := make(map[string]map[string]apiKeyUsageEntry)
	for _, auth := range manager.List() {
		addAPIKeyUsageAuth(out, auth, now)
	}
	c.JSON(http.StatusOK, out)
}

// addAPIKeyUsageAuth 把单个 API-key auth 的 recent_requests 合并进响应结构。
func addAPIKeyUsageAuth(out map[string]map[string]apiKeyUsageEntry, auth *coreauth.Auth, now time.Time) {
	if out == nil || auth == nil {
		return
	}
	apiKey := apiKeyUsageAPIKey(auth)
	if apiKey == "" {
		return
	}
	provider := apiKeyUsageProvider(auth.Provider)
	providerBucket := ensureAPIKeyUsageProvider(out, provider)
	groupKey := apiKeyUsageGroupKey(auth, apiKey)
	recent := auth.RecentRequestsSnapshot(now)
	success, failed := recentRequestBucketTotals(recent)
	entry := providerBucket[groupKey]
	entry.Success += success
	entry.Failed += failed
	entry.RecentRequests = mergeRecentRequestBuckets(entry.RecentRequests, recent)
	providerBucket[groupKey] = entry
}

// apiKeyUsageAPIKey 直接读取业务请求使用的 api_key，不受 AccountInfo 展示类型影响。
func apiKeyUsageAPIKey(auth *coreauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["api_key"])
}

// apiKeyUsageGroupKey 生成上游兼容的 base_url|api_key 分组 key。
func apiKeyUsageGroupKey(auth *coreauth.Auth, apiKey string) string {
	return apiKeyUsageBaseURL(auth) + "|" + strings.TrimSpace(apiKey)
}

// apiKeyUsageBaseURL 兼容历史配置里的 base_url 与 base-url 两种写法。
func apiKeyUsageBaseURL(auth *coreauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	baseURL := strings.TrimSpace(auth.Attributes["base_url"])
	if baseURL != "" {
		return baseURL
	}
	return strings.TrimSpace(auth.Attributes["base-url"])
}

// ensureAPIKeyUsageProvider 确保 provider 分组存在。
func ensureAPIKeyUsageProvider(out map[string]map[string]apiKeyUsageEntry, provider string) map[string]apiKeyUsageEntry {
	bucket, ok := out[provider]
	if ok {
		return bucket
	}
	bucket = make(map[string]apiKeyUsageEntry)
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
