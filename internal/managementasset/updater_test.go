package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

func TestResolveDirectDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		repo string
		want string
	}{
		{
			name: "github repository url",
			repo: "https://github.com/majorcheng/Cli-Proxy-API-Management-Center",
			want: "https://github.com/majorcheng/Cli-Proxy-API-Management-Center/releases/latest/download/management.html",
		},
		{
			name: "github repository url with git suffix",
			repo: "https://github.com/majorcheng/Cli-Proxy-API-Management-Center.git",
			want: "https://github.com/majorcheng/Cli-Proxy-API-Management-Center/releases/latest/download/management.html",
		},
		{
			name: "github api repo url",
			repo: "https://api.github.com/repos/majorcheng/Cli-Proxy-API-Management-Center",
			want: "https://github.com/majorcheng/Cli-Proxy-API-Management-Center/releases/latest/download/management.html",
		},
		{
			name: "github api latest release url",
			repo: "https://api.github.com/repos/majorcheng/Cli-Proxy-API-Management-Center/releases/latest",
			want: "https://github.com/majorcheng/Cli-Proxy-API-Management-Center/releases/latest/download/management.html",
		},
		{
			name: "empty repo falls back to default direct asset",
			repo: "",
			want: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/latest/download/management.html",
		},
		{
			name: "non github repo falls back to default direct asset",
			repo: "https://example.com/majorcheng/Cli-Proxy-API-Management-Center",
			want: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/latest/download/management.html",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDirectDownloadURL(tc.repo); got != tc.want {
				t.Fatalf("resolveDirectDownloadURL(%q) = %q, want %q", tc.repo, got, tc.want)
			}
		})
	}
}

func TestEnsureLatestManagementHTML_FetchFailureFallsBackToDirectRelease(t *testing.T) {
	resetManagementAssetTestState(t)

	staticDir := t.TempDir()
	localPath := filepath.Join(staticDir, managementAssetName)
	if err := os.WriteFile(localPath, []byte("old-management"), 0o644); err != nil {
		t.Fatalf("write old management asset: %v", err)
	}

	panelRepository := "https://github.com/majorcheng/Cli-Proxy-API-Management-Center"
	expectedDirectURL := resolveDirectDownloadURL(panelRepository)
	expectedBody := []byte("new-management-from-direct-release")
	var downloadedURLs []string

	fetchLatestAssetFn = func(context.Context, *http.Client, string) (*releaseAsset, string, error) {
		return nil, "", fmt.Errorf("unexpected release status 403")
	}
	downloadAssetFn = func(_ context.Context, _ *http.Client, downloadURL string) ([]byte, string, error) {
		downloadedURLs = append(downloadedURLs, downloadURL)
		if downloadURL != expectedDirectURL {
			return nil, "", fmt.Errorf("unexpected download url: %s", downloadURL)
		}
		return expectedBody, sha256Hex(expectedBody), nil
	}

	if ok := EnsureLatestManagementHTML(context.Background(), staticDir, "", panelRepository); !ok {
		t.Fatal("EnsureLatestManagementHTML() = false, want true")
	}

	gotBody, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read management asset: %v", err)
	}
	if string(gotBody) != string(expectedBody) {
		t.Fatalf("management asset body = %q, want %q", string(gotBody), string(expectedBody))
	}
	if !reflect.DeepEqual(downloadedURLs, []string{expectedDirectURL}) {
		t.Fatalf("downloaded urls = %#v, want only direct release url %q", downloadedURLs, expectedDirectURL)
	}
}

