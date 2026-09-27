// Scripted WS check for the M10 slice (TESTMAP=1 default server): the area
// system over the REAL world.json groups (no synthetic bands inject when the
// base map already carries areas) — music song enter/exit (id 266
// 'codingroom'), overlay Set/Remove enter/exit (id 367 'inside', darkness
// 0.6), PVP state + IN_PVP_ZONE/NOT_IN_PVP_ZONE notifies (id 316), camera
// lockX enter + FreeFlow exit (via the TESTMAP band x98..107 y105..108 —
// world.json itself has no camera areas), and the full chest-area flow on
// chest area 248 (items bronzesword, spawnX/Y 110,106): mob adoption, kill
// -> reward chest Spawn at spawnX/spawnY, mob-respawn guard re-adopt ->
// chest removal, second clear -> chest, open -> bronzesword item drop ->
// step pickup Container Add.
//
// Movement notes: Step frames set the position from playerX (landing tile)
// and pass the speed check via the >2s idle grace — every hop sleeps 2.2s
// first. Exits hop from inside the area to a verified open tile outside in
// one frame; the server evaluates area membership on the landing tile only.
// Chest legs stand adjacent to the mob tile so the passive crab (attackRange
// 1, spawned via m10test chestmob) fights in place and dies INSIDE the area
// before the engine's roam pass can move it out.
//
// Not part of the stub build (underscore dirs are ignored by the go tool).
// Usage: go run ./e2e/m10 (stub must run with TESTMAP=1, the default).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var incoming = make(chan []json.RawMessage, 4096)

// Packet ids (mirrors go-server/packets.go).
const (
	pktSpawn        = 5
	pktDespawn      = 13
	pktNotification = 25
	pktContainer    = 21
	pktMusic        = 30
	pktPVP          = 37
	pktOverlay      = 41
	pktCamera       = 42
	pktMinigame     = 46
)

// sFrame is one S->C frame: [id, op|nil, data] with the opcode element
// omitted when the packet shape has none. OpPresent distinguishes "[op,..]"
// from "[null,..]" (PVP/Music serialize a null opcode element) and from
// 2-element shapes.
type sFrame struct {
	ID        int
	Op        *int
	OpPresent bool
	Data      json.RawMessage
}

func reader(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
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

var lastFrames []sFrame

func drain(d time.Duration) {
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
			fr := sFrame{ID: id}
			switch len(f) {
			case 2:
				fr.Data = f[1]
			case 3:
				// packet.ts serialize: the opcode element may be null.
				fr.OpPresent = true
				if string(f[1]) != "null" {
					var op int
					if json.Unmarshal(f[1], &op) == nil {
						fr.Op = &op
					}
				}
				fr.Data = f[2]
			}
			lastFrames = append(lastFrames, fr)
		case <-timer.C:
			return
		}
	}
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

// pvpData is the PVP payload {state}.
type pvpData struct {
	State bool `json:"state"`
}

// overlayData is the Overlay Set payload {image,colour}.
type overlayData struct {
	Image  string `json:"image"`
	Colour string `json:"colour"`
}

// containerAddData is the Container Add payload {type, slot:{index,key,count}}.
type containerAddData struct {
	Type int `json:"type"`
	Slot struct {
		Index int    `json:"index"`
		Key   string `json:"key"`
		Count int    `json:"count"`
	} `json:"slot"`
}

// notifications collects Notification messages from the drained frames.
func notifications() []string {
	var out []string
	for _, f := range lastFrames {
		if f.ID != pktNotification {
			continue
		}
		var n struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(f.Data, &n)
		if n.Message != "" {
			out = append(out, n.Message)
		}
	}
	return out
}

// framesOf filters the drained frames by packet id.
func framesOf(id int) []sFrame {
	var out []sFrame
	for _, f := range lastFrames {
		if f.ID == id {
			out = append(out, f)
		}
	}
	return out
}

// waitPkt drains in windows until pred matches a frame of the given id.
func waitPkt(conn *websocket.Conn, id int, pred func(f sFrame) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		lastFrames = nil
		drain(500 * time.Millisecond)
		for _, f := range framesOf(id) {
			if pred(f) {
				return true
			}
		}
	}
	return false
}

// tp repositions the hero server-side (m9test tp: Teleport + aggro scan +
// M10 area callbacks — one packet drives enter AND exit change-detection).
func tp(conn *websocket.Conn, x, y int) {
	send(conn, fmt.Sprintf(`[46,{"m9test":"tp","x":%d,"y":%d}]`, x, y))
}

