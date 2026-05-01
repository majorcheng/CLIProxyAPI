package auth

import (
	"context"
	"testing"
	"time"
)

func TestManagerMarkResultRecordsRecentRequests(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusActive,
		Attributes: map[string]string{
			"runtime_only": "true",
		},
		Metadata: map[string]any{
			"type": "codex",
		},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: true})
	mgr.MarkResult(context.Background(), Result{AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: false})

	gotAuth, ok := mgr.GetByID("auth-1")
	if !ok || gotAuth == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, gotAuth)
	}

	snapshot := gotAuth.RecentRequestsSnapshot(time.Now())
	var successTotal int64
	var failedTotal int64
	for _, bucket := range snapshot {
		successTotal += bucket.Success
		failedTotal += bucket.Failed
	}
	if successTotal != 1 || failedTotal != 1 {
		t.Fatalf("totals = success=%d failed=%d, want 1/1", successTotal, failedTotal)
	}
}

func TestManagerUpdatePreservesRecentRequests(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusActive,
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	mgr.MarkResult(context.Background(), Result{AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: true})

	updated := &Auth{
		ID:       "auth-1",
		Provider: "codex",
		Status:   StatusActive,
		Label:    "updated label",
	}
	if _, err := mgr.Update(WithSkipPersist(context.Background()), updated); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	gotAuth, ok := mgr.GetByID("auth-1")
	if !ok || gotAuth == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, gotAuth)
	}
	if gotAuth.Label != "updated label" {
		t.Fatalf("label = %q, want updated label", gotAuth.Label)
	}
	if totalRecentSuccesses(gotAuth.RecentRequestsSnapshot(time.Now())) != 1 {
		t.Fatalf("recent success total was not preserved after Update")
	}
}

// totalRecentSuccesses 汇总测试快照里的成功次数，避免断言依赖具体桶位置。
func totalRecentSuccesses(snapshot []RecentRequestBucket) int64 {
	var total int64
	for _, bucket := range snapshot {
		total += bucket.Success
	}
	return total
}
