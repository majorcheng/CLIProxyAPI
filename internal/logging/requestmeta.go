package logging

import (
	"context"
	"sync/atomic"
)

type endpointKey struct{}
type responseStatusKey struct{}

type responseStatusHolder struct {
	status atomic.Int32
}

// WithEndpoint 将稳定的入站端点快照写入 context，避免异步统计读取已复用的 Gin context。
func WithEndpoint(ctx context.Context, endpoint string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, endpointKey{}, endpoint)
}

// GetEndpoint 从 context 读取稳定的入站端点快照。
func GetEndpoint(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	endpoint, ok := ctx.Value(endpointKey{}).(string)
	if !ok {
		return ""
	}
	return endpoint
}

// WithResponseStatusHolder 为请求创建可后置写入的响应状态 holder。
func WithResponseStatusHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseStatusKey{}, &responseStatusHolder{})
}

// SetResponseStatus 在响应结束时记录最终 HTTP 状态，供异步 usage 插件读取。
func SetResponseStatus(ctx context.Context, status int) {
	if ctx == nil || status <= 0 {
		return
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return
	}
	holder.status.Store(int32(status))
}

// GetResponseStatus 返回已记录的最终 HTTP 状态；0 表示尚未记录。
func GetResponseStatus(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return 0
	}
	return int(holder.status.Load())
}
