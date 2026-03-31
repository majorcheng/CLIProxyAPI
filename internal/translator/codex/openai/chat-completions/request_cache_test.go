package chat_completions

import "testing"

func TestPrimeOpenAIRequestCachesParsedRequest(t *testing.T) {
	originalCache := openAIRequestCache
	openAIRequestCache = newParsedRequestCache(parsedRequestCacheSize)
	t.Cleanup(func() {
		openAIRequestCache = originalCache
	})

	raw := []byte(`{
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "hello"}
		],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "get_weather",
					"parameters": {"type": "object", "properties": {}}
				}
			}
		]
	}`)

	if _, ok := cachedOpenAIRequest(raw); ok {
		t.Fatal("expected cache miss before priming request")
	}

	PrimeOpenAIRequest(raw)

	req, ok := cachedOpenAIRequest(raw)
	if !ok {
		t.Fatal("expected cache hit after priming request")
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages in cached request, got %d", len(req.Messages))
	}
	if req.Messages[1].Role != "user" {
		t.Fatalf("expected second message role to be user, got %q", req.Messages[1].Role)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function == nil {
		t.Fatalf("expected cached function tool to be preserved, got %#v", req.Tools)
	}
	if req.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("expected cached function tool name to be preserved, got %q", req.Tools[0].Function.Name)
	}
}

func TestPrimeOpenAIRequestEvictsOldestEntryAtCapacity(t *testing.T) {
	originalCache := openAIRequestCache
	openAIRequestCache = newParsedRequestCache(1)
	t.Cleanup(func() {
		openAIRequestCache = originalCache
	})

	rawFirst := []byte(`{"messages":[{"role":"user","content":"first"}]}`)
	rawSecond := []byte(`{"messages":[{"role":"user","content":"second"}]}`)

	PrimeOpenAIRequest(rawFirst)
	PrimeOpenAIRequest(rawSecond)

	if _, ok := cachedOpenAIRequest(rawFirst); ok {
		t.Fatal("expected oldest cache entry to be evicted after capacity is exceeded")
	}
	req, ok := cachedOpenAIRequest(rawSecond)
	if !ok {
		t.Fatal("expected newest cache entry to remain after eviction")
	}
	if len(req.Messages) != 1 || string(req.Messages[0].Content) != "\"second\"" {
		t.Fatalf("expected cached newest request payload to remain intact, got %#v", req.Messages)
	}
}
