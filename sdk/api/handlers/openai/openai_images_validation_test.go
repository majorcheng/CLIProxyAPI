package openai

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

type multipartImageSpec struct {
	fieldName string
	filename  string
	data      []byte
}

func TestDecodeImagesRequests_RejectsUnsupportedN(t *testing.T) {
	if _, err := decodeImagesGenerationsRequest([]byte(`{"prompt":"draw it","n":3}`)); err == nil || !strings.Contains(err.Error(), "n is not supported") {
		t.Fatalf("decodeImagesGenerationsRequest() err = %v, want unsupported n", err)
	}
	if _, err := decodeImagesEditsJSONRequest([]byte(`{"prompt":"edit it","images":[{"image_url":"data:image/png;base64,aGVsbG8="}],"n":3}`)); err == nil || !strings.Contains(err.Error(), "n is not supported") {
		t.Fatalf("decodeImagesEditsJSONRequest() err = %v, want unsupported n", err)
	}

	ctx := newMultipartImagesEditContext(t, map[string]string{
		"prompt": "edit it",
		"n":      "3",
	}, []multipartImageSpec{{fieldName: "image", filename: "image.png", data: testPNGBytes(t)}})
	if _, err := decodeImagesEditsMultipartRequest(ctx); err == nil || !strings.Contains(err.Error(), "n is not supported") {
		t.Fatalf("decodeImagesEditsMultipartRequest() err = %v, want unsupported n", err)
	}
}

func TestDecodeImagesEditsMultipartRequest_RejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "invalid stream",
			fields: map[string]string{"prompt": "edit it", "stream": "maybe"},
			want:   "stream must be a boolean",
		},
		{
			name:   "invalid output compression",
			fields: map[string]string{"prompt": "edit it", "output_compression": "abc"},
			want:   "output_compression must be an integer",
		},
		{
			name:   "invalid partial images",
			fields: map[string]string{"prompt": "edit it", "partial_images": "abc"},
			want:   "partial_images must be an integer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := newMultipartImagesEditContext(t, tt.fields, []multipartImageSpec{{fieldName: "image", filename: "image.png", data: testPNGBytes(t)}})
			if _, err := decodeImagesEditsMultipartRequest(ctx); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("decodeImagesEditsMultipartRequest() err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDecodeImagesEditsMultipartRequest_RejectsInvalidUploads(t *testing.T) {
	t.Run("too many images", func(t *testing.T) {
		files := make([]multipartImageSpec, 0, maxImageUploadCount+1)
		for i := 0; i < maxImageUploadCount+1; i++ {
			files = append(files, multipartImageSpec{fieldName: "image[]", filename: "image.png", data: testPNGBytes(t)})
		}
		ctx := newMultipartImagesEditContext(t, map[string]string{"prompt": "edit it"}, files)
		if _, err := decodeImagesEditsMultipartRequest(ctx); err == nil || !strings.Contains(err.Error(), "too many image files") {
			t.Fatalf("decodeImagesEditsMultipartRequest() err = %v, want too many image files", err)
		}
	})

	t.Run("invalid mime", func(t *testing.T) {
		ctx := newMultipartImagesEditContext(t, map[string]string{"prompt": "edit it"}, []multipartImageSpec{{fieldName: "image", filename: "image.txt", data: []byte("plain text")}})
		if _, err := decodeImagesEditsMultipartRequest(ctx); err == nil || !strings.Contains(err.Error(), "unsupported image content type") {
			t.Fatalf("decodeImagesEditsMultipartRequest() err = %v, want unsupported image content type", err)
		}
	})

	t.Run("oversized image", func(t *testing.T) {
		big := append(bytes.Clone(testPNGBytes(t)), bytes.Repeat([]byte{0}, int(maxImageUploadBytes)+1-len(testPNGBytes(t)))...)
		ctx := newMultipartImagesEditContext(t, map[string]string{"prompt": "edit it"}, []multipartImageSpec{{fieldName: "image", filename: "big.png", data: big}})
		if _, err := decodeImagesEditsMultipartRequest(ctx); err == nil || !strings.Contains(err.Error(), "image file exceeds") {
			t.Fatalf("decodeImagesEditsMultipartRequest() err = %v, want image file exceeds", err)
		}
	})
}

func TestDecodeImagesGenerationsRequest_RejectsUnsupportedModel(t *testing.T) {
	_, err := decodeImagesGenerationsRequest([]byte(`{"prompt":"draw it","model":"unknown-image-model"}`))
	if err == nil || !strings.Contains(err.Error(), "only gpt-image-2 is supported") {
		t.Fatalf("decodeImagesGenerationsRequest() err = %v, want unsupported model", err)
	}
}

func TestDecodeImagesGenerationsRequest_AcceptsPrefixedToolModel(t *testing.T) {
	payload, err := decodeImagesGenerationsRequest([]byte(`{"prompt":"draw it","model":"team-a/gpt-image-2"}`))
	if err != nil {
		t.Fatalf("decodeImagesGenerationsRequest() error = %v", err)
	}
	if payload.Model != "team-a/gpt-image-2" {
		t.Fatalf("model = %q, want %q", payload.Model, "team-a/gpt-image-2")
	}
}

func TestImagesGenerations_RejectsUnsupportedModelBeforePromptValidation(t *testing.T) {
	resp := performImagesEndpointRequest(
		t,
		imagesGenerationsPath,
		"application/json",
		strings.NewReader(`{"model":"gpt-5.4-mini"}`),
		(&OpenAIAPIHandler{}).ImagesGenerations,
	)
	assertUnsupportedImagesEndpointResponse(t, resp, imagesGenerationsPath, "gpt-5.4-mini")
}

func TestImagesGenerations_DisableImageGenerationReturns404BeforeBodyValidation(t *testing.T) {
	handler := disabledImageGenerationOpenAIHandler()
	resp := performImagesEndpointRequest(
		t,
		imagesGenerationsPath,
		"application/json",
		strings.NewReader(`{`),
		handler.ImagesGenerations,
	)
	assertDisabledImagesEndpointResponse(t, resp)
}

func TestImagesEditsJSON_RejectsUnsupportedModelBeforeImageValidation(t *testing.T) {
	resp := performImagesEndpointRequest(
		t,
		imagesEditsPath,
		"application/json",
		strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit it"}`),
		(&OpenAIAPIHandler{}).ImagesEdits,
	)
	assertUnsupportedImagesEndpointResponse(t, resp, imagesEditsPath, "gpt-5.4-mini")
}

func TestImagesEditsJSON_DisableImageGenerationReturns404BeforeBodyValidation(t *testing.T) {
	handler := disabledImageGenerationOpenAIHandler()
	resp := performImagesEndpointRequest(
		t,
		imagesEditsPath,
		"application/json",
		strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit it"}`),
		handler.ImagesEdits,
	)
	assertDisabledImagesEndpointResponse(t, resp)
}

func TestImagesEditsMultipart_RejectsUnsupportedModelBeforeUploadValidation(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-5.4-mini"); err != nil {
		t.Fatalf("WriteField(model): %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close(): %v", err)
	}

	resp := performImagesEndpointRequest(
		t,
		imagesEditsPath,
		writer.FormDataContentType(),
		&body,
		(&OpenAIAPIHandler{}).ImagesEdits,
	)
	assertUnsupportedImagesEndpointResponse(t, resp, imagesEditsPath, "gpt-5.4-mini")
}

