package handlers

import (
	"net/http"
	"testing"
)

func TestFilterUpstreamHeaders_RemovesConnectionScopedHeaders(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "keep-alive, x-hop-a, x-hop-b")
	src.Add("Connection", "x-hop-c")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("X-Hop-A", "a")
	src.Set("X-Hop-B", "b")
	src.Set("X-Hop-C", "c")
	src.Set("X-Request-Id", "req-1")
	src.Set("Set-Cookie", "session=secret")

	filtered := FilterUpstreamHeaders(src)
	if filtered == nil {
		t.Fatalf("expected filtered headers, got nil")
	}

	requestID := filtered.Get("X-Request-Id")
	if requestID != "req-1" {
		t.Fatalf("expected X-Request-Id to be preserved, got %q", requestID)
	}

	blockedHeaderKeys := []string{
		"Connection",
		"Keep-Alive",
		"X-Hop-A",
		"X-Hop-B",
		"X-Hop-C",
		"Set-Cookie",
	}
	for _, key := range blockedHeaderKeys {
		value := filtered.Get(key)
		if value != "" {
			t.Fatalf("expected %s to be removed, got %q", key, value)
		}
	}
}

func TestFilterUpstreamHeaders_ReturnsNilWhenAllHeadersBlocked(t *testing.T) {
	src := http.Header{}
	src.Add("Connection", "x-hop-a")
	src.Set("X-Hop-A", "a")
	src.Set("Set-Cookie", "session=secret")

	filtered := FilterUpstreamHeaders(src)
	if filtered != nil {
		t.Fatalf("expected nil when all headers are filtered, got %#v", filtered)
	}
}

func TestFilterUpstreamHeaders_RemovesGatewayFingerprintHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("x-litellm-model-id", "claude")
	src.Set("Helicone-Auth", "secret")
	src.Set("x-portkey-request-id", "req-1")
	src.Set("cf-aig-cache-status", "hit")
	src.Set("x-kong-upstream-latency", "1")
	src.Set("x-bt-trace-id", "trace")
	src.Set("X-Request-Id", "keep-me")

	filtered := FilterUpstreamHeaders(src)
	if filtered == nil {
		t.Fatalf("expected filtered headers, got nil")
	}
	if got := filtered.Get("X-Request-Id"); got != "keep-me" {
		t.Fatalf("expected non-gateway header to survive, got %q", got)
	}
	for _, key := range []string{
		"x-litellm-model-id",
		"Helicone-Auth",
		"x-portkey-request-id",
		"cf-aig-cache-status",
		"x-kong-upstream-latency",
		"x-bt-trace-id",
	} {
		if value := filtered.Get(key); value != "" {
			t.Fatalf("expected %s to be removed, got %q", key, value)
		}
	}
}
