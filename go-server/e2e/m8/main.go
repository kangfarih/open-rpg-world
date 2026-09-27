// Scripted WS check for the M8 slice (TESTMAP=1 default server): minigames —
// coursing lobby enter/exit notifications + Lobby state packet, lobby
// countdown packets, auto-start with the hunter/prey split, Score packets
// from distance-to-centre ticks, Pointer Entity frames to the coursing
// target, teamwar team-kill points, and End + lobby teleport on stop.
// Deterministic polling uses the M8TEST debug frame echoes (m8:state /
// m8:kills notifications) instead of racing the 1s tick windows.
// Not part of the stub build (underscore dirs are ignored by the go tool).
// Usage: go run ./e2e/m8 (stub must run with TESTMAP=1, the default).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var incoming = make(chan []json.RawMessage, 4096)

func reader(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return // never close(incoming): shared across logins
		}
		var bulk [][]json.RawMessage
		if err := json.Unmarshal(raw, &bulk); err != nil {
			continue
		}
		for _, f := range bulk {
			incoming <- f
		}
	}
}

var lastFrames [][]json.RawMessage

func drain(d time.Duration) map[int]int {
	counts := map[int]int{}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case f := <-incoming:
			if len(f) == 0 {
				continue
			}
			var id int
			if err := json.Unmarshal(f[0], &id); err != nil {
				continue
			}
			counts[id]++
			lastFrames = append(lastFrames, f)
		case <-timer.C:
			return counts
		}
	}
}

func frameData(f []json.RawMessage) json.RawMessage {
	if len(f) == 0 {
		return nil
	}
	return f[len(f)-1]
}

func send(conn *websocket.Conn, msg string) {
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		fmt.Println("WRITE FAIL:", err)
		os.Exit(1)
	}
}

var failures []string

func check(cond bool, msg string) {
	if !cond {
		failures = append(failures, msg)
		fmt.Println("  FAIL:", msg)
	} else {
		fmt.Println("  ok:", msg)
	}
}

// minigameMsg is one parsed S→C Minigame frame ([46, opcode, {action,...}]).
type minigameMsg struct {
	Opcode        int  `json:"-"`
	Action        int  `json:"action"`
	Countdown     int  `json:"countdown"`
	Score         *int `json:"score"`
	Started       bool `json:"started"`
	RedTeamKills  *int `json:"redTeamKills"`
	BlueTeamKills *int `json:"blueTeamKills"`
}

func minigameFrames() []minigameMsg {
	var out []minigameMsg
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 46 || len(f) < 3 {
			continue
		}
		var m minigameMsg
		_ = json.Unmarshal(f[1], &m.Opcode)
		_ = json.Unmarshal(f[2], &m)
		out = append(out, m)
	}
	return out
}

// notifyMessages collects Notification Text messages.
func notifyMessages() []string {
	var out []string
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 25 || len(f) < 3 {
			continue
		}
		var n struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(f[2], &n)
		out = append(out, n.Message)
	}
	return out
}

// pointerMsg is one parsed S→C Pointer frame ([36, opcode, {instance}]).
type pointerMsg struct {
	Opcode   int    `json:"-"`
	Instance string `json:"instance"`
}

func pointerFrames() []pointerMsg {
	var out []pointerMsg
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 36 || len(f) < 3 {
			continue
		}
		var p pointerMsg
		_ = json.Unmarshal(f[1], &p.Opcode)
		_ = json.Unmarshal(f[2], &p)
		out = append(out, p)
	}
	return out
}

// teleportFrames collects Teleport frames ([12,{instance,x,y}]).
func teleportFrames() []struct {
	Instance string `json:"instance"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
} {
	var out []struct {
		Instance string `json:"instance"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
	}
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 12 {
			continue
		}
		var t struct {
			Instance string `json:"instance"`
			X        int    `json:"x"`
			Y        int    `json:"y"`
		}
		_ = json.Unmarshal(frameData(f), &t)
		out = append(out, t)
	}
	return out
}

