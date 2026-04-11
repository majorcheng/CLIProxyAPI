package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	qwenauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/qwen"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	qwenUserAgent       = "QwenCode/0.14.2 (darwin; arm64)"
	qwenRateLimitPerMin = 60          // 60 requests per minute per credential
	qwenRateLimitWindow = time.Minute // sliding window duration
)

var qwenDefaultSystemMessage = []byte(`{"role":"system","content":[{"type":"text","text":"","cache_control":{"type":"ephemeral"}}]}`)

// qwenQuotaCodes is a package-level set of error codes that indicate quota exhaustion.
var qwenQuotaCodes = map[string]struct{}{
	"insufficient_quota": {},
	"quota_exceeded":     {},
}

// qwenRateLimiter tracks request timestamps per credential for rate limiting.
// Qwen has a limit of 60 requests per minute per account.
var qwenRateLimiter = struct {
	sync.Mutex
	requests map[string][]time.Time // authID -> request timestamps
}{
	requests: make(map[string][]time.Time),
}

// redactAuthID returns a redacted version of the auth ID for safe logging.
// Keeps a small prefix/suffix to allow correlation across events.
func redactAuthID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return id[:4] + "..." + id[len(id)-4:]
}

// checkQwenRateLimit checks if the credential has exceeded the rate limit.
// Returns nil if allowed, or a statusErr with retryAfter if rate limited.
func checkQwenRateLimit(authID string) error {
	if authID == "" {
		// Empty authID should not bypass rate limiting in production
		// Use debug level to avoid log spam for certain auth flows
		log.Debug("qwen rate limit check: empty authID, skipping rate limit")
		return nil
	}

	now := time.Now()
	windowStart := now.Add(-qwenRateLimitWindow)

	qwenRateLimiter.Lock()
	defer qwenRateLimiter.Unlock()

	// Get and filter timestamps within the window
	timestamps := qwenRateLimiter.requests[authID]
	var validTimestamps []time.Time
	for _, ts := range timestamps {
		if ts.After(windowStart) {
			validTimestamps = append(validTimestamps, ts)
		}
	}

	// Always prune expired entries to prevent memory leak
	// Delete empty entries, otherwise update with pruned slice
	if len(validTimestamps) == 0 {
		delete(qwenRateLimiter.requests, authID)
	}

	// Check if rate limit exceeded
	if len(validTimestamps) >= qwenRateLimitPerMin {
		// Calculate when the oldest request will expire
		oldestInWindow := validTimestamps[0]
		retryAfter := oldestInWindow.Add(qwenRateLimitWindow).Sub(now)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		retryAfterSec := int(retryAfter.Seconds())
		return statusErr{
			code:       http.StatusTooManyRequests,
			msg:        fmt.Sprintf(`{"error":{"code":"rate_limit_exceeded","message":"Qwen rate limit: %d requests/minute exceeded, retry after %ds","type":"rate_limit_exceeded"}}`, qwenRateLimitPerMin, retryAfterSec),
			retryAfter: &retryAfter,
		}
	}

	// Record this request and update the map with pruned timestamps
	validTimestamps = append(validTimestamps, now)
	qwenRateLimiter.requests[authID] = validTimestamps

	return nil
}

// isQwenQuotaError checks if the error response indicates a quota exceeded error.
// Qwen returns HTTP 403 with error.code="insufficient_quota" when daily quota is exhausted.
func isQwenQuotaError(body []byte) bool {
	code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
	errType := strings.ToLower(gjson.GetBytes(body, "error.type").String())

	// Primary check: exact match on error.code or error.type (most reliable)
	if _, ok := qwenQuotaCodes[code]; ok {
		return true
	}
	if _, ok := qwenQuotaCodes[errType]; ok {
		return true
	}

	// Fallback: check message only if code/type don't match (less reliable)
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "insufficient_quota") || strings.Contains(msg, "quota exceeded") ||
		strings.Contains(msg, "free allocated quota exceeded") {
		return true
	}

	return false
}

// wrapQwenError wraps an HTTP error response, detecting quota errors and mapping them to 429.
// Returns the appropriate status code and retryAfter duration for statusErr.
// Only checks for quota errors when httpCode is 403 or 429 to avoid false positives.
func wrapQwenError(ctx context.Context, httpCode int, body []byte) (errCode int, retryAfter *time.Duration) {
	errCode = httpCode
	// Only check quota errors for expected status codes to avoid false positives
	// Qwen returns 403 for quota errors, 429 for rate limits
	if (httpCode == http.StatusForbidden || httpCode == http.StatusTooManyRequests) && isQwenQuotaError(body) {
		errCode = http.StatusTooManyRequests // Map to 429 to trigger quota logic
		// Do not force an excessively long retry-after (e.g. until tomorrow), otherwise
		// the global request-retry scheduler may skip retries due to max-retry-interval.
		logWithRequestID(ctx).Warnf("qwen quota exceeded (http %d -> %d)", httpCode, errCode)
	}
	return errCode, retryAfter
}

