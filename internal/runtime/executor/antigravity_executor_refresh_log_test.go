package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func captureExecutorLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	logger := log.StandardLogger()
	originalOutput := logger.Out
	originalLevel := logger.Level
	originalFormatter := logger.Formatter

	var buf bytes.Buffer
	logger.SetOutput(&buf)
	logger.SetLevel(log.InfoLevel)
	logger.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableColors:    true,
	})

	t.Cleanup(func() {
		logger.SetOutput(originalOutput)
		logger.SetLevel(originalLevel)
		logger.SetFormatter(originalFormatter)
	})

	return &buf
}

func TestAntigravityEnsureAccessToken_LogsRTExchangeSuccess(t *testing.T) {
	logBuf := captureExecutorLogger(t)

	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "antigravity-auth",
		Metadata: map[string]any{
			"refresh_token": "old-refresh-token",
			"project_id":    "project-1",
		},
	}

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://oauth2.googleapis.com/token" {
			t.Fatalf("unexpected refresh URL: %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"access_token":"new-access-token",
				"refresh_token":"new-refresh-token",
				"expires_in":3600,
				"token_type":"Bearer"
			}`)),
			Request: req,
		}, nil
	}))

	token, updated, err := exec.ensureAccessToken(ctx, auth)
	if err != nil {
		t.Fatalf("ensureAccessToken error: %v", err)
	}
	if token != "new-access-token" {
		t.Fatalf("access token = %q, want %q", token, "new-access-token")
	}
	if updated == nil {
		t.Fatal("expected updated auth")
	}

	logText := logBuf.String()
	if !strings.Contains(logText, "antigravity rt 交换完成") ||
		!strings.Contains(logText, "auth_id=antigravity-auth") ||
		!strings.Contains(logText, "rt_rotated=true") {
		t.Fatalf("expected antigravity rt exchange log, got %q", logText)
	}
}
