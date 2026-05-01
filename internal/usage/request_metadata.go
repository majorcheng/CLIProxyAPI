package usage

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
)

const (
	// APIResponseTimestampContextKey 保存首次上游响应到达时间，供 TTFB/首 token 统计复用。
	APIResponseTimestampContextKey = "API_RESPONSE_TIMESTAMP"
	requestMetadataGinKey          = "CLI_PROXY_USAGE_REQUEST_METADATA"
)

type requestMetadataKey struct{}

type requestMetadataHolder struct {
	clientIP           atomic.Value
	userAgent          atomic.Value
	requestType        atomic.Value
	reasoningEffort    atomic.Value
	firstTokenUnixNano atomic.Int64
}

// WithRequestMetadataFromGin 把 Gin 上的易变字段快照到 context，并把 holder 反挂到 Gin 供后续事件更新。
func WithRequestMetadataFromGin(ctx context.Context, c *gin.Context) context.Context {
	ctx, holder := ensureRequestMetadataHolder(ctx)
	if c == nil || holder == nil {
		return ctx
	}
	c.Set(requestMetadataGinKey, holder)
	if c.Request != nil {
		storeStringValue(&holder.clientIP, logging.ResolveClientIP(c))
		storeStringValue(&holder.userAgent, logging.NormalizeUserAgent(c.Request.UserAgent()))
	}
	if requestType := requestTypeFromGin(c); requestType != "" {
		storeStringValue(&holder.requestType, requestType)
	}
	if reasoning := reasoningEffortFromGin(c); reasoning != "" {
		storeStringValue(&holder.reasoningEffort, reasoning)
	}
	if ts := apiResponseTimestampFromGin(c); !ts.IsZero() {
		storeFirstTokenTime(holder, ts)
	}
	return ctx
}

// SetRequestType 同步更新 Gin 与稳定 holder，避免流式分类被异步统计读丢。
func SetRequestType(c *gin.Context, requestType string) {
	requestType = normalizeRequestType(requestType)
	if c == nil || requestType == "" {
		return
	}
	c.Set(RequestTypeContextKey, requestType)
	if holder := requestMetadataHolderFromGin(c); holder != nil {
		storeStringValue(&holder.requestType, requestType)
	}
}

// SetAPIResponseTimestamp 只记录第一次响应时间，并同步到稳定 holder。
func SetAPIResponseTimestamp(c *gin.Context, ts time.Time) {
	if c == nil || ts.IsZero() {
		return
	}
	if _, exists := c.Get(APIResponseTimestampContextKey); !exists {
		c.Set(APIResponseTimestampContextKey, ts)
	}
	if holder := requestMetadataHolderFromGin(c); holder != nil {
		storeFirstTokenTime(holder, ts)
	}
}

// SetReasoningEffort 同步记录请求的 reasoning effort 快照。
func SetReasoningEffort(c *gin.Context, effort string) {
	effort = strings.TrimSpace(effort)
	if c == nil || effort == "" {
		return
	}
	c.Set(RequestReasoningEffortContextKey, effort)
	if holder := requestMetadataHolderFromGin(c); holder != nil {
		storeStringValue(&holder.reasoningEffort, effort)
	}
}

// ensureRequestMetadataHolder 复用或创建本次请求的稳定 usage metadata holder。
func ensureRequestMetadataHolder(ctx context.Context) (context.Context, *requestMetadataHolder) {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder := requestMetadataHolderFromContext(ctx); holder != nil {
		return ctx, holder
	}
	holder := &requestMetadataHolder{}
	return context.WithValue(ctx, requestMetadataKey{}, holder), holder
}

// requestMetadataHolderFromContext 从 context 读取稳定 holder。
func requestMetadataHolderFromContext(ctx context.Context) *requestMetadataHolder {
	if ctx == nil {
		return nil
	}
	holder, _ := ctx.Value(requestMetadataKey{}).(*requestMetadataHolder)
	return holder
}