// hop sends one Step frame. playerX is the landing tile (the server applies
// it and runs the area callbacks on it); the 2.2s sleep rides the >2s idle
// grace in checkSpeed so hop distance never trips the anticheat.
func hop(conn *websocket.Conn, landX, landY int) {
	time.Sleep(2200 * time.Millisecond)
	send(conn, fmt.Sprintf(`[11,{"opcode":2,"playerX":%d,"playerY":%d,"nextGridX":%d,"nextGridY":%d}]`,
		landX, landY, landX, landY))
}

// pollChestEcho sends the m10test chest probe and collects its echo from the
// response window ("m10:chest=<inst> x=.. y=.." or "m10:chest=none").
func pollChestEcho(conn *websocket.Conn, window time.Duration) (inst string, none bool) {
	lastFrames = nil
	send(conn, `[46,{"m10test":"chest"}]`)
	drain(window)
	for _, m := range notifications() {
		if !strings.HasPrefix(m, "m10:chest=") {
			continue
		}
		body := strings.TrimPrefix(m, "m10:chest=")
		if body == "none" {
			return "", true
		}
		if i := strings.Index(body, " "); i >= 0 {
			body = body[:i]
		}
		return body, false
	}
	return "", false
}

var heroInst string // captured from the Welcome PlayerData

