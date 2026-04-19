package gemini

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToGemini_StreamEmptyOutputUsesOutputItemDoneMessageFallback(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunks := [][]byte{
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}"),
	}

	var outputs []string
	for _, chunk := range chunks {
		outputs = append(outputs, ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)...)
	}

	found := false
	for _, out := range outputs {
		if gjson.Get(out, "candidates.0.content.parts.0.text").String() == "ok" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected fallback content from response.output_item.done message; outputs=%q", outputs)
	}
}

func TestConvertCodexResponseToGemini_StreamPartialImageEmitsInlineData(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	chunk := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)
	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}
	if got := gjson.Get(out[0], "candidates.0.content.parts.0.inlineData.data").String(); got != "aGVsbG8=" {
		t.Fatalf("inlineData.data = %q", got)
	}
	if got := gjson.Get(out[0], "candidates.0.content.parts.0.inlineData.mimeType").String(); got != "image/png" {
		t.Fatalf("inlineData.mimeType = %q", got)
	}

	out = ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, chunk, &param)
	if len(out) != 0 {
		t.Fatalf("expected duplicate partial image to be suppressed, got %d", len(out))
	}
}

func TestConvertCodexResponseToGemini_StreamImageGenerationCallDoneEmitsInlineData(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	var param any

	partial := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)
	if out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, partial, &param); len(out) != 1 {
		t.Fatalf("expected 1 partial chunk, got %d", len(out))
	}

	sameFinal := []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"png","result":"aGVsbG8="}}`)
	if out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, sameFinal, &param); len(out) != 0 {
		t.Fatalf("expected duplicate final image to be suppressed, got %d", len(out))
	}

	changedFinal := []byte(`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","output_format":"jpeg","result":"Ymll"}}`)
	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, changedFinal, &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 changed final chunk, got %d", len(out))
	}
	if got := gjson.Get(out[0], "candidates.0.content.parts.0.inlineData.data").String(); got != "Ymll" {
		t.Fatalf("inlineData.data = %q", got)
	}
	if got := gjson.Get(out[0], "candidates.0.content.parts.0.inlineData.mimeType").String(); got != "image/jpeg" {
		t.Fatalf("inlineData.mimeType = %q", got)
	}
}

func TestConvertCodexResponseToGemini_NonStreamImageGenerationCallAddsInlineDataPart(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[]}`)
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_123","created_at":1700000000,"usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"image_generation_call","output_format":"png","result":"aGVsbG8="}]}}`)
	out := ConvertCodexResponseToGeminiNonStream(ctx, "gemini-2.5-pro", originalRequest, nil, raw, nil)
	if got := gjson.Get(out, "candidates.0.content.parts.1.inlineData.data").String(); got != "aGVsbG8=" {
		t.Fatalf("inlineData.data = %q", got)
	}
	if got := gjson.Get(out, "candidates.0.content.parts.1.inlineData.mimeType").String(); got != "image/png" {
		t.Fatalf("inlineData.mimeType = %q", got)
	}
}

func TestConvertCodexResponseToGemini_StreamFunctionCallFlushesBeforePartialImage(t *testing.T) {
	ctx := context.Background()
	originalRequest := []byte(`{"tools":[{"functionDeclarations":[{"name":"web_search"}]}]}`)
	var param any

	toolDone := []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","name":"web_search","arguments":"{}"}}`)
	if out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, toolDone, &param); len(out) != 0 {
		t.Fatalf("expected tool call to stay buffered, got %d", len(out))
	}

	partial := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_123","output_format":"png","partial_image_b64":"aGVsbG8=","partial_image_index":0}`)
	out := ConvertCodexResponseToGemini(ctx, "gemini-2.5-pro", originalRequest, nil, partial, &param)
	if len(out) != 2 {
		t.Fatalf("expected buffered tool call + image chunk, got %d", len(out))
	}
	if got := gjson.Get(out[0], "candidates.0.content.parts.0.functionCall.name").String(); got != "web_search" {
		t.Fatalf("first chunk functionCall.name = %q", got)
	}
	if got := gjson.Get(out[1], "candidates.0.content.parts.0.inlineData.data").String(); got != "aGVsbG8=" {
		t.Fatalf("second chunk inlineData.data = %q", got)
	}
}
