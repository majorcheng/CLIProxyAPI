package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestPersistAndRestoreRequestStatisticsRoundTrip(t *testing.T) {
	stats := NewRequestStatistics()
	firstRequestedAt := time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC)
	recordUsageWithOptionsForTest(t, stats, usageRecordTestOptions{
		RemoteAddr:      "203.0.113.10:54321",
		RequestType:     RequestTypeStream,
		UserAgent:       "  test-client/1.0 \n " + strings.Repeat("x", 200),
		FirstResponseAt: firstRequestedAt.Add(275 * time.Millisecond),
		ReasoningEffort: "xhigh",
		Record: coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: firstRequestedAt,
			Latency:     1500 * time.Millisecond,
			Source:      "user@example.com",
			AuthIndex:   "0",
			Detail: coreusage.Detail{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
		},
	})
	secondRequestedAt := time.Date(2026, 3, 26, 11, 0, 0, 0, time.UTC)
	recordUsageWithOptionsForTest(t, stats, usageRecordTestOptions{
		RemoteAddr:      "[2001:db8::1]:443",
		RequestType:     RequestTypeWebsocket,
		UserAgent:       "codex-cli/0.118.0",
		FirstResponseAt: secondRequestedAt.Add(110 * time.Millisecond),
		Record: coreusage.Record{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: secondRequestedAt,
			Latency:     900 * time.Millisecond,
			Source:      "user@example.com",
			AuthIndex:   "0",
			Failed:      true,
			Detail: coreusage.Detail{
				InputTokens:  5,
				OutputTokens: 7,
				TotalTokens:  12,
			},
		},
	})

	path := filepath.Join(t.TempDir(), "logs", StatisticsFileName)
	saved, err := PersistRequestStatistics(path, stats)
	if err != nil {
		t.Fatalf("PersistRequestStatistics() error = %v", err)
	}
	if !saved {
		t.Fatalf("PersistRequestStatistics() saved = false, want true")
	}
	if stats.HasPendingPersistence() {
		t.Fatalf("stats should be clean after persistence")
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("persisted file missing: %v", errStat)
	}

	restored := NewRequestStatistics()
	loaded, result, err := RestoreRequestStatistics(path, restored)
	if err != nil {
		t.Fatalf("RestoreRequestStatistics() error = %v", err)
	}
	if !loaded {
		t.Fatalf("RestoreRequestStatistics() loaded = false, want true")
	}
	if result.Added != 2 || result.Skipped != 0 {
		t.Fatalf("RestoreRequestStatistics() result = %+v, want added=2 skipped=0", result)
	}

	snapshot := restored.Snapshot()
	if snapshot.TotalRequests != 2 {
		t.Fatalf("snapshot.TotalRequests = %d, want 2", snapshot.TotalRequests)
	}
	if snapshot.SuccessCount != 1 {
		t.Fatalf("snapshot.SuccessCount = %d, want 1", snapshot.SuccessCount)
	}
	if snapshot.FailureCount != 1 {
		t.Fatalf("snapshot.FailureCount = %d, want 1", snapshot.FailureCount)
	}
	if snapshot.TotalTokens != 42 {
		t.Fatalf("snapshot.TotalTokens = %d, want 42", snapshot.TotalTokens)
	}
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 2 {
		t.Fatalf("details len = %d, want 2", len(details))
	}
	if details[0].ClientIP != "203.0.113.10" {
		t.Fatalf("details[0].client_ip = %q, want %q", details[0].ClientIP, "203.0.113.10")
	}
	if details[0].ReasoningEffort != "xhigh" {
		t.Fatalf("details[0].reasoning_effort = %q, want %q", details[0].ReasoningEffort, "xhigh")
	}
	if details[0].RequestType != RequestTypeStream {
		t.Fatalf("details[0].request_type = %q, want %q", details[0].RequestType, RequestTypeStream)
	}
	if details[0].FirstTokenMs != 275 {
		t.Fatalf("details[0].first_token_ms = %d, want 275", details[0].FirstTokenMs)
	}
	if strings.Contains(details[0].UserAgent, "\n") {
		t.Fatalf("details[0].user_agent should be single-line, got %q", details[0].UserAgent)
	}
	if details[1].ClientIP != "2001:db8::1" {
		t.Fatalf("details[1].client_ip = %q, want %q", details[1].ClientIP, "2001:db8::1")
	}
	if details[1].RequestType != RequestTypeWebsocket {
		t.Fatalf("details[1].request_type = %q, want %q", details[1].RequestType, RequestTypeWebsocket)
	}
	if details[1].FirstTokenMs != 110 {
		t.Fatalf("details[1].first_token_ms = %d, want 110", details[1].FirstTokenMs)
	}
	if details[1].UserAgent != "codex-cli/0.118.0" {
		t.Fatalf("details[1].user_agent = %q, want %q", details[1].UserAgent, "codex-cli/0.118.0")
	}
	if restored.HasPendingPersistence() {
		t.Fatalf("restored stats should be clean immediately after restore")
	}

	recordUsageForTest(restored, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 26, 12, 0, 0, 0, time.UTC),
		Latency:     300 * time.Millisecond,
		Source:      "user@example.com",
		AuthIndex:   "1",
		Detail: coreusage.Detail{
			InputTokens:  3,
			OutputTokens: 4,
			TotalTokens:  7,
		},
	})
	saved, err = PersistRequestStatistics(path, restored)
	if err != nil {
		t.Fatalf("PersistRequestStatistics() after restore error = %v", err)
	}
	if !saved {
		t.Fatalf("PersistRequestStatistics() after restore saved = false, want true")
	}

	reloaded := NewRequestStatistics()
	loaded, result, err = RestoreRequestStatistics(path, reloaded)
	if err != nil {
		t.Fatalf("RestoreRequestStatistics() second restore error = %v", err)
	}
	if !loaded {
		t.Fatalf("RestoreRequestStatistics() second restore loaded = false, want true")
	}
	if result.Added != 3 || result.Skipped != 0 {
		t.Fatalf("RestoreRequestStatistics() second restore result = %+v, want added=3 skipped=0", result)
	}
	if got := reloaded.Snapshot().TotalRequests; got != 3 {
		t.Fatalf("reloaded snapshot.TotalRequests = %d, want 3", got)
	}
}

