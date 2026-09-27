// Scripted WS check for the world wiring (warps + events + lights/signs).
// Black-box client against a TESTMAP server booted with shortened event and
// cooldown envs so every leg is observable without long waits:
//
//	PORT=9136 DB_PATH=/tmp/world.db WORLD_EVENT_MS=1500 WORLD_WARP_COOLDOWN_MS=0 go run .
//	PORT=9136 go run ./e2e/world
//
// Legs: login, worldtest echo, warp list, Lamp frames on region-enter (login
// push of the TESTMAP synthetic lamp at 102,96), sign Bubble via worldtest +
// via Target on a real sign position, menu warp [39] to mudwich (Teleport +
// WARPED_TO), quest-gated warp denial (aynor), and a global event notice.
// No skips: every leg is exercisable from spawn (sign Target is
// distance-lenient by design, warps teleport remotely).
// Not part of the stub build (run via `go run`).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Packet ids (mirrors go-server/packets.go).
const (
	pktWelcome  = 3
	pktTeleport = 12
	pktChat     = 19
	pktNotify   = 25
	pktOverlay  = 41
	pktBubble   = 43
	pktMinigame = 46
	pktWarp     = 39
	pktTarget   = 14
)

var failures []string

func check(cond bool, msg string) {
	if !cond {
		failures = append(failures, msg)
		fmt.Println("  FAIL:", msg)
	} else {
		fmt.Println("  ok:", msg)
	}
}

type client struct {
	conn *websocket.Conn
	inst string
	mu   sync.Mutex
	log  [][]json.RawMessage
	ch   chan []json.RawMessage
}

func (c *client) reader() {
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var bulk [][]json.RawMessage
		if err := json.Unmarshal(raw, &bulk); err != nil {
			continue
		}
		for _, f := range bulk {
			c.mu.Lock()
			c.log = append(c.log, f)
			c.mu.Unlock()
			c.ch <- f
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
		if err := json.Unmarshal(f[1], &op); err == nil {
			return op
		}
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

// snapshot returns all frames seen so far.
func (c *client) snapshot() [][]json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]json.RawMessage(nil), c.log...)
}

// waitFor polls up to d for a frame matching pred (scanning history too).
func (c *client) waitFor(d time.Duration, pred func([]json.RawMessage) bool) []json.RawMessage {
	dead := time.Now().Add(d)
	for _, f := range c.snapshot() {
		if pred(f) {
			return f
		}
	}
	for time.Now().Before(dead) {
		select {
		case f := <-c.ch:
			if pred(f) {
				return f
			}
		case <-time.After(100 * time.Millisecond):
			for _, f := range c.snapshot() {
				if pred(f) {
					return f
				}
			}
		}
	}
	return nil
}

func notifyText(f []json.RawMessage) string {
	if frameID(f) != pktNotify {
		return ""
	}
	var d struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(frameData(f), &d)
	return d.Message
}