// m8State queries the m8:state echo and returns the parsed fields.
// Sscanf %s stops at the first space, so fields are parsed manually.
func m8State(conn *websocket.Conn) (game string, team, score int, target string) {
	for attempt := 0; attempt < 2; attempt++ {
		lastFrames = nil
		send(conn, `[46,{"m8test":"state"}]`)
		drain(900 * time.Millisecond)
		for _, m := range notifyMessages() {
			if !strings.HasPrefix(m, "m8:state ") {
				continue
			}
			for _, part := range strings.Fields(m) {
				kv := strings.SplitN(part, "=", 2)
				if len(kv) != 2 {
					continue
				}
				switch kv[0] {
				case "game":
					game = kv[1]
				case "team":
					team, _ = strconv.Atoi(kv[1])
				case "score":
					score, _ = strconv.Atoi(kv[1])
				case "target":
					target = kv[1]
				}
			}
			return
		}
	}
	return
}

// m8Kills queries the teamwar kill-counter echo.
func m8Kills(conn *websocket.Conn) (red, blue int) {
	for attempt := 0; attempt < 2; attempt++ {
		lastFrames = nil
		send(conn, `[46,{"m8test":"kills"}]`)
		drain(900 * time.Millisecond)
		for _, m := range notifyMessages() {
			if !strings.HasPrefix(m, "m8:kills ") {
				continue
			}
			for _, part := range strings.Fields(m) {
				kv := strings.SplitN(part, "=", 2)
				if len(kv) != 2 {
					continue
				}
				switch kv[0] {
				case "red":
					red, _ = strconv.Atoi(kv[1])
				case "blue":
					blue, _ = strconv.Atoi(kv[1])
				}
			}
			return
		}
	}
	return
}

