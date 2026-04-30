package amp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AmpRouteType represents the type of routing decision made for an Amp request
type AmpRouteType string

const (
	// RouteTypeLocalProvider indicates the request is handled by a local OAuth provider (free)
	RouteTypeLocalProvider AmpRouteType = "LOCAL_PROVIDER"
	// RouteTypeModelMapping indicates the request was remapped to another available model (free)
	RouteTypeModelMapping AmpRouteType = "MODEL_MAPPING"
	// RouteTypeAmpCredits indicates the request is forwarded to ampcode.com (uses Amp credits)
	RouteTypeAmpCredits AmpRouteType = "AMP_CREDITS"
	// RouteTypeNoProvider indicates no provider or fallback available
	RouteTypeNoProvider AmpRouteType = "NO_PROVIDER"
)

// MappedModelContextKey is the Gin context key for passing mapped model names.
const MappedModelContextKey = "mapped_model"

// logAmpRouting logs the routing decision for an Amp request with structured fields
func logAmpRouting(routeType AmpRouteType, requestedModel, resolvedModel, provider, path string) {
	fields := log.Fields{
		"component":       "amp-routing",
		"route_type":      string(routeType),
		"requested_model": requestedModel,
		"path":            path,
		"timestamp":       time.Now().Format(time.RFC3339),
	}

	if resolvedModel != "" && resolvedModel != requestedModel {
		fields["resolved_model"] = resolvedModel
	}
	if provider != "" {
		fields["provider"] = provider
	}

	switch routeType {
	case RouteTypeLocalProvider:
		fields["cost"] = "free"
		fields["source"] = "local_oauth"
		log.WithFields(fields).Debugf("amp using local provider for model: %s", requestedModel)

	case RouteTypeModelMapping:
		fields["cost"] = "free"
		fields["source"] = "local_oauth"
		fields["mapping"] = requestedModel + " -> " + resolvedModel
		// model mapping already logged in mapper; avoid duplicate here

	case RouteTypeAmpCredits:
		fields["cost"] = "amp_credits"
		fields["source"] = "ampcode.com"
		fields["model_id"] = requestedModel // Explicit model_id for easy config reference
		log.WithFields(fields).Warnf("forwarding to ampcode.com (uses amp credits) - model_id: %s | To use local provider, add to config: ampcode.model-mappings: [{from: \"%s\", to: \"<your-local-model>\"}]", requestedModel, requestedModel)

	case RouteTypeNoProvider:
		fields["cost"] = "none"
		fields["source"] = "error"
		fields["model_id"] = requestedModel // Explicit model_id for easy config reference
		log.WithFields(fields).Warnf("no provider available for model_id: %s", requestedModel)
	}
}

// FallbackHandler wraps a standard handler with fallback logic to ampcode.com
// when the model's provider is not available in CLIProxyAPI
type FallbackHandler struct {
	getProxy           func() *httputil.ReverseProxy
	modelMapper        ModelMapper
	forceModelMappings func() bool
	disableImages      func() bool
	disableImageMode   func() internalconfig.DisableImageGenerationMode
}

// NewFallbackHandler creates a new fallback handler wrapper
// The getProxy function allows lazy evaluation of the proxy (useful when proxy is created after routes)
func NewFallbackHandler(getProxy func() *httputil.ReverseProxy) *FallbackHandler {
	return &FallbackHandler{
		getProxy:           getProxy,
		forceModelMappings: func() bool { return false },
	}
}

// NewFallbackHandlerWithMapper creates a new fallback handler with model mapping support
func NewFallbackHandlerWithMapper(getProxy func() *httputil.ReverseProxy, mapper ModelMapper, forceModelMappings func() bool) *FallbackHandler {
	if forceModelMappings == nil {
		forceModelMappings = func() bool { return false }
	}
	return &FallbackHandler{
		getProxy:           getProxy,
		modelMapper:        mapper,
		forceModelMappings: forceModelMappings,
	}
}

// SetModelMapper sets the model mapper for this handler (allows late binding)
func (fh *FallbackHandler) SetModelMapper(mapper ModelMapper) {
	fh.modelMapper = mapper
}

// SetDisableImageGeneration 绑定全局图片禁用开关，保证 fallback 代理前也能拦截图片入口。
func (fh *FallbackHandler) SetDisableImageGeneration(disabled func() bool) {
	fh.disableImages = disabled
}

// SetDisableImageGenerationMode 绑定图片禁用三态配置，保证 fallback 代理语义与本地 executor 一致。
func (fh *FallbackHandler) SetDisableImageGenerationMode(mode func() internalconfig.DisableImageGenerationMode) {
	fh.disableImageMode = mode
}