func TestRestoreRequestStatisticsMissingFileNoop(t *testing.T) {
	stats := NewRequestStatistics()
	path := filepath.Join(t.TempDir(), "logs", StatisticsFileName)

	loaded, result, err := RestoreRequestStatistics(path, stats)
	if err != nil {
		t.Fatalf("RestoreRequestStatistics() error = %v", err)
	}
	if loaded {
		t.Fatalf("RestoreRequestStatistics() loaded = true, want false")
	}
	if result.Added != 0 || result.Skipped != 0 {
		t.Fatalf("RestoreRequestStatistics() result = %+v, want zero", result)
	}
}

func TestRestoreRequestStatisticsInvalidFileReturnsError(t *testing.T) {
	stats := NewRequestStatistics()
	path := filepath.Join(t.TempDir(), StatisticsFileName)
	if err := os.WriteFile(path, []byte("{invalid"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	loaded, _, err := RestoreRequestStatistics(path, stats)
	if err == nil {
		t.Fatalf("RestoreRequestStatistics() error = nil, want non-nil")
	}
	if loaded {
		t.Fatalf("RestoreRequestStatistics() loaded = true, want false")
	}
	if got := stats.Snapshot().TotalRequests; got != 0 {
		t.Fatalf("stats changed after invalid restore, total_requests = %d", got)
	}
}

func TestRestoreRequestStatisticsDeduplicatesRepeatedLoads(t *testing.T) {
	stats := NewRequestStatistics()
	recordUsageForTest(stats, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC),
		Source:      "user@example.com",
		AuthIndex:   "0",
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	path := filepath.Join(t.TempDir(), StatisticsFileName)
	if _, err := PersistRequestStatistics(path, stats); err != nil {
		t.Fatalf("PersistRequestStatistics() error = %v", err)
	}

	restored := NewRequestStatistics()
	loaded, result, err := RestoreRequestStatistics(path, restored)
	if err != nil {
		t.Fatalf("first RestoreRequestStatistics() error = %v", err)
	}
	if !loaded || result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first RestoreRequestStatistics() = loaded=%t result=%+v", loaded, result)
	}

	loaded, result, err = RestoreRequestStatistics(path, restored)
	if err != nil {
		t.Fatalf("second RestoreRequestStatistics() error = %v", err)
	}
	if !loaded || result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("second RestoreRequestStatistics() = loaded=%t result=%+v", loaded, result)
	}
	if got := restored.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("restored snapshot.TotalRequests = %d, want 1", got)
	}
}

