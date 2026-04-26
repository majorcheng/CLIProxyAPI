package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	defaultImagesMainModel      = "gpt-5.4-mini"
	defaultImagesToolModel      = "gpt-image-2"
	defaultImagesResponseFormat = "b64_json"
)

type imageInputRef struct {
	ImageURL string `json:"image_url"`
	FileID   string `json:"file_id"`
}

type imagesGenerateRequest struct {
	Prompt            string `json:"prompt"`
	Model             string `json:"model"`
	ResponseFormat    string `json:"response_format"`
	Stream            bool   `json:"stream"`
	Size              string `json:"size"`
	Quality           string `json:"quality"`
	Background        string `json:"background"`
	OutputFormat      string `json:"output_format"`
	OutputCompression *int64 `json:"output_compression"`
	PartialImages     *int64 `json:"partial_images"`
	Moderation        string `json:"moderation"`
}

type imagesEditJSONRequest struct {
	Prompt            string          `json:"prompt"`
	Model             string          `json:"model"`
	ResponseFormat    string          `json:"response_format"`
	Stream            bool            `json:"stream"`
	Images            []imageInputRef `json:"images"`
	Mask              *imageInputRef  `json:"mask"`
	Size              string          `json:"size"`
	Quality           string          `json:"quality"`
	Background        string          `json:"background"`
	OutputFormat      string          `json:"output_format"`
	OutputCompression *int64          `json:"output_compression"`
	PartialImages     *int64          `json:"partial_images"`
	InputFidelity     string          `json:"input_fidelity"`
	Moderation        string          `json:"moderation"`
}

type imagesRequestPayload struct {
	Action            string
	Prompt            string
	Model             string
	ResponseFormat    string
	Stream            bool
	Images            []string
	MaskImageURL      string
	Size              string
	Quality           string
	Background        string
	OutputFormat      string
	OutputCompression *int64
	PartialImages     *int64
	InputFidelity     string
	Moderation        string
}

// readValidJSONBody 统一读取并校验 JSON 请求体，避免重复散落错误格式。
func readValidJSONBody(c *gin.Context) ([]byte, bool) {
	rawJSON, err := c.GetRawData()
	if err != nil {
		writeInvalidRequestError(c, fmt.Sprintf("Invalid request: %v", err))
		return nil, false
	}
	if !json.Valid(rawJSON) {
		writeInvalidRequestError(c, "Invalid request: body must be valid JSON")
		return nil, false
	}
	return rawJSON, true
}

// decodeImagesGenerationsRequest 把 JSON 生图请求收口成统一的内部 payload。
func decodeImagesGenerationsRequest(rawJSON []byte) (imagesRequestPayload, error) {
	if err := rejectUnsupportedImagesField(rawJSON, "n"); err != nil {
		return imagesRequestPayload{}, err
	}
	var req imagesGenerateRequest
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		return imagesRequestPayload{}, fmt.Errorf("Invalid request: %v", err)
	}
	payload := imagesRequestPayload{
		Action:            "generate",
		Prompt:            strings.TrimSpace(req.Prompt),
		Model:             strings.TrimSpace(req.Model),
		ResponseFormat:    strings.TrimSpace(req.ResponseFormat),
		Stream:            req.Stream,
		Size:              strings.TrimSpace(req.Size),
		Quality:           strings.TrimSpace(req.Quality),
		Background:        strings.TrimSpace(req.Background),
		OutputFormat:      strings.TrimSpace(req.OutputFormat),
		OutputCompression: req.OutputCompression,
		PartialImages:     req.PartialImages,
		Moderation:        strings.TrimSpace(req.Moderation),
	}
	return normalizeImagesPayload(payload)
}

// decodeImagesEditsJSONRequest 负责解析 JSON 版编辑接口，并显式拒绝 file_id 语义。
func decodeImagesEditsJSONRequest(rawJSON []byte) (imagesRequestPayload, error) {
	if err := rejectUnsupportedImagesField(rawJSON, "n"); err != nil {
		return imagesRequestPayload{}, err
	}
	var req imagesEditJSONRequest
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		return imagesRequestPayload{}, fmt.Errorf("Invalid request: %v", err)
	}
	images, err := collectImageURLs(req.Images)
	if err != nil {
		return imagesRequestPayload{}, err
	}
	maskURL, err := collectMaskImageURL(req.Mask)
	if err != nil {
		return imagesRequestPayload{}, err
	}
	payload := imagesRequestPayload{
		Action:            "edit",
		Prompt:            strings.TrimSpace(req.Prompt),
		Model:             strings.TrimSpace(req.Model),
		ResponseFormat:    strings.TrimSpace(req.ResponseFormat),
		Stream:            req.Stream,
		Images:            images,
		MaskImageURL:      maskURL,
		Size:              strings.TrimSpace(req.Size),
		Quality:           strings.TrimSpace(req.Quality),
		Background:        strings.TrimSpace(req.Background),
		OutputFormat:      strings.TrimSpace(req.OutputFormat),
		OutputCompression: req.OutputCompression,
		PartialImages:     req.PartialImages,
		InputFidelity:     strings.TrimSpace(req.InputFidelity),
		Moderation:        strings.TrimSpace(req.Moderation),
	}
	return normalizeImagesPayload(payload)
}

