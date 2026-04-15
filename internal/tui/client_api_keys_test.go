package tui

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestClientGetAPIKeys_ParsesObjectShape(t *testing.T) {
	client := NewClient(8317, "")
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"api-keys":[{"key":"alpha"},{"key":"beta","max-priority":0}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	keys, err := client.GetAPIKeys()
	if err != nil {
		t.Fatalf("GetAPIKeys() error = %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(keys))
	}
	if keys[1].MaxPriority == nil || *keys[1].MaxPriority != 0 {
		t.Fatalf("keys[1].MaxPriority = %v, want 0", keys[1].MaxPriority)
	}
}

func TestClientGetAPIKeys_FallsBackToLegacyStrings(t *testing.T) {
	client := NewClient(8317, "")
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"api-keys":["alpha","beta"]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	keys, err := client.GetAPIKeys()
	if err != nil {
		t.Fatalf("GetAPIKeys() error = %v", err)
	}
	if len(keys) != 2 || keys[0].Key != "alpha" || keys[1].Key != "beta" {
		t.Fatalf("keys = %#v, want legacy keys converted", keys)
	}
}