func TestEnsureLatestManagementHTML_DownloadFailureFallsBackToDirectRelease(t *testing.T) {
	resetManagementAssetTestState(t)

	staticDir := t.TempDir()
	localPath := filepath.Join(staticDir, managementAssetName)
	panelRepository := "https://github.com/majorcheng/Cli-Proxy-API-Management-Center"
	expectedDirectURL := resolveDirectDownloadURL(panelRepository)
	apiDownloadURL := "https://objects.githubusercontent.example/management.html"
	expectedBody := []byte("new-management-from-direct-release-after-api-download-failure")
	var downloadedURLs []string

	fetchLatestAssetFn = func(context.Context, *http.Client, string) (*releaseAsset, string, error) {
		return &releaseAsset{
			Name:               managementAssetName,
			BrowserDownloadURL: apiDownloadURL,
		}, "", nil
	}
	downloadAssetFn = func(_ context.Context, _ *http.Client, downloadURL string) ([]byte, string, error) {
		downloadedURLs = append(downloadedURLs, downloadURL)
		switch downloadURL {
		case apiDownloadURL:
			return nil, "", fmt.Errorf("api download failed")
		case expectedDirectURL:
			return expectedBody, sha256Hex(expectedBody), nil
		default:
			return nil, "", fmt.Errorf("unexpected download url: %s", downloadURL)
		}
	}

	if ok := EnsureLatestManagementHTML(context.Background(), staticDir, "", panelRepository); !ok {
		t.Fatal("EnsureLatestManagementHTML() = false, want true")
	}

	gotBody, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read management asset: %v", err)
	}
	if string(gotBody) != string(expectedBody) {
		t.Fatalf("management asset body = %q, want %q", string(gotBody), string(expectedBody))
	}
	if !reflect.DeepEqual(downloadedURLs, []string{apiDownloadURL, expectedDirectURL}) {
		t.Fatalf("downloaded urls = %#v, want [%q %q]", downloadedURLs, apiDownloadURL, expectedDirectURL)
	}
}

func TestEnsureLatestManagementHTML_DirectReleaseFailureFallsBackToFixedPageWhenLocalMissing(t *testing.T) {
	resetManagementAssetTestState(t)

	staticDir := t.TempDir()
	localPath := filepath.Join(staticDir, managementAssetName)
	panelRepository := "https://github.com/majorcheng/Cli-Proxy-API-Management-Center"
	expectedDirectURL := resolveDirectDownloadURL(panelRepository)
	expectedFallbackBody := []byte("fallback-management-page")
	var downloadedURLs []string

	fetchLatestAssetFn = func(context.Context, *http.Client, string) (*releaseAsset, string, error) {
		return nil, "", fmt.Errorf("unexpected release status 403")
	}
	downloadAssetFn = func(_ context.Context, _ *http.Client, downloadURL string) ([]byte, string, error) {
		downloadedURLs = append(downloadedURLs, downloadURL)
		switch downloadURL {
		case expectedDirectURL:
			return nil, "", fmt.Errorf("direct release download failed")
		case defaultManagementFallbackURL:
			return expectedFallbackBody, sha256Hex(expectedFallbackBody), nil
		default:
			return nil, "", fmt.Errorf("unexpected download url: %s", downloadURL)
		}
	}

	if ok := EnsureLatestManagementHTML(context.Background(), staticDir, "", panelRepository); !ok {
		t.Fatal("EnsureLatestManagementHTML() = false, want true")
	}

	gotBody, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read management asset: %v", err)
	}
	if string(gotBody) != string(expectedFallbackBody) {
		t.Fatalf("management asset body = %q, want %q", string(gotBody), string(expectedFallbackBody))
	}
	if !reflect.DeepEqual(downloadedURLs, []string{expectedDirectURL, defaultManagementFallbackURL}) {
		t.Fatalf("downloaded urls = %#v, want [%q %q]", downloadedURLs, expectedDirectURL, defaultManagementFallbackURL)
	}
}

func resetManagementAssetTestState(t *testing.T) {
	t.Helper()

	lastUpdateCheckMu.Lock()
	lastUpdateCheckTime = time.Time{}
	lastUpdateCheckMu.Unlock()
	sfGroup = singleflight.Group{}
	fetchLatestAssetFn = fetchLatestAsset
	downloadAssetFn = downloadAsset

	t.Cleanup(func() {
		lastUpdateCheckMu.Lock()
		lastUpdateCheckTime = time.Time{}
		lastUpdateCheckMu.Unlock()
		sfGroup = singleflight.Group{}
		fetchLatestAssetFn = fetchLatestAsset
		downloadAssetFn = downloadAsset
	})
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
