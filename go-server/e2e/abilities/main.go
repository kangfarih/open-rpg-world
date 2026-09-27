// Scripted WS check for the abilities/status wiring (TESTMAP=1 default
// server): login Ability Batch (22,0), achievement ability-reward unlock
// (boxingman stage 26 -> run -> Ability Add 22,1), cast run ->
// Ability Toggle (22,5) + Effect Add (47,0 effect 10), immediate re-cast ->
// misc:NEED_WAIT_ABILITY notify with no new Toggle/Effect, debug poison
// apply -> Points (17) HP ticks via the status tracker.
// Not part of the stub build (underscore dirs are ignored by the go tool).
// Usage: PORT=9133 go run ./e2e/abilities (server on the same PORT).
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
	pktWelcome = 3
	pktPoints  = 17
	pktAbility = 22
	pktNotify  = 25
	pktEffect  = 47
	pktMinig   = 46
)

// Ability opcodes (Opcodes.Ability).
const (
	abBatch  = 0
	abAdd    = 1
	abToggle = 5
	abUse    = 3
)

// Effect opcodes (Opcodes.Effect).
const (
	fxAdd = 0
)

// Modules.Effects.Running (run ability server effect).
const effectRunning = 10

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

// abilityFrames returns parsed S->C Ability frames with the given opcode.
func abilityFrames(opcode int) []struct {
	Key   string `json:"key"`
	Level int    `json:"level"`
} {
	var out []struct {
		Key   string `json:"key"`
		Level int    `json:"level"`
	}
	for _, f := range lastFrames {
		if frameID(f) != pktAbility || frameOpcode(f) != opcode || len(f) < 3 {
			continue
		}
		var d struct {
			Key   string `json:"key"`
			Level int    `json:"level"`
		}
		if json.Unmarshal(f[2], &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

func notifyHas(substr string) bool {
	for _, f := range lastFrames {
		if frameID(f) != pktNotify {
			continue
		}
		var n struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(frameData(f), &n)
		if strings.Contains(n.Message, substr) {
			return true
		}
	}
	return false
}

func main() {
	user := fmt.Sprintf("ab-%d", time.Now().UnixNano())
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+dialPort()+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	defer conn.Close()
	go reader(conn)

	fmt.Println("connected:", drain(1500*time.Millisecond))
	lastFrames = nil

	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	fmt.Println("handshake:", drain(1500*time.Millisecond))
	lastFrames = nil

	send(conn, fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":"x","seedRank":2}]`, user))
	fmt.Println("login:", drain(2500*time.Millisecond))

	// Own instance from the Welcome frame.
	var welcome struct {
		Instance string `json:"instance"`
	}
	for _, f := range lastFrames {
		if frameID(f) == pktWelcome {
			_ = json.Unmarshal(frameData(f), &welcome)
		}
	}
	check(welcome.Instance != "", fmt.Sprintf("welcome carries instance (%q)", welcome.Instance))

	// Fresh user: Ability Batch present with an empty list (new wiring).
	batchOK, batchEmpty := false, false
	for _, f := range lastFrames {
		if frameID(f) != pktAbility || frameOpcode(f) != abBatch || len(f) < 3 {
			continue
		}
		var d struct {
			Abilities []any `json:"abilities"`
		}
		if json.Unmarshal(f[2], &d) == nil {
			batchOK = true
			batchEmpty = len(d.Abilities) == 0
		}
	}
	check(batchOK, "login -> Ability Batch (22,0)")
	check(batchEmpty, "fresh user batch has no abilities")
	lastFrames = nil

	send(conn, `[9,{"regionsLoaded":true,"userAgent":"check"}]`)
	fmt.Println("ready:", drain(2*time.Second))
	lastFrames = nil

	// --- Unlock via the achievement ability reward (boxingman: 25 snek
	// kills + discovery = 26 stages, rewardAbility run). ---
	send(conn, `[46,{"m11test":"setach","key":"boxingman","stage":26}]`)
	drain(2500 * time.Millisecond)
	adds := abilityFrames(abAdd)
	gotRun := false
	for _, a := range adds {
		if a.Key == "run" && a.Level == 1 {
			gotRun = true
		}
	}
	check(gotRun, fmt.Sprintf("boxingman finish -> Ability Add run lv1 (adds=%v)", adds))
	check(notifyHas("run"), "unlock notify mentions run")
	lastFrames = nil

	// --- Cast run: Toggle + Running Effect. ---
	send(conn, `[22,{"opcode":3,"key":"run"}]`)
	drain(1500 * time.Millisecond)
	toggles := abilityFrames(abToggle)
	sawToggle := false
	for _, t := range toggles {
		if t.Key == "run" {
			sawToggle = true
		}
	}
	check(sawToggle, "cast run -> Ability Toggle (22,5)")
	sawFx := false
	for _, f := range lastFrames {
		if frameID(f) != pktEffect || frameOpcode(f) != fxAdd || len(f) < 3 {
			continue
		}
		var d struct {
			Instance string `json:"instance"`
			Effect   int    `json:"effect"`
		}
		if json.Unmarshal(f[2], &d) == nil && d.Instance == welcome.Instance && d.Effect == effectRunning {
			sawFx = true
		}
	}
	check(sawFx, "cast run -> Effect Add Running(10) on self")
	lastFrames = nil

	// --- Immediate re-cast: cooldown reject, no new Toggle/Effect. ---
	send(conn, `[22,{"opcode":3,"key":"run"}]`)
	drain(1500 * time.Millisecond)
	check(notifyHas("NEED_WAIT_ABILITY"), "re-cast during cooldown -> NEED_WAIT_ABILITY notify")
	extraToggle, extraFx := false, false
	for _, f := range lastFrames {
		if frameID(f) == pktAbility && frameOpcode(f) == abToggle {
			extraToggle = true
		}
		if frameID(f) == pktEffect && frameOpcode(f) == fxAdd {
			extraFx = true
		}
	}
	check(!extraToggle && !extraFx, "rejected re-cast emits no Toggle/Effect")
	lastFrames = nil

	// --- DoT: debug poison apply -> Points HP ticks. ---
	send(conn, `[46,{"abtest":"apply","kind":"poison"}]`)
	drain(4 * time.Second)
	minHP, sawPoints := -1, false
	for _, f := range lastFrames {
		if frameID(f) != pktPoints || len(f) < 2 {
			continue
		}
		var d struct {
			Instance  string `json:"instance"`
			HitPoints *int   `json:"hitPoints"`
		}
		if json.Unmarshal(frameData(f), &d) != nil || d.Instance != welcome.Instance || d.HitPoints == nil {
			continue
		}
		sawPoints = true
		if minHP < 0 || *d.HitPoints < minHP {
			minHP = *d.HitPoints
		}
	}
	check(sawPoints && minHP >= 0 && minHP < 100, fmt.Sprintf("poison -> Points HP ticks below 100 (min=%d)", minHP))
	lastFrames = nil

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("FAILED: %d checks\n", len(failures))
		for _, f := range failures {
			fmt.Println(" -", f)
		}
		os.Exit(1)
	}
	fmt.Println("CHECK PASSED (abilities: unlock + cast + cooldown + DoT)")
}