// WrapHandler wraps a gin.HandlerFunc with fallback logic
// If the model's provider is not configured in CLIProxyAPI, it forwards to ampcode.com
func (fh *FallbackHandler) WrapHandler(handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestPath := c.Request.URL.Path
		disableMode := fh.imageGenerationMode()
		if disableMode == internalconfig.DisableImageGenerationAll && isAmpImagesRequestPath(requestPath) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		// Read the request body to extract the model name
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			log.Errorf("amp fallback: failed to read request body: %v", err)
			handler(c)
			return
		}

		// Sanitize request body: remove thinking blocks with invalid signatures
		// to prevent upstream API 400 errors.
		bodyBytes = SanitizeAmpRequestBody(bodyBytes)

		// Restore the body for the handler to read
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Try to extract model from request body or URL path (for Gemini)
		modelName := extractModelFromRequest(bodyBytes, c)
		if modelName == "" {
			// Can't determine model, proceed with normal handler
			handler(c)
			return
		}

		// Normalize model (handles dynamic thinking suffixes)
		suffixResult := thinking.ParseSuffix(modelName)
		normalizedModel := suffixResult.ModelName
		thinkingSuffix := ""
		if suffixResult.HasSuffix {
			thinkingSuffix = "(" + suffixResult.RawSuffix + ")"
		}

		resolveMappedModel := func() (string, []string) {
			if fh.modelMapper == nil {
				return "", nil
			}

			mappedModel := fh.modelMapper.MapModel(modelName)
			if mappedModel == "" {
				mappedModel = fh.modelMapper.MapModel(normalizedModel)
			}
			mappedModel = strings.TrimSpace(mappedModel)
			if mappedModel == "" {
				return "", nil
			}

			// Preserve dynamic thinking suffix (e.g. "(xhigh)") when mapping applies, unless the target
			// already specifies its own thinking suffix.
			if thinkingSuffix != "" {
				mappedSuffixResult := thinking.ParseSuffix(mappedModel)
				if !mappedSuffixResult.HasSuffix {
					mappedModel += thinkingSuffix
				}
			}

			mappedBaseModel := thinking.ParseSuffix(mappedModel).ModelName
			mappedProviders := util.GetProviderName(mappedBaseModel)
			if len(mappedProviders) == 0 {
				return "", nil
			}

			return mappedModel, mappedProviders
		}

		// Track resolved model for logging (may change if mapping is applied)
		resolvedModel := normalizedModel
		usedMapping := false
		var providers []string

		// Check if model mappings should be forced ahead of local API keys
		forceMappings := fh.forceModelMappings != nil && fh.forceModelMappings()

		if forceMappings {
			// FORCE MODE: Check model mappings FIRST (takes precedence over local API keys)
			// This allows users to route Amp requests to their preferred OAuth providers
			if mappedModel, mappedProviders := resolveMappedModel(); mappedModel != "" {
				// Mapping found and provider available - rewrite the model in request body
				bodyBytes = rewriteModelInRequest(bodyBytes, mappedModel)
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				// Store mapped model in context for handlers that check it (like gemini bridge)
				c.Set(MappedModelContextKey, mappedModel)
				resolvedModel = mappedModel
				usedMapping = true
				providers = mappedProviders
			}

			// If no mapping applied, check for local providers
			if !usedMapping {
				providers = util.GetProviderName(normalizedModel)
			}
		} else {
			// DEFAULT MODE: Check local providers first, then mappings as fallback
			providers = util.GetProviderName(normalizedModel)

			if len(providers) == 0 {
				// No providers configured - check if we have a model mapping
				if mappedModel, mappedProviders := resolveMappedModel(); mappedModel != "" {
					// Mapping found and provider available - rewrite the model in request body
					bodyBytes = rewriteModelInRequest(bodyBytes, mappedModel)
					c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					// Store mapped model in context for handlers that check it (like gemini bridge)
					c.Set(MappedModelContextKey, mappedModel)
					resolvedModel = mappedModel
					usedMapping = true
					providers = mappedProviders
				}
			}
		}

		// If no providers available, fallback to ampcode.com
		if len(providers) == 0 {
			proxy := fh.getProxy()
			if proxy != nil {
				// Log: Forwarding to ampcode.com (uses Amp credits)
				logAmpRouting(RouteTypeAmpCredits, modelName, "", "", requestPath)

				// Restore body again for the proxy
				proxyBody := applyAmpImageGenerationFallbackPolicy(bodyBytes, requestPath, disableMode)
				c.Request.Body = io.NopCloser(bytes.NewReader(proxyBody))
				c.Request.ContentLength = int64(len(proxyBody))

				// Forward to ampcode.com
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}

			// No proxy available, let the normal handler return the error
			logAmpRouting(RouteTypeNoProvider, modelName, "", "", requestPath)
		}

		// Log the routing decision
		providerName := ""
		if len(providers) > 0 {
			providerName = providers[0]
		}

		if usedMapping {
			// Log: Model was mapped to another model
			log.Debugf("amp model mapping: request %s -> %s", normalizedModel, resolvedModel)
			logAmpRouting(RouteTypeModelMapping, modelName, resolvedModel, providerName, requestPath)
			rewriter := NewResponseRewriter(c.Writer, modelName)
			rewriter.suppressThinking = true
			c.Writer = rewriter
			// Filter Anthropic-Beta header only for local handling paths
			filterAntropicBetaHeader(c)
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
			rewriter.Flush()
			log.Debugf("amp model mapping: response %s -> %s", resolvedModel, modelName)
		} else if len(providers) > 0 {
			// Log: Using local provider (free)
			logAmpRouting(RouteTypeLocalProvider, modelName, resolvedModel, providerName, requestPath)
			// 本地 provider 同样需要经过 rewriter，保证返回模型名与 Amp 期望字段一致。
			rewriter := NewResponseRewriter(c.Writer, modelName)
			rewriter.suppressThinking = providerName != "claude"
			c.Writer = rewriter
			// Filter Anthropic-Beta header only for local handling paths
			filterAntropicBetaHeader(c)
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
			rewriter.Flush()
		} else {
			// No provider, no mapping, no proxy: fall back to the wrapped handler so it can return an error response
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			handler(c)
		}
	}
}

