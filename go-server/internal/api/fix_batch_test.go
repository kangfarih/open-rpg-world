package api

import (
	"testing"
)

func TestAddrFromEnvLoopbackDefault(t *testing.T) {
	t.Setenv("API_PORT", "8080")
	addr, ok := AddrFromEnv()
	if !ok {
		t.Fatal("AddrFromEnv with API_PORT=8080 ok = false, want true")
	}
	if addr != "127.0.0.1:8080" {
		t.Fatalf("AddrFromEnv(8080) = %q, want loopback 127.0.0.1:8080", addr)
	}
	// Explicit addrs still opt into wider binds.
	for _, tc := range []struct{ in, want string }{
		{":8080", ":8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"0.0.0.0:8080", "0.0.0.0:8080"},
	} {
		t.Setenv("API_PORT", tc.in)
		got, ok := AddrFromEnv()
		if !ok || got != tc.want {
			t.Fatalf("AddrFromEnv(%q) = %q,%v; want %q,true", tc.in, got, ok, tc.want)
		}
	}
}
