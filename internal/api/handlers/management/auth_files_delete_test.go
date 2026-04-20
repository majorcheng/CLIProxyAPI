package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestDeleteAuthFile_UsesAuthPathFromManager(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	authDir := filepath.Join(tempDir, "auth")
	externalDir := filepath.Join(tempDir, "external")
	logDir := filepath.Join(tempDir, "logs")
	if errMkdirAuth := os.MkdirAll(authDir, 0o700); errMkdirAuth != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdirAuth)
	}
	if errMkdirExternal := os.MkdirAll(externalDir, 0o700); errMkdirExternal != nil {
		t.Fatalf("failed to create external dir: %v", errMkdirExternal)
	}

	fileName := "codex-user@example.com-plus.json"
	shadowPath := filepath.Join(authDir, fileName)
	realPath := filepath.Join(externalDir, fileName)
	if errWriteShadow := os.WriteFile(shadowPath, []byte(`{"type":"codex","email":"shadow@example.com"}`), 0o600); errWriteShadow != nil {
		t.Fatalf("failed to write shadow file: %v", errWriteShadow)
	}
	if errWriteReal := os.WriteFile(realPath, []byte(`{"type":"codex","email":"real@example.com"}`), 0o600); errWriteReal != nil {
		t.Fatalf("failed to write real file: %v", errWriteReal)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:          "legacy/" + fileName,
		FileName:    fileName,
		Provider:    "codex",
		Status:      coreauth.StatusError,
		Unavailable: true,
		Attributes: map[string]string{
			"path": realPath,
		},
		Metadata: map[string]any{
			"type":  "codex",
			"email": "real@example.com",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	store := &memoryAuthStore{}
	h.tokenStore = store
	h.logDir = logDir

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusOK {
		t.Fatalf("expected delete status %d, got %d with body %s", http.StatusOK, deleteRec.Code, deleteRec.Body.String())
	}
	if _, errStatReal := os.Stat(realPath); !os.IsNotExist(errStatReal) {
		t.Fatalf("expected managed auth file to be removed, stat err: %v", errStatReal)
	}
	if _, errStatShadow := os.Stat(shadowPath); errStatShadow != nil {
		t.Fatalf("expected shadow auth file to remain, stat err: %v", errStatShadow)
	}
	archivedPath := filepath.Join(logDir, "delete", managementDeleteTrashBucket, fileName)
	if _, errStatArchived := os.Stat(archivedPath); errStatArchived != nil {
		t.Fatalf("expected managed auth file to be archived, stat err: %v", errStatArchived)
	}
	if len(store.persistCalls) != 1 {
		t.Fatalf("expected one persist call, got %d", len(store.persistCalls))
	}
	if got := store.persistCalls[0].Paths; len(got) != 1 || got[0] != realPath {
		t.Fatalf("persist paths = %#v, want [%q]", got, realPath)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("expected delete fallback to stay unused, got %#v", store.deletedIDs)
	}

	listRec := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRec)
	listReq := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	listCtx.Request = listReq
	h.ListAuthFiles(listCtx)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}
	var listPayload map[string]any
	if errUnmarshal := json.Unmarshal(listRec.Body.Bytes(), &listPayload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := listPayload["files"].([]any)
	if !ok {
		t.Fatalf("expected files array, payload: %#v", listPayload)
	}
	if len(filesRaw) != 0 {
		t.Fatalf("expected removed auth to be hidden from list, got %d entries", len(filesRaw))
	}
}