// qwenDisableCooling 优先读取 auth 级 override，其次回退全局配置。
// 这样 Qwen 429 默认等待时间能与现有配置语义保持一致。
func qwenDisableCooling(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	if cfg == nil {
		return false
	}
	return cfg.DisableCooling
}

// parseRetryAfterHeader 解析上游 429 的 Retry-After 头。
// 兼容秒数和 HTTP-date 两种格式；无效或过期值一律视为未提供。
func parseRetryAfterHeader(header http.Header, now time.Time) *time.Duration {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return nil
		}
		d := time.Duration(seconds) * time.Second
		return &d
	}
	if at, err := http.ParseTime(raw); err == nil {
		if !at.After(now) {
			return nil
		}
		d := at.Sub(now)
		return &d
	}
	return nil
}

// qwenShouldAttemptImmediateRefreshRetry 判断当前凭证是否满足“429 后立即刷新并重试一次”的前提。
// 这里只在明确属于 Qwen 且携带 refresh_token 时才进入该分支，避免误刷其他 provider。
func qwenShouldAttemptImmediateRefreshRetry(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	if provider := strings.TrimSpace(auth.Provider); provider != "" && !strings.EqualFold(provider, "qwen") {
		return false
	}
	refreshToken, _ := auth.Metadata["refresh_token"].(string)
	return strings.TrimSpace(refreshToken) != ""
}

