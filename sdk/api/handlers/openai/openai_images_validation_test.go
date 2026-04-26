package openai

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
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