// imageGenerationMode 返回当前图片禁用模式，兼容旧 bool 回调构造方式。
func (fh *FallbackHandler) imageGenerationMode() internalconfig.DisableImageGenerationMode {
	if fh == nil {
		return internalconfig.DisableImageGenerationOff
	}
	if fh.disableImageMode != nil {
		return fh.disableImageMode()
	}
	if fh.disableImages != nil && fh.disableImages() {
		return internalconfig.DisableImageGenerationAll
	}
	return internalconfig.DisableImageGenerationOff
}

// isAmpImagesRequestPath 识别 AMP provider alias 下的 OpenAI Images 入口。
func isAmpImagesRequestPath(path string) bool {
	path = strings.TrimSpace(path)
	return strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/images/edits")
}

// applyAmpImageGenerationFallbackPolicy 在 upstream fallback 前执行图片禁用策略，避免绕过本地 executor。
func applyAmpImageGenerationFallbackPolicy(body []byte, path string, mode internalconfig.DisableImageGenerationMode) []byte {
	switch mode {
	case internalconfig.DisableImageGenerationAll:
		return removeAmpImageGenerationTools(body)
	case internalconfig.DisableImageGenerationChat:
		if isAmpImagesRequestPath(path) {
			return body
		}
		return removeAmpImageGenerationTools(body)
	default:
		return body
	}
}

// removeAmpImageGenerationTools 清理 OpenAI 兼容请求中的 image_generation 工具与 tool_choice。
func removeAmpImageGenerationTools(body []byte) []byte {
	out := removeAmpImageGenerationToolsArray(body, "tools")
	out = removeAmpImageGenerationToolsArray(out, "tool_choice.tools")
	out = removeAmpEmptyAllowedToolsChoice(out)
	return removeAmpImageGenerationToolChoice(out)
}

// removeAmpImageGenerationToolsArray 删除指定 tools 数组里的 image_generation 条目。
func removeAmpImageGenerationToolsArray(body []byte, path string) []byte {
	tools := gjson.GetBytes(body, path)
	if !tools.IsArray() {
		return body
	}
	rawItems := make([]string, 0, len(tools.Array()))
	removed := false
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
			removed = true
			continue
		}
		rawItems = append(rawItems, ampJSONRaw(tool))
	}
	if !removed {
		return body
	}
	if len(rawItems) == 0 {
		updated, errDel := sjson.DeleteBytes(body, path)
		if errDel == nil {
			return updated
		}
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, path, []byte("["+strings.Join(rawItems, ",")+"]"))
	if errSet != nil {
		return body
	}
	return updated
}

