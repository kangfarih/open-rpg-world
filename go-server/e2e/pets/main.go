// Scripted WS check for the pets wiring (TESTMAP=1 default server):
// login -> pettest grant (Spawn type 7 PetData with owner + movementSpeed)
// -> pettest state (notify probe, hunger/expiry report-only) -> m9test tp
// far (Despawn + Spawn at owner = teleport) -> m9test mob spawn + owner
// swing (pet Combat Hit mirror + shared kill) -> Pet Pickup [58,0]
// (Despawn + ContainerAdd item return).
// Hunger/expiry enforcement is a documented-skip (no TS source; the state
// probe only reports the predicates).
// Not part of the stub build (e2e dirs are run via `go run`).
// Usage: PORT=9134 go run ./e2e/pets (server on the same PORT).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Packet ids (mirrors go-server/packets.go).
const (
	pktWelcome   = 3
	pktSpawn     = 5
	pktMovement  = 11
	pktTeleport  = 12
	pktDespawn   = 13
	pktCombat    = 15
	pktContainer = 21
	pktNotify    = 25
	pktMinig     = 46
	pktPet       = 58
)

// Opcodes.
const (
	moveMove   = 4
	moveFollow = 5
	combatHit  = 1
	contAdd    = 1
	petPickup  = 0
)

// Modules.EntityType.Pet (no Go constant; literal 7 per pets.go).
const entityPet = 7

var incoming = make(chan []json.RawMessage, 4096)

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

var lastFrames [][]json.RawMessage

func drain(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case f := <-incoming:
			if len(f) == 0 {
				continue
			}
			lastFrames = append(lastFrames, f)
		case <-timer.C:
			return
		}
	}
}

func frameID(f []json.RawMessage) int {
	var id int
	if len(f) > 0 {
		_ = json.Unmarshal(f[0], &id)
	}
	return id
}

func frameOpcode(f []json.RawMessage) int {
	if len(f) >= 3 {
		var op int
		_ = json.Unmarshal(f[1], &op)
		return op
	}
	return -1
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

func dialPort() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "9001"
}

type spawnData struct {
	Instance      string `json:"instance"`
	Type          int    `json:"type"`
	Key           string `json:"key"`
	X             int    `json:"x"`
	Y             int    `json:"y"`
	Owner         string `json:"owner"`
	MovementSpeed *int   `json:"movementSpeed"`
}

func spawns() []spawnData {
	var out []spawnData
	for _, f := range lastFrames {
		if frameID(f) != pktSpawn {
			continue
		}
		var d spawnData
		if json.Unmarshal(frameData(f), &d) == nil && d.Instance != "" {
			out = append(out, d)
		}
	}
	return out
}

func despawned(inst string) bool {
	for _, f := range lastFrames {
		if frameID(f) != pktDespawn {
			continue
		}
		var d struct {
			Instance string `json:"instance"`
		}
		if json.Unmarshal(frameData(f), &d) == nil && d.Instance == inst {
			return true
		}
	}
	return false
}

func notifyHas(substr string) (string, bool) {
	for _, f := range lastFrames {
		if frameID(f) != pktNotify {
			continue
		}
		var n struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(frameData(f), &n)
		if strings.Contains(n.Message, substr) {
			return n.Message, true
		}
	}
	return "", false
}

type combatData struct {
	Instance string `json:"instance"`
	Target   string `json:"target"`
	Hit      struct {
		Damage int `json:"damage"`
	} `json:"hit"`
}

