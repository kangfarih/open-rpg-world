// Scripted WS check for the login identity layer (phases 1, 3, 4 and 7).
//
// Usage (server must be running; see go-server/README.md):
//
//	SKIP_DATABASE=0 go run ./login   # full credential coverage
//	go run ./login                   # opcode / guest / duplicate coverage
//	PORT=9002 go run ./login         # when 9001 is taken
//
// Asserted against the real close frames the stock client reads
// (socket.ts:73 only surfaces code 1010 plus a known reason):
//
//	Connected(0) -> Handshake(1) -> guest Login(2,opcode=2) yields Welcome(3)
//	  with a TS guest nameplate (Guest0, Guest1, ...) instead of the old
//	  hardcoded "hero", followed by Map(4).
//	A short password is rejected with close 1010 'invalidpassword' before any
//	  database work (so this check runs in both modes).
//	A Register(2,opcode=1) frame on a live GUEST socket upgrades the session in
//	  place: no close frame, and the player is told progress is now saved.
//	  The TS server has no equivalent — a guest there can never keep progress.
//	A second session for a name that is already online answers 1010 'loggedin'.
//	With SKIP_DATABASE=0 additionally: the registered password logs in, and a
//	  wrong password answers the shared 1010 'invalidlogin'.
//
// The harness opens one session after another, so it waits out the server's
// reconnect gate between dials (TS Network.timeoutThreshold = 5000ms -> close
// 1010 'toofast'; packages/server/src/network/network.ts:73). Boot the server
// with DEBUGGING=1 to skip that gate and the per-dial wait (TS
// config.debugging does the same there).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Packet ids under test.
const (
	packetWelcome = 3
	packetMap     = 4
	packetChat    = 25 // server -> client message/notice frames
)

// CloseReason code every rejection uses (net.RejectCloseCode).
const rejectCloseCode = 1010

var failures int

func check(cond bool, msg string) {
	if cond {
		fmt.Println("PASS:", msg)
		return
	}
	failures++
	fmt.Println("FAIL:", msg)
}

// closeEvent is the close frame the client observes.
type closeEvent struct {
	code   int
	reason string
}

// session is one client socket: a reader goroutine pushes frames and the close
// event onto channels, and the main goroutine only ever reads the collected
// slice.
type session struct {
	conn      *websocket.Conn
	frames    chan []json.RawMessage
	closed    chan closeEvent
	collected [][]json.RawMessage
}

// dialWait is how long a dial waits for the server's reconnect gate window
// (5000ms) to elapse. Only the first dial skips it.
const dialWait = 5200 * time.Millisecond

var dialCount int

// dial connects, completes Connected(0) + Handshake(1), and returns the session.
func dial(port string) *session {
	if dialCount > 0 {
		time.Sleep(dialWait)
	}
	dialCount++
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	s := &session{conn: conn, frames: make(chan []json.RawMessage, 4096), closed: make(chan closeEvent, 4)}
	go func() {
		defer close(s.closed)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				ev := closeEvent{}
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					ev = closeEvent{code: ce.Code, reason: ce.Text}
				}
				s.closed <- ev
				return
			}
			var bulk [][]json.RawMessage
			if err := json.Unmarshal(raw, &bulk); err != nil {
				continue
			}
			for _, f := range bulk {
				select {
				case s.frames <- f:
				default:
				}
			}
		}
	}()
	s.collect(2 * time.Second) // Connected(0)
	s.send(`[1,{"gVer":"0.5.5-beta"}]`)
	s.collect(2 * time.Second) // Handshake(1)
	s.reset()
	return s
}

func (s *session) send(msg string) {
	if err := s.conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		fmt.Println("WRITE FAIL:", err)
		os.Exit(1)
	}
}

// collect accumulates frames for d (or until the close event arrives).
func (s *session) collect(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				return
			}
			s.collected = append(s.collected, f)
		case <-s.closed:
			return
		case <-timer.C:
			return
		}
	}
}

