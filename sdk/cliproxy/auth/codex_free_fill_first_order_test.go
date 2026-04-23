package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestFillFirstSelectorPick_CodexFreeKeepsSharedOrderAcrossModels(t *testing.T) {
	t.Parallel()

	modelA := "codex-free-fill-first-model-a"
	modelB := "codex-free-fill-first-model-b"
	auths := codexFreeFillFirstOrderAuths(modelA, modelB)
	selector := &FillFirstSelector{}

	gotA, err := selector.Pick(context.Background(), "codex", modelA, cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick(modelA) error = %v", err)
	}
	if gotA == nil || gotA.ID != "a-newer" {
		t.Fatalf("Pick(modelA) auth = %#v, want a-newer", gotA)
	}

	gotB, err := selector.Pick(context.Background(), "codex", modelB, cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick(modelB) error = %v", err)
	}
	if gotB == nil || gotB.ID != "z-older" {
		t.Fatalf("Pick(modelB) auth = %#v, want z-older", gotB)
	}
}

func TestSchedulerPickSingle_CodexFreeKeepsSharedOrderAcrossModels(t *testing.T) {
	t.Parallel()

	modelA := "codex-free-scheduler-model-a"
	modelB := "codex-free-scheduler-model-b"
	registerSchedulerModelSet(t, "codex", []string{modelA, modelB}, "z-older", "a-newer")
	scheduler := newSchedulerForTest(&FillFirstSelector{}, codexFreeFillFirstOrderAuths(modelA, modelB)...)

	gotA, err := scheduler.pickSingle(context.Background(), "codex", modelA, cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle(modelA) error = %v", err)
	}
	if gotA == nil || gotA.ID != "a-newer" {
		t.Fatalf("pickSingle(modelA) auth = %#v, want a-newer", gotA)
	}

	gotB, err := scheduler.pickSingle(context.Background(), "codex", modelB, cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle(modelB) error = %v", err)
	}
	if gotB == nil || gotB.ID != "z-older" {
		t.Fatalf("pickSingle(modelB) auth = %#v, want z-older", gotB)
	}
}

func codexFreeFillFirstOrderAuths(modelA, modelB string) []*Auth {
	older := time.Now().Add(-2 * time.Hour).UTC()
	newer := older.Add(time.Hour)
	blockedUntil := time.Now().Add(5 * time.Minute)
	return []*Auth{
		{
			ID:       "a-newer",
			Provider: "codex",
			Attributes: map[string]string{
				"plan_type": "free",
			},
			Metadata: map[string]any{FirstRegisteredAtMetadataKey: newer.Format(time.RFC3339Nano)},
			ModelStates: map[string]*ModelState{
				modelA: {Status: StatusActive},
				modelB: {Status: StatusActive},
			},
		},
		{
			ID:       "z-older",
			Provider: "codex",
			Attributes: map[string]string{
				"plan_type": "free",
			},
			Metadata: map[string]any{FirstRegisteredAtMetadataKey: older.Format(time.RFC3339Nano)},
			ModelStates: map[string]*ModelState{
				modelA: {
					Status:            StatusError,
					Unavailable:       true,
					NextRetryAfter:    blockedUntil,
					FailureHTTPStatus: http.StatusNotFound,
				},
				modelB: {Status: StatusActive},
			},
		},
	}
}

func registerSchedulerModelSet(t *testing.T, provider string, modelIDs []string, authIDs ...string) {
	t.Helper()
	models := make([]*registry.ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		models = append(models, &registry.ModelInfo{ID: modelID})
	}
	reg := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		reg.RegisterClient(authID, provider, models)
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			reg.UnregisterClient(authID)
		}
	})
}
