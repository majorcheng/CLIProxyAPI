package util

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestResolveAuthDirDefaultsWhenEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, err := ResolveAuthDir("")
	if err != nil {
		t.Fatalf("ResolveAuthDir() error = %v", err)
	}
	want := filepath.Join(home, ".cli-proxy-api")
	if got != want {
		t.Fatalf("ResolveAuthDir(\"\") = %q, want %q", got, want)
	}
}

func TestResolveAuthDirUsesDefaultConstant(t *testing.T) {
	if config.DefaultAuthDir != "~/.cli-proxy-api" {
		t.Fatalf("DefaultAuthDir = %q, want ~/.cli-proxy-api", config.DefaultAuthDir)
	}
}

func TestResolveAuthDirKeepsExplicitPath(t *testing.T) {
	explicit := filepath.Join(string(os.PathSeparator), "tmp", "auth")
	got, err := ResolveAuthDir(explicit)
	if err != nil {
		t.Fatalf("ResolveAuthDir() error = %v", err)
	}
	if got != filepath.Clean(explicit) {
		t.Fatalf("ResolveAuthDir(%q) = %q, want %q", explicit, got, filepath.Clean(explicit))
	}
}