// decodeImagesEditsMultipartRequest 负责把 multipart 编辑请求转换成统一 payload。
func decodeImagesEditsMultipartRequest(c *gin.Context) (imagesRequestPayload, error) {
	if c.Request != nil && c.Writer != nil && c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImagesMultipartBodyBytes)
	}
	form, err := c.MultipartForm()
	if err != nil {
		return imagesRequestPayload{}, fmt.Errorf("Invalid request: %v", err)
	}
	images, err := collectMultipartImages(form)
	if err != nil {
		return imagesRequestPayload{}, err
	}
	maskURL, err := collectMultipartMask(form)
	if err != nil {
		return imagesRequestPayload{}, err
	}
	if strings.TrimSpace(c.PostForm("n")) != "" {
		return imagesRequestPayload{}, fmt.Errorf("Invalid request: n is not supported for this endpoint")
	}
	stream, err := parseOptionalBoolField(c.PostForm("stream"), "stream", false)
	if err != nil {
		return imagesRequestPayload{}, err
	}
	outputCompression, err := parseOptionalInt64Field(c.PostForm("output_compression"), "output_compression")
	if err != nil {
		return imagesRequestPayload{}, err
	}
	partialImages, err := parseOptionalInt64Field(c.PostForm("partial_images"), "partial_images")
	if err != nil {
		return imagesRequestPayload{}, err
	}
	payload := imagesRequestPayload{
		Action:            "edit",
		Prompt:            strings.TrimSpace(c.PostForm("prompt")),
		Model:             strings.TrimSpace(c.PostForm("model")),
		ResponseFormat:    strings.TrimSpace(c.PostForm("response_format")),
		Stream:            stream,
		Images:            images,
		MaskImageURL:      maskURL,
		Size:              strings.TrimSpace(c.PostForm("size")),
		Quality:           strings.TrimSpace(c.PostForm("quality")),
		Background:        strings.TrimSpace(c.PostForm("background")),
		OutputFormat:      strings.TrimSpace(c.PostForm("output_format")),
		OutputCompression: outputCompression,
		PartialImages:     partialImages,
		InputFidelity:     strings.TrimSpace(c.PostForm("input_fidelity")),
		Moderation:        strings.TrimSpace(c.PostForm("moderation")),
	}
	return normalizeImagesPayload(payload)
}

// normalizeImagesPayload 统一补默认值并校验 OpenAI Images 的必填字段。
func normalizeImagesPayload(payload imagesRequestPayload) (imagesRequestPayload, error) {
	payload.Prompt = strings.TrimSpace(payload.Prompt)
	if payload.Prompt == "" {
		return imagesRequestPayload{}, fmt.Errorf("Invalid request: prompt is required")
	}
	if payload.Model == "" {
		payload.Model = defaultImagesToolModel
	}
	if !isSupportedImagesToolModel(payload.Model) {
		return imagesRequestPayload{}, fmt.Errorf("Invalid request: model %q is not supported for /v1/images (only %s is supported)", payload.Model, defaultImagesToolModel)
	}
	if payload.ResponseFormat == "" {
		payload.ResponseFormat = defaultImagesResponseFormat
	}
	if payload.Action == "edit" && len(payload.Images) == 0 {
		return imagesRequestPayload{}, fmt.Errorf("Invalid request: image is required")
	}
	return payload, nil
}

func isSupportedImagesToolModel(model string) bool {
	trimmed := strings.TrimSpace(model)
	if strings.EqualFold(trimmed, defaultImagesToolModel) {
		return true
	}
	if idx := strings.LastIndex(trimmed, "/"); idx > 0 && idx < len(trimmed)-1 {
		return strings.EqualFold(strings.TrimSpace(trimmed[idx+1:]), defaultImagesToolModel)
	}
	return false
}

// collectImageURLs 提取 JSON 模式下的输入图片 URL，并拒绝 file_id。
func collectImageURLs(refs []imageInputRef) ([]string, error) {
	images := make([]string, 0, len(refs))
	for _, ref := range refs {
		if strings.TrimSpace(ref.FileID) != "" {
			return nil, fmt.Errorf("Invalid request: images[].file_id is not supported (use images[].image_url instead)")
		}
		if url := strings.TrimSpace(ref.ImageURL); url != "" {
			images = append(images, url)
		}
	}
	return images, nil
}

// collectMaskImageURL 提取 JSON 模式下的 mask URL，并显式拒绝 file_id。
func collectMaskImageURL(mask *imageInputRef) (string, error) {
	if mask == nil {
		return "", nil
	}
	if strings.TrimSpace(mask.FileID) != "" {
		return "", fmt.Errorf("Invalid request: mask.file_id is not supported (use mask.image_url instead)")
	}
	return strings.TrimSpace(mask.ImageURL), nil
}

func rejectUnsupportedImagesField(rawJSON []byte, field string) error {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rawJSON, &payload); err != nil {
		return fmt.Errorf("Invalid request: %v", err)
	}
	if _, exists := payload[field]; exists {
		return fmt.Errorf("Invalid request: %s is not supported for this endpoint", field)
	}
	return nil
}

func parseOptionalInt64Field(raw string, field string) (*int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("Invalid request: %s must be an integer", field)
	}
	return &parsed, nil
}

func parseOptionalBoolField(raw string, field string, fallback bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	case "":
		return fallback, nil
	default:
		return false, fmt.Errorf("Invalid request: %s must be a boolean", field)
	}
}
