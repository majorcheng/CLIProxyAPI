package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestRequestStatisticsRecordIncludesLatency(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].LatencyMs != 1500 {
		t.Fatalf("latency_ms = %d, want 1500", details[0].LatencyMs)
	}
}

func TestRequestStatisticsRecordUsesStableMetadataAfterGinMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stats := NewRequestStatistics()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	req.Header.Set("User-Agent", "codex-cli/0.1")
	ginCtx.Request = req
	requestedAt := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	SetRequestType(ginCtx, RequestTypeStream)
	SetReasoningEffort(ginCtx, "xhigh")
	SetAPIResponseTimestamp(ginCtx, requestedAt.Add(120*time.Millisecond))

	ctx := logging.WithEndpoint(context.Background(), "POST /v1/chat/completions")
	ctx = logging.WithResponseStatusHolder(ctx)
	ctx = WithRequestMetadataFromGin(ctx, ginCtx)
	ctx = context.WithValue(ctx, "gin", ginCtx)
	logging.SetResponseStatus(ctx, http.StatusInternalServerError)

	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/mutated", nil)
	ginCtx.Status(http.StatusOK)
	ginCtx.Set(RequestTypeContextKey, RequestTypeSync)
	ginCtx.Set(RequestReasoningEffortContextKey, "low")
	ginCtx.Set(APIResponseTimestampContextKey, requestedAt.Add(900*time.Millisecond))

	stats.Record(ctx, coreusage.Record{
		Model:       "gpt-5.4",
		RequestedAt: requestedAt,
		Detail:      coreusage.Detail{TotalTokens: 30},
	})

	details := stats.Snapshot().APIs["POST /v1/chat/completions"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	detail := details[0]
	if !detail.Failed {
		t.Fatalf("failed = false, want true from stable response status")
	}
	if detail.ClientIP != "203.0.113.10" {
		t.Fatalf("client_ip = %q, want stable original IP", detail.ClientIP)
	}
	if detail.UserAgent != "codex-cli/0.1" {
		t.Fatalf("user_agent = %q, want stable original UA", detail.UserAgent)
	}
	if detail.RequestType != RequestTypeStream {
		t.Fatalf("request_type = %q, want %q", detail.RequestType, RequestTypeStream)
	}
	if detail.FirstTokenMs != 120 {
		t.Fatalf("first_token_ms = %d, want 120", detail.FirstTokenMs)
	}
	if detail.ReasoningEffort != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh", detail.ReasoningEffort)
	}
}

func TestRequestStatisticsRecordIncludesClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		remoteAddr string
		wantIP     string
	}{
		{name: "ipv4", remoteAddr: "203.0.113.10:54321", wantIP: "203.0.113.10"},
		{name: "ipv6", remoteAddr: "[2001:db8::1]:443", wantIP: "2001:db8::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := NewRequestStatistics()
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.RemoteAddr = tt.remoteAddr
			ginCtx.Request = req

			ctx := context.WithValue(context.Background(), "gin", ginCtx)
			stats.Record(ctx, coreusage.Record{
				APIKey:      "test-key",
				Model:       "gpt-5.4",
				RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
				Detail: coreusage.Detail{
					InputTokens:  10,
					OutputTokens: 20,
					TotalTokens:  30,
				},
			})

			snapshot := stats.Snapshot()
			details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
			if len(details) != 1 {
				t.Fatalf("details len = %d, want 1", len(details))
			}
			if details[0].ClientIP != tt.wantIP {
				t.Fatalf("client_ip = %q, want %q", details[0].ClientIP, tt.wantIP)
			}
		})
	}
}

func TestRequestStatisticsRecordMatchesHTTPLogClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stats := NewRequestStatistics()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	req.Header.Set("X-Forwarded-For", "198.51.100.8")
	ginCtx.Request = req

	expectedIP := logging.ResolveClientIP(ginCtx)

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	stats.Record(ctx, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].ClientIP != expectedIP {
		t.Fatalf("client_ip = %q, want same as HTTP log %q", details[0].ClientIP, expectedIP)
	}
}

func TestRequestStatisticsRecordIncludesReasoningEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stats := NewRequestStatistics()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request = req
	ginCtx.Set(RequestReasoningEffortContextKey, "xhigh")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	stats.Record(ctx, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].ReasoningEffort != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want %q", details[0].ReasoningEffort, "xhigh")
	}
}

func TestRequestStatisticsRecordIncludesStreamMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stats := NewRequestStatistics()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "  test-client/1.0 \n "+strings.Repeat("x", 200))
	ginCtx.Request = req
	ginCtx.Set(RequestTypeContextKey, RequestTypeStream)
	requestedAt := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	ginCtx.Set("API_RESPONSE_TIMESTAMP", requestedAt.Add(350*time.Millisecond))

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	stats.Record(ctx, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: requestedAt,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	details := stats.Snapshot().APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].RequestType != RequestTypeStream {
		t.Fatalf("request_type = %q, want %q", details[0].RequestType, RequestTypeStream)
	}
	if details[0].FirstTokenMs != 350 {
		t.Fatalf("first_token_ms = %d, want 350", details[0].FirstTokenMs)
	}
	if strings.Contains(details[0].UserAgent, "\n") {
		t.Fatalf("user_agent should be single-line, got %q", details[0].UserAgent)
	}
	if len([]rune(details[0].UserAgent)) != 160 {
		t.Fatalf("user_agent rune length = %d, want 160", len([]rune(details[0].UserAgent)))
	}
}

func TestRequestStatisticsRecordDerivesWebsocketTypeFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stats := NewRequestStatistics()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("User-Agent", "codex-cli/0.1")
	ginCtx.Request = req
	requestedAt := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	ginCtx.Set("API_RESPONSE_TIMESTAMP", requestedAt.Add(120*time.Millisecond))

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	stats.Record(ctx, coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: requestedAt,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	details := stats.Snapshot().APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].RequestType != RequestTypeWebsocket {
		t.Fatalf("request_type = %q, want %q", details[0].RequestType, RequestTypeWebsocket)
	}
	if details[0].FirstTokenMs != 120 {
		t.Fatalf("first_token_ms = %d, want 120", details[0].FirstTokenMs)
	}
	if details[0].UserAgent != "codex-cli/0.1" {
		t.Fatalf("user_agent = %q, want %q", details[0].UserAgent, "codex-cli/0.1")
	}
}

func TestClassifyRequestType(t *testing.T) {
	tests := []struct {
		name            string
		req             *http.Request
		requestBody     []byte
		responseHeaders http.Header
		want            string
	}{
		{
			name: "websocket upgrade",
			req: &http.Request{
				Header: http.Header{"Upgrade": []string{"websocket"}},
			},
			want: RequestTypeWebsocket,
		},
		{
			name:            "sse response",
			responseHeaders: http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
			want:            RequestTypeStream,
		},
		{
			name:        "stream request body",
			requestBody: []byte(`{"stream":true}`),
			want:        RequestTypeStream,
		},
		{
			name: "sync default",
			req: &http.Request{
				Method: http.MethodPost,
				Header: http.Header{},
			},
			want: RequestTypeSync,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyRequestType(tt.req, tt.requestBody, tt.responseHeaders); got != tt.want {
				t.Fatalf("ClassifyRequestType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestStatisticsMergeSnapshotDedupIgnoresLatency(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	first := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 0,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}
	second := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 2500,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}

	result := stats.MergeSnapshot(first)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=1 skipped=0", result)
	}

	result = stats.MergeSnapshot(second)
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("second merge = %+v, want added=0 skipped=1", result)
	}

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
}

func TestRequestStatisticsMergeSnapshotKeepsDistinctClientIPs(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	snapshot := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{
							{
								Timestamp: timestamp,
								Source:    "user@example.com",
								ClientIP:  "198.51.100.1",
								AuthIndex: "0",
								Tokens: TokenStats{
									InputTokens:  10,
									OutputTokens: 20,
									TotalTokens:  30,
								},
							},
							{
								Timestamp: timestamp,
								Source:    "user@example.com",
								ClientIP:  "198.51.100.2",
								AuthIndex: "0",
								Tokens: TokenStats{
									InputTokens:  10,
									OutputTokens: 20,
									TotalTokens:  30,
								},
							},
						},
					},
				},
			},
		},
	}

	result := stats.MergeSnapshot(snapshot)
	if result.Added != 2 || result.Skipped != 0 {
		t.Fatalf("merge = %+v, want added=2 skipped=0", result)
	}

	details := stats.Snapshot().APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 2 {
		t.Fatalf("details len = %d, want 2", len(details))
	}

	seenIPs := make(map[string]bool, len(details))
	for _, detail := range details {
		seenIPs[detail.ClientIP] = true
	}
	if !seenIPs["198.51.100.1"] || !seenIPs["198.51.100.2"] {
		t.Fatalf("details client_ip set = %#v, want both client IPs preserved", seenIPs)
	}
}

func TestRequestStatisticsMergeSnapshotNormalizesUsageMetadata(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	result := stats.MergeSnapshot(StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp:    timestamp,
							RequestType:  " STREAM ",
							FirstTokenMs: -5,
							UserAgent:    "  test-client/1.0 \n " + strings.Repeat("x", 200),
							Source:       "user@example.com",
							AuthIndex:    "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	})
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("merge = %+v, want added=1 skipped=0", result)
	}

	details := stats.Snapshot().APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].RequestType != RequestTypeStream {
		t.Fatalf("request_type = %q, want %q", details[0].RequestType, RequestTypeStream)
	}
	if details[0].FirstTokenMs != 0 {
		t.Fatalf("first_token_ms = %d, want 0", details[0].FirstTokenMs)
	}
	if strings.Contains(details[0].UserAgent, "\n") {
		t.Fatalf("user_agent should be single-line, got %q", details[0].UserAgent)
	}
	if len([]rune(details[0].UserAgent)) != 160 {
		t.Fatalf("user_agent rune length = %d, want 160", len([]rune(details[0].UserAgent)))
	}
}

