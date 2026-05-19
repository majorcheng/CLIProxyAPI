package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	openAICompatImageHandlerType            = "openai-image"
	openAICompatImagesGenerationsPath       = "/images/generations"
	openAICompatImagesEditsPath             = "/images/edits"
	openAICompatDefaultImageEndpoint        = openAICompatImagesGenerationsPath
	openAICompatMultipartMemory       int64 = 32 << 20
)

type openAICompatImageRequestInput struct {
	ctx          context.Context
	auth         *cliproxyauth.Auth
	req          cliproxyexecutor.Request
	opts         cliproxyexecutor.Options
	endpointPath string
	stream       bool
}

// executeImages 向 OpenAI-compatible Images 非流式入口转发请求，并原样返回上游响应体。
func (e *OpenAICompatExecutor) executeImages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	httpReq, errBuild := e.buildOpenAICompatImageRequest(openAICompatImageRequestInput{
		ctx: ctx, auth: auth, req: req, opts: opts, endpointPath: endpointPath,
	})
	if errBuild != nil {
		err = errBuild
		return resp, err
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		err = errDo
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	body, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		recordAPIResponseError(ctx, e.cfg, errRead)
		err = errRead
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		e.logOpenAICompatImageStatusError(ctx, httpResp, body)
		err = statusErr{code: httpResp.StatusCode, msg: string(body)}
		return resp, err
	}

	reporter.publish(ctx, parseOpenAIUsage(body))
	reporter.ensurePublished(ctx)
	return cliproxyexecutor.Response{Payload: body, Headers: httpResp.Header.Clone()}, nil
}

// executeImagesStream 向 OpenAI-compatible Images 流式入口转发请求，并原样透传 SSE chunk。
func (e *OpenAICompatExecutor) executeImagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointPath string) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	httpReq, errBuild := e.buildOpenAICompatImageRequest(openAICompatImageRequestInput{
		ctx: ctx, auth: auth, req: req, opts: opts, endpointPath: endpointPath, stream: true,
	})
	if errBuild != nil {
		err = errBuild
		return nil, err
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, e.readOpenAICompatImageError(ctx, httpResp)
	}

	return &cliproxyexecutor.StreamResult{
		Headers: httpResp.Header.Clone(),
		Chunks:  e.streamOpenAICompatImageBody(ctx, httpResp.Body, reporter),
	}, nil
}

// buildOpenAICompatImageRequest 构造兼容图片上游请求，并记录最终发出的 payload。
func (e *OpenAICompatExecutor) buildOpenAICompatImageRequest(input openAICompatImageRequestInput) (*http.Request, error) {
	baseModel := thinking.ParseSuffix(input.req.Model).ModelName
	baseURL, apiKey := e.resolveCredentials(input.auth)
	if baseURL == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
	}
	payload, contentType, errPrepare := prepareOpenAICompatImagesPayload(input.req.Payload, baseModel, input.opts.Headers.Get("Content-Type"), input.stream)
	if errPrepare != nil {
		return nil, errPrepare
	}
	if contentType == "" {
		contentType = "application/json"
	}
	url := strings.TrimSuffix(baseURL, "/") + input.endpointPath
	httpReq, errReq := http.NewRequestWithContext(input.ctx, http.MethodPost, url, bytes.NewReader(payload))
	if errReq != nil {
		return nil, errReq
	}
	prepareOpenAICompatImageRequest(httpReq, apiKey, input.auth, contentType, input.stream)
	recordOpenAICompatImageRequest(input.ctx, e, input.auth, httpReq, payload)
	return httpReq, nil
}

// readOpenAICompatImageError 读取错误响应体并关闭 body，返回带状态码的上游错误。
func (e *OpenAICompatExecutor) readOpenAICompatImageError(ctx context.Context, httpResp *http.Response) error {
	body, errRead := io.ReadAll(httpResp.Body)
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("openai compat executor: close response body error: %v", errClose)
	}
	if errRead != nil {
		recordAPIResponseError(ctx, e.cfg, errRead)
		return errRead
	}
	appendAPIResponseChunk(ctx, e.cfg, body)
	e.logOpenAICompatImageStatusError(ctx, httpResp, body)
	return statusErr{code: httpResp.StatusCode, msg: string(body)}
}

// streamOpenAICompatImageBody 把上游 body 转成 executor stream chunk，并负责关闭 body。
func (e *OpenAICompatExecutor) streamOpenAICompatImageBody(ctx context.Context, body io.ReadCloser, reporter *usageReporter) <-chan cliproxyexecutor.StreamChunk {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
			reporter.ensurePublished(ctx)
		}()
		buffer := make([]byte, 32*1024)
		for {
			n, errRead := body.Read(buffer)
			if n > 0 {
				chunk := bytes.Clone(buffer[:n])
				appendAPIResponseChunk(ctx, e.cfg, chunk)
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunk}) {
					return
				}
			}
			if errRead != nil {
				if errRead != io.EOF {
					recordAPIResponseError(ctx, e.cfg, errRead)
					reporter.publishFailure(ctx)
					_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errRead})
				}
				return
			}
		}
	}()
	return out
}

// logOpenAICompatImageStatusError 用统一格式记录兼容图片上游非 2xx 响应。
func (e *OpenAICompatExecutor) logOpenAICompatImageStatusError(ctx context.Context, httpResp *http.Response, body []byte) {
	logWithRequestID(ctx).Debugf(
		"request error, error status: %d, error message: %s",
		httpResp.StatusCode,
		summarizeErrorBody(httpResp.Header.Get("Content-Type"), body),
	)
}

// openAICompatImageEndpointPath 根据 handler 类型和下游路径选择上游 Images 子路径。
func openAICompatImageEndpointPath(opts cliproxyexecutor.Options) string {
	if opts.SourceFormat.String() != openAICompatImageHandlerType {
		return ""
	}
	path := payloadRequestPath(opts)
	if strings.HasSuffix(path, "/images/edits") {
		return openAICompatImagesEditsPath
	}
	if strings.HasSuffix(path, "/images/generations") {
		return openAICompatImagesGenerationsPath
	}
	return openAICompatDefaultImageEndpoint
}

// prepareOpenAICompatImageRequest 写入兼容图片请求必需 header 与 provider 自定义 header。
func prepareOpenAICompatImageRequest(req *http.Request, apiKey string, auth *cliproxyauth.Auth, contentType string, stream bool) {
	req.Header.Set("Content-Type", contentType)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
}

// recordOpenAICompatImageRequest 记录兼容图片上游请求，便于 request log 还原真实转发 payload。
func recordOpenAICompatImageRequest(ctx context.Context, e *OpenAICompatExecutor, auth *cliproxyauth.Auth, req *http.Request, payload []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       req.URL.String(),
		Method:    http.MethodPost,
		Headers:   req.Header.Clone(),
		Body:      payload,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}
