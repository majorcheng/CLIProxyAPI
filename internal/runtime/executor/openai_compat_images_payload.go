package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// prepareOpenAICompatImagesPayload 规范 OpenAI-compatible Images 上游 payload，仅改 model/stream 两个路由字段。
func prepareOpenAICompatImagesPayload(payload []byte, model string, contentType string, stream bool) ([]byte, string, error) {
	model = strings.TrimSpace(model)
	contentType = strings.TrimSpace(contentType)
	if json.Valid(payload) {
		return prepareOpenAICompatImagesJSONPayload(payload, model, stream), "application/json", nil
	}

	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return payload, contentType, nil
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return rewriteOpenAICompatImagesMultipartPayload(payload, model, boundary, stream)
}

// prepareOpenAICompatImagesJSONPayload 保留客户端 JSON 字段，只覆盖 provider 路由必需字段。
func prepareOpenAICompatImagesJSONPayload(payload []byte, model string, stream bool) []byte {
	out := bytes.Clone(payload)
	if model != "" {
		out, _ = sjson.SetBytes(out, "model", model)
	}
	if stream {
		out, _ = sjson.SetBytes(out, "stream", true)
		return out
	}
	out, _ = sjson.DeleteBytes(out, "stream")
	return out
}

// rewriteOpenAICompatImagesMultipartPayload 重建 multipart，避免原始 form 解析后无法重复发送。
func rewriteOpenAICompatImagesMultipartPayload(payload []byte, model string, boundary string, stream bool) ([]byte, string, error) {
	form, errRead := readOpenAICompatMultipartForm(payload, boundary)
	if errRead != nil {
		return nil, "", errRead
	}
	defer removeOpenAICompatMultipartFormFiles(form)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writeOpenAICompatImageRouteFields(writer, model, stream); errWrite != nil {
		return nil, "", errWrite
	}
	if errWrite := copyOpenAICompatMultipartFields(writer, form); errWrite != nil {
		return nil, "", errWrite
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

// readOpenAICompatMultipartForm 按固定内存上限读取 multipart，剩余大文件由标准库落临时文件。
func readOpenAICompatMultipartForm(payload []byte, boundary string) (*multipart.Form, error) {
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	form, errRead := reader.ReadForm(openAICompatMultipartMemory)
	if errRead != nil {
		return nil, fmt.Errorf("read multipart form failed: %w", errRead)
	}
	return form, nil
}

// removeOpenAICompatMultipartFormFiles 清理标准库可能创建的 multipart 临时文件。
func removeOpenAICompatMultipartFormFiles(form *multipart.Form) {
	if form == nil {
		return
	}
	if errRemove := form.RemoveAll(); errRemove != nil {
		log.Errorf("openai compat executor: remove multipart form files error: %v", errRemove)
	}
}

// writeOpenAICompatImageRouteFields 写入 executor 决定的上游 model 与流式开关。
func writeOpenAICompatImageRouteFields(writer *multipart.Writer, model string, stream bool) error {
	if model != "" {
		if errWrite := writer.WriteField("model", model); errWrite != nil {
			return fmt.Errorf("write model field failed: %w", errWrite)
		}
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	return nil
}

// copyOpenAICompatMultipartFields 复制业务字段和文件，但跳过下游 model/stream 路由字段。
func copyOpenAICompatMultipartFields(writer *multipart.Writer, form *multipart.Form) error {
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
		if errWrite := copyOpenAICompatTextFields(writer, key, values); errWrite != nil {
			return errWrite
		}
	}
	return copyOpenAICompatFileFields(writer, form.File)
}

// copyOpenAICompatTextFields 逐个复制同名文本字段，保留客户端多值语义。
func copyOpenAICompatTextFields(writer *multipart.Writer, key string, values []string) error {
	for _, value := range values {
		if errWrite := writer.WriteField(key, value); errWrite != nil {
			return fmt.Errorf("write form field %s failed: %w", key, errWrite)
		}
	}
	return nil
}

// copyOpenAICompatFileFields 复制 multipart 文件字段，保留同名多文件上传。
func copyOpenAICompatFileFields(writer *multipart.Writer, files map[string][]*multipart.FileHeader) error {
	for key, fileHeaders := range files {
		for _, fileHeader := range fileHeaders {
			if errCopy := copyOpenAICompatMultipartFile(writer, key, fileHeader); errCopy != nil {
				return errCopy
			}
		}
	}
	return nil
}

// cloneOpenAICompatMIMEHeader 深拷贝 MIME header，避免改写原始 form 元数据。
func cloneOpenAICompatMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

// copyOpenAICompatMultipartFile 复制单个上传文件，并补齐上游需要的基础文件 header。
func copyOpenAICompatMultipartFile(writer *multipart.Writer, key string, fileHeader *multipart.FileHeader) error {
	if fileHeader == nil {
		return nil
	}
	header := cloneOpenAICompatMIMEHeader(fileHeader.Header)
	header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/octet-stream")
	}
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		return fmt.Errorf("create file field %s failed: %w", key, errCreate)
	}
	return copyOpenAICompatFileContent(part, fileHeader)
}

// copyOpenAICompatFileContent 负责打开、复制并关闭上传文件。
func copyOpenAICompatFileContent(part io.Writer, fileHeader *multipart.FileHeader) error {
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
