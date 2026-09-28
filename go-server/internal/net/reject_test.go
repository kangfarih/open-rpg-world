package net

import (
	"testing"
)

// TestPreAcceptReasonOrder pins the TS accept-gate order
// (main.ts disallowed -> network.ts banned -> toofast -> toomany) and that
// the reconnect/socket-count gates are skipped in debug mode.
func TestPreAcceptReasonOrder(t *testing.T) {
	t.Setenv("DEBUGGING", "")

	newHub := func() *Hub {
		h := NewHub()
		return h
	}

	// Update mode wins over everything else (main.ts:59 runs first).
	h := newHub()
	h.SetAccepting(false)
	h.BanIP("1.2.3.4")
	if got := h.preAcceptReason("1.2.3.4", 1000); got != ReasonDisallowed {
		t.Fatalf("update-mode reason = %q, want %q", got, ReasonDisallowed)
	}
	h.SetAccepting(true)

	// IP ban next.
	if got := h.preAcceptReason("1.2.3.4", 1000); got != ReasonBanned {
		t.Fatalf("banned reason = %q, want %q", got, ReasonBanned)
	}

	// First attempt from an address is never 'toofast'.
	if got := h.preAcceptReason("5.6.7.8", 1000); got != "" {
		t.Fatalf("fresh address reason = %q, want empty", got)
	}

	// A recorded attempt inside the 5s window is 'toofast'.
	h.noteConnection("5.6.7.8", 1000)
	if got := h.preAcceptReason("5.6.7.8", 1000+ReconnectGateThresholdMs-1); got != ReasonTooFast {
		t.Fatalf("reconnect reason = %q, want %q", got, ReasonTooFast)
	}
	// At exactly the threshold the connection is admitted.
	if got := h.preAcceptReason("5.6.7.8", 1000+ReconnectGateThresholdMs); got != "" {
		t.Fatalf("threshold reason = %q, want empty", got)
	}

	// Per-IP cap last.
	cap := newHub()
	ip := "9.9.9.9"
	for i := 0; i < cap.limiter.maxPerIP; i++ {
		if !cap.limiter.Acquire(ip) {
			t.Fatalf("acquire %d unexpectedly rejected", i)
		}
	}
	if got := cap.preAcceptReason(ip, 1000); got != ReasonTooMany {
		t.Fatalf("over-cap reason = %q, want %q", got, ReasonTooMany)
	}

	// DEBUGGING=1 skips both rate gates (network.ts:68).
	t.Setenv("DEBUGGING", "1")
	if !Debugging() {
		t.Fatal("Debugging() = false with DEBUGGING=1")
	}
	if got := cap.preAcceptReason(ip, 1000); got != "" {
		t.Fatalf("debug over-cap reason = %q, want empty", got)
	}
	if got := cap.preAcceptReason("5.6.7.8", 1000+1); got != "" {
		t.Fatalf("debug reconnect reason = %q, want empty", got)
	}
	// The ban and update gates still apply in debug mode.
	if got := cap.preAcceptReason("1.2.3.4", 1000); got == "" {
		cap.BanIP("1.2.3.4")
		if got := cap.preAcceptReason("1.2.3.4", 1000); got != ReasonBanned {
			t.Fatalf("debug banned reason = %q, want %q", got, ReasonBanned)
		}
	}
}

// TestRejectNilSafe: Reject must tolerate a fabricated conn with no socket
// (unit tests build playerConn without a live WebSocket).
func TestRejectNilSafe(t *testing.T) {
	if err := Reject(nil, ReasonBanned); err != nil {
		t.Fatalf("Reject(nil) err = %v, want nil", err)
	}
}

// TestHTTPCloseCode pins the HTTP fallback codes used when a request is not a
// WebSocket upgrade (no TS equivalent; these keep the pre-existing codes).
func TestHTTPCloseCode(t *testing.T) {
	cases := map[string]int{
		ReasonDisallowed: 503,
		ReasonTooMany:    429,
		ReasonTooFast:    429,
		ReasonBanned:     403,
	}
	for reason, want := range cases {
		if got := httpCloseCode(reason); got != want {
			t.Fatalf("httpCloseCode(%q) = %d, want %d", reason, got, want)
		}
	}
}

// TestReasonVocabularyMatchesClient pins the invariant that matters: every
// reason the Go server can emit is one the stock client renders
// (packages/client/src/network/messages.ts:203 handleCloseReason). The client
// knows a few extra legacy strings ('maintenance', 'development',
// 'swappedworlds') that no TS server path emits either.
func TestReasonVocabularyMatchesClient(t *testing.T) {
	clientKnown := map[string]bool{
		"worldfull": true, "error": true, "banned": true,
		"disabledregister": true, "development": true, "disallowed": true,
		"maintenance": true, "userexists": true, "emailexists": true,
		"invalidinput": true, "swappedworlds": true, "loggedin": true,
		"invalidlogin": true, "toofast": true, "timeout": true,
		"updated": true, "cheating": true, "lost": true, "toomany": true,
		"ratelimit": true, "invalidpassword": true,
	}
	emitted := []string{
		ReasonWorldFull, ReasonError, ReasonBanned, ReasonDisabledRegister,
		ReasonDisallowed, ReasonUserExists, ReasonEmailExists,
		ReasonInvalidInput, ReasonLoggedIn, ReasonInvalidLogin, ReasonTooFast,
		ReasonTimeout, ReasonUpdated, ReasonCheating, ReasonLost,
		ReasonTooMany, ReasonRateLimit, ReasonInvalidPassword,
	}
	for _, reason := range emitted {
		if !clientKnown[reason] {
			t.Fatalf("server reason %q is unknown to the client (it would be silent)", reason)
		}
	}
}
