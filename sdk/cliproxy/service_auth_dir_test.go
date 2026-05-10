package cliproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestServiceEnsureAuthDirDefaultsWhenEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	service := &Service{cfg: &config.Config{}}

	if err := service.ensureAuthDir(); err != nil {
		t.Fatalf("ensureAuthDir() error = %v", err)
	}
	want := filepath.Join(home, ".cli-proxy-api")
	if service.cfg.AuthDir != want {
		t.Fatalf("AuthDir = %q, want %q", service.cfg.AuthDir, want)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatalf("default auth dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("default auth path is not a directory: %s", want)
	}
}