func TestRequestStatisticsMergeSnapshotBackfillsDuplicateUsageMetadata(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	baseDetail := RequestDetail{
		Timestamp: timestamp,
		Source:    "user@example.com",
		ClientIP:  "198.51.100.1",
		AuthIndex: "0",
		Tokens: TokenStats{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	}
	first := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {Details: []RequestDetail{baseDetail}},
				},
			},
		},
	}
	secondDetail := baseDetail
	secondDetail.RequestType = RequestTypeStream
	secondDetail.FirstTokenMs = 333
	secondDetail.UserAgent = "test-client/1.0"
	second := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {Details: []RequestDetail{secondDetail}},
				},
			},
		},
	}

	result := stats.MergeSnapshot(first)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=1 skipped=0", result)
	}
	result = stats.MergeSnapshot(second)
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("second merge = %+v, want added=0 skipped=1", result)
	}

	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 1 {
		t.Fatalf("snapshot.TotalRequests = %d, want 1", snapshot.TotalRequests)
	}
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].RequestType != RequestTypeStream {
		t.Fatalf("request_type = %q, want %q", details[0].RequestType, RequestTypeStream)
	}
	if details[0].FirstTokenMs != 333 {
		t.Fatalf("first_token_ms = %d, want 333", details[0].FirstTokenMs)
	}
	if details[0].UserAgent != "test-client/1.0" {
		t.Fatalf("user_agent = %q, want %q", details[0].UserAgent, "test-client/1.0")
	}
}

func TestRequestStatisticsApplyRetentionByLocalDate(t *testing.T) {
	stats := NewRequestStatistics()
	records := []coreusage.Record{
		{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: time.Date(2026, 3, 1, 23, 59, 59, 0, time.Local),
			Detail:      coreusage.Detail{TotalTokens: 1},
		},
		{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: time.Date(2026, 3, 2, 0, 0, 0, 0, time.Local),
			Detail:      coreusage.Detail{TotalTokens: 2},
		},
		{
			APIKey:      "test-key",
			Model:       "gpt-5.4",
			RequestedAt: time.Date(2026, 3, 31, 12, 0, 0, 0, time.Local),
			Failed:      true,
			Detail:      coreusage.Detail{TotalTokens: 3},
		},
	}
	for _, record := range records {
		stats.Record(context.Background(), record)
	}

	changed := stats.ApplyRetention(time.Date(2026, 3, 31, 16, 0, 0, 0, time.Local), 30)
	if !changed {
		t.Fatalf("ApplyRetention() changed = false, want true")
	}

	snapshot := stats.Snapshot()
	if snapshot.TotalRequests != 2 {
		t.Fatalf("snapshot.TotalRequests = %d, want 2", snapshot.TotalRequests)
	}
	if snapshot.SuccessCount != 1 {
		t.Fatalf("snapshot.SuccessCount = %d, want 1", snapshot.SuccessCount)
	}
	if snapshot.FailureCount != 1 {
		t.Fatalf("snapshot.FailureCount = %d, want 1", snapshot.FailureCount)
	}
	if snapshot.TotalTokens != 5 {
		t.Fatalf("snapshot.TotalTokens = %d, want 5", snapshot.TotalTokens)
	}
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 2 {
		t.Fatalf("details len = %d, want 2", len(details))
	}
	if details[0].Timestamp.Before(time.Date(2026, 3, 2, 0, 0, 0, 0, time.Local)) {
		t.Fatalf("first detail timestamp = %s, want >= 2026-03-02 00:00:00 local", details[0].Timestamp)
	}
	if _, ok := snapshot.RequestsByDay["2026-03-01"]; ok {
		t.Fatalf("requests_by_day should not contain 2026-03-01 after retention")
	}
	if _, ok := snapshot.TokensByDay["2026-03-01"]; ok {
		t.Fatalf("tokens_by_day should not contain 2026-03-01 after retention")
	}
}

func TestRequestStatisticsRecordAppliesRetentionOnDayChange(t *testing.T) {
	previousDays := RetentionDays()
	SetRetentionDays(1)
	t.Cleanup(func() {
		SetRetentionDays(previousDays)
	})

	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Detail:      coreusage.Detail{TotalTokens: 1},
	})

	if changed := stats.ApplyRetention(time.Date(2026, 3, 21, 1, 0, 0, 0, time.Local), 1); !changed {
		t.Fatalf("ApplyRetention() changed = false, want true for old records")
	}

	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Now(),
		Detail:      coreusage.Detail{TotalTokens: 2},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if snapshot.TotalTokens != 2 {
		t.Fatalf("snapshot.TotalTokens = %d, want 2", snapshot.TotalTokens)
	}
}
