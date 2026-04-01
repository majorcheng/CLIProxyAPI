package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type sharedExitTestExecutor struct {
	id string

	mu           sync.Mutex
	executeCalls []string
	countCalls   []string
	streamCalls  []string

	executeErrors map[string]error
	countErrors   map[string]error
	streamErrors  map[string]error
}

func (e *sharedExitTestExecutor) Identifier() string { return e.id }

func (e *sharedExitTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	authID := testAuthID(auth)
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, authID)
	err := e.executeErrors[authID]
	e.mu.Unlock()
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(authID)}, nil
}

func (e *sharedExitTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	authID := testAuthID(auth)
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, authID)
	err := e.streamErrors[authID]
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(authID)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: make(http.Header), Chunks: chunks}, nil
}

func (e *sharedExitTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *sharedExitTestExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	authID := testAuthID(auth)
	e.mu.Lock()
	e.countCalls = append(e.countCalls, authID)
	err := e.countErrors[authID]
	e.mu.Unlock()
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(authID)}, nil
}

func (e *sharedExitTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *sharedExitTestExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeCalls))
	copy(out, e.executeCalls)
	return out
}

func (e *sharedExitTestExecutor) CountCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.countCalls))
	copy(out, e.countCalls)
	return out
}

func (e *sharedExitTestExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.streamCalls))
	copy(out, e.streamCalls)
	return out
}

func testAuthID(auth *Auth) string {
	if auth == nil {
		return ""
	}
	return auth.ID
}

func registerSharedExitModel(t *testing.T, authID, provider, model string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
}

func mustRegisterSharedExitAuth(t *testing.T, m *Manager, auth *Auth) {
	t.Helper()
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth %s: %v", auth.ID, err)
	}
}

func priorityZeroOAuthAuth(id, provider, email string, registeredAt time.Time) *Auth {
	return &Auth{
		ID:       id,
		Provider: provider,
		Status:   StatusActive,
		Metadata: map[string]any{
			"email":                      email,
			FirstRegisteredAtMetadataKey: registeredAt.Format(time.RFC3339Nano),
		},
		Attributes: map[string]string{
			"priority": "0",
		},
	}
}

func compatFallbackAuth(id, provider string, priority int) *Auth {
	return &Auth{
		ID:       id,
		Provider: provider,
		Status:   StatusActive,
		Attributes: map[string]string{
			"priority": strconv.Itoa(priority),
			"api_key":  provider + "-key",
		},
	}
}

func TestManager_Execute_PriorityZeroOAuthNetworkJitterFallsBackToLowerPriorityCompat(t *testing.T) {
	const model = "shared-exit-model"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{SharedExitPriorityZeroOAuthNetworkJitterFallback: true})

	codexExec := &sharedExitTestExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"token-a": errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": dial tcp 116.89.243.8:443: connect: connection timed out`),
		},
	}
	compatAExec := &sharedExitTestExecutor{id: "compat-a"}
	compatBExec := &sharedExitTestExecutor{id: "compat-b"}
	manager.RegisterExecutor(codexExec)
	manager.RegisterExecutor(compatAExec)
	manager.RegisterExecutor(compatBExec)

	now := time.Now().UTC()
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-a", "codex", "a@example.com", now.Add(-2*time.Minute)))
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-b", "codex", "b@example.com", now.Add(-1*time.Minute)))
	mustRegisterSharedExitAuth(t, manager, compatFallbackAuth("compat-a-1", "compat-a", -1))
	mustRegisterSharedExitAuth(t, manager, compatFallbackAuth("compat-b-1", "compat-b", -1))

	for _, item := range []struct{ id, provider string }{
		{"token-a", "codex"},
		{"token-b", "codex"},
		{"compat-a-1", "compat-a"},
		{"compat-b-1", "compat-b"},
	} {
		registerSharedExitModel(t, item.id, item.provider, model)
	}

	resp, err := manager.Execute(context.Background(), []string{"codex", "compat-a", "compat-b"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := string(resp.Payload); got != "compat-a-1" {
		t.Fatalf("Execute() payload = %q, want %q", got, "compat-a-1")
	}
	if got := codexExec.ExecuteCalls(); len(got) != 1 || got[0] != "token-a" {
		t.Fatalf("codex execute calls = %v, want [token-a]", got)
	}
	if got := compatAExec.ExecuteCalls(); len(got) != 1 || got[0] != "compat-a-1" {
		t.Fatalf("compat-a execute calls = %v, want [compat-a-1]", got)
	}
	if got := compatBExec.ExecuteCalls(); len(got) != 0 {
		t.Fatalf("compat-b execute calls = %v, want empty", got)
	}
}

func TestManager_Execute_PriorityZeroOAuthHTTP500KeepsCurrentLayerRetry(t *testing.T) {
	const model = "shared-exit-http500-model"
	manager := NewManager(nil, &FillFirstSelector{}, nil)

	codexExec := &sharedExitTestExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"token-a": &Error{HTTPStatus: http.StatusInternalServerError, Message: "boom"},
		},
	}
	manager.RegisterExecutor(codexExec)

	now := time.Now().UTC()
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-a", "codex", "a@example.com", now.Add(-2*time.Minute)))
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-b", "codex", "b@example.com", now.Add(-1*time.Minute)))
	registerSharedExitModel(t, "token-a", "codex", model)
	registerSharedExitModel(t, "token-b", "codex", model)

	resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := string(resp.Payload); got != "token-b" {
		t.Fatalf("Execute() payload = %q, want %q", got, "token-b")
	}
	if got := codexExec.ExecuteCalls(); len(got) != 2 || got[0] != "token-a" || got[1] != "token-b" {
		t.Fatalf("codex execute calls = %v, want [token-a token-b]", got)
	}
}

