package openai

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

const (
	imagesGenerationsPath = "/v1/images/generations"
	imagesEditsPath       = "/v1/images/edits"
)

// rejectUnsupportedImagesModel 在 handler 入口优先拒绝不支持的图片模型，
// 保证 `/v1/images/*` 先返回 model 错误，而不是先落到 prompt/image 等字段校验。
func rejectUnsupportedImagesModel(c *gin.Context, endpointPath string, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		model = defaultImagesToolModel
	}
	if isSupportedImagesToolModel(model) {
		return false
	}
	writeInvalidRequestError(c, fmt.Sprintf(
		"Invalid request: model %q is not supported for %s (only %s is supported)",
		model,
		endpointPath,
		defaultImagesToolModel,
	))
	return true
}

// ImagesGenerations 处理 `/v1/images/generations`，并桥接到 Codex Responses 图片调用链。
func (h *OpenAIAPIHandler) ImagesGenerations(c *gin.Context) {
	if h.rejectDisabledImageGeneration(c) {
		return
	}
	rawJSON, ok := readValidJSONBody(c)
	if !ok {
		return
	}
	if rejectUnsupportedImagesModel(c, imagesGenerationsPath, gjson.GetBytes(rawJSON, "model").String()) {
		return
	}
	payload, err := decodeImagesGenerationsRequest(rawJSON)
	if err != nil {
		writeInvalidRequestError(c, err.Error())
		return
	}
	h.executeImagesRequest(c, payload)
}

// ImagesEdits 处理 `/v1/images/edits`，同时兼容 JSON 与 multipart/form-data。
func (h *OpenAIAPIHandler) ImagesEdits(c *gin.Context) {
	if h.rejectDisabledImageGeneration(c) {
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	switch {
	case strings.HasPrefix(contentType, "application/json"):
		h.imagesEditsFromJSON(c)
	case strings.HasPrefix(contentType, "multipart/form-data"), contentType == "":
		h.imagesEditsFromMultipart(c)
	default:
		writeInvalidRequestError(c, fmt.Sprintf("Invalid request: unsupported Content-Type %q", contentType))
	}
}

// rejectDisabledImageGeneration 在全局禁用图片能力时让图片入口表现为不存在，避免继续解析请求体。
func (h *OpenAIAPIHandler) rejectDisabledImageGeneration(c *gin.Context) bool {
	if h == nil || h.BaseAPIHandler == nil || h.BaseAPIHandler.Cfg == nil {
		return false
	}
	if !h.BaseAPIHandler.Cfg.DisableImageGeneration {
		return false
	}
	c.AbortWithStatus(http.StatusNotFound)
	return true
}

// imagesEditsFromJSON 负责 JSON 版编辑接口的参数校验与执行。
func (h *OpenAIAPIHandler) imagesEditsFromJSON(c *gin.Context) {
	rawJSON, ok := readValidJSONBody(c)
	if !ok {
		return
	}
	if rejectUnsupportedImagesModel(c, imagesEditsPath, gjson.GetBytes(rawJSON, "model").String()) {
		return
	}
	payload, err := decodeImagesEditsJSONRequest(rawJSON)
	if err != nil {
		writeInvalidRequestError(c, err.Error())
		return
	}
	h.executeImagesRequest(c, payload)
}

// imagesEditsFromMultipart 负责 multipart 版编辑接口的参数校验与执行。
func (h *OpenAIAPIHandler) imagesEditsFromMultipart(c *gin.Context) {
	form, err := imagesMultipartFormWithLimit(c)
	if err != nil {
		writeInvalidRequestError(c, err.Error())
		return
	}
	if rejectUnsupportedImagesModel(c, imagesEditsPath, firstMultipartFormValue(form, "model")) {
		return
	}
	payload, err := decodeImagesEditsMultipartRequest(c)
	if err != nil {
		writeInvalidRequestError(c, err.Error())
		return
	}
	h.executeImagesRequest(c, payload)
}

// executeImagesRequest 统一完成 Responses 请求构造，并根据 stream 选择返回路径。
func (h *OpenAIAPIHandler) executeImagesRequest(c *gin.Context, payload imagesRequestPayload) {
	responsesReq, err := buildImagesResponsesRequest(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{Message: fmt.Sprintf("failed to build image request: %v", err), Type: "server_error"},
		})
		return
	}
	if payload.Stream {
		h.streamImagesFromResponses(c, responsesReq, payload.Model, payload.ResponseFormat, imageStreamPrefix(payload.Action))
		return
	}
	h.collectImagesFromResponses(c, responsesReq, payload.Model, payload.ResponseFormat)
}

// collectImagesFromResponses 走非流式收口：内部仍用 Responses SSE，再聚合成 Images JSON。
func (h *OpenAIAPIHandler) collectImagesFromResponses(c *gin.Context, responsesReq []byte, routeModel string, responseFormat string) {
	c.Header("Content-Type", "application/json")
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManagerForRoute(cliCtx, "openai-response", handlers.StreamRouteConfig{
		ExecutionModel:      imagesExecutionModelFromRequest(responsesReq),
		SelectionModel:      routeModel,
		AllowedProviders:    []string{"codex"},
		AllowImageOnlyModel: true,
	}, responsesReq, "")
	out, errMsg := collectImagesFromResponsesStream(cliCtx, dataChan, errChan, responseFormat)
	stopKeepAlive()
	if errMsg != nil {
		h.writeImagesError(c, func(err error) { cliCancel(err) }, errMsg)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel(nil)
}

// streamImagesFromResponses 走流式收口，并在第一字节前保留标准 JSON 错误体语义。
func (h *OpenAIAPIHandler) streamImagesFromResponses(c *gin.Context, responsesReq []byte, routeModel string, responseFormat string, streamPrefix string) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{Message: "Streaming not supported", Type: "server_error"},
		})
		return
	}
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = handlers.WithDisallowFreeAuth(cliCtx)
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManagerForRoute(cliCtx, "openai-response", handlers.StreamRouteConfig{
		ExecutionModel:      imagesExecutionModelFromRequest(responsesReq),
		SelectionModel:      routeModel,
		AllowedProviders:    []string{"codex"},
		AllowImageOnlyModel: true,
	}, responsesReq, "")
	h.startImagesStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, upstreamHeaders, responseFormat, streamPrefix)
}

