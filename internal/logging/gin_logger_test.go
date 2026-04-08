package logging

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func TestGinLogrusRecoveryRepanicsErrAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/abort", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	recorder := httptest.NewRecorder()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic, got nil")
		}
		err, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T", recovered)
		}
		if !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("expected ErrAbortHandler, got %v", err)
		}
		if err != http.ErrAbortHandler {
			t.Fatalf("expected exact ErrAbortHandler sentinel, got %v", err)
		}
	}()

	engine.ServeHTTP(recorder, req)
}

func TestGinLogrusRecoveryHandlesRegularPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
}

func TestGinLogrusLogger_MainLogIncludesUserAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	restore := installTestLoggerOutput(&buf)
	defer restore()

	engine := gin.New()
	engine.Use(GinLogrusLogger())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	req.Header.Set("User-Agent", "python-httpx/0.28.1")

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, `ua="python-httpx/0.28.1"`) {
		t.Fatalf("main log missing user-agent, got %q", logOutput)
	}
}

func TestSummarizeUserAgentForMainLog_NormalizesAndTruncates(t *testing.T) {
	longTail := strings.Repeat("x", maxLoggedUserAgentRunes)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("User-Agent", "  test-client/1.0 \n "+longTail+"  ")

	got := summarizeUserAgentForMainLog(req)
	if strings.Contains(got, "\n") {
		t.Fatalf("user-agent should be single-line, got %q", got)
	}
	if len([]rune(got)) != maxLoggedUserAgentRunes {
		t.Fatalf("user-agent rune length = %d, want %d", len([]rune(got)), maxLoggedUserAgentRunes)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("user-agent should be truncated with ellipsis, got %q", got)
	}
}

type testMessageFormatter struct{}

func (testMessageFormatter) Format(entry *log.Entry) ([]byte, error) {
	return []byte(entry.Message + "\n"), nil
}

func installTestLoggerOutput(buf *bytes.Buffer) func() {
	logger := log.StandardLogger()
	originalOut := logger.Out
	originalFormatter := logger.Formatter
	originalLevel := logger.Level
	originalReportCaller := logger.ReportCaller

	logger.SetOutput(buf)
	logger.SetFormatter(testMessageFormatter{})
	logger.SetLevel(log.InfoLevel)
	logger.SetReportCaller(false)

	return func() {
		logger.SetOutput(originalOut)
		logger.SetFormatter(originalFormatter)
		logger.SetLevel(originalLevel)
		logger.SetReportCaller(originalReportCaller)
	}
}
