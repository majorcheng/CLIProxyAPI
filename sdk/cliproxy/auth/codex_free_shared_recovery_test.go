package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerMarkResult_CodexFreeSuccessKeepsSiblingModelSpecificFailure(t *testing.T) {
	const provider = "codex"
	modelA, modelB := "codex-free-keep-failure-a", "codex-free-keep-failure-b"
	authID := "codex-free-keep-failure"
	manager, reg := newCodexFreeBoundaryManager(t, authID, provider, modelA, modelB)

	manager.auths[authID].ModelStates[modelA] = &ModelState{
		Status:            StatusError,
		Unavailable:       true,
		NextRetryAfter:    time.Now().Add(12 * time.Hour),
		FailureHTTPStatus: http.StatusNotFound,
		LastError:         &Error{HTTPStatus: http.StatusNotFound, Message: "not found"},
		StatusMessage:     "not found",
	}
	reg.SuspendClientModel(authID, modelA, "not_found")

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    modelB,
		Success:  true,
	})

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, modelA, time.Now()); !blocked {
		t.Fatalf("expected modelA to remain blocked, reason=%v next=%v", reason, next)
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, modelB, time.Now()); blocked {
		t.Fatalf("expected modelB to stay available, reason=%v next=%v", reason, next)
	}
	visible := visibleModelsByProvider(reg, provider)
	if visible[modelA] {
		t.Fatalf("expected modelA to remain hidden, got %#v", visible)
	}
	if !visible[modelB] {
		t.Fatalf("expected modelB to remain visible, got %#v", visible)
	}
}

func TestManagerMarkResult_CodexFreeSharedSuccessReappliesSiblingModelSpecificFailure(t *testing.T) {
	const provider = "codex"
	modelA, modelB := "codex-free-shared-clear-a", "codex-free-shared-clear-b"
	authID := "codex-free-shared-clear"
	manager, reg := newCodexFreeBoundaryManager(t, authID, provider, modelA, modelB)

	manager.auths[authID].ModelStates[modelA] = &ModelState{
		Status:            StatusError,
		Unavailable:       true,
		NextRetryAfter:    time.Now().Add(12 * time.Hour),
		FailureHTTPStatus: http.StatusNotFound,
		LastError:         &Error{HTTPStatus: http.StatusNotFound, Message: "not found"},
		StatusMessage:     "not found",
	}
	manager.auths[authID].ModelStates[modelB] = &ModelState{
		Status:            StatusError,
		Unavailable:       true,
		NextRetryAfter:    time.Now().Add(30 * time.Minute),
		FailureHTTPStatus: http.StatusUnauthorized,
		LastError:         &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		StatusMessage:     "unauthorized",
	}
	reg.SuspendClientModel(authID, modelA, "unauthorized")
	reg.SuspendClientModel(authID, modelB, "unauthorized")

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    modelB,
		Success:  true,
	})

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, modelA, time.Now()); !blocked {
		t.Fatalf("expected modelA to stay blocked after shared recovery, reason=%v next=%v", reason, next)
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, modelB, time.Now()); blocked {
		t.Fatalf("expected modelB shared block to clear, reason=%v next=%v", reason, next)
	}
	visible := visibleModelsByProvider(reg, provider)
	if visible[modelA] {
		t.Fatalf("expected modelA to remain hidden after shared recovery, got %#v", visible)
	}
	if !visible[modelB] {
		t.Fatalf("expected modelB to be resumed after shared recovery, got %#v", visible)
	}
}

func TestManagerMarkResult_CodexFreeSharedSuccessClearsThinkingSuffixState(t *testing.T) {
	const provider = "codex"
	baseModel := "codex-free-thinking-base"
	requestedModel := "codex-free-thinking-base(high)"
	siblingModel := "codex-free-thinking-sibling"
	authID := "codex-free-thinking-clear"

	manager, reg := newCodexFreeBoundaryManager(t, authID, provider, baseModel, siblingModel)
	manager.auths[authID].ModelStates = map[string]*ModelState{
		requestedModel: {
			Status:            StatusError,
			Unavailable:       true,
			NextRetryAfter:    time.Now().Add(30 * time.Minute),
			FailureHTTPStatus: http.StatusUnauthorized,
			LastError:         &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
			StatusMessage:     "unauthorized",
		},
		siblingModel: {Status: StatusActive},
	}
	reg.SuspendClientModel(authID, baseModel, "unauthorized")
	reg.SuspendClientModel(authID, siblingModel, "unauthorized")

	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    siblingModel,
		Success:  true,
	})

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("expected updated auth")
	}
	if blocked, reason, next := isAuthBlockedForModel(updated, requestedModel, time.Now()); blocked {
		t.Fatalf("expected thinking suffix state to clear, reason=%v next=%v", reason, next)
	}
	selector := &FillFirstSelector{}
	got, err := selector.Pick(context.Background(), provider, requestedModel, cliproxyexecutor.Options{}, []*Auth{updated})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil || got.ID != authID {
		t.Fatalf("Pick() auth = %#v, want %q", got, authID)
	}
	visible := visibleModelsByProvider(reg, provider)
	if !visible[baseModel] || !visible[siblingModel] {
		t.Fatalf("expected base and sibling models to be visible, got %#v", visible)
	}
}