// requestMetadataHolderFromGin 从 Gin context 读取反挂的稳定 holder。
func requestMetadataHolderFromGin(c *gin.Context) *requestMetadataHolder {
	if c == nil {
		return nil
	}
	raw, exists := c.Get(requestMetadataGinKey)
	if !exists {
		return nil
	}
	holder, _ := raw.(*requestMetadataHolder)
	return holder
}

// requestMetadataClientIP 返回请求创建时解析出的客户端 IP。
func requestMetadataClientIP(ctx context.Context) string {
	return loadMetadataString(ctx, func(holder *requestMetadataHolder) *atomic.Value { return &holder.clientIP })
}

// requestMetadataUserAgent 返回请求创建时规范化后的 User-Agent。
func requestMetadataUserAgent(ctx context.Context) string {
	return loadMetadataString(ctx, func(holder *requestMetadataHolder) *atomic.Value { return &holder.userAgent })
}

// requestMetadataRequestType 返回已稳定化的请求类型。
func requestMetadataRequestType(ctx context.Context) string {
	return normalizeRequestType(loadMetadataString(ctx, func(holder *requestMetadataHolder) *atomic.Value { return &holder.requestType }))
}

// requestMetadataReasoningEffort 返回请求体解析出的 reasoning effort。
func requestMetadataReasoningEffort(ctx context.Context) string {
	return loadMetadataString(ctx, func(holder *requestMetadataHolder) *atomic.Value { return &holder.reasoningEffort })
}

// requestMetadataFirstTokenTime 返回首次响应时间；零值表示未记录。
func requestMetadataFirstTokenTime(ctx context.Context) time.Time {
	holder := requestMetadataHolderFromContext(ctx)
	if holder == nil {
		return time.Time{}
	}
	nano := holder.firstTokenUnixNano.Load()
	if nano <= 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

// loadMetadataString 统一读取 atomic.Value 中的字符串字段。
func loadMetadataString(ctx context.Context, pick func(*requestMetadataHolder) *atomic.Value) string {
	holder := requestMetadataHolderFromContext(ctx)
	if holder == nil || pick == nil {
		return ""
	}
	raw, _ := pick(holder).Load().(string)
	return strings.TrimSpace(raw)
}

// storeStringValue 只写入非空字符串，避免空值覆盖已有有效快照。
func storeStringValue(slot *atomic.Value, value string) {
	value = strings.TrimSpace(value)
	if slot == nil || value == "" {
		return
	}
	slot.Store(value)
}

// storeFirstTokenTime 只保留第一次响应时间，避免后续 chunk 覆盖 TTFB。
func storeFirstTokenTime(holder *requestMetadataHolder, ts time.Time) {
	if holder == nil || ts.IsZero() {
		return
	}
	holder.firstTokenUnixNano.CompareAndSwap(0, ts.UnixNano())
}

// requestTypeFromGin 读取 Gin 上现有的请求类型快照。
func requestTypeFromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	raw, exists := c.Get(RequestTypeContextKey)
	if !exists {
		return ""
	}
	requestType, _ := raw.(string)
	return normalizeRequestType(requestType)
}

// reasoningEffortFromGin 读取 Gin 上现有的 reasoning effort 快照。
func reasoningEffortFromGin(c *gin.Context) string {
	if c == nil {
		return ""
	}
	raw, exists := c.Get(RequestReasoningEffortContextKey)
	if !exists {
		return ""
	}
	effort, _ := raw.(string)
	return strings.TrimSpace(effort)
}

// apiResponseTimestampFromGin 读取 Gin 上现有的首次响应时间。
func apiResponseTimestampFromGin(c *gin.Context) time.Time {
	if c == nil {
		return time.Time{}
	}
	raw, exists := c.Get(APIResponseTimestampContextKey)
	if !exists {
		return time.Time{}
	}
	ts, _ := raw.(time.Time)
	return ts
}