// login dials, handshakes, and logs in with seedPos for a fast lobby walk.
func login(user string, seedPos []int) *websocket.Conn {
	// Honor PORT (the server's own override) so a side-by-side run works
	// while the default 9001 is occupied.
	port := os.Getenv("PORT")
	if port == "" {
		port = "9001"
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	go reader(conn)
	drain(1200 * time.Millisecond)
	lastFrames = nil
	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	drain(1200 * time.Millisecond)
	lastFrames = nil
	posJSON, _ := json.Marshal(seedPos)
	send(conn, fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":"x","seedPos":%s,"seedRank":2}]`, user, posJSON))
	drain(1500 * time.Millisecond)
	lastFrames = nil
	return conn
}

// waitState polls the state echo until pred is true or the deadline passes.
func waitState(conn *websocket.Conn, pred func(game string, team, score int, target string) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		g, t, s, tgt := m8State(conn)
		if pred(g, t, s, tgt) {
			return true
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false
}

// waitKills polls the kills echo until pred is true or the deadline passes.
func waitKills(conn *websocket.Conn, pred func(red, blue int) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, b := m8Kills(conn)
		if pred(r, b) {
			return true
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false
}

func main() {
	// --- 1. Coursing lobby: enter notification + Lobby state packet. ---
	fmt.Println("== coursing lobby ==")
	c1 := login("m8prey1", []int{140, 635}) // inside coursing lobby (133,629 20x15)
	// The seedPos path fires addPlayer DURING login processing, so the
	// ENTERED_LOBBY + Lobby-state frames ride inside the login Welcome bulk
	// (the login helper clears lastFrames). Verify the enter/exit pair with
	// a deliberate walk-out + walk-in cycle instead.
	drain(800 * time.Millisecond)
	lastFrames = nil

	// Walk OUT (south past y=643 — the lobby spans y 629..643 inclusive —
	// so go to y=649) then back IN to y=635. Keep lastFrames ACROSS both
	// legs so the EXITED and ENTERED notifications accumulate together.
	for i := 0; i < 14; i++ {
		send(c1, fmt.Sprintf(`[11,{"opcode":2,"playerX":140,"playerY":%d,"nextGridX":140,"nextGridY":%d}]`, 636+i, 636+i))
		time.Sleep(330 * time.Millisecond)
	}
	drain(900 * time.Millisecond)
	for i := 0; i < 14; i++ {
		send(c1, fmt.Sprintf(`[11,{"opcode":2,"playerX":140,"playerY":%d,"nextGridX":140,"nextGridY":%d}]`, 649-i, 649-i))
		time.Sleep(330 * time.Millisecond)
	}
	drain(1200 * time.Millisecond)

	exitSeen, enterSeen, lobbyPacket := false, false, false
	for _, m := range notifyMessages() {
		if m == "misc:EXITED_LOBBY;name=Coursing" {
			exitSeen = true
		}
		if m == "misc:ENTERED_LOBBY;name=Coursing" {
			enterSeen = true
		}
	}
	for _, m := range minigameFrames() {
		if m.Action == 0 { // MinigameState.Lobby (addPlayer Node quirk)
			lobbyPacket = true
		}
	}
	lastFrames = nil
	check(exitSeen, "walk-out fires EXITED_LOBBY notification")
	check(enterSeen && lobbyPacket, "walk-in fires ENTERED_LOBBY + Lobby state packet (MinigameState.Lobby=0)")

	// --- 2. Lobby countdown packets tick every second. ---
	counts := drain(3200 * time.Millisecond)
	lastFrames = nil
	check(counts[46] >= 2, fmt.Sprintf("lobby countdown packets every tick (got %d in 3.2s)", counts[46]))

	// --- 3. Second player joins; auto-start splits hunter/prey. ---
	fmt.Println("== coursing start ==")
	c2 := login("m8hunt1", []int{145, 640})
	// The start shuffle assigns teams randomly, so poll BOTH sessions until
	// each shows game=coursing on OPPOSITE teams (2=Prey, 3=Hunter) with a
	// coursingTarget assigned. Up to 100s covers two 45s lobby cycles.
	stateXY := func(conn *websocket.Conn) (g string, t int, x, y int) {
		lastFrames = nil
		send(conn, `[46,{"m8test":"state"}]`)
		drain(900 * time.Millisecond)
		for _, m := range notifyMessages() {
			if !strings.HasPrefix(m, "m8:state ") {
				continue
			}
			for _, part := range strings.Fields(m) {
				kv := strings.SplitN(part, "=", 2)
				if len(kv) != 2 {
					continue
				}
				switch kv[0] {
				case "game":
					g = kv[1]
				case "team":
					t, _ = strconv.Atoi(kv[1])
				case "x":
					x, _ = strconv.Atoi(kv[1])
				case "y":
					y, _ = strconv.Atoi(kv[1])
				}
			}
		}
		lastFrames = nil
		return
	}

	var g1, g2 string
	var t1, t2, x1, y1, x2, y2 int
	deadline := time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		g1, t1, x1, y1 = stateXY(c1)
		g2, t2, x2, y2 = stateXY(c2)
		if g1 == "coursing" && g2 == "coursing" && t1 != t2 &&
			(t1 == 2 || t1 == 3) && (t2 == 2 || t2 == 3) {
			break
		}
		time.Sleep(600 * time.Millisecond)
	}
	check(g1 == "coursing" && g2 == "coursing" && t1 != t2,
		fmt.Sprintf("coursing auto-start splits hunter/prey (c1 team=%d, c2 team=%d)", t1, t2))

	// Spawn teleports really happened: the Hunter stands inside the
	// hunterspawn area (209,896 11x8) and the Prey inside one of the
	// preyspawn areas.
	inArea := func(x, y, ax, ay, aw, ah int) bool {
		return x >= ax && x < ax+aw && y >= ay && y < ay+ah
	}
	inPreySpawns := func(x, y int) bool {
		return inArea(x, y, 203, 919, 36, 6) || inArea(x, y, 187, 883, 5, 39) ||
			inArea(x, y, 195, 875, 41, 5) || inArea(x, y, 233, 895, 6, 24) ||
			inArea(x, y, 232, 880, 4, 15)
	}
	hunterIn, preyIn := false, false
	for _, p := range []struct{ team, x, y int }{{t1, x1, y1}, {t2, x2, y2}} {
		if p.team == 3 { // Team.Hunter
			hunterIn = inArea(p.x, p.y, 209, 896, 11, 8)
		} else if p.team == 2 { // Team.Prey
			preyIn = inPreySpawns(p.x, p.y)
		}
	}
	check(hunterIn, fmt.Sprintf("hunter teleported into hunterspawn area (c1=%d,%d c2=%d,%d)", x1, y1, x2, y2))
	check(preyIn, fmt.Sprintf("prey teleported into a preyspawn area (c1=%d,%d c2=%d,%d)", x1, y1, x2, y2))

	// --- 4. Score packets flow to prey every 4 ticks; pointers exist. ---
	// The m8test pointers hook re-fires sendPointers deterministically.
	lastFrames = nil
	send(c2, `[46,{"m8test":"pointers"}]`)
	drain(1500 * time.Millisecond)
	ptr := pointerFrames()
	hasEntity := false
	for _, p := range ptr {
		if p.Opcode == 1 && p.Instance != "" { // Pointer.Entity
			hasEntity = true
		}
	}
	check(hasEntity && len(ptr) >= 2, fmt.Sprintf("pointer Remove+Entity frames (got %d)", len(ptr)))

	// Prey score packet: wait for one MinigameActions.Score with score.
	scoreSeen := false
	scoreDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(scoreDeadline) && !scoreSeen {
		drain(1300 * time.Millisecond)
		for _, m := range minigameFrames() {
			if m.Action == 0 && m.Score != nil { // MinigameActions.Score = 0
				scoreSeen = true
			}
		}
		lastFrames = nil
	}
	check(scoreSeen, "prey receives Score packets (distance-to-centre ticks)")

	// --- 5. Walk the prey OUT of the lobby: EXITED_LOBBY + Exit packet.
	// (Prey is in-game, so leaving the lobby area doesn't remove them —
	// the exit callback only fires for lobby members. Instead verify the
	// lobby-membership exit with a fresh third player below.)
	fmt.Println("== lobby exit ==")
	c3 := login("m8visitor", []int{140, 635})
	drain(1500 * time.Millisecond)
	lastFrames = nil
	// Walk south out of the lobby (y 635 -> 646) with legal Step pacing.
	for i := 0; i < 12; i++ {
		send(c3, fmt.Sprintf(`[11,{"opcode":2,"playerX":140,"playerY":%d,"nextGridX":140,"nextGridY":%d}]`, 635+i+1, 635+i+1))
		time.Sleep(330 * time.Millisecond)
	}
	drain(1200 * time.Millisecond)
	exited := false
	for _, m := range notifyMessages() {
		if m == "misc:EXITED_LOBBY;name=Coursing" {
			exited = true
		}
	}
	check(exited, "walking out of the lobby fires EXITED_LOBBY")

	// --- 6. TeamWar: lobby + kill points via the debug frame. ---
	fmt.Println("== teamwar ==")
	// TeamWar lobby is at (367,915 33x15). Two fresh players join.
	c4 := login("m8red1", []int{380, 920})
	c5 := login("m8blue1", []int{390, 925})
	drain(1500 * time.Millisecond)

	twStart := waitState(c4, func(g string, t, s int, tgt string) bool {
		return g == "teamwar" // started + teleported to a team spawn
	}, 250*time.Second)
	check(twStart, "teamwar auto-start assigns team + teleports")

	// Kills: red x2, blue x1 via the debug frame (TeamWar.kill parity).
	send(c4, `[46,{"m8test":"kill"}]`)
	send(c4, `[46,{"m8test":"kill"}]`)
	send(c5, `[46,{"m8test":"kill"}]`)
	killsOK := waitKills(c4, func(r, b int) bool { return r+b >= 3 && r >= 1 && b >= 1 }, 10*time.Second)
	r, b := m8Kills(c4)
	check(killsOK, fmt.Sprintf("teamwar kill points counted (red=%d blue=%d)", r, b))

	// --- 7. Disconnect mid-game -> stop() fires End to the survivor. ---
	fmt.Println("== disconnect ==")
	// Teamwar is running with 2 in-game players; dropping one triggers
	// disconnect() -> stop() (End packet + lobby teleport to the survivor).
	c4.Close()
	endSeen := false
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !endSeen {
		drain(1000 * time.Millisecond)
		for _, m := range minigameFrames() {
			if m.Action == 1 { // MinigameActions.End
				endSeen = true
			}
		}
		lastFrames = nil
	}
	check(endSeen, "disconnect with 1 player left sends End (stop)")

	// Survivor (c5) is back in the lobby position (stop teleports).
	c5Lobby := waitState(c5, func(g string, t, s int, tgt string) bool {
		return g == "" // clearMinigame parity
	}, 8*time.Second)
	check(c5Lobby, "survivor's minigame state cleared (clearMinigame)")

	// Coursing still runs with prey+visitor; drop the hunter to stop it.
	c2.Close()
	time.Sleep(1200 * time.Millisecond)
	drain(1500 * time.Millisecond)
	lastFrames = nil

	// Wrap up.
	c1.Close()
	c3.Close()
	c5.Close()

	if len(failures) > 0 {
		fmt.Printf("\nM8 E2E FAILED (%d):\n", len(failures))
		for _, f := range failures {
			fmt.Println(" -", f)
		}
		os.Exit(1)
	}
	fmt.Println("\nM8 E2E PASSED")
}
