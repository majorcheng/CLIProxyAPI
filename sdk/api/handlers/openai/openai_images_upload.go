package openai

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

const (
	maxImagesMultipartBodyBytes = 20 << 20
	maxImageUploadCount         = 4
	maxImageUploadBytes         = 4 << 20
	maxMaskUploadBytes          = 4 << 20
)

var allowedImagesUploadMIMEs = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
}

// collectMultipartImages 兼容 `image` 与 `image[]` 两种上传字段，并限制数量与大小。
func collectMultipartImages(form *multipart.Form) ([]string, error) {
	files := append([]*multipart.FileHeader(nil), form.File["image[]"]...)
	files = append(files, form.File["image"]...)
	if len(files) > maxImageUploadCount {
		return nil, fmt.Errorf("Invalid request: too many image files (max %d)", maxImageUploadCount)
	}
	images := make([]string, 0, len(files))
	for _, fileHeader := range files {
		dataURL, err := multipartFileToDataURL(fileHeader, maxImageUploadBytes, "image")
		if err != nil {
			return nil, err
		}
		images = append(images, dataURL)
	}
	return images, nil
}

// collectMultipartMask 读取可选 mask 文件，并对大小与 MIME 做同样限制。
func collectMultipartMask(form *multipart.Form) (string, error) {
	maskFiles := form.File["mask"]
	if len(maskFiles) == 0 || maskFiles[0] == nil {
		return "", nil
	}
	if len(maskFiles) > 1 {
		return "", fmt.Errorf("Invalid request: only one mask file is supported")
	}
	return multipartFileToDataURL(maskFiles[0], maxMaskUploadBytes, "mask")
}

// multipartFileToDataURL 把上传文件转换成 data URL，并显式限制大小与 MIME。
func multipartFileToDataURL(fileHeader *multipart.FileHeader, maxBytes int64, fieldName string) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("Invalid request: %s upload file is nil", fieldName)
	}
	if fileHeader.Size > 0 && fileHeader.Size > maxBytes {
		return "", fmt.Errorf("Invalid request: %s file exceeds %d bytes", fieldName, maxBytes)
	}
	file, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("Invalid request: open %s upload failed: %w", fieldName, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("Invalid request: read %s upload failed: %w", fieldName, err)
	}
	if int64(len(data)) > maxBytes {
		return "", fmt.Errorf("Invalid request: %s file exceeds %d bytes", fieldName, maxBytes)
	}
	mediaType := detectMultipartImageType(fileHeader, data)
	if _, ok := allowedImagesUploadMIMEs[mediaType]; !ok {
		return "", fmt.Errorf("Invalid request: unsupported %s content type %q", fieldName, mediaType)
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func detectMultipartImageType(fileHeader *multipart.FileHeader, data []byte) string {
	headerType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if headerType != "" {
		if parsed, _, err := mime.ParseMediaType(headerType); err == nil {
			headerType = strings.TrimSpace(strings.ToLower(parsed))
		}
	}
	if _, ok := allowedImagesUploadMIMEs[headerType]; ok {
		return headerType
	}
	return strings.TrimSpace(strings.ToLower(http.DetectContentType(data)))
}