func login(user string) *websocket.Conn {
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
	drain(800 * time.Millisecond)
	lastFrames = nil
	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	drain(800 * time.Millisecond)
	lastFrames = nil
	send(conn, fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":"x","seedRank":2}]`, user))
	drain(1200 * time.Millisecond)
	for _, f := range lastFrames {
		if f.ID == 3 { // PacketWelcome
			var w struct {
				Instance string `json:"instance"`
			}
			_ = json.Unmarshal(f.Data, &w)
			if w.Instance != "" && user == "m10hero" && heroInst == "" {
				heroInst = w.Instance
			}
		}
	}
	lastFrames = nil
	return conn
}

// chestSpawnFrame reports the reward-chest Spawn (type 4, key chest, tile
// 110,106 — area 248's spawnX/spawnY).
func chestSpawnFrame(f sFrame) bool {
	var d struct {
		Type int    `json:"type"`
		Key  string `json:"key"`
		X    int    `json:"x"`
		Y    int    `json:"y"`
	}
	if json.Unmarshal(f.Data, &d) != nil {
		return false
	}
	return d.Type == 4 && d.Key == "chest" && d.X == 110 && d.Y == 106
}

// chestmobDespawnFrame reports the test mob's Despawn.
func chestmobDespawnFrame(f sFrame) bool {
	return f.ID == pktDespawn && strings.Contains(string(f.Data), "m10chestmob")
}

// chestDespawnFrame reports the reward chest's Despawn by instance.
func chestDespawnFrame(inst string) func(sFrame) bool {
	return func(f sFrame) bool {
		return f.ID == pktDespawn && strings.Contains(string(f.Data), inst)
	}
}

func main() {
	c1 := login("m10hero")

	// --- 1. Music: enter area 266 -> song, exit -> stop. ---
	fmt.Println("== music (area 266 'codingroom') ==")
	// The hero spawns inside music area 407 ('forest'); the tp exits it and
	// enters 266 in one update — both frames arrive, we assert the enter one.
	tp(c1, 325, 890) // inside 266 (x323..334 y888..896)
	got := waitPkt(c1, pktMusic, func(f sFrame) bool {
		return f.OpPresent && f.Op == nil && strings.Contains(string(f.Data), "codingroom")
	}, 6*time.Second)
	check(got, "music enter -> [30,null,\"codingroom\"]")
	// Exit hop: lands at (336,890) — outside 266 (ends x<335), open terrain,
	// no other music area (verified).
	hop(c1, 336, 890)
	got = waitPkt(c1, pktMusic, func(f sFrame) bool {
		// [30,null,null] or [30,null,""] — the Go stub sends the empty song
		// as "" (Node serializes undefined -> null; both are "stop music").
		return f.OpPresent && f.Op == nil &&
			(string(f.Data) == "null" || string(f.Data) == `""`)
	}, 6*time.Second)
	check(got, "music exit -> [30,null,<empty>] (stop frame)")

	// --- 2. Overlay: enter area 367 -> Set, exit -> Remove. ---
	fmt.Println("== overlay (area 367 'inside') ==")
	tp(c1, 198, 770) // inside 367 (x195..202 y768..783, darkness 0.6)
	got = waitPkt(c1, pktOverlay, func(f sFrame) bool {
		if !f.OpPresent || f.Op == nil || *f.Op != 0 {
			return false
		}
		var d overlayData
		if json.Unmarshal(f.Data, &d) != nil {
			return false
		}
		return strings.Contains(d.Colour, "0.6")
	}, 6*time.Second)
	check(got, "overlay enter -> [41,Set,{image,colour rgba(..,0.6)}]")
	// Exit hop: lands at (193,770) — outside 367 (starts x=195), open.
	hop(c1, 193, 770)
	got = waitPkt(c1, pktOverlay, func(f sFrame) bool {
		return f.OpPresent && f.Op != nil && *f.Op == 1 // Overlay Remove
	}, 6*time.Second)
	check(got, "overlay exit -> [41,Remove]")

	// --- 3. PVP: enter area 316 -> IN_PVP_ZONE notify + {state:true}; exit
	// -> NOT_IN_PVP_ZONE notify + {state:false}. The notify + PVP frame
	// co-arrive in one batch, so one merged loop owns each side. ---
	fmt.Println("== pvp (area 316) ==")
	tp(c1, 670, 780) // inside 316 (x645..698 y761..802)
	inState, inNotify := false, false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && (!inState || !inNotify) {
		lastFrames = nil
		drain(500 * time.Millisecond)
		for _, f := range framesOf(pktPVP) {
			var d pvpData
			if json.Unmarshal(f.Data, &d) == nil && d.State {
				inState = true
			}
		}
		for _, m := range notifications() {
			if m == "misc:IN_PVP_ZONE" {
				inNotify = true
			}
		}
	}
	check(inState, "pvp enter -> PVP {state:true}")
	check(inNotify, "pvp enter notify misc:IN_PVP_ZONE")
	// Exit hop: lands at (640,780) — outside 316 (starts x=645), open.
	hop(c1, 640, 780)
	outState, outNotify := false, false
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && (!outState || !outNotify) {
		lastFrames = nil
		drain(500 * time.Millisecond)
		for _, f := range framesOf(pktPVP) {
			var d pvpData
			if json.Unmarshal(f.Data, &d) == nil && !d.State {
				outState = true
			}
		}
		for _, m := range notifications() {
			if m == "misc:NOT_IN_PVP_ZONE" {
				outNotify = true
			}
		}
	}
	check(outState, "pvp exit -> PVP {state:false}")
	check(outNotify, "pvp exit notify misc:NOT_IN_PVP_ZONE")

	// --- 4. Camera: TESTMAP band (x98..107 y105..108, lockX) enter ->
	// LockX, exit -> FreeFlow. world.json itself has no camera areas, so
	// this exercises the group end-to-end without touching the real map. ---
	fmt.Println("== camera (TESTMAP band lockX) ==")
	tp(c1, 106, 106)
	got = waitPkt(c1, pktCamera, func(f sFrame) bool {
		return f.OpPresent && f.Op != nil && *f.Op == 0 // Camera LockX
	}, 6*time.Second)
	check(got, "camera enter -> [42,LockX]")
	// Exit hop: lands at (97,105) — outside the band (starts x=98), open.
	hop(c1, 97, 105)
	got = waitPkt(c1, pktCamera, func(f sFrame) bool {
		return f.OpPresent && f.Op != nil && *f.Op == 2 // Camera FreeFlow
	}, 6*time.Second)
	check(got, "camera exit -> [42,FreeFlow]")

	// --- 5. Chest area 248: adopt a passive crab (m10test chestmob, 4s
	// respawn override), kill it -> reward chest Spawn at spawnX/Y 110,106.
	fmt.Println("== chest area (id 248) clear -> chest ==")
	// The crab only retargets when hit (passive); standing adjacent keeps it
	// attacking in place, so it dies INSIDE the area (m10KillHooks needs the
	// death tile in the area to spawn the chest).
	send(c1, `[46,{"m10test":"chestmob","instance":"m10chestmob","key":"crab","x":111,"y":108,"delay":4000}]`)
	tp(c1, 110, 107) // adjacent to the mob tile (111,108), outside 248 (y<108)
	drain(900 * time.Millisecond)
	killed, chest := false, false
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !(killed && chest) {
		lastFrames = nil
		drain(700 * time.Millisecond)
		for _, f := range lastFrames {
			if chestmobDespawnFrame(f) {
				killed = true
			}
			if chestSpawnFrame(f) {
				chest = true
			}
		}
		if !killed {
			send(c1, fmt.Sprintf(`[15,{"instance":%q,"target":"m10chestmob"}]`, heroInst))
		}
	}
	check(killed, "chest mob killed (Despawn m10chestmob)")
	check(chest, "reward chest Spawn (type 4, key chest) at spawnX/spawnY 110,106")

	// --- 6. Respawn guard: the crab re-adopts on engine respawn (4s) ->
	// chest Despawn + empty registry. The chest's Despawn frame co-arrives
	// with the cycle's traffic, so poll the echo INSIDE the wait loop and
	// scan the same drained batch for the Despawn. ---
	fmt.Println("== respawn guard (mob re-adopt removes chest) ==")
	removed, despawnSeen, chestInst := false, false, ""
	deadline = time.Now().Add(14 * time.Second)
	for time.Now().Before(deadline) && !(removed && despawnSeen) {
		inst, none := pollChestEcho(c1, 700*time.Millisecond)
		if none {
			removed = true
		} else if inst != "" {
			chestInst = inst // first live echo names the chest to watch
		}
		for _, f := range framesOf(pktDespawn) {
			if chestInst != "" && chestDespawnFrame(chestInst)(f) {
				despawnSeen = true
			}
		}
	}
	check(removed && despawnSeen, "mob respawn re-adopt -> chest Despawned + registry empty")

	// --- 7. Clear again -> chest again -> open -> bronzesword -> pickup. ---
	fmt.Println("== chest open + item pickup ==")
	killed, chest = false, false
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !(killed && chest) {
		lastFrames = nil
		drain(700 * time.Millisecond)
		for _, f := range lastFrames {
			if chestmobDespawnFrame(f) {
				killed = true
			}
			if chestSpawnFrame(f) {
				chest = true
			}
		}
		if !killed {
			// The mob respawned adjacent to us; keep swinging (no re-approach
			// needed — the spawn tile is the tile we stand next to).
			send(c1, fmt.Sprintf(`[15,{"instance":%q,"target":"m10chestmob"}]`, heroInst))
		}
	}
	check(killed, "chest mob killed again after respawn")
	check(chest, "reward chest re-spawned after second clear")
	chestInst = ""
	for i := 0; i < 8 && chestInst == ""; i++ {
		inst, none := pollChestEcho(c1, 600*time.Millisecond)
		if !none && inst != "" {
			chestInst = inst
		}
	}
	if chestInst == "" {
		check(false, "chest echo after second clear (no chest instance)")
		return // cannot continue the open leg without the instance
	}
	// Open: Target Talk(0) on the chest instance (handleTarget Talk branch
	// -> m10OpenChest). Hero stands at (110,107), adjacent to the chest tile
	// (110,106). The item Spawn + chest Despawn co-arrive — merged loop.
	send(c1, fmt.Sprintf(`[14,[0,%q]]`, chestInst))
	gotItem, opened := false, false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (!gotItem || !opened) {
		lastFrames = nil
		drain(600 * time.Millisecond)
		for _, f := range framesOf(pktSpawn) {
			var d struct {
				Type  int    `json:"type"`
				Key   string `json:"key"`
				Count *int   `json:"count"`
			}
			if json.Unmarshal(f.Data, &d) == nil && d.Type == 2 &&
				d.Key == "bronzesword" && d.Count != nil && *d.Count == 1 {
				gotItem = true
			}
		}
		for _, f := range framesOf(pktDespawn) {
			if chestDespawnFrame(chestInst)(f) {
				opened = true
			}
		}
	}
	check(gotItem, "chest open -> bronzesword Item Spawn (type 2, count 1)")
	check(opened, "chest Despawned on open")
	// Step onto the item tile (110,106) -> M5 pickup -> Container Add
	// {type:1, slot:{key:bronzesword, count:1}}.
	hop(c1, 110, 106)
	got = waitPkt(c1, pktContainer, func(f sFrame) bool {
		if !f.OpPresent || f.Op == nil || *f.Op != 1 { // Container Add
			return false
		}
		var d containerAddData
		if json.Unmarshal(f.Data, &d) != nil {
			return false
		}
		return d.Type == 1 && d.Slot.Key == "bronzesword" && d.Slot.Count == 1
	}, 6*time.Second)
	check(got, "step pickup -> Container Add bronzesword x1")

	// Cleanup: remove the test mob so later runs start from a quiet area.
	send(c1, `[46,{"m9test":"remove","instance":"m10chestmob"}]`)
	drain(500 * time.Millisecond)

	if len(failures) > 0 {
		fmt.Printf("\nM10 E2E FAILED: %d checks\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("\nM10 E2E PASSED")
}
