package executor

import (
	"testing"
	"time"
)

func TestParseRetryDelaySupportsSecondsMessage(t *testing.T) {
	retryAfter, err := parseRetryDelay([]byte(`{"error":{"message":"Your quota will reset after 45s."}}`))
	if err != nil {
		t.Fatalf("parseRetryDelay() error = %v", err)
	}
	if retryAfter == nil || *retryAfter != 45*time.Second {
		t.Fatalf("retryAfter = %v, want %s", retryAfter, 45*time.Second)
	}
}

func TestParseRetryDelaySupportsHumanDurationMessage(t *testing.T) {
	retryAfter, err := parseRetryDelay([]byte(`{"error":{"message":"Your quota will reset after 1h43m56s."}}`))
	if err != nil {
		t.Fatalf("parseRetryDelay() error = %v", err)
	}
	want := time.Hour + 43*time.Minute + 56*time.Second
	if retryAfter == nil || *retryAfter != want {
		t.Fatalf("retryAfter = %v, want %s", retryAfter, want)
	}
}