func TestDeleteAuthFile_FallbackToAuthDirPath(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	authDir := filepath.Join(tempDir, "auth")
	logDir := filepath.Join(tempDir, "logs")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdir)
	}
	fileName := "fallback-user.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	store := &memoryAuthStore{}
	h.tokenStore = store
	h.logDir = logDir

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusOK {
		t.Fatalf("expected delete status %d, got %d with body %s", http.StatusOK, deleteRec.Code, deleteRec.Body.String())
	}
	if _, errStat := os.Stat(filePath); !os.IsNotExist(errStat) {
		t.Fatalf("expected auth file to be removed from auth dir, stat err: %v", errStat)
	}
	archivedPath := filepath.Join(logDir, "delete", managementDeleteTrashBucket, fileName)
	if _, errStatArchived := os.Stat(archivedPath); errStatArchived != nil {
		t.Fatalf("expected auth file to be archived, stat err: %v", errStatArchived)
	}
	if len(store.persistCalls) != 1 {
		t.Fatalf("expected one persist call, got %d", len(store.persistCalls))
	}
	if got := store.persistCalls[0].Paths; len(got) != 1 || got[0] != filePath {
		t.Fatalf("persist paths = %#v, want [%q]", got, filePath)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("expected delete fallback to stay unused, got %#v", store.deletedIDs)
	}
}

func TestDeleteAuthFile_AlreadyMissingStillDisablesAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	authDir := filepath.Join(tempDir, "auth")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdir)
	}

	fileName := "already-missing-user.json"
	filePath := filepath.Join(authDir, fileName)
	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:          "managed/" + fileName,
		FileName:    fileName,
		Provider:    "codex",
		Status:      coreauth.StatusActive,
		Unavailable: false,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{
			"type":  "codex",
			"email": "missing@example.com",
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	store := &memoryAuthStore{}
	h.tokenStore = store

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusOK {
		t.Fatalf("expected delete status %d, got %d with body %s", http.StatusOK, deleteRec.Code, deleteRec.Body.String())
	}
	var deletePayload map[string]any
	if errUnmarshal := json.Unmarshal(deleteRec.Body.Bytes(), &deletePayload); errUnmarshal != nil {
		t.Fatalf("failed to decode delete payload: %v", errUnmarshal)
	}
	if alreadyMissing, ok := deletePayload["already_missing"].(bool); !ok || !alreadyMissing {
		t.Fatalf("expected already_missing=true, payload: %#v", deletePayload)
	}

	auth, ok := manager.GetByID(record.ID)
	if !ok {
		t.Fatalf("expected auth record to remain in manager for disabled-state tracking")
	}
	if !auth.Disabled {
		t.Fatalf("expected auth to be disabled after idempotent delete")
	}
	if auth.Status != coreauth.StatusDisabled {
		t.Fatalf("expected auth status %q, got %q", coreauth.StatusDisabled, auth.Status)
	}

	listRec := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRec)
	listReq := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	listCtx.Request = listReq
	h.ListAuthFiles(listCtx)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}
	var listPayload map[string]any
	if errUnmarshal := json.Unmarshal(listRec.Body.Bytes(), &listPayload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := listPayload["files"].([]any)
	if !ok {
		t.Fatalf("expected files array, payload: %#v", listPayload)
	}
	if len(filesRaw) != 0 {
		t.Fatalf("expected missing auth to be hidden from list after delete, got %d entries", len(filesRaw))
	}
	if len(store.persistCalls) != 1 {
		t.Fatalf("expected one persist call, got %d", len(store.persistCalls))
	}
	if got := store.persistCalls[0].Paths; len(got) != 1 || got[0] != filePath {
		t.Fatalf("persist paths = %#v, want [%q]", got, filePath)
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("expected delete fallback to stay unused, got %#v", store.deletedIDs)
	}
}

func TestDeleteAuthFile_AlreadyMissingWithoutAuthStill404(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	h.tokenStore = &memoryAuthStore{}

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name=ghost-user.json", nil)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusNotFound {
		t.Fatalf("expected delete status %d, got %d with body %s", http.StatusNotFound, deleteRec.Code, deleteRec.Body.String())
	}
}