// awaitClose waits for the socket to close, returning the close event and
// whether it arrived. Frames that land first are still collected.
func (s *session) awaitClose(d time.Duration) (closeEvent, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				select {
				case ev := <-s.closed:
					return ev, true
				default:
					return closeEvent{}, true
				}
			}
			s.collected = append(s.collected, f)
		case ev := <-s.closed:
			return ev, true
		case <-timer.C:
			return closeEvent{}, false
		}
	}
}

func (s *session) reset() { s.collected = nil }

// firstData returns the data object of the first collected frame with packet id.
func (s *session) firstData(packet int) (map[string]any, bool) {
	for _, f := range s.collected {
		if len(f) == 0 {
			continue
		}
		var id int
		if err := json.Unmarshal(f[0], &id); err != nil || id != packet {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal(f[len(f)-1], &out); err != nil {
			continue
		}
		return out, true
	}
	return nil, false
}

// sawPacket reports whether a frame with the given packet id was collected.
// The scan is payload-agnostic (firstData decodes an object): Map(4) carries a
// base64 string, not a data object.
func (s *session) sawPacket(packet int) bool {
	for _, f := range s.collected {
		if len(f) == 0 {
			continue
		}
		var id int
		if err := json.Unmarshal(f[0], &id); err == nil && id == packet {
			return true
		}
	}
	return false
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9001"
	}
	skipDB := true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SKIP_DATABASE"))) {
	case "0", "false", "no", "off":
		skipDB = false
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1000000)
	userA := "autologin" + suffix + "a"
	userB := "autologin" + suffix + "b"
	passA := "secret" + suffix
	passB := "secret" + suffix

	fmt.Printf("login e2e against 127.0.0.1:%s (skip_database=%v)\n", port, skipDB)

	// 1. Guest login: Welcome with a real guest nameplate, then the Map.
	guest := dial(port)
	guest.send(`[2,{"opcode":2}]`)
	guest.collect(3 * time.Second)
	if welcome, ok := guest.firstData(packetWelcome); ok {
		name, _ := welcome["name"].(string)
		if !strings.HasPrefix(name, "Guest") || name == "hero" {
			check(false, fmt.Sprintf("guest nameplate is a guest name (got %q)", name))
		} else {
			check(true, fmt.Sprintf("guest nameplate is %q (was hardcoded \"hero\" before)", name))
		}
	} else {
		check(false, "guest login yields Welcome(3)")
	}
	check(guest.sawPacket(packetMap), "guest login yields Map(4)")
	guest.conn.Close()
	time.Sleep(300 * time.Millisecond) // let the disconnect fan-out settle

	// 2. A short password is refused before any database work.
	short := dial(port)
	short.send(`[2,{"opcode":0,"username":"somebody","password":"ab"}]`)
	ev, closed := short.awaitClose(3 * time.Second)
	check(closed && ev.code == rejectCloseCode && ev.reason == "invalidpassword",
		fmt.Sprintf("short password -> close %d invalidpassword (got %d %q)", rejectCloseCode, ev.code, ev.reason))

	// 3. Register two accounts (with skip-database the name is accepted without
	//    creating a row: that shortcut is TS behavior, not a bug).
	regA := dial(port)
	regA.send(fmt.Sprintf(`[2,{"opcode":1,"username":%q,"password":%q,"email":%q}]`, userA, passA, userA+"@example.com"))
	regA.collect(3 * time.Second)
	if welcome, ok := regA.firstData(packetWelcome); ok {
		name, _ := welcome["name"].(string)
		check(strings.EqualFold(name, userA), fmt.Sprintf("register yields a Welcome named after the account (got %q)", name))
	} else {
		check(false, "register yields Welcome(3)")
	}
	regA.conn.Close()
	time.Sleep(300 * time.Millisecond) // let the disconnect fan-out settle

	regB := dial(port)
	regB.send(fmt.Sprintf(`[2,{"opcode":1,"username":%q,"password":%q,"email":%q}]`, userB, passB, userB+"@example.com"))
	regB.collect(3 * time.Second)
	check(regB.sawPacket(packetWelcome), "second registration yields Welcome(3)")
	regB.conn.Close()
	time.Sleep(300 * time.Millisecond)

	// 4. A wrong password is refused (only meaningful with credentials on).
	if skipDB {
		fmt.Println("SKIP: the invalidlogin check needs SKIP_DATABASE=0")
	} else {
		bad := dial(port)
		bad.send(fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":"definitely-wrong"}]`, userB))
		ev, closed = bad.awaitClose(3 * time.Second)
		check(closed && ev.code == rejectCloseCode && ev.reason == "invalidlogin",
			fmt.Sprintf("wrong password -> close %d invalidlogin (got %d %q)", rejectCloseCode, ev.code, ev.reason))
	}

	// 5. Live session for userA, then a duplicate attempt for the same name.
	live := dial(port)
	live.send(fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":%q}]`, userA, passA))
	live.collect(3 * time.Second)
	check(live.sawPacket(packetWelcome), "an accepted login yields Welcome(3)")

	dup := dial(port)
	dup.send(fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":%q}]`, userA, passA))
	ev, closed = dup.awaitClose(3 * time.Second)
	check(closed && ev.code == rejectCloseCode && ev.reason == "loggedin",
		fmt.Sprintf("duplicate session -> close %d loggedin (got %d %q)", rejectCloseCode, ev.code, ev.reason))

	// 6. Guest -> account upgrade on a live socket (the phase-7 improvement).
	up := dial(port)
	up.send(`[2,{"opcode":2}]`)
	up.collect(3 * time.Second)
	up.reset()

	// 6a. A name that is already taken fails IN PLACE: the same register path
	//     answers 'userexists', which the guest socket reports as a notice
	//     instead of ending the session (a guest never gets a close frame for
	//     a bad form).
	up.send(fmt.Sprintf(`[2,{"opcode":1,"username":%q,"password":%q}]`, userA, passA))
	up.collect(1500 * time.Millisecond)
	ev, closed = up.awaitClose(600 * time.Millisecond)
	check(!closed, "a refused upgrade (taken username) does not drop the session")
	check(up.sawPacket(packetChat), "the refusal is announced to the player")

	// 6b. The upgrade itself succeeds in place.
	up.reset()
	up.send(fmt.Sprintf(`[2,{"opcode":1,"username":%q,"password":%q}]`, "upgraded"+suffix, passA))
	up.collect(2 * time.Second)
	ev, closed = up.awaitClose(1200 * time.Millisecond)
	check(!closed, "the guest socket survives the in-place upgrade (no close frame)")
	check(up.sawPacket(packetChat), "the upgrade is announced to the player")

	// 6c. The socket is now an account, so a second Register is the double-login
	//     case: refused with 1010 'loggedin' (incoming.ts:208 parity) instead of
	//     silently switching identity mid-connection.
	up.reset()
	up.send(fmt.Sprintf(`[2,{"opcode":1,"username":%q,"password":%q}]`, "otheraccount"+suffix, passA))
	ev, closed = up.awaitClose(3 * time.Second)
	check(closed && ev.code == rejectCloseCode && ev.reason == "loggedin",
		fmt.Sprintf("a second Register on the upgraded socket -> close %d loggedin (got %d %q)",
			rejectCloseCode, ev.code, ev.reason))

	// 6d. The upgrade produced a real account: it logs back in with the new
	//     credentials (only meaningful with SKIP_DATABASE=0).
	if skipDB {
		fmt.Println("SKIP: the upgrade re-login check needs SKIP_DATABASE=0")
	} else {
		back := dial(port)
		back.send(fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":%q}]`, "upgraded"+suffix, passA))
		back.collect(3 * time.Second)
		if welcome, ok := back.firstData(packetWelcome); ok {
			name, _ := welcome["name"].(string)
			check(strings.EqualFold(name, "upgraded"+suffix),
				fmt.Sprintf("the upgraded account logs back in (got %q)", name))
		} else {
			check(false, "the upgraded account logs back in")
		}
		back.conn.Close()
	}

	up.conn.Close()
	live.conn.Close()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("%d check(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("all login checks passed")
}