func combatHits() []combatData {
	var out []combatData
	for _, f := range lastFrames {
		if frameID(f) != pktCombat || frameOpcode(f) != combatHit {
			continue
		}
		var d combatData
		if json.Unmarshal(frameData(f), &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

func main() {
	user := fmt.Sprintf("pet-%d", time.Now().UnixNano())
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+dialPort()+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	defer conn.Close()
	go reader(conn)

	fmt.Println("connecting...")
	drain(1500 * time.Millisecond)
	lastFrames = nil

	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	drain(1500 * time.Millisecond)
	lastFrames = nil

	send(conn, fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":"x","seedRank":2}]`, user))
	drain(2500 * time.Millisecond)

	var welcome struct {
		Instance string `json:"instance"`
	}
	for _, f := range lastFrames {
		if frameID(f) == pktWelcome {
			_ = json.Unmarshal(frameData(f), &welcome)
		}
	}
	check(welcome.Instance != "", fmt.Sprintf("welcome carries instance (%q)", welcome.Instance))
	lastFrames = nil

	send(conn, `[9,{"regionsLoaded":true,"userAgent":"check"}]`)
	drain(2 * time.Second)
	lastFrames = nil

	// --- Grant via TESTMAP debug. ---
	send(conn, `[46,{"pettest":"grant","key":"ratpet"}]`)
	drain(2 * time.Second)
	var petInst string
	for _, s := range spawns() {
		if s.Type == entityPet && s.Owner == welcome.Instance {
			petInst = s.Instance
			check(s.Key == "rat", fmt.Sprintf("pet spawn key=rat (got %q)", s.Key))
			check(s.MovementSpeed != nil, "pet spawn carries movementSpeed (PetData)")
			check(s.X == 100 && s.Y == 96, fmt.Sprintf("pet spawns at owner 100,96 (got %d,%d)", s.X, s.Y))
		}
	}
	check(petInst != "", "pettest grant -> Spawn type 7 with owner=self")
	if msg, ok := notifyHas("pet:grant"); ok {
		check(strings.Contains(msg, petInst), fmt.Sprintf("grant notify names pet (%q)", msg))
	} else {
		check(false, "grant notify pet:grant")
	}
	lastFrames = nil

	// --- State probe (hunger/expiry report-only). ---
	send(conn, `[46,{"pettest":"state"}]`)
	drain(1500 * time.Millisecond)
	if msg, ok := notifyHas("pet:state"); ok {
		check(strings.Contains(msg, petInst), fmt.Sprintf("state names pet (%q)", msg))
		check(strings.Contains(msg, "hungry=false"), fmt.Sprintf("fresh pet not hungry (%q)", msg))
		check(strings.Contains(msg, "expired=false"), fmt.Sprintf("expiry not enforced (%q)", msg))
	} else {
		check(false, "state notify pet:state")
	}
	fmt.Println("  note: hunger/expiry enforcement is a documented-skip (no TS source)")
	lastFrames = nil

	// --- Teleport: move the owner far, pet must despawn + respawn at owner. ---
	send(conn, `[46,{"m9test":"tp","x":120,"y":96}]`)
	drain(2500 * time.Millisecond)
	check(despawned(petInst), "far move -> pet Despawn (teleport leg)")
	teleported := false
	for _, s := range spawns() {
		if s.Instance == petInst && s.Type == entityPet {
			teleported = s.X == 120 && s.Y == 96
			check(teleported, fmt.Sprintf("pet respawned at owner 120,96 (got %d,%d)", s.X, s.Y))
		}
	}
	check(teleported, "far move -> pet Spawn at owner (teleport observed)")
	lastFrames = nil

	// --- Attack-mirror: spawn a rat next to the owner, swing, pet mirrors. ---
	send(conn, `[46,{"m9test":"spawn","instance":"m-pet-1","key":"rat","x":121,"y":96}]`)
	drain(1500 * time.Millisecond)
	lastFrames = nil
	send(conn, fmt.Sprintf(`[15,{"instance":%q,"target":"m-pet-1"}]`, welcome.Instance))
	drain(1500 * time.Millisecond)
	mirrored := false
	for _, h := range combatHits() {
		if h.Instance == petInst && h.Target == "m-pet-1" {
			mirrored = true
			check(h.Hit.Damage == 3, fmt.Sprintf("pet mirror damage=3 (got %d)", h.Hit.Damage))
		}
	}
	check(mirrored, "owner swing -> pet Combat Hit mirror (instance=pet)")
	lastFrames = nil

	// Shared kill: keep swinging until the rat despawns (owner + pet damage).
	killed := false
	for i := 0; i < 12 && !killed; i++ {
		send(conn, fmt.Sprintf(`[15,{"instance":%q,"target":"m-pet-1"}]`, welcome.Instance))
		drain(700 * time.Millisecond)
		if despawned("m-pet-1") {
			killed = true
		}
	}
	check(killed, "shared kill -> m-pet-1 Despawn (pet damage credits owner)")
	lastFrames = nil

	// --- Pickup: C->S Pet Pickup returns the item + despawns. ---
	send(conn, `[58,{"opcode":0}]`)
	drain(2 * time.Second)
	check(despawned(petInst), "pickup -> pet Despawn")
	addBack := false
	for _, f := range lastFrames {
		if frameID(f) != pktContainer || frameOpcode(f) != contAdd {
			continue
		}
		var d struct {
			Slot *struct {
				Key   string `json:"key"`
				Count int    `json:"count"`
			} `json:"slot"`
		}
		if json.Unmarshal(frameData(f), &d) == nil && d.Slot != nil && d.Slot.Key == "ratpet" {
			addBack = true
		}
	}
	check(addBack, "pickup -> ContainerAdd ratpet (inventory return)")
	lastFrames = nil

	send(conn, `[46,{"pettest":"state"}]`)
	drain(1500 * time.Millisecond)
	_, none := notifyHas("pet:state none")
	check(none, "pet gone after pickup (state=none)")

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("FAILED: %d checks\n", len(failures))
		for _, f := range failures {
			fmt.Println(" -", f)
		}
		os.Exit(1)
	}
	fmt.Println("CHECK PASSED (pets: grant + state + teleport + mirror + kill + pickup)")
}