func imagesExecutionModelFromRequest(responsesReq []byte) string {
	model := strings.TrimSpace(gjson.GetBytes(responsesReq, "model").String())
	if model == "" {
		return defaultImagesMainModel
	}
	return model
}

// startImagesStream 先窥探首帧，决定返回 JSON 错误还是正式切换到 SSE。
func (h *OpenAIAPIHandler) startImagesStream(c *gin.Context, flusher http.Flusher, cancel func(error), dataChan <-chan []byte, errChan <-chan *interfaces.ErrorMessage, upstreamHeaders http.Header, responseFormat string, streamPrefix string) {
	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if ok {
				h.writeImagesError(c, cancel, errMsg)
				return
			}
			errChan = nil
		case chunk, ok := <-dataChan:
			if !ok {
				errMsg := &interfaces.ErrorMessage{
					StatusCode: http.StatusBadGateway,
					Error:      fmt.Errorf("stream disconnected before completion"),
				}
				h.writeImagesError(c, cancel, errMsg)
				return
			}
			h.setImagesSSEHeaders(c, upstreamHeaders)
			h.forwardImagesStream(c, flusher, cancel, dataChan, errChan, chunk, responseFormat, streamPrefix)
			return
		}
	}
}

// forwardImagesStream 负责把 Responses 图片事件转成 OpenAI Images SSE 事件。
func (h *OpenAIAPIHandler) forwardImagesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, firstChunk []byte, responseFormat string, streamPrefix string) {
	accumulator := &sseFrameAccumulator{}
	for _, frame := range accumulator.AddChunk(firstChunk) {
		done, err := h.processImagesStreamFrame(c, flusher, frame, responseFormat, streamPrefix)
		if err != nil {
			writeImagesStreamError(c.Writer, flusher, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
			cancel(err)
			return
		}
		if done {
			cancel(nil)
			return
		}
	}
	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errs:
			if ok && errMsg != nil {
				writeImagesStreamError(c.Writer, flusher, errMsg)
				cancel(errMsg.Error)
				return
			}
			errs = nil
		case chunk, ok := <-data:
			if !ok {
				done, err := h.flushImagesStreamFrames(c, flusher, accumulator.Flush(), responseFormat, streamPrefix)
				if err != nil {
					writeImagesStreamError(c.Writer, flusher, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
					cancel(err)
					return
				}
				if !done {
					errMsg := &interfaces.ErrorMessage{
						StatusCode: http.StatusBadGateway,
						Error:      fmt.Errorf("stream disconnected before completion"),
					}
					writeImagesStreamError(c.Writer, flusher, errMsg)
					cancel(errMsg.Error)
					return
				}
				cancel(nil)
				return
			}
			done, err := h.flushImagesStreamFrames(c, flusher, accumulator.AddChunk(chunk), responseFormat, streamPrefix)
			if err != nil {
				writeImagesStreamError(c.Writer, flusher, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
				cancel(err)
				return
			}
			if done {
				cancel(nil)
				return
			}
		}
	}
}

// flushImagesStreamFrames 逐帧处理已完成的 SSE 事件，命中 completed 后立即停止。
func (h *OpenAIAPIHandler) flushImagesStreamFrames(c *gin.Context, flusher http.Flusher, frames [][]byte, responseFormat string, streamPrefix string) (bool, error) {
	for _, frame := range frames {
		done, err := h.processImagesStreamFrame(c, flusher, frame, responseFormat, streamPrefix)
		if err != nil {
			return false, err
		}
		if done {
			return true, nil
		}
	}
	return false, nil
}

// processImagesStreamFrame 解析单帧中的 partial/completed 事件并输出 OpenAI Images SSE。
func (h *OpenAIAPIHandler) processImagesStreamFrame(c *gin.Context, flusher http.Flusher, frame []byte, responseFormat string, streamPrefix string) (bool, error) {
	for _, payload := range extractDataPayloads(frame) {
		done, err := writeImageStreamPayload(c.Writer, flusher, payload, responseFormat, streamPrefix)
		if err != nil {
			return false, err
		}
		if done {
			return true, nil
		}
	}
	return false, nil
}

// writeImagesError 统一处理非流式与首帧前错误，保证取消函数总会被调用。
func (h *OpenAIAPIHandler) writeImagesError(c *gin.Context, cancel func(error), errMsg *interfaces.ErrorMessage) {
	h.WriteErrorResponse(c, errMsg)
	if errMsg != nil {
		cancel(errMsg.Error)
		return
	}
	cancel(nil)
}

// setImagesSSEHeaders 与现有 OpenAI SSE 行为保持一致，并透传上游头。
func (h *OpenAIAPIHandler) setImagesSSEHeaders(c *gin.Context, upstreamHeaders http.Header) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
}

// imageStreamPrefix 让生成/编辑两类图片流事件保持可区分的命名前缀。
func imageStreamPrefix(action string) string {
	if action == "edit" {
		return "image_edit"
	}
	return "image_generation"
}