// removeAmpEmptyAllowedToolsChoice 删除已被清空的 allowed_tools 限制对象，避免上游收到无效请求形状。
func removeAmpEmptyAllowedToolsChoice(body []byte) []byte {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() || strings.TrimSpace(choice.Get("type").String()) != "allowed_tools" {
		return body
	}
	tools := choice.Get("tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		return body
	}
	updated, errDel := sjson.DeleteBytes(body, "tool_choice")
	if errDel != nil {
		return body
	}
	return updated
}

// removeAmpImageGenerationToolChoice 删除直接要求使用 image_generation 的 tool_choice。
func removeAmpImageGenerationToolChoice(body []byte) []byte {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.Exists() {
		return body
	}
	if choice.Type == gjson.String {
		return deleteAmpImageGenerationStringChoice(body, choice)
	}
	if !choice.IsObject() {
		return body
	}
	return deleteAmpImageGenerationObjectChoice(body, choice)
}

// deleteAmpImageGenerationStringChoice 处理字符串形式的 tool_choice。
func deleteAmpImageGenerationStringChoice(body []byte, choice gjson.Result) []byte {
	if !strings.EqualFold(strings.TrimSpace(choice.String()), "image_generation") {
		return body
	}
	updated, errDel := sjson.DeleteBytes(body, "tool_choice")
	if errDel != nil {
		return body
	}
	return updated
}

// deleteAmpImageGenerationObjectChoice 处理对象形式的 tool_choice。
func deleteAmpImageGenerationObjectChoice(body []byte, choice gjson.Result) []byte {
	choiceType := strings.TrimSpace(choice.Get("type").String())
	name := strings.TrimSpace(choice.Get("name").String())
	matchesTool := strings.EqualFold(choiceType, "tool") && strings.EqualFold(name, "image_generation")
	if !strings.EqualFold(choiceType, "image_generation") && !matchesTool {
		return body
	}
	updated, errDel := sjson.DeleteBytes(body, "tool_choice")
	if errDel != nil {
		return body
	}
	return updated
}

// ampJSONRaw 返回 gjson 节点原始 JSON，用于重建 tools 数组时保持原字段不变。
func ampJSONRaw(result gjson.Result) string {
	if result.Raw != "" {
		return result.Raw
	}
	raw, err := json.Marshal(result.Value())
	if err != nil {
		return "null"
	}
	return string(raw)
}

// filterAntropicBetaHeader filters Anthropic-Beta header to remove features requiring special subscription
// This is needed when using local providers (bypassing the Amp proxy)
func filterAntropicBetaHeader(c *gin.Context) {
	if betaHeader := c.Request.Header.Get("Anthropic-Beta"); betaHeader != "" {
		if filtered := filterBetaFeatures(betaHeader, "context-1m-2025-08-07"); filtered != "" {
			c.Request.Header.Set("Anthropic-Beta", filtered)
		} else {
			c.Request.Header.Del("Anthropic-Beta")
		}
	}
}

// rewriteModelInRequest replaces the model name in a JSON request body
func rewriteModelInRequest(body []byte, newModel string) []byte {
	if !gjson.GetBytes(body, "model").Exists() {
		return body
	}
	result, err := sjson.SetBytes(body, "model", newModel)
	if err != nil {
		log.Warnf("amp model mapping: failed to rewrite model in request body: %v", err)
		return body
	}
	return result
}

// extractModelFromRequest attempts to extract the model name from various request formats
func extractModelFromRequest(body []byte, c *gin.Context) string {
	// First try to parse from JSON body (OpenAI, Claude, etc.)
	// Check common model field names
	if result := gjson.GetBytes(body, "model"); result.Exists() && result.Type == gjson.String {
		return result.String()
	}

	// For Gemini requests, model is in the URL path
	// Standard format: /models/{model}:generateContent -> :action parameter
	if action := c.Param("action"); action != "" {
		// Split by colon to get model name (e.g., "gemini-pro:generateContent" -> "gemini-pro")
		parts := strings.Split(action, ":")
		if len(parts) > 0 && parts[0] != "" {
			return parts[0]
		}
	}

	// AMP CLI format: /publishers/google/models/{model}:method -> *path parameter
	// Example: /publishers/google/models/gemini-3-pro-preview:streamGenerateContent
	if path := c.Param("path"); path != "" {
		// Look for /models/{model}:method pattern
		if idx := strings.Index(path, "/models/"); idx >= 0 {
			modelPart := path[idx+8:] // Skip "/models/"
			// Split by colon to get model name
			if colonIdx := strings.Index(modelPart, ":"); colonIdx > 0 {
				return modelPart[:colonIdx]
			}
		}
	}

	return ""
}
