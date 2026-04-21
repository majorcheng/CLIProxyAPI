package util

import (
	"net/http"
	"testing"
)

func TestApplyCustomHeadersFromAttrs_MirrorsHostToRequestAndHeaderMap(t *testing.T) {
	req := &http.Request{Header: make(http.Header)}

	ApplyCustomHeadersFromAttrs(req, map[string]string{
		"header:Host":        "tenant.example.test",
		"header:X-Test-Flag": "enabled",
	})

	if req.Host != "tenant.example.test" {
		t.Fatalf("Host = %q，期望 %q", req.Host, "tenant.example.test")
	}
	if got := req.Header.Get("Host"); got != "tenant.example.test" {
		t.Fatalf("Header Host = %q，期望 %q", got, "tenant.example.test")
	}
	if got := req.Header.Get("X-Test-Flag"); got != "enabled" {
		t.Fatalf("X-Test-Flag = %q，期望 %q", got, "enabled")
	}
}
