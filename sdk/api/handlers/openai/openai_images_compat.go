package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAICompatImageHandlerType = "openai-image"

// normalizedImagesModelFromRaw 读取 JSON Images 请求模型，空值回落到内建图片模型。
func normalizedImagesModelFromRaw(rawJSON []byte) string {
	model := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if model == "" {
		return defaultImagesToolModel
	}
	return model
}

// normalizedImagesModelFromMultipart 读取 multipart Images 请求模型，空值回落到内建图片模型。
func normalizedImagesModelFromMultipart(form *multipart.Form) string {
	model := firstMultipartFormValue(form, "model")
	if model == "" {
		return defaultImagesToolModel
	}
	return model
}

// buildOpenAICompatImagesJSONRequest 为兼容提供商保留原始 Images JSON，只规范 model/stream 字段。
func buildOpenAICompatImagesJSONRequest(rawJSON []byte, imageModel string, stream bool) []byte {
	payload := bytes.Clone(rawJSON)
	if model := strings.TrimSpace(imageModel); model != "" {
		payload, _ = sjson.SetBytes(payload, "model", model)
	}
	if stream {
		payload, _ = sjson.SetBytes(payload, "stream", true)
	} else {
		payload, _ = sjson.DeleteBytes(payload, "stream")
	}
	return payload
}

// cloneImagesMIMEHeader 深拷贝上传文件 header，避免重建 multipart 时污染原始 form。
func cloneImagesMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

// buildOpenAICompatImagesMultipartRequest 重建 multipart body，避免解析表单后原始 request body 已不可再次读取。
func buildOpenAICompatImagesMultipartRequest(form *multipart.Form, imageModel string, stream bool) ([]byte, string, error) {
	if form == nil {
		return nil, "", fmt.Errorf("multipart form is nil")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if errWrite := writer.WriteField("model", imageModel); errWrite != nil {
		return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, "", fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
		for _, value := range values {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				return nil, "", fmt.Errorf("write form field %s failed: %w", key, errWrite)
			}
		}
	}
	for key, files := range form.File {
		for _, fileHeader := range files {
			if errCopy := copyOpenAICompatImageFile(writer, key, fileHeader); errCopy != nil {
				return nil, "", errCopy
			}
		}
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

// copyOpenAICompatImageFile 复制单个 multipart 上传文件，保留文件名和 Content-Type。
func copyOpenAICompatImageFile(writer *multipart.Writer, key string, fileHeader *multipart.FileHeader) error {
	if fileHeader == nil {
		return nil
	}
	header := cloneImagesMIMEHeader(fileHeader.Header)
	header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/octet-stream")
	}
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		return fmt.Errorf("create file field %s failed: %w", key, errCreate)
	}
	src, errOpen := fileHeader.Open()
	if errOpen != nil {
		return fmt.Errorf("open upload file failed: %w", errOpen)
	}
	_, errCopy := io.Copy(part, src)
	if errClose := src.Close(); errClose != nil && errCopy == nil {
		errCopy = errClose
	}
	if errCopy != nil {
		return fmt.Errorf("copy upload file failed: %w", errCopy)
	}
	return nil
}

// executeOpenAICompatImagesJSON 处理 JSON Images 请求，并保留兼容提供商自定义字段。
func (h *OpenAIAPIHandler) executeOpenAICompatImagesJSON(c *gin.Context, rawJSON []byte, imageModel string, stream bool) {
	payload := buildOpenAICompatImagesJSONRequest(rawJSON, imageModel, stream)
	h.executeOpenAICompatImagesRaw(c, payload, imageModel, stream)
}

// executeOpenAICompatImagesRaw 根据 stream 字段分派到原始响应转发路径。
func (h *OpenAIAPIHandler) executeOpenAICompatImagesRaw(c *gin.Context, payload []byte, imageModel string, stream bool) {
	if stream {
		h.streamOpenAICompatImages(c, payload, imageModel)
		return
	}
	h.collectOpenAICompatImages(c, payload, imageModel)
}

// collectOpenAICompatImages 非流式转发兼容提供商原始 Images 响应。
func (h *OpenAIAPIHandler) collectOpenAICompatImages(c *gin.Context, payload []byte, imageModel string) {
	c.Header("Content-Type", "application/json")
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, openAICompatImageHandlerType, imageModel, payload, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
			return
		}
		cliCancel(nil)
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel(nil)
}

// streamOpenAICompatImages 流式转发兼容提供商原始 Images SSE。
func (h *OpenAIAPIHandler) streamOpenAICompatImages(c *gin.Context, payload []byte, imageModel string) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{Message: "Streaming not supported", Type: "server_error"},
		})
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, openAICompatImageHandlerType, imageModel, payload, "")
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			h.WriteErrorResponse(c, errMsg)
			if errMsg != nil && errMsg.Error != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				setOpenAICompatImagesSSEHeaders(c, upstreamHeaders)
				flusher.Flush()
				cliCancel(nil)
				return
			}
			setOpenAICompatImagesSSEHeaders(c, upstreamHeaders)
			_, _ = c.Writer.Write(chunk)
			flusher.Flush()
			h.forwardOpenAICompatImagesStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan)
			return
		}
	}
}

// setOpenAICompatImagesSSEHeaders 设置兼容图片流式响应头，并透传允许的上游响应头。
func setOpenAICompatImagesSSEHeaders(c *gin.Context, upstreamHeaders http.Header) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Access-Control-Allow-Origin", "*")
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
}

// forwardOpenAICompatImagesStream 原样转发兼容提供商 SSE，并把终端错误写成 SSE error 事件。
func (h *OpenAIAPIHandler) forwardOpenAICompatImagesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage) {
	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		WriteChunk: func(next []byte) {
			_, _ = c.Writer.Write(next)
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			if errMsg == nil {
				return
			}
			status := http.StatusInternalServerError
			if errMsg.StatusCode > 0 {
				status = errMsg.StatusCode
			}
			errText := http.StatusText(status)
			if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
				errText = errMsg.Error.Error()
			}
			body := handlers.BuildErrorResponseBody(status, errText)
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", string(body))
		},
	})
}
