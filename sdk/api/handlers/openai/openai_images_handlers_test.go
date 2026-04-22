package openai

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/tidwall/gjson"
)

func TestBuildImagesResponsesRequest_BuildsToolChoiceAndInputImages(t *testing.T) {
	payload := imagesRequestPayload{
		Action:         "edit",
		Prompt:         "make it brighter",
		Model:          "gpt-image-2",
		ResponseFormat: "b64_json",
		Images:         []string{"data:image/png;base64,aGVsbG8="},
		MaskImageURL:   "data:image/png;base64,bWFzaw==",
		OutputFormat:   "png",
	}

	raw, err := buildImagesResponsesRequest(payload)
	if err != nil {
		t.Fatalf("buildImagesResponsesRequest() error = %v", err)
	}
	if got := gjson.GetBytes(raw, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("model = %q, want %q", got, defaultImagesMainModel)
	}
	if got := gjson.GetBytes(raw, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation", got)
	}
	if got := gjson.GetBytes(raw, "tools.0.action").String(); got != "edit" {
		t.Fatalf("tools.0.action = %q, want edit", got)
	}
	if got := gjson.GetBytes(raw, "tools.0.input_image_mask.image_url").String(); got != payload.MaskImageURL {
		t.Fatalf("mask image url = %q, want %q", got, payload.MaskImageURL)
	}
	if got := gjson.GetBytes(raw, "input.0.content.1.type").String(); got != "input_image" {
		t.Fatalf("input image type = %q, want input_image", got)
	}
}

func TestExtractImagesFromResponsesCompleted_ParsesImagesAndUsage(t *testing.T) {
	payload := []byte(`{
		"type":"response.completed",
		"response":{
			"created_at":1700000000,
			"tool_usage":{"image_gen":{"num_images":1}},
			"output":[
				{"type":"message","content":[{"type":"output_text","text":"ok"}]},
				{"type":"image_generation_call","result":"aGVsbG8=","revised_prompt":"cat","output_format":"png","size":"1024x1024","quality":"high","background":"transparent"}
			]
		}
	}`)

	results, createdAt, usageRaw, firstMeta, err := extractImagesFromResponsesCompleted(payload)
	if err != nil {
		t.Fatalf("extractImagesFromResponsesCompleted() error = %v", err)
	}
	if createdAt != 1700000000 {
		t.Fatalf("createdAt = %d, want %d", createdAt, int64(1700000000))
	}
	if len(results) != 1 || results[0].Result != "aGVsbG8=" {
		t.Fatalf("results = %+v, want single image result", results)
	}
	if firstMeta.Background != "transparent" || firstMeta.Quality != "high" {
		t.Fatalf("firstMeta = %+v, want transparent/high", firstMeta)
	}
	if string(usageRaw) != `{"num_images":1}` {
		t.Fatalf("usageRaw = %s, want image_gen JSON", string(usageRaw))
	}
}

func TestBuildImagesAPIResponse_SupportsB64JSONAndURL(t *testing.T) {
	results := []imageCallResult{{Result: "aGVsbG8=", RevisedPrompt: "cat", OutputFormat: "png"}}

	b64Resp, err := buildImagesAPIResponse(results, 1700000000, nil, results[0], "b64_json")
	if err != nil {
		t.Fatalf("buildImagesAPIResponse(b64_json) error = %v", err)
	}
	if got := gjson.GetBytes(b64Resp, "data.0.b64_json").String(); got != "aGVsbG8=" {
		t.Fatalf("b64_json = %q, want %q", got, "aGVsbG8=")
	}

	urlResp, err := buildImagesAPIResponse(results, 1700000000, nil, results[0], "url")
	if err != nil {
		t.Fatalf("buildImagesAPIResponse(url) error = %v", err)
	}
	if got := gjson.GetBytes(urlResp, "data.0.url").String(); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("url = %q, want data URL", got)
	}
}

func TestCollectImagesFromResponsesStream_ReturnsCompletedOutput(t *testing.T) {
	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":1700000000,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aGVsbG8=\",\"output_format\":\"png\"}]}}\n\n")
	close(data)
	close(errs)

	out, errMsg := collectImagesFromResponsesStream(context.Background(), data, errs, "b64_json")
	if errMsg != nil {
		t.Fatalf("collectImagesFromResponsesStream() err = %v", errMsg)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "aGVsbG8=" {
		t.Fatalf("data.0.b64_json = %q, want %q", got, "aGVsbG8=")
	}
}
