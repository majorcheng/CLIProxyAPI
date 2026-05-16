package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// ReadRequestBody 统一读取请求体，并按 Content-Encoding 解码受支持的压缩格式。
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("request context is nil")
	}
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}

	encoding := requestContentEncoding(c)
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	decoded, err := decodeRequestBody(raw, encoding)
	if err != nil {
		// 有些客户端会误带 Content-Encoding，但实际发送明文 JSON；这里保留兼容。
		if json.Valid(raw) {
			return raw, nil
		}
		return nil, err
	}
	return decoded, nil
}

func requestContentEncoding(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
}

func decodeRequestBody(raw []byte, encoding string) ([]byte, error) {
	body := raw
	parts := strings.Split(encoding, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, err := decodeZstdRequestBody(body)
			if err != nil {
				return nil, err
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeZstdRequestBody(raw []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	decoded, err := io.ReadAll(decoder)
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	return decoded, nil
}