func TestRestoreRequestStatisticsFallsBackToLegacyJSONFile(t *testing.T) {
	stats := NewRequestStatistics()
	recordUsageForTest(stats, coreusage.Record{
		APIKey:      "legacy-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC),
		Source:      "legacy@example.com",
		AuthIndex:   "0",
		Detail: coreusage.Detail{
			InputTokens:  1,
			OutputTokens: 2,
			TotalTokens:  3,
		},
	})

	dir := t.TempDir()
	legacyPath := filepath.Join(dir, legacyStatisticsFileName)
	if _, err := PersistRequestStatistics(legacyPath, stats); err != nil {
		t.Fatalf("PersistRequestStatistics(legacyPath) error = %v", err)
	}

	restored := NewRequestStatistics()
	currentPath := filepath.Join(dir, StatisticsFileName)
	loaded, result, err := RestoreRequestStatistics(currentPath, restored)
	if err != nil {
		t.Fatalf("RestoreRequestStatistics() error = %v", err)
	}
	if !loaded {
		t.Fatalf("RestoreRequestStatistics() loaded = false, want true")
	}
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("RestoreRequestStatistics() result = %+v, want added=1 skipped=0", result)
	}
	if got := restored.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("restored snapshot.TotalRequests = %d, want 1", got)
	}
}

func TestRestoreRequestStatisticsAppliesConfiguredRetention(t *testing.T) {
	previousDays := RetentionDays()
	SetRetentionDays(0)
	t.Cleanup(func() {
		SetRetentionDays(previousDays)
	})

	now := time.Now()
	stats := NewRequestStatistics()
	recordUsageForTest(stats, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: now.AddDate(0, 0, -40),
		Detail:      coreusage.Detail{TotalTokens: 1},
	})
	recordUsageForTest(stats, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: now.AddDate(0, 0, -5),
		Detail:      coreusage.Detail{TotalTokens: 2},
	})

	path := filepath.Join(t.TempDir(), StatisticsFileName)
	if err := SaveSnapshotFile(path, stats.Snapshot()); err != nil {
		t.Fatalf("SaveSnapshotFile() error = %v", err)
	}

	SetRetentionDays(30)
	restored := NewRequestStatistics()
	loaded, _, err := RestoreRequestStatistics(path, restored)
	if err != nil {
		t.Fatalf("RestoreRequestStatistics() error = %v", err)
	}
	if !loaded {
		t.Fatalf("RestoreRequestStatistics() loaded = false, want true")
	}

	snapshot := restored.Snapshot()
	if snapshot.TotalRequests != 1 {
		t.Fatalf("snapshot.TotalRequests = %d, want 1", snapshot.TotalRequests)
	}
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if !restored.HasPendingPersistence() {
		t.Fatalf("restored stats should remain dirty after retention trims loaded snapshot")
	}
}

func recordUsageForTest(stats *RequestStatistics, record coreusage.Record) {
	stats.Record(context.Background(), record)
}

type usageRecordTestOptions struct {
	RemoteAddr      string
	RequestType     string
	UserAgent       string
	FirstResponseAt time.Time
	ReasoningEffort string
	Record          coreusage.Record
}

func recordUsageWithRemoteAddrForTest(t *testing.T, stats *RequestStatistics, remoteAddr string, record coreusage.Record, reasoningEffort ...string) {
	t.Helper()

	options := usageRecordTestOptions{
		RemoteAddr: remoteAddr,
		Record:     record,
	}
	if len(reasoningEffort) > 0 {
		options.ReasoningEffort = reasoningEffort[0]
	}
	recordUsageWithOptionsForTest(t, stats, options)
}

func recordUsageWithOptionsForTest(t *testing.T, stats *RequestStatistics, options usageRecordTestOptions) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = options.RemoteAddr
	if strings.TrimSpace(options.UserAgent) != "" {
		req.Header.Set("User-Agent", options.UserAgent)
	}
	ginCtx.Request = req
	if strings.TrimSpace(options.ReasoningEffort) != "" {
		ginCtx.Set(RequestReasoningEffortContextKey, options.ReasoningEffort)
	}
	if strings.TrimSpace(options.RequestType) != "" {
		ginCtx.Set(RequestTypeContextKey, options.RequestType)
	}
	if !options.FirstResponseAt.IsZero() {
		ginCtx.Set("API_RESPONSE_TIMESTAMP", options.FirstResponseAt)
	}

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	stats.Record(ctx, options.Record)
}