func TestImagesEditsMultipart_DisableImageGenerationReturns404BeforeUploadValidation(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-5.4-mini"); err != nil {
		t.Fatalf("WriteField(model): %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close(): %v", err)
	}

	handler := disabledImageGenerationOpenAIHandler()
	resp := performImagesEndpointRequest(
		t,
		imagesEditsPath,
		writer.FormDataContentType(),
		&body,
		handler.ImagesEdits,
	)
	assertDisabledImagesEndpointResponse(t, resp)
}

func TestImagesEditsMultipart_UnsupportedModelDoesNotBypassBodyLimit(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-5.4-mini"); err != nil {
		t.Fatalf("WriteField(model): %v", err)
	}
	part, err := writer.CreateFormFile("image", "huge.png")
	if err != nil {
		t.Fatalf("CreateFormFile(image): %v", err)
	}
	huge := bytes.Repeat([]byte("a"), int(maxImagesMultipartBodyBytes)+1)
	if _, err := part.Write(huge); err != nil {
		t.Fatalf("part.Write(huge): %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close(): %v", err)
	}

	resp := performImagesEndpointRequest(
		t,
		imagesEditsPath,
		writer.FormDataContentType(),
		&body,
		(&OpenAIAPIHandler{}).ImagesEdits,
	)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	if strings.Contains(message, "only "+defaultImagesToolModel+" is supported") {
		t.Fatalf("error.message = %q, want body-size error instead of unsupported-model shortcut", message)
	}
	if !strings.Contains(strings.ToLower(message), "too large") {
		t.Fatalf("error.message = %q, want request body too large style error", message)
	}
}

func performImagesEndpointRequest(t *testing.T, path string, contentType string, body io.Reader, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(path, handler)

	req := httptest.NewRequest(http.MethodPost, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

// disabledImageGenerationOpenAIHandler 构造启用全局图片禁用开关的 OpenAI handler。
func disabledImageGenerationOpenAIHandler() *OpenAIAPIHandler {
	base := sdkhandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: true}, nil)
	return NewOpenAIAPIHandler(base)
}

// assertDisabledImagesEndpointResponse 校验禁用图片能力时的隐藏式 404 响应。
func assertDisabledImagesEndpointResponse(t *testing.T, resp *httptest.ResponseRecorder) {
	t.Helper()
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
	if resp.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty 404 body", resp.Body.String())
	}
}

func assertUnsupportedImagesEndpointResponse(t *testing.T, resp *httptest.ResponseRecorder, endpointPath string, model string) {
	t.Helper()
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	wantMessage := "Invalid request: model \"" + model + "\" is not supported for " + endpointPath + " (only " + defaultImagesToolModel + " is supported)"
	if message != wantMessage {
		t.Fatalf("error.message = %q, want %q", message, wantMessage)
	}
	if gotType := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); gotType != "invalid_request_error" {
		t.Fatalf("error.type = %q, want %q", gotType, "invalid_request_error")
	}
}

func newMultipartImagesEditContext(t *testing.T, fields map[string]string, files []multipartImageSpec) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("WriteField(%s): %v", key, err)
		}
	}
	for _, file := range files {
		part, err := writer.CreateFormFile(file.fieldName, file.filename)
		if err != nil {
			t.Fatalf("CreateFormFile(%s): %v", file.fieldName, err)
		}
		if _, err := part.Write(file.data); err != nil {
			t.Fatalf("part.Write(%s): %v", file.filename, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close(): %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req
	return ctx
}

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO3Z0x8AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("DecodeString(): %v", err)
	}
	return raw
}
