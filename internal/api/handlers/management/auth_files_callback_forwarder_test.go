package management

import "testing"

func TestCallbackForwarderListenAddr_UsesAllInterfaces(t *testing.T) {
	if got := callbackForwarderListenAddr(1455); got != "0.0.0.0:1455" {
		t.Fatalf("callbackForwarderListenAddr(1455) = %q, want %q", got, "0.0.0.0:1455")
	}
}