func waitNotify(c *client, d time.Duration, substr string) []json.RawMessage {
	return c.waitFor(d, func(f []json.RawMessage) bool {
		return strings.Contains(notifyText(f), substr)
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9001"
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	defer conn.Close()
	c := &client{conn: conn, ch: make(chan []json.RawMessage, 4096)}
	go c.reader()

	c.waitFor(2*time.Second, func(f []json.RawMessage) bool { return frameID(f) == 0 })
	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	c.waitFor(2*time.Second, func(f []json.RawMessage) bool { return frameID(f) == 1 })
	send(conn, `[2,{"opcode":0,"username":"worldtester","password":"x","seedRank":2}]`)
	wf := c.waitFor(3*time.Second, func(f []json.RawMessage) bool { return frameID(f) == pktWelcome })
	check(wf != nil, "welcome received")
	if wf != nil {
		var w struct {
			Instance string `json:"instance"`
		}
		_ = json.Unmarshal(frameData(wf), &w)
		c.inst = w.Instance
		check(c.inst != "", "welcome instance non-empty ("+c.inst+")")
	}
	send(conn, `[9,{"regionsLoaded":true,"userAgent":"world-e2e"}]`)
	time.Sleep(1500 * time.Millisecond) // let Ready spawns + login Lamp push land

	// --- worldtest echo ---
	send(conn, `[46,{"worldtest":"echo"}]`)
	n := waitNotify(c, 3*time.Second, "world:ok")
	check(n != nil, "worldtest echo")
	if n != nil {
		msg := notifyText(n)
		check(strings.Contains(msg, "warps=6"), "echo reports 6 warps ("+msg+")")
		check(strings.Contains(msg, "signs=14"), "echo reports 14 signs ("+msg+")")
	}

	// --- warp list ---
	send(conn, `[46,{"worldtest":"warps"}]`)
	n = waitNotify(c, 3*time.Second, "world:warps")
	check(n != nil, "warp list echo")
	if n != nil {
		msg := notifyText(n)
		check(strings.Contains(msg, "mudwich"), "warp list has mudwich ("+msg+")")
		check(strings.Contains(msg, "undersea"), "warp list has undersea ("+msg+")")
	}

	// --- lights: Lamp frames on region-enter (login push) ---
	lamp := c.waitFor(1*time.Second, func(f []json.RawMessage) bool {
		if frameID(f) != pktOverlay || frameOpcode(f) != 2 {
			return false
		}
		var d struct {
			Light struct {
				Instance string `json:"instance"`
				X        int    `json:"x"`
				Y        int    `json:"y"`
			} `json:"light"`
		}
		_ = json.Unmarshal(frameData(f), &d)
		return d.Light.X == 102 && d.Light.Y == 96
	})
	check(lamp != nil, "overlay Lamp for test lamp 102,96 on region-enter")
	send(conn, `[46,{"worldtest":"lights"}]`)
	n = waitNotify(c, 3*time.Second, "world:lights")
	check(n != nil, "lights echo")

	// --- signs list + sign bubble via debug ---
	send(conn, `[46,{"worldtest":"signs"}]`)
	n = waitNotify(c, 3*time.Second, "world:signs n=14")
	check(n != nil, "signs echo n=14")
	send(conn, `[46,{"worldtest":"sign"}]`)
	bub := c.waitFor(3*time.Second, func(f []json.RawMessage) bool {
		if frameID(f) != pktBubble || frameOpcode(f) != 1 {
			return false
		}
		var d struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(frameData(f), &d)
		return d.Text != ""
	})
	check(bub != nil, "bubble Position with sign text via worldtest")
	n = waitNotify(c, 3*time.Second, "world:sign ")
	check(n != nil, "sign echo notify")

	// --- sign bubble via Target on a real sign position ---
	send(conn, `[14,[0,"796-646"]]`)
	bub2 := c.waitFor(3*time.Second, func(f []json.RawMessage) bool {
		if frameID(f) != pktBubble || frameOpcode(f) != 1 {
			return false
		}
		var d struct {
			Instance string `json:"instance"`
			Text     string `json:"text"`
		}
		_ = json.Unmarshal(frameData(f), &d)
		return d.Instance == "796-646" && strings.Contains(d.Text, "Tread carefully")
	})
	check(bub2 != nil, "bubble Position on Target 796-646 (Tread carefully...)")

	// --- tutorial gate (fresh account warps denied until tutorial finished) ---
	send(conn, `[39,{"id":0}]`)
	n = waitNotify(c, 3*time.Second, "warps:CANNOT_WARP_TUTORIAL")
	check(n != nil, "tutorial gate denies warp id 0 on fresh account")
	send(conn, `[46,{"m11test":"setstage","key":"tutorial","stage":16,"sub":0}]`)
	time.Sleep(500 * time.Millisecond)

	// --- menu warp [39] to mudwich (id 0) ---
	send(conn, `[39,{"id":0}]`)
	tp := c.waitFor(3*time.Second, func(f []json.RawMessage) bool {
		if frameID(f) != pktTeleport {
			return false
		}
		var d struct {
			Instance string `json:"instance"`
			X        int    `json:"x"`
			Y        int    `json:"y"`
		}
		_ = json.Unmarshal(frameData(f), &d)
		return d.Instance == c.inst && d.X >= 188 && d.X < 192 && d.Y >= 157 && d.Y < 161
	})
	check(tp != nil, "teleport into mudwich rect on warp id 0")
	n = waitNotify(c, 3*time.Second, "warps:WARPED_TO")
	check(n != nil, "WARPED_TO notify")

	// --- quest-gated warp denial (aynor needs ancientlands) ---
	send(conn, `[46,{"worldtest":"warp","name":"aynor"}]`)
	n = waitNotify(c, 3*time.Second, "warps:CANNOT_WARP_QUEST")
	check(n != nil, "quest gate denies aynor warp")

	// --- warp geometry probe ---
	send(conn, `[46,{"worldtest":"at","x":189,"y":158}]`)
	n = waitNotify(c, 3*time.Second, "world:at 189,158")
	check(n != nil, "warp At() probe echo")

	// --- event notice (server runs WORLD_EVENT_MS=1500, fires every ~1.5s) ---
	ev := c.waitFor(15*time.Second, func(f []json.RawMessage) bool {
		if frameID(f) != pktChat {
			return false
		}
		var d struct {
			Source  string `json:"source"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(frameData(f), &d)
		return strings.Contains(d.Source, "WORLD") && strings.Contains(d.Message, "event has started")
	})
	check(ev != nil, "global event notice observed")
	send(conn, `[46,{"worldtest":"events"}]`)
	n = waitNotify(c, 3*time.Second, "world:events")
	check(n != nil, "events state echo")

	if len(failures) > 0 {
		fmt.Printf("CHECK FAILED (%d)\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("CHECK PASSED")
}
