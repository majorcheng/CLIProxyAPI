package logging

import (
	"context"
	"net/http"
	"testing"
)

func TestResponseHeadersHolderClonesOnSetAndGet(t *testing.T) {
	ctx := WithResponseHeadersHolder(context.Background())
	headers := http.Header{"X-Usage": []string{"first"}}

	SetResponseHeaders(ctx, headers)
	headers.Set("X-Usage", "mutated")

	got := GetResponseHeaders(ctx)
	if got.Get("X-Usage") != "first" {
		t.Fatalf("stored header = %q, want first", got.Get("X-Usage"))
	}

	got.Set("X-Usage", "changed-by-caller")
	again := GetResponseHeaders(ctx)
	if again.Get("X-Usage") != "first" {
		t.Fatalf("header clone was mutated through getter: %q", again.Get("X-Usage"))
	}
}