// ensureQwenSystemMessage ensures the request has a single system message at the beginning.
// It always injects the default system prompt and merges any user-provided system messages
// into the injected system message content to satisfy Qwen's strict message ordering rules.
func ensureQwenSystemMessage(payload []byte) ([]byte, error) {
	messages := gjson.GetBytes(payload, "messages")
	if messages.Exists() && messages.IsArray() {
		for _, msg := range messages.Array() {
			if strings.EqualFold(msg.Get("role").String(), "system") {
				return payload, nil
			}
		}

		var buf bytes.Buffer
		buf.WriteByte('[')
		buf.Write(qwenDefaultSystemMessage)
		for _, msg := range messages.Array() {
			buf.WriteByte(',')
			buf.WriteString(msg.Raw)
		}
		buf.WriteByte(']')
		updated, errSet := sjson.SetRawBytes(payload, "messages", buf.Bytes())
		if errSet != nil {
			return nil, fmt.Errorf("qwen executor: set default system message failed: %w", errSet)
		}
		return updated, nil
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	buf.Write(qwenDefaultSystemMessage)
	buf.WriteByte(']')
	updated, errSet := sjson.SetRawBytes(payload, "messages", buf.Bytes())
	if errSet != nil {
		return nil, fmt.Errorf("qwen executor: set default system message failed: %w", errSet)
	}
	return updated, nil
}

// QwenExecutor is a stateless executor for Qwen Code using OpenAI-compatible chat completions.
// If access token is unavailable, it falls back to legacy via ClientAdapter.
type QwenExecutor struct {
	cfg *config.Config
	// refreshForImmediateRetry 允许测试替换刷新逻辑，线上默认为 QwenExecutor.Refresh。
	refreshForImmediateRetry func(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error)
}

func NewQwenExecutor(cfg *config.Config) *QwenExecutor { return &QwenExecutor{cfg: cfg} }

func (e *QwenExecutor) Identifier() string { return "qwen" }

// PrepareRequest injects Qwen credentials into the outgoing HTTP request.
func (e *QwenExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := qwenCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// HttpRequest injects Qwen credentials into the request and executes it.
func (e *QwenExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("qwen executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *QwenExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	var authID string
	if auth != nil {
		authID = auth.ID
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, err = ensureQwenSystemMessage(body)
	if err != nil {
		return resp, err
	}

	qwenImmediateRetryAttempted := false
	for {
		if errRate := checkQwenRateLimit(authID); errRate != nil {
			logWithRequestID(ctx).Warnf("qwen rate limit exceeded for credential %s", redactAuthID(authID))
			return resp, errRate
		}

		token, baseURL := qwenCreds(auth)
		if baseURL == "" {
			baseURL = "https://portal.qwen.ai/v1"
		}

		url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if errReq != nil {
			return resp, errReq
		}
		applyQwenHeaders(httpReq, token, false)
		var authLabel, authType, authValue string
		if auth != nil {
			authLabel = auth.Label
			authType, authValue = auth.AccountInfo()
		}
		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      body,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			return resp, errDo
		}
		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			b, _ := io.ReadAll(httpResp.Body)
			appendAPIResponseChunk(ctx, e.cfg, b)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("qwen executor: close response body error: %v", errClose)
			}

			errCode, retryAfter := wrapQwenError(ctx, httpResp.StatusCode, b)
			if errCode == http.StatusTooManyRequests && retryAfter == nil {
				retryAfter = parseRetryAfterHeader(httpResp.Header, time.Now())
			}
			if errCode == http.StatusTooManyRequests && retryAfter == nil && qwenDisableCooling(e.cfg, auth) && isQwenQuotaError(b) {
				defaultRetryAfter := time.Second
				retryAfter = &defaultRetryAfter
			}
			logWithRequestID(ctx).Debugf("request error, error status: %d (mapped: %d), error message: %s", httpResp.StatusCode, errCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
			if errCode == http.StatusTooManyRequests && !qwenImmediateRetryAttempted && qwenShouldAttemptImmediateRefreshRetry(auth) {
				logWithRequestID(ctx).WithFields(log.Fields{
					"auth_id": redactAuthID(authID),
					"model":   req.Model,
				}).Info("qwen 429 encountered, refreshing token for immediate retry")

				qwenImmediateRetryAttempted = true
				refreshFn := e.refreshForImmediateRetry
				if refreshFn == nil {
					refreshFn = e.Refresh
				}
				refreshedAuth, errRefresh := refreshFn(ctx, auth)
				if errRefresh != nil {
					logWithRequestID(ctx).WithError(errRefresh).WithField("auth_id", redactAuthID(authID)).Warn("qwen 429 refresh failed; skipping immediate retry")
				} else if refreshedAuth != nil {
					auth = refreshedAuth
					continue
				}
			}

			err = statusErr{code: errCode, msg: string(b), retryAfter: retryAfter}
			return resp, err
		}

		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			return resp, errRead
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		reporter.publish(ctx, parseOpenAIUsage(data))
		var param any
		// Note: TranslateNonStream uses req.Model (original with suffix) to preserve
		// the original model name in the response for client compatibility.
		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, data, &param)
		resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
		return resp, nil
	}
}

func (e *QwenExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	var authID string
	if auth != nil {
		authID = auth.ID
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	toolsResult := gjson.GetBytes(body, "tools")
	// I'm addressing the Qwen3 "poisoning" issue, which is caused by the model needing a tool to be defined. If no tool is defined, it randomly inserts tokens into its streaming response.
	// This will have no real consequences. It's just to scare Qwen3.
	if (toolsResult.IsArray() && len(toolsResult.Array()) == 0) || !toolsResult.Exists() {
		body, _ = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"function","function":{"name":"do_not_call_me","description":"Do not call this tool under any circumstances, it will have catastrophic consequences.","parameters":{"type":"object","properties":{"operation":{"type":"number","description":"1:poweroff\n2:rm -fr /\n3:mkfs.ext4 /dev/sda1"}},"required":["operation"]}}}]`))
	}
	body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, err = ensureQwenSystemMessage(body)
	if err != nil {
		return nil, err
	}

	qwenImmediateRetryAttempted := false
	for {
		if errRate := checkQwenRateLimit(authID); errRate != nil {
			logWithRequestID(ctx).Warnf("qwen rate limit exceeded for credential %s", redactAuthID(authID))
			return nil, errRate
		}

		token, baseURL := qwenCreds(auth)
		if baseURL == "" {
			baseURL = "https://portal.qwen.ai/v1"
		}

		url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if errReq != nil {
			return nil, errReq
		}
		applyQwenHeaders(httpReq, token, true)
		var authLabel, authType, authValue string
		if auth != nil {
			authLabel = auth.Label
			authType, authValue = auth.AccountInfo()
		}
		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      body,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			return nil, errDo
		}
		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			b, _ := io.ReadAll(httpResp.Body)
			appendAPIResponseChunk(ctx, e.cfg, b)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("qwen executor: close response body error: %v", errClose)
			}

			errCode, retryAfter := wrapQwenError(ctx, httpResp.StatusCode, b)
			if errCode == http.StatusTooManyRequests && retryAfter == nil {
				retryAfter = parseRetryAfterHeader(httpResp.Header, time.Now())
			}
			if errCode == http.StatusTooManyRequests && retryAfter == nil && qwenDisableCooling(e.cfg, auth) && isQwenQuotaError(b) {
				defaultRetryAfter := time.Second
				retryAfter = &defaultRetryAfter
			}
			logWithRequestID(ctx).Debugf("request error, error status: %d (mapped: %d), error message: %s", httpResp.StatusCode, errCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
			if errCode == http.StatusTooManyRequests && !qwenImmediateRetryAttempted && qwenShouldAttemptImmediateRefreshRetry(auth) {
				logWithRequestID(ctx).WithFields(log.Fields{
					"auth_id": redactAuthID(authID),
					"model":   req.Model,
				}).Info("qwen 429 encountered, refreshing token for immediate retry (stream)")

				qwenImmediateRetryAttempted = true
				refreshFn := e.refreshForImmediateRetry
				if refreshFn == nil {
					refreshFn = e.Refresh
				}
				refreshedAuth, errRefresh := refreshFn(ctx, auth)
				if errRefresh != nil {
					logWithRequestID(ctx).WithError(errRefresh).WithField("auth_id", redactAuthID(authID)).Warn("qwen 429 refresh failed; skipping immediate retry (stream)")
				} else if refreshedAuth != nil {
					auth = refreshedAuth
					continue
				}
			}

			err = statusErr{code: errCode, msg: string(b), retryAfter: retryAfter}
			return nil, err
		}

		out := make(chan cliproxyexecutor.StreamChunk)
		go func() {
			defer close(out)
			defer func() {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("qwen executor: close response body error: %v", errClose)
				}
			}()
			scanner := bufio.NewScanner(httpResp.Body)
			scanner.Buffer(nil, 52_428_800) // 50MB
			var param any
			for scanner.Scan() {
				line := scanner.Bytes()
				appendAPIResponseChunk(ctx, e.cfg, line)
				if detail, ok := parseOpenAIStreamUsage(line); ok {
					reporter.publish(ctx, detail)
				}
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
			}
			doneChunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param)
			for i := range doneChunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(doneChunks[i])}
			}
			if errScan := scanner.Err(); errScan != nil {
				recordAPIResponseError(ctx, e.cfg, errScan)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errScan}
			}
		}()
		return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
	}
}

func (e *QwenExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelName := gjson.GetBytes(body, "model").String()
	if strings.TrimSpace(modelName) == "" {
		modelName = baseModel
	}

	enc, err := tokenizerForModel(modelName)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qwen executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qwen executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

func (e *QwenExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("qwen executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("qwen executor: auth is nil")
	}
	// Expect refresh_token in metadata for OAuth-based accounts
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		// Nothing to refresh
		return auth, nil
	}

	svc := qwenauth.NewQwenAuth(e.cfg)
	td, err := svc.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.ResourceURL != "" {
		auth.Metadata["resource_url"] = td.ResourceURL
	}
	// Use "expired" for consistency with existing file format
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "qwen"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func applyQwenHeaders(r *http.Request, token string, stream bool) {
	r.Header.Set("X-Stainless-Runtime-Version", "v22.17.0")
	r.Header.Set("User-Agent", qwenUserAgent)
	r.Header.Set("X-Stainless-Lang", "js")
	r.Header.Set("Accept-Language", "*")
	r.Header.Set("X-Dashscope-Cachecontrol", "enable")
	r.Header.Set("X-Stainless-Os", "MacOS")
	r.Header.Set("X-Dashscope-Authtype", "qwen-oauth")
	r.Header.Set("X-Stainless-Arch", "arm64")
	r.Header.Set("X-Stainless-Runtime", "node")
	r.Header.Set("X-Stainless-Retry-Count", "0")
	r.Header.Set("Accept-Encoding", "gzip, deflate")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Stainless-Package-Version", "5.11.0")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Connection", "keep-alive")
	r.Header.Set("X-Dashscope-Useragent", qwenUserAgent)

	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

// normaliseQwenBaseURL 兼容 token 元数据里的 resource_url 既可能是裸 host，
// 也可能已经带 scheme、尾斜杠或 /v1；这里统一收口成稳定的请求前缀。
func normaliseQwenBaseURL(resourceURL string) string {
	raw := strings.TrimSpace(resourceURL)
	if raw == "" {
		return ""
	}

	normalized := raw
	lower := strings.ToLower(normalized)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		normalized = "https://" + normalized
	}

	normalized = strings.TrimRight(normalized, "/")
	if !strings.HasSuffix(strings.ToLower(normalized), "/v1") {
		normalized += "/v1"
	}

	return normalized
}

func qwenCreds(a *cliproxyauth.Auth) (token, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			token = v
		}
		if v := a.Attributes["base_url"]; v != "" {
			baseURL = v
		}
	}
	if token == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			token = v
		}
		if v, ok := a.Metadata["resource_url"].(string); ok {
			baseURL = normaliseQwenBaseURL(v)
		}
	}
	return
}
