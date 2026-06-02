package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body)

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := gjson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanDoesNotSignalOnRecoverableInvalidate(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "sess-recoverable-" + t.Name()
	defer exec.CloseExecutionSession(sessionID)
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	if disconnectCh == nil {
		t.Fatal("expected disconnect channel")
	}

	sess := exec.getOrCreateSession(sessionID)
	if sess == nil {
		t.Fatal("expected session")
	}
	sess.connMu.Lock()
	sess.conn = conn
	sess.authID = "auth-1"
	sess.wsURL = "ws://example.test/responses"
	sess.readerConn = conn
	sess.connMu.Unlock()

	upstreamErr := errors.New("upstream gone")
	exec.invalidateUpstreamConn(sess, conn, "test_invalidate", upstreamErr)

	select {
	case errRead, ok := <-disconnectCh:
		t.Fatalf("recoverable invalidate signaled disconnect: ok=%v err=%v", ok, errRead)
	default:
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanSignalsOnIdleReadDisconnect(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	accepted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		close(accepted)
		_ = conn.Close()
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	<-accepted

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "sess-idle-" + t.Name()
	defer exec.CloseExecutionSession(sessionID)
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	sess := exec.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = conn
	sess.readerConn = conn
	sess.connMu.Unlock()

	go exec.readUpstreamLoop(sess, conn)

	select {
	case errRead, ok := <-disconnectCh:
		if !ok || errRead == nil {
			t.Fatalf("disconnect signal = ok:%v err:%v, want error before close", ok, errRead)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for idle disconnect signal")
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanSurvivesExecutorRebind(t *testing.T) {
	sessionID := "sess-rebind-" + t.Name()
	first := NewCodexWebsocketsExecutor(&config.Config{})
	disconnectCh := first.UpstreamDisconnectChan(sessionID)
	if disconnectCh == nil {
		t.Fatal("expected disconnect channel")
	}

	first.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)

	second := NewCodexWebsocketsExecutor(&config.Config{})
	defer second.CloseExecutionSession(sessionID)
	if got := second.UpstreamDisconnectChan(sessionID); got != disconnectCh {
		t.Fatalf("disconnect channel changed across executor rebind")
	}

	upstreamErr := errors.New("upstream gone after rebind")
	second.getOrCreateSession(sessionID).notifyUpstreamDisconnect(upstreamErr)
	select {
	case errRead, ok := <-disconnectCh:
		if !ok || errRead == nil || errRead.Error() != upstreamErr.Error() {
			t.Fatalf("disconnect signal = ok:%v err:%v, want %v", ok, errRead, upstreamErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for rebind disconnect signal")
	}
}

func TestApplyCodexWebsocketHeadersDefaultsToCurrentResponsesBeta(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want %s", got, codexUserAgent)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := headers.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
}

func TestApplyCodexWebsocketHeadersUsesConfigDefaultsForOAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "my-codex-client/1.0",
			BetaFeatures: "feature-a,feature-b",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "my-codex-client/1.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "my-codex-client/1.0")
	}
	if got := headers.Get("x-codex-beta-features"); got != "feature-a,feature-b" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "feature-a,feature-b")
	}
	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
}

func TestApplyCodexWebsocketHeadersUsesDefaultWhenIncomingUAMissesCodex(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})
	headers := http.Header{}
	headers.Set("User-Agent", "existing-ua")
	headers.Set("X-Codex-Beta-Features", "existing-beta")

	got := applyCodexWebsocketHeaders(ctx, headers, auth, "", cfg)

	if gotVal := got.Get("User-Agent"); gotVal != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", gotVal, "config-ua")
	}
	if gotVal := got.Get("x-codex-beta-features"); gotVal != "existing-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", gotVal, "existing-beta")
	}
	if _, ok := auth.Metadata["cli_ua"]; ok {
		t.Fatalf("cli_ua should not be written for non-codex user-agent")
	}
}

func TestApplyCodexWebsocketHeadersConfigUserAgentOverridesClientHeader(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := headers.Get("x-codex-beta-features"); got != "client-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "client-beta")
	}
}

func TestApplyCodexWebsocketHeadersIgnoresConfigForAPIKeyAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "sk-test", cfg)

	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want %s", got, codexUserAgent)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersAuthFileUserAgentOverridesAllSources(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":  "user@example.com",
			"cli_ua": "auth-file-ua",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	})
	headers := http.Header{}
	headers.Set("User-Agent", "existing-ua")

	got := applyCodexWebsocketHeaders(ctx, headers, auth, "", cfg)

	if gotVal := got.Get("User-Agent"); gotVal != "auth-file-ua" {
		t.Fatalf("User-Agent = %s, want %s", gotVal, "auth-file-ua")
	}
}

