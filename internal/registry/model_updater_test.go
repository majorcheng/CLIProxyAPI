package registry

import (
	"net/http"
	"strings"
	"testing"
)

func proxyForRequest(t *testing.T, client *http.Client, rawURL string) string {
	t.Helper()
	if client == nil {
		t.Fatal("client is nil")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		if client.Transport == nil {
			return ""
		}
		t.Fatalf("client transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy == nil {
		return ""
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy() error = %v", err)
	}
	if proxyURL == nil {
		return ""
	}
	return proxyURL.String()
}

func TestNewModelsHTTPClientUsesConfiguredProxy(t *testing.T) {
	SetGlobalProxyURL("http://proxy.example.com:8080")
	defer SetGlobalProxyURL("")

	client := newModelsHTTPClient()
	if got := proxyForRequest(t, client, "https://example.com"); got != "http://proxy.example.com:8080" {
		t.Fatalf("proxy = %q, want %q", got, "http://proxy.example.com:8080")
	}
}

func TestNewModelsHTTPClientSupportsDirectProxyMode(t *testing.T) {
	SetGlobalProxyURL("direct")
	defer SetGlobalProxyURL("")

	client := newModelsHTTPClient()
	if got := proxyForRequest(t, client, "https://example.com"); got != "" {
		t.Fatalf("proxy = %q, want empty direct transport", got)
	}
}

func TestNewModelsHTTPClientFallsBackWhenProxyEmpty(t *testing.T) {
	SetGlobalProxyURL("")
	client := newModelsHTTPClient()
	if client == nil {
		t.Fatal("client is nil")
	}
	if client.Transport != nil {
		t.Fatalf("client.Transport = %T, want nil default transport", client.Transport)
	}
}

func TestModelsURLsPointToCLIProxyAPIRegistryCatalog(t *testing.T) {
	if len(modelsURLs) == 0 {
		t.Fatal("modelsURLs is empty")
	}
	for _, rawURL := range modelsURLs {
		if !strings.Contains(rawURL, "router-for-me/CLIProxyAPI") {
			t.Fatalf("models URL %q does not point to CLIProxyAPI upstream catalog", rawURL)
		}
		if !strings.Contains(rawURL, "/internal/registry/models/models.json") {
			t.Fatalf("models URL %q is not the registry models.json path", rawURL)
		}
	}
}
