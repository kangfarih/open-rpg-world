// Scripted WS check for CLEAN mode (CLEAN=1): pure original 9 regions with
// zero overlays + exactly ONE fully-equipped adventurer showcase.
// Not part of the stub build (run via `go run ./_cleancheck` from
// the stub dir so module deps resolve; or copy under the stub as _cleancheck).
// Usage: go run ./_cleancheck (stub must run with CLEAN=1 on :9001).
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var incoming = make(chan []json.RawMessage, 4096)

func reader(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			close(incoming)
			return
		}
		var bulk [][]json.RawMessage
		if err := json.Unmarshal(raw, &bulk); err != nil {
			fmt.Println("parse fail:", string(raw))
			continue
		}
		for _, f := range bulk {
			incoming <- f
		}
	}
}

var lastFrames [][]json.RawMessage

func drain(conn *websocket.Conn, d time.Duration) map[int]int {
	counts := map[int]int{}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-incoming:
			if !ok {
				return counts
			}
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

type tile struct {
	X    int             `json:"x"`
	Y    int             `json:"y"`
	Data json.RawMessage `json:"data"`
	C    bool            `json:"c"`
}

// slotFolder maps equipment slot -> player/ sprite folder ("" = items/ fallback).
func slotFolder(slot int) string {
	switch slot {
	case 0:
		return "helmet"
	case 3:
		return "chestplate"
	case 4:
		return "weapon"
	case 5:
		return "shield"
	case 9:
		return "legplates"
	case 10:
		return "cape"
	default:
		return ""
	}
}

// spriteBaseDir resolves the client sprites dir portably: SPRITES_DIR env
// wins, else a relative path (go-server CWD), else the legacy absolute path.
func spriteBaseDir() string {
	if p := os.Getenv("SPRITES_DIR"); p != "" {
		return p
	}
	for _, c := range []string{
		"../packages/client/public/img/sprites",
		"../../../packages/client/public/img/sprites",
		"packages/client/public/img/sprites",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "../packages/client/public/img/sprites"
}

func main() {
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
	defer conn.Close()
	go reader(conn)

	fmt.Println("connected:", drain(conn, 2*time.Second))
	lastFrames = nil

	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	fmt.Println("handshake:", drain(conn, 2*time.Second))
	lastFrames = nil

	send(conn, `[2,{"opcode":0,"username":"tester","password":"x"}]`)
	fmt.Println("login:", drain(conn, 3*time.Second))

	var welcome struct {
		Instance string `json:"instance"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
	}
	var mapB64 string
	var mapBuf float64
	var mapElems int
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id == 3 {
			_ = json.Unmarshal(frameData(f), &welcome)
		}
		if id == 4 {
			mapElems = len(f)
			_ = json.Unmarshal(f[1], &mapB64)
			if len(f) >= 3 {
				_ = json.Unmarshal(f[2], &mapBuf)
			}
		}
	}
	check((welcome.Instance == "p1" || strings.HasPrefix(welcome.Instance, "p-")) && welcome.X == 100 && welcome.Y == 96,
		fmt.Sprintf("hero Welcome p1 at 100,96 (got %s %d,%d)", welcome.Instance, welcome.X, welcome.Y))
	check(mapElems == 3, fmt.Sprintf("map framing [4,b64,bufSize] (got %d elems)", mapElems))

	gz, err := base64.StdEncoding.DecodeString(mapB64)
	check(err == nil, "map base64 decodes")
	zr, err := zlib.NewReader(bytes.NewReader(gz))
	check(err == nil, "map zlib reader opens")
	rawJSON, err := io.ReadAll(zr)
	check(err == nil, "map zlib reads")
	check(int(mapBuf) == len(rawJSON),
		fmt.Sprintf("map bufSize==json bytes (%d==%d)", int(mapBuf), len(rawJSON)))

	var regions map[string][]tile
	check(json.Unmarshal(rawJSON, &regions) == nil, "map regions JSON parses")
	for _, rid := range []string{"25", "26", "27", "49", "50", "51", "73", "74", "75"} {
		_, ok := regions[rid]
		check(ok, "pure region "+rid+" present")
	}
	check(len(regions) == 9, fmt.Sprintf("exactly 9 regions (got %d)", len(regions)))

	byXY := map[[2]int]tile{}
	totalTiles := 0
	for _, tiles := range regions {
		totalTiles += len(tiles)
		for _, t := range tiles {
			byXY[[2]int{t.X, t.Y}] = t
		}
	}
	fmt.Printf("  info: totalTiles=%d\n", totalTiles)

	// Pure-original spot check: pond center must be base terrain, NOT water 29.
	t, ok := byXY[[2]int{104, 104}]
	check(ok, "tile (104,104) present")
	if ok {
		check(string(t.Data) != "29",
			fmt.Sprintf("(104,104) is base value not water 29 (got data=%s c=%v)", string(t.Data), t.C))
		check(string(t.Data) == "3907" && !t.C,
			fmt.Sprintf("(104,104) pure base 3907 walkable (got data=%s c=%v)", string(t.Data), t.C))
	}
	// No forced-grass stamps on overlay spots: showcase corner + demo spots
	// must NOT be stamped grass (they keep whatever base has, or are absent).
	// The key discriminator is absence of test entities (checked via spawns).
	lastFrames = nil

	send(conn, `[9,{"regionsLoaded":true,"userAgent":"cleancheck"}]`)
	fmt.Println("ready:", drain(conn, 4*time.Second))

	// --- Spawn + Equipment Batch checks ---
	type equip struct {
		Type int    `json:"type"`
		Key  string `json:"key"`
	}
	type spawn struct {
		Instance   string  `json:"instance"`
		Type       int     `json:"type"`
		Key        string  `json:"key"`
		X          int     `json:"x"`
		Y          int     `json:"y"`
		Equipments []equip `json:"equipments"`
	}
	spawns := map[string]spawn{}
	batchCount := 0
	var batchEquips []equip
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		switch id {
		case 5:
			var s spawn
			_ = json.Unmarshal(frameData(f), &s)
			if s.Instance != "" {
				spawns[s.Instance] = s
			}
		case 8:
			if len(f) < 3 {
				continue
			}
			var opcode int
			_ = json.Unmarshal(f[1], &opcode)
			if opcode != 0 {
				continue
			}
			var body struct {
				Data struct {
					Equipments []equip `json:"equipments"`
				} `json:"data"`
			}
			if json.Unmarshal(frameData(f), &body) == nil {
				batchCount++
				batchEquips = body.Data.Equipments
			}
		}
	}
	lastFrames = nil

	check(len(spawns) == 1, fmt.Sprintf("exactly 1 Spawn frame (got %d: %v)", len(spawns), spawns))
	adv, ok := spawns["p-adv-1"]
	check(ok, "adventurer p-adv-1 spawned")
	players := 1 // hero via Welcome
	if ok {
		players++
		check(adv.Type == 0 && adv.Key == "base" && adv.X == 102 && adv.Y == 96,
			fmt.Sprintf("adventurer Player base at 102,96 (got type=%d key=%s %d,%d)", adv.Type, adv.Key, adv.X, adv.Y))
	}
	check(players == 2, fmt.Sprintf("spawn count == 2 (hero+adventurer, got %d)", players))
	for _, gone := range []string{"p2", "m1", "t1", "t-test-1", "m-show-1", "n-show-1", "p-show-1", "p-show-2"} {
		_, dup := spawns[gone]
		check(!dup, "overlay gone: "+gone)
	}
	// Adventurer stands on walkable pure terrain.
	if at, ok := byXY[[2]int{102, 96}]; ok {
		check(!at.C, fmt.Sprintf("adventurer tile (102,96) walkable (data=%s c=%v)", string(at.Data), at.C))
	} else {
		check(false, "adventurer tile (102,96) present in map")
	}

	// 6+ equipment pieces, all with valid sprite keys.
	if ok {
		spriteBase := spriteBaseDir()
		check(len(adv.Equipments) >= 6,
			fmt.Sprintf("adventurer has 6+ equipment pieces (got %d)", len(adv.Equipments)))
		for _, e := range adv.Equipments {
			var path string
			if folder := slotFolder(e.Type); folder != "" {
				path = fmt.Sprintf("%s/player/%s/%s.png", spriteBase, folder, e.Key)
			} else {
				path = fmt.Sprintf("%s/items/%s.png", spriteBase, e.Key)
			}
			_, statErr := os.Stat(path)
			check(statErr == nil, fmt.Sprintf("sprite exists slot=%d key=%s (%s)", e.Type, e.Key, path))
		}
	}

	// Paperdoll Batch received (opcode 0) with matching set.
	check(batchCount >= 1, fmt.Sprintf("Equipment Batch (8,0) received (got %d)", batchCount))
	if batchCount >= 1 {
		check(len(batchEquips) >= 6,
			fmt.Sprintf("Batch has 6+ pieces (got %d)", len(batchEquips)))
		want := map[string]int{}
		for _, e := range adv.Equipments {
			want[e.Key] = e.Type
		}
		for _, e := range batchEquips {
			typ, dup := want[e.Key]
			check(dup && typ == e.Type, fmt.Sprintf("Batch piece matches Spawn: slot=%d key=%s", e.Type, e.Key))
		}
	}

	// Showcase ticker disabled: 6s window must carry zero Animation broadcasts.
	fmt.Println("waiting 6s for (absent) showcase anim tick...")
	counts := drain(conn, 6*time.Second)
	lastFrames = nil
	check(counts[16] == 0, fmt.Sprintf("no Animation broadcasts in CLEAN (got %d)", counts[16]))
	_ = counts

	if len(failures) > 0 {
		fmt.Printf("CHECK FAILED (%d failures)\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("CHECK PASSED")
}
