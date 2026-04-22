package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestGetUsageStatistics_IncludesClientIP(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	stats := internalusage.NewRequestStatistics()
	requestedAt := time.Date(2026, 3, 27, 8, 0, 0, 0, time.UTC)
	recordManagementUsageWithOptions(t, stats, managementUsageRecordOptions{
		RemoteAddr:      "198.51.100.7:1234",
		RequestType:     internalusage.RequestTypeStream,
		UserAgent:       "  test-client/1.0 \n " + strings.Repeat("x", 200),
		FirstResponseAt: requestedAt.Add(420 * time.Millisecond),
		ReasoningEffort: "xhigh",
		Record: coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: requestedAt,
			Detail: coreusage.Detail{
				InputTokens:  11,
				OutputTokens: 22,
				TotalTokens:  33,
			},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetUsageStatistics(stats)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage", nil)

	h.GetUsageStatistics(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		Usage          internalusage.StatisticsSnapshot `json:"usage"`
		FailedRequests int64                            `json:"failed_requests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	details := payload.Usage.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].ClientIP != "198.51.100.7" {
		t.Fatalf("client_ip = %q, want %q", details[0].ClientIP, "198.51.100.7")
	}
	if details[0].ReasoningEffort != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want %q", details[0].ReasoningEffort, "xhigh")
	}
	if details[0].RequestType != internalusage.RequestTypeStream {
		t.Fatalf("request_type = %q, want %q", details[0].RequestType, internalusage.RequestTypeStream)
	}
	if details[0].FirstTokenMs != 420 {
		t.Fatalf("first_token_ms = %d, want 420", details[0].FirstTokenMs)
	}
	if strings.Contains(details[0].UserAgent, "\n") {
		t.Fatalf("user_agent should be single-line, got %q", details[0].UserAgent)
	}
	if len([]rune(details[0].UserAgent)) != 160 {
		t.Fatalf("user_agent rune length = %d, want 160", len([]rune(details[0].UserAgent)))
	}
	if payload.FailedRequests != 0 {
		t.Fatalf("failed_requests = %d, want 0", payload.FailedRequests)
	}
}

func TestExportImportUsageStatistics_PreservesClientIP(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	sourceStats := internalusage.NewRequestStatistics()
	requestedAt := time.Date(2026, 3, 27, 9, 0, 0, 0, time.UTC)
	recordManagementUsageWithOptions(t, sourceStats, managementUsageRecordOptions{
		RemoteAddr:      "[2001:db8::1]:443",
		RequestType:     internalusage.RequestTypeWebsocket,
		UserAgent:       "codex-cli/0.118.0",
		FirstResponseAt: requestedAt.Add(125 * time.Millisecond),
		ReasoningEffort: "high",
		Record: coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: requestedAt,
			Detail: coreusage.Detail{
				InputTokens:  5,
				OutputTokens: 8,
				TotalTokens:  13,
			},
		},
	})

	exportHandler := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	exportHandler.SetUsageStatistics(sourceStats)

	exportRec := httptest.NewRecorder()
	exportCtx, _ := gin.CreateTestContext(exportRec)
	exportCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage/export", nil)
	exportHandler.ExportUsageStatistics(exportCtx)

	if exportRec.Code != http.StatusOK {
		t.Fatalf("export status = %d, want %d, body=%s", exportRec.Code, http.StatusOK, exportRec.Body.String())
	}

	targetStats := internalusage.NewRequestStatistics()
	importHandler := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	importHandler.SetUsageStatistics(targetStats)

	importRec := httptest.NewRecorder()
	importCtx, _ := gin.CreateTestContext(importRec)
	importReq := httptest.NewRequest(http.MethodPost, "/v0/management/usage/import", bytes.NewReader(exportRec.Body.Bytes()))
	importReq.Header.Set("Content-Type", "application/json")
	importCtx.Request = importReq
	importHandler.ImportUsageStatistics(importCtx)

	if importRec.Code != http.StatusOK {
		t.Fatalf("import status = %d, want %d, body=%s", importRec.Code, http.StatusOK, importRec.Body.String())
	}

	snapshot := targetStats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].ClientIP != "2001:db8::1" {
		t.Fatalf("client_ip = %q, want %q", details[0].ClientIP, "2001:db8::1")
	}
	if details[0].ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want %q", details[0].ReasoningEffort, "high")
	}
	if details[0].RequestType != internalusage.RequestTypeWebsocket {
		t.Fatalf("request_type = %q, want %q", details[0].RequestType, internalusage.RequestTypeWebsocket)
	}
	if details[0].FirstTokenMs != 125 {
		t.Fatalf("first_token_ms = %d, want 125", details[0].FirstTokenMs)
	}
	if details[0].UserAgent != "codex-cli/0.118.0" {
		t.Fatalf("user_agent = %q, want %q", details[0].UserAgent, "codex-cli/0.118.0")
	}
}

func TestImportUsageStatistics_AppliesRetentionDays(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	targetStats := internalusage.NewRequestStatistics()
	handler := NewHandlerWithoutConfigFilePath(&config.Config{
		UsageStatisticsRetentionDays: 30,
	}, nil)
	handler.SetUsageStatistics(targetStats)

	now := time.Now()
	payload := usageImportPayload{
		Version: 1,
		Usage: internalusage.StatisticsSnapshot{
			APIs: map[string]internalusage.APISnapshot{
				"test-key": {
					Models: map[string]internalusage.ModelSnapshot{
						"gpt-5.4": {
							Details: []internalusage.RequestDetail{
								{
									Timestamp: now.AddDate(0, 0, -40),
									Tokens:    internalusage.TokenStats{TotalTokens: 1},
								},
								{
									Timestamp: now.AddDate(0, 0, -2),
									Tokens:    internalusage.TokenStats{TotalTokens: 2},
								},
							},
						},
					},
				},
			},
		},
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/usage/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	handler.ImportUsageStatistics(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	snapshot := targetStats.Snapshot()
	if snapshot.TotalRequests != 1 {
		t.Fatalf("snapshot.TotalRequests = %d, want 1", snapshot.TotalRequests)
	}
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
}

type managementUsageRecordOptions struct {
	RemoteAddr      string
	RequestType     string
	UserAgent       string
	FirstResponseAt time.Time
	ReasoningEffort string
	Record          coreusage.Record
}

func recordManagementUsageWithOptions(t *testing.T, stats *internalusage.RequestStatistics, options managementUsageRecordOptions) {
	t.Helper()

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = options.RemoteAddr
	if strings.TrimSpace(options.UserAgent) != "" {
		req.Header.Set("User-Agent", options.UserAgent)
	}
	ginCtx.Request = req
	if strings.TrimSpace(options.ReasoningEffort) != "" {
		ginCtx.Set(internalusage.RequestReasoningEffortContextKey, options.ReasoningEffort)
	}
	if strings.TrimSpace(options.RequestType) != "" {
		ginCtx.Set(internalusage.RequestTypeContextKey, options.RequestType)
	}
	if !options.FirstResponseAt.IsZero() {
		ginCtx.Set("API_RESPONSE_TIMESTAMP", options.FirstResponseAt)
	}

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	stats.Record(ctx, options.Record)
}