func TestApplyCodexWebsocketHeadersCapturesFirstCodexUserAgent(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent": "codex-cli/1.2.3",
	})

	got := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if gotVal := got.Get("User-Agent"); gotVal != "codex-cli/1.2.3" {
		t.Fatalf("User-Agent = %s, want %s", gotVal, "codex-cli/1.2.3")
	}
	if gotVal, _ := auth.Metadata["cli_ua"].(string); gotVal != "codex-cli/1.2.3" {
		t.Fatalf("cli_ua = %q, want %q", gotVal, "codex-cli/1.2.3")
	}
}

func TestApplyCodexHeadersUsesConfigUserAgentForOAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
		"Originator": "client-originator",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := req.Header.Get("Originator"); got != "client-originator" {
		t.Fatalf("Originator = %s, want %s", got, "client-originator")
	}
	if got := req.Header.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %s, want %s", got, "text/event-stream")
	}

	var meta struct {
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
		Sandbox   string `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(req.Header.Get("X-Codex-Turn-Metadata")), &meta); err != nil {
		t.Fatalf("X-Codex-Turn-Metadata unmarshal error = %v", err)
	}
	if meta.SessionID != req.Header.Get("Session_id") {
		t.Fatalf("turn metadata session_id = %s, want %s", meta.SessionID, req.Header.Get("Session_id"))
	}
	if meta.TurnID == "" {
		t.Fatal("turn metadata turn_id is empty")
	}
	if meta.Sandbox != codexSandbox {
		t.Fatalf("turn metadata sandbox = %s, want %s", meta.Sandbox, codexSandbox)
	}
}

func TestCodexAutoExecutorExecuteStream_WebsocketStripsPrefixedModelFromOutboundRequest(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	reqPathCh := make(chan string, 1)
	reqBodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read websocket request: %v", err)
			return
		}
		if msgType != websocket.TextMessage {
			t.Errorf("message type = %d, want %d", msgType, websocket.TextMessage)
			return
		}
		reqPathCh <- r.URL.Path
		reqBodyCh <- payload

		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_ws"}}`)); err != nil {
			t.Errorf("write websocket created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(codexCompletedEventJSON("resp_ws", "gpt-5.4", "ok-ws"))); err != nil {
			t.Errorf("write websocket completed event: %v", err)
		}
	}))
	defer server.Close()

	auth := newCodexTestAuth(server.URL, "ws-key")
	auth.Prefix = "team"
	auth.Attributes["websockets"] = "true"

	executor := NewCodexAutoExecutor(&config.Config{})
	ctx, cancel := context.WithTimeout(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), 5*time.Second)
	defer cancel()

	result, err := executor.ExecuteStream(
		ctx,
		auth,
		cliproxyexecutor.Request{
			Model: "gpt-5.4",
			Payload: []byte(`{
				"model":"team/gpt-5.4",
				"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
				"stream":true
			}`),
		},
		cliproxyexecutor.Options{
			Stream:       true,
			SourceFormat: sdktranslator.FromString("openai-response"),
			Metadata: map[string]any{
				cliproxyexecutor.RequestedModelMetadataKey: "team/gpt-5.4",
			},
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	select {
	case path := <-reqPathCh:
		if path != "/responses" {
			t.Fatalf("websocket path = %q, want %q", path, "/responses")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for websocket request path")
	}

	select {
	case payload := <-reqBodyCh:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("websocket request type = %q, want %q", got, "response.create")
		}
		if got := gjson.GetBytes(payload, "model").String(); got != "gpt-5.4" {
			t.Fatalf("websocket request model = %q, want %q", got, "gpt-5.4")
		}
		if got := gjson.GetBytes(payload, "model").String(); got == "team/gpt-5.4" {
			t.Fatalf("websocket request leaked prefixed model: %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for websocket request body")
	}
}

func TestCodexWebsocketsExecutePatchesCompletedOutputFromItemDone(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read websocket request: %v", errRead)
			return
		}
		itemDone := `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_ws","role":"assistant","content":[{"type":"output_text","text":"ws-patched"}]}}`
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(itemDone)); errWrite != nil {
			t.Errorf("write output item done: %v", errWrite)
			return
		}
		completed := `{"type":"response.completed","response":{"id":"resp_ws","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(completed)); errWrite != nil {
			t.Errorf("write completed event: %v", errWrite)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(&config.Config{})
	resp, err := executor.Execute(
		context.Background(),
		newCodexTestAuth(server.URL, "ws-patch-key"),
		cliproxyexecutor.Request{
			Model:   "gpt-5.4",
			Payload: []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(string(resp.Payload), "ws-patched") {
		t.Fatalf("response payload missing patched output item: %s", string(resp.Payload))
	}
}

func TestCodexWebsocketsExecuteInjectsReplayAfterSessionLock(t *testing.T) {
	t.Parallel()

	encryptedContent := validCodexReplayEncryptedContentForTest(31)
	executionSession := "ws-lock-execution-" + strings.ReplaceAll(t.Name(), "/", "-")
	internalcache.DeleteCodexReasoningReplayItem("gpt-5.4", "execution:"+executionSession)
	t.Cleanup(func() {
		internalcache.DeleteCodexReasoningReplayItem("gpt-5.4", "execution:"+executionSession)
	})

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	firstRead := make(chan struct{})
	allowFirstComplete := make(chan struct{})
	secondBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		for index := 0; index < 2; index++ {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Errorf("read websocket request %d: %v", index+1, errRead)
				return
			}
			if index == 0 {
				close(firstRead)
				<-allowFirstComplete
				completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.4","status":"completed","output":[{"type":"reasoning","summary":[],"content":null,"encrypted_content":%q}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, encryptedContent)
				if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(completed)); errWrite != nil {
					t.Errorf("write first completed event: %v", errWrite)
				}
				continue
			}
			secondBody <- append([]byte(nil), payload...)
			if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(codexCompletedEventJSON("resp_second", "gpt-5.4", "second"))); errWrite != nil {
				t.Errorf("write second completed event: %v", errWrite)
			}
			return
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(&config.Config{})
	auth := newCodexTestAuth(server.URL, "ws-lock-key")
	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"device_id\":\"device-a\",\"session_id\":\"ws-lock-session\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}],
			"max_tokens":100
		}`),
	}
	newOpts := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatClaude,
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: executionSession,
			},
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := executor.Execute(context.Background(), auth, req, newOpts())
		errCh <- err
	}()

	select {
	case <-firstRead:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first websocket request")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := executor.Execute(context.Background(), auth, req, newOpts())
		errCh <- err
	}()
	time.Sleep(100 * time.Millisecond)
	close(allowFirstComplete)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	}

	select {
	case payload := <-secondBody:
		if got := gjson.GetBytes(payload, `input.#(type=="reasoning").encrypted_content`).String(); got != encryptedContent {
			t.Fatalf("second websocket body replay encrypted_content = %q, want %q; body=%s", got, encryptedContent, string(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second websocket body")
	}
}

func TestApplyCodexHeadersPassesThroughClientOriginator(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"Originator": "Codex Desktop",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
}

func TestApplyCodexHeadersAuthFileUserAgentOverridesAllSources(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent: "config-ua",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":  "user@example.com",
			"cli_ua": "auth-file-ua",
		},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	}))
	req.Header.Set("User-Agent", "existing-ua")

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "auth-file-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "auth-file-ua")
	}
}

