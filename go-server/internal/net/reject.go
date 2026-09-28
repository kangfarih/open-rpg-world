// Package net — reject-with-reason parity (TS `connection.reject(reason)`).
//
// TS closes every rejected connection the same way:
//
//	connection.close(reason) -> this.socket.end(1010, reason)
//	(packages/server/src/network/connection.ts:69)
//
// and the stock client only surfaces a server rejection when the close code is
// 1010 and the reason is one of its known strings:
//
//	if (event.code === 1010 && event.reason) this.messages.handleCloseReason(event.reason);
//	(packages/client/src/network/socket.ts:73 -> messages.ts:203)
//
// A plain text frame is NOT visible to the client: socket.ts:100 ignores any
// message that does not start with '['. Before this file the Go server wrote a
// bare "ban" frame (boot.go) which the client silently dropped, so a banned
// player saw only a generic disconnect.
//
// This file is ADDITIVE: it introduces Reject + the reason vocabulary and the
// reconnect gate, without changing the existing Accept/send paths beyond the
// post-upgrade rejection order documented in transport.go.
package net

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// RejectCloseCode is the close code every rejection uses (TS socket.end(1010)).
const RejectCloseCode = 1010

// ReconnectGateThresholdMs mirrors TS Network.timeoutThreshold = 5000
// (packages/server/src/network/network.ts:20): a second connection from the
// same IP inside the window is rejected with 'toofast'.
const ReconnectGateThresholdMs = int64(5000)

// Close-reason vocabulary, mirrored from the TS reject call sites so the
// client's handleCloseReason switch matches exactly. Sources:
//   - incoming.ts: lost/invalidpassword/loggedin/disabledregister/updated
//   - mongodb.ts login/register: invalidlogin/invalidinput/emailexists/userexists
//   - network.ts: banned/toofast/toomany
//   - uws.ts: ratelimit
//   - main.ts: disallowed/worldfull
//   - player.ts/handler.ts: banned/cheating/error
//   - connection.ts: timeout
const (
	ReasonLost             = "lost"
	ReasonUpdated          = "updated"
	ReasonInvalidLogin     = "invalidlogin"
	ReasonInvalidPassword  = "invalidpassword"
	ReasonInvalidInput     = "invalidinput"
	ReasonUserExists       = "userexists"
	ReasonEmailExists      = "emailexists"
	ReasonLoggedIn         = "loggedin"
	ReasonDisabledRegister = "disabledregister"
	ReasonDisallowed       = "disallowed"
	ReasonWorldFull        = "worldfull"
	ReasonBanned           = "banned"
	ReasonTooFast          = "toofast"
	ReasonTooMany          = "toomany"
	ReasonRateLimit        = "ratelimit"
	ReasonTimeout          = "timeout"
	ReasonCheating         = "cheating"
	ReasonError            = "error"
)

// Debugging reports the TS config.debugging flag (DEBUGGING env). TS skips the
// reconnect/socket-count gates in debug mode (network.ts:68); the same escape
// hatch is used here so harnesses can dial repeatedly.
func Debugging() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEBUGGING"))) {
	case "", "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// Reject closes ws with the TS close code and reason (connection.reject
// parity). The close frame is written under the hub write lock — gorilla
// forbids concurrent writers — and the socket is then closed, which also ends
// the conn's read loop and runs the normal disconnect fan-out.
func (h *Hub) Reject(ws *websocket.Conn, reason string) error {
	if ws == nil {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	h.writeMu.Lock()
	_ = ws.SetWriteDeadline(deadline)
	err := ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(RejectCloseCode, reason), deadline)
	h.writeMu.Unlock()
	_ = ws.Close()
	log.Printf("reject: reason=%s", reason)
	return err
}

// Reject closes the connection with the TS close reason (package-level form
// over DefaultHub).
func Reject(ws *websocket.Conn, reason string) error { return DefaultHub.Reject(ws, reason) }

// lastConnMs reports the previous connection attempt time for ip (0 = never).
func (h *Hub) lastConnMs(ip string) int64 {
	h.reconnMu.Lock()
	defer h.reconnMu.Unlock()
	return h.lastConn[ip]
}

// noteConnection records a connection attempt for ip (TS
// socketHandler.updateLastTime, called for every non-banned attempt).
func (h *Hub) noteConnection(ip string, nowMs int64) {
	h.reconnMu.Lock()
	defer h.reconnMu.Unlock()
	h.lastConn[ip] = nowMs
}

// banned reports whether ip is in the ban set.
func (h *Hub) banned(ip string) bool {
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	return h.ipBans[ip]
}

// preAcceptReason mirrors the TS accept-order rejection checks and returns ""
// when the connection may proceed:
//
//	main.ts:59  !ready || !allowConnections -> 'disallowed'
//	network.ts:59 isIpBanned               -> 'banned'
//	network.ts:73 reconnect inside 5s      -> 'toofast'   (skipped when debugging)
//	network.ts:76 isMaxConnections         -> 'toomany'   (skipped when debugging)
func (h *Hub) preAcceptReason(ip string, nowMs int64) string {
	if !h.accepting.Load() {
		return ReasonDisallowed
	}
	if h.banned(ip) {
		return ReasonBanned
	}
	if Debugging() {
		return ""
	}
	// TS reads the difference before updating the timestamp, so the first
	// attempt from an address (no stored value) always passes.
	if prev := h.lastConnMs(ip); prev != 0 && nowMs-prev < ReconnectGateThresholdMs {
		return ReasonTooFast
	}
	if h.limiter.Count(ip) >= h.limiter.maxPerIP {
		return ReasonTooMany
	}
	return ""
}
