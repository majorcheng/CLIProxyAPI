package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestServiceApplyCoreAuthRemovalRemovesRuntimeAuth 验证普通删除事件会移除运行态 auth。
func TestServiceApplyCoreAuthRemovalRemovesRuntimeAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{coreManager: manager}
	auth := &coreauth.Auth{ID: "runtime-remove-service", Provider: "claude", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	service.applyCoreAuthRemoval(context.Background(), auth.ID)

	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatalf("expected runtime auth %q to be removed", auth.ID)
	}
}

// TestServiceApplyCoreAuthRemovalPendingDeleteKeepsRuntimeMarker 验证维护待删除阶段保留可重试标记。
func TestServiceApplyCoreAuthRemovalPendingDeleteKeepsRuntimeMarker(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{coreManager: manager}
	auth := &coreauth.Auth{ID: "runtime-pending-delete", Provider: "claude", Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	service.applyCoreAuthRemovalWithReason(context.Background(), auth.ID, "quota_exceeded", true)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected pending-delete auth %q to stay in runtime", auth.ID)
	}
	if !updated.Disabled || updated.Status != coreauth.StatusDisabled {
		t.Fatalf("expected pending-delete auth to be disabled, got disabled=%v status=%q", updated.Disabled, updated.Status)
	}
	if !authMaintenancePendingDelete(updated) {
		t.Fatalf("expected pending-delete metadata marker")
	}
}
