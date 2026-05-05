package executor

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestSendStreamChunkWritesWhenReceiverAvailable(t *testing.T) {
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	if !sendStreamChunk(context.Background(), out, cliproxyexecutor.StreamChunk{Payload: []byte("ok")}) {
		t.Fatalf("expected send to succeed")
	}
	chunk := <-out
	if string(chunk.Payload) != "ok" {
		t.Fatalf("payload = %q, want ok", string(chunk.Payload))
	}
}

func TestSendStreamChunkStopsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan cliproxyexecutor.StreamChunk)
	if sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: []byte("blocked")}) {
		t.Fatalf("expected canceled context to stop send")
	}
}