func TestDeleteAuthFile_AllArchivesFiles(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	authDir := filepath.Join(tempDir, "auth")
	logDir := filepath.Join(tempDir, "logs")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("failed to create auth dir: %v", errMkdir)
	}
	files := []string{"alpha.json", "beta.json"}
	for _, name := range files {
		if errWrite := os.WriteFile(filepath.Join(authDir, name), []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
			t.Fatalf("failed to write auth file %s: %v", name, errWrite)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	store := &memoryAuthStore{}
	h.tokenStore = store
	h.logDir = logDir

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?all=true", nil)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusOK {
		t.Fatalf("expected delete status %d, got %d with body %s", http.StatusOK, deleteRec.Code, deleteRec.Body.String())
	}
	for _, name := range files {
		if _, errStat := os.Stat(filepath.Join(authDir, name)); !os.IsNotExist(errStat) {
			t.Fatalf("expected auth file %s to leave auth dir, stat err: %v", name, errStat)
		}
		archivedPath := filepath.Join(logDir, "delete", managementDeleteTrashBucket, name)
		if _, errStatArchived := os.Stat(archivedPath); errStatArchived != nil {
			t.Fatalf("expected auth file %s to be archived, stat err: %v", name, errStatArchived)
		}
	}
	if len(store.persistCalls) != len(files) {
		t.Fatalf("expected %d persist calls, got %d", len(files), len(store.persistCalls))
	}
	if len(store.deletedIDs) != 0 {
		t.Fatalf("expected delete fallback to stay unused, got %#v", store.deletedIDs)
	}
}

func TestDeleteAuthFile_AlreadyMissingConcurrentStormDisablesAllAuths(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	const totalDeletes = 291

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	names := make([]string, 0, totalDeletes)
	for i := 0; i < totalDeletes; i++ {
		fileName := fmt.Sprintf("storm-user-%03d.json", i)
		filePath := filepath.Join(authDir, fileName)
		record := &coreauth.Auth{
			ID:          "storm/" + fileName,
			FileName:    fileName,
			Provider:    "codex",
			Status:      coreauth.StatusActive,
			Unavailable: false,
			Attributes: map[string]string{
				"path": filePath,
			},
			Metadata: map[string]any{
				"type":  "codex",
				"email": fmt.Sprintf("storm-%03d@example.com", i),
			},
		}
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register auth record %s: %v", record.ID, errRegister)
		}
		names = append(names, fileName)
	}

	start := make(chan struct{})
	errCh := make(chan error, totalDeletes)
	var wg sync.WaitGroup
	for _, name := range names {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			deleteRec := httptest.NewRecorder()
			deleteCtx, _ := gin.CreateTestContext(deleteRec)
			deleteReq := httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(name), nil)
			deleteCtx.Request = deleteReq
			h.DeleteAuthFile(deleteCtx)

			if deleteRec.Code != http.StatusOK {
				errCh <- fmt.Errorf("delete %s status=%d body=%s", name, deleteRec.Code, deleteRec.Body.String())
				return
			}
			var payload map[string]any
			if errUnmarshal := json.Unmarshal(deleteRec.Body.Bytes(), &payload); errUnmarshal != nil {
				errCh <- fmt.Errorf("decode payload for %s: %w", name, errUnmarshal)
				return
			}
			if alreadyMissing, ok := payload["already_missing"].(bool); !ok || !alreadyMissing {
				errCh <- fmt.Errorf("expected already_missing=true for %s, payload=%#v", name, payload)
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range names {
		auth, ok := manager.GetByID("storm/" + name)
		if !ok {
			t.Fatalf("expected auth %s to remain in manager", name)
		}
		if !auth.Disabled || auth.Status != coreauth.StatusDisabled {
			t.Fatalf("expected auth %s to be disabled after concurrent delete storm, got disabled=%v status=%q", name, auth.Disabled, auth.Status)
		}
	}

	listRec := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRec)
	listReq := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	listCtx.Request = listReq
	h.ListAuthFiles(listCtx)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d with body %s", http.StatusOK, listRec.Code, listRec.Body.String())
	}
	var listPayload map[string]any
	if errUnmarshal := json.Unmarshal(listRec.Body.Bytes(), &listPayload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	filesRaw, ok := listPayload["files"].([]any)
	if !ok {
		t.Fatalf("expected files array, payload: %#v", listPayload)
	}
	if len(filesRaw) != 0 {
		t.Fatalf("expected concurrent delete storm auths to be hidden from list, got %d entries", len(filesRaw))
	}
}