func TestManager_Execute_PriorityNonZeroOAuthNetworkJitterKeepsCurrentLayerRetry(t *testing.T) {
	const model = "shared-exit-nonzero-model"
	manager := NewManager(nil, &FillFirstSelector{}, nil)

	codexExec := &sharedExitTestExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"token-a": errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": dial tcp 116.89.243.8:443: connect: connection timed out`),
		},
	}
	manager.RegisterExecutor(codexExec)

	now := time.Now().UTC()
	authA := priorityZeroOAuthAuth("token-a", "codex", "a@example.com", now.Add(-2*time.Minute))
	authA.Attributes["priority"] = "1"
	authB := priorityZeroOAuthAuth("token-b", "codex", "b@example.com", now.Add(-1*time.Minute))
	authB.Attributes["priority"] = "1"
	mustRegisterSharedExitAuth(t, manager, authA)
	mustRegisterSharedExitAuth(t, manager, authB)
	registerSharedExitModel(t, "token-a", "codex", model)
	registerSharedExitModel(t, "token-b", "codex", model)

	resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := string(resp.Payload); got != "token-b" {
		t.Fatalf("Execute() payload = %q, want %q", got, "token-b")
	}
	if got := codexExec.ExecuteCalls(); len(got) != 2 || got[0] != "token-a" || got[1] != "token-b" {
		t.Fatalf("codex execute calls = %v, want [token-a token-b]", got)
	}
}

func TestManager_ExecuteCount_PriorityZeroOAuthNetworkJitterFallsBackToLowerPriorityCompat(t *testing.T) {
	const model = "shared-exit-count-model"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{SharedExitPriorityZeroOAuthNetworkJitterFallback: true})

	codexExec := &sharedExitTestExecutor{
		id: "codex",
		countErrors: map[string]error{
			"token-a": errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": dial tcp 116.89.243.8:443: connect: connection timed out`),
		},
	}
	compatAExec := &sharedExitTestExecutor{id: "compat-a"}
	manager.RegisterExecutor(codexExec)
	manager.RegisterExecutor(compatAExec)

	now := time.Now().UTC()
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-a", "codex", "a@example.com", now.Add(-2*time.Minute)))
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-b", "codex", "b@example.com", now.Add(-1*time.Minute)))
	mustRegisterSharedExitAuth(t, manager, compatFallbackAuth("compat-a-1", "compat-a", -1))
	for _, item := range []struct{ id, provider string }{
		{"token-a", "codex"},
		{"token-b", "codex"},
		{"compat-a-1", "compat-a"},
	} {
		registerSharedExitModel(t, item.id, item.provider, model)
	}

	resp, err := manager.ExecuteCount(context.Background(), []string{"codex", "compat-a"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteCount() error = %v", err)
	}
	if got := string(resp.Payload); got != "compat-a-1" {
		t.Fatalf("ExecuteCount() payload = %q, want %q", got, "compat-a-1")
	}
	if got := codexExec.CountCalls(); len(got) != 1 || got[0] != "token-a" {
		t.Fatalf("codex count calls = %v, want [token-a]", got)
	}
	if got := compatAExec.CountCalls(); len(got) != 1 || got[0] != "compat-a-1" {
		t.Fatalf("compat-a count calls = %v, want [compat-a-1]", got)
	}
}