func TestApplyCodexHeadersCapturesFirstCodexUserAgent(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "codex-tui/0.118.0",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("User-Agent"); got != "codex-tui/0.118.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "codex-tui/0.118.0")
	}
	if got, _ := auth.Metadata["cli_ua"].(string); got != "codex-tui/0.118.0" {
		t.Fatalf("cli_ua = %q, want %q", got, "codex-tui/0.118.0")
	}
}

func TestApplyCodexHeadersPassesThroughClientIdentityMetadata(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := req.Header.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
}

func TestApplyCodexHeadersGeneratesTurnMetadataByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	applyCodexHeaders(req, nil, "oauth-token", true, nil)

	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := req.Header.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %s, want %s", got, "text/event-stream")
	}
	if got := req.Header.Get("Connection"); got != "close" {
		t.Fatalf("Connection = %s, want %s", got, "close")
	}
	if !req.Close {
		t.Fatal("req.Close = false, want true")
	}
	if got := req.Header.Get("X-Client-Request-Id"); got == "" {
		t.Fatal("X-Client-Request-Id is empty")
	}
	if got := req.Header.Get("Session_id"); got == "" {
		t.Fatal("Session_id is empty")
	}

	var meta struct {
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
		Sandbox   string `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(req.Header.Get("X-Codex-Turn-Metadata")), &meta); err != nil {
		t.Fatalf("X-Codex-Turn-Metadata unmarshal error = %v", err)
	}
	if meta.SessionID != req.Header.Get("Session_id") {
		t.Fatalf("turn metadata session_id = %s, want %s", meta.SessionID, req.Header.Get("Session_id"))
	}
	if meta.TurnID == "" {
		t.Fatal("turn metadata turn_id is empty")
	}
	if meta.Sandbox != codexSandbox {
		t.Fatalf("turn metadata sandbox = %s, want %s", meta.Sandbox, codexSandbox)
	}
}

func contextWithGinHeaders(headers map[string]string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ginCtx.Request.Header = make(http.Header, len(headers))
	for key, value := range headers {
		ginCtx.Request.Header.Set(key, value)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestNewProxyAwareWebsocketDialerDirectDisablesProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy != nil {
		t.Fatal("expected websocket proxy function to be nil for direct mode")
	}
}