func TestManager_ExecuteStream_PriorityZeroOAuthNetworkJitterFallsBackToLowerPriorityCompat(t *testing.T) {
	const model = "shared-exit-stream-model"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{SharedExitPriorityZeroOAuthNetworkJitterFallback: true})

	codexExec := &sharedExitTestExecutor{
		id: "codex",
		streamErrors: map[string]error{
			"token-a": errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": dial tcp 116.89.243.8:443: connect: connection timed out`),
		},
	}
	compatAExec := &sharedExitTestExecutor{id: "compat-a"}
	manager.RegisterExecutor(codexExec)
	manager.RegisterExecutor(compatAExec)

	now := time.Now().UTC()
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-a", "codex", "a@example.com", now.Add(-2*time.Minute)))
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-b", "codex", "b@example.com", now.Add(-1*time.Minute)))
	mustRegisterSharedExitAuth(t, manager, compatFallbackAuth("compat-a-1", "compat-a", -1))
	for _, item := range []struct{ id, provider string }{
		{"token-a", "codex"},
		{"token-b", "codex"},
		{"compat-a-1", "compat-a"},
	} {
		registerSharedExitModel(t, item.id, item.provider, model)
	}

	streamResult, err := manager.ExecuteStream(context.Background(), []string{"codex", "compat-a"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if streamResult == nil {
		t.Fatal("ExecuteStream() result = nil")
	}
	chunk, ok := <-streamResult.Chunks
	if !ok {
		t.Fatal("ExecuteStream() chunks closed before payload")
	}
	if got := string(chunk.Payload); got != "compat-a-1" {
		t.Fatalf("ExecuteStream() payload = %q, want %q", got, "compat-a-1")
	}
	if got := codexExec.StreamCalls(); len(got) != 1 || got[0] != "token-a" {
		t.Fatalf("codex stream calls = %v, want [token-a]", got)
	}
	if got := compatAExec.StreamCalls(); len(got) != 1 || got[0] != "compat-a-1" {
		t.Fatalf("compat-a stream calls = %v, want [compat-a-1]", got)
	}
}

func TestManager_Execute_PriorityZeroOAuthNetworkJitterFallbackDisabledKeepsCurrentLayerRetry(t *testing.T) {
	const model = "shared-exit-disabled-model"
	manager := NewManager(nil, &FillFirstSelector{}, nil)

	codexExec := &sharedExitTestExecutor{
		id: "codex",
		executeErrors: map[string]error{
			"token-a": errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": dial tcp 116.89.243.8:443: connect: connection timed out`),
		},
	}
	manager.RegisterExecutor(codexExec)

	now := time.Now().UTC()
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-a", "codex", "a@example.com", now.Add(-2*time.Minute)))
	mustRegisterSharedExitAuth(t, manager, priorityZeroOAuthAuth("token-b", "codex", "b@example.com", now.Add(-1*time.Minute)))
	registerSharedExitModel(t, "token-a", "codex", model)
	registerSharedExitModel(t, "token-b", "codex", model)

	resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := string(resp.Payload); got != "token-b" {
		t.Fatalf("Execute() payload = %q, want %q", got, "token-b")
	}
	if got := codexExec.ExecuteCalls(); len(got) != 2 || got[0] != "token-a" || got[1] != "token-b" {
		t.Fatalf("codex execute calls = %v, want [token-a token-b]", got)
	}
}
