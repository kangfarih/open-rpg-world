// Scripted WS check for SHOWCASE mode (TESTMAP=1, the default): CLONED 9 real
// regions as base + center pond (104,104 rx4 ry3) overlay + 5-resource mixed
// demo line at y=98 on forced grass (2 oaks + coalrock + shrimpspot +
// blueberrybush) + 232-entity static showcase grid (156 m-show-* Mobs
// + 76 n-show-* NPCs, x96-126 y110-138, forced grass) + 2 paperdoll demo
// players + 5s rotating Attack/Idle anim broadcasts. Gather ONLY on Target
// Object(3) (one click = one swing, probabilistic exhaust per swing).
// Not part of the stub build (underscore dirs are ignored by the go tool).
// Usage: go run ./e2e/testmap (stub must run with TESTMAP=1, the default).
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// readOne reads a single WS message (bulk [[id,..],...]) and returns the
// packet ids it contains. Empty slice on timeout.
var incoming = make(chan []json.RawMessage, 4096)

func reader(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage() // no deadline: timeouts handled via select
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

// drain collects packet ids for d, returning counts per id.
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

type spawnInfo struct {
	x, y     int
	key      string
	typ      int
	hasEquip bool
}

var (
	anim     = map[string]int{}
	res1     = map[string]int{}
	res0     = map[string]int{}
	spawns   = map[string]spawnInfo{}
	showAtk  = map[string]int{} // m-show-* action==1 showcase anims
	showIdle = map[string]int{} // n-show-* action==0 showcase anims
	failures []string
)

var oakTiles = map[[2]int]bool{
	{98, 98}: true, {100, 98}: true, {102, 98}: true, {104, 98}: true, {106, 98}: true,
}

func isPond(x, y int) bool {
	dx := float64(x - 104)
	dy := float64(y - 104)
	return (dx*dx)/16+(dy*dy)/9 <= 1
}

func check(cond bool, msg string) {
	if !cond {
		failures = append(failures, msg)
		fmt.Println("  FAIL:", msg)
	} else {
		fmt.Println("  ok:", msg)
	}
}

func tally(counts map[int]int) {
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		switch id {
		case 5:
			var e struct {
				Instance   string `json:"instance"`
				Type       int    `json:"type"`
				Key        string `json:"key"`
				Name       string `json:"name"`
				X          int    `json:"x"`
				Y          int    `json:"y"`
				Equipments []any  `json:"equipments"`
			}
			_ = json.Unmarshal(frameData(f), &e)
			if e.Instance != "" {
				spawns[e.Instance] = spawnInfo{x: e.X, y: e.Y, key: e.Key, typ: e.Type, hasEquip: len(e.Equipments) > 0}
			}
		case 16:
			var a struct {
				Instance         string `json:"instance"`
				Action           int    `json:"action"`
				ResourceInstance string `json:"resourceInstance"`
			}
			_ = json.Unmarshal(frameData(f), &a)
			// Chop hits carry resourceInstance from any player instance
			// (Welcome ids are random p-<rand> per connection since M2).
			if a.Action == 1 && a.ResourceInstance != "" {
				anim[a.ResourceInstance]++
			}
			if strings.HasPrefix(a.Instance, "m-show-") && a.Action == 1 {
				showAtk[a.Instance]++
			}
			if strings.HasPrefix(a.Instance, "n-show-") && a.Action == 0 {
				showIdle[a.Instance]++
			}
		case 59:
			var r struct {
				Instance string `json:"instance"`
				State    int    `json:"state"`
			}
			_ = json.Unmarshal(frameData(f), &r)
			if r.State == 1 {
				res1[r.Instance]++
			} else {
				res0[r.Instance]++
			}
		}
	}
	lastFrames = nil
	_ = counts
}

// stops counts S Stop ([11,3]) + Teleport ([12]) frames in lastFrames — the
// reject signal.
func stops() (stop, teleport int) {
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id == 12 {
			teleport++
		}
		if id == 11 && len(f) >= 3 {
			var opcode int
			if _ = json.Unmarshal(f[1], &opcode); opcode == 3 {
				stop++
			}
		}
	}
	lastFrames = nil
	return stop, teleport
}

type tile struct {
	X    int             `json:"x"`
	Y    int             `json:"y"`
	Data json.RawMessage `json:"data"`
	C    bool            `json:"c"`
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

	fmt.Println("connected:", drain(conn, 2*time.Second)) // Connected(0)
	lastFrames = nil

	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	fmt.Println("handshake:", drain(conn, 2*time.Second)) // Handshake(1)
	lastFrames = nil

	send(conn, `[2,{"opcode":0,"username":"tester","password":"x"}]`)
	fmt.Println("login:", drain(conn, 3*time.Second)) // Welcome(3)+Map(4)

	// --- Welcome + Map frame checks ---
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
	check(welcome.X == 100 && welcome.Y == 96,
		fmt.Sprintf("hero welcome at 100,96 (got %d,%d)", welcome.X, welcome.Y))
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
		check(ok, "real region "+rid+" present (cloned base)")
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
	check(totalTiles > 15000 && totalTiles < 25000,
		fmt.Sprintf("base tile count ~19k real + overlays (got %d)", totalTiles))
	grassAt := func(x, y int) (tile, bool) { t, ok := byXY[[2]int{x, y}]; return t, ok }
	checkTile := func(x, y int, wantData string, wantC bool, label string) {
		t, ok := grassAt(x, y)
		if !ok {
			check(false, label+" present")
			return
		}
		check(string(t.Data) == wantData && t.C == wantC,
			fmt.Sprintf("%s data=%s c=%v (got data=%s c=%v)", label, wantData, wantC, string(t.Data), t.C))
	}
	// Hero/guest stand on cloned base terrain (walkable, presence only).
	for _, p := range [][2]int{{100, 96}, {101, 96}} {
		t, ok := grassAt(p[0], p[1])
		check(ok && !t.C, fmt.Sprintf("base tile walkable at (%d,%d) (got %+v ok=%v)", p[0], p[1], t, ok))
	}
	// Base terrain proof far from overlays: real layered/data tiles survive.
	for _, p := range [][2]int{{140, 100}, {140, 140}} {
		t, ok := grassAt(p[0], p[1])
		check(ok, fmt.Sprintf("cloned base tile present at (%d,%d) (got %+v ok=%v)", p[0], p[1], t, ok))
	}
	checkTile(104, 104, "29", true, "pond center (104,104)")
	checkTile(107, 104, "29", true, "pond inside (107,104)")
	checkTile(104, 107, "29", true, "pond south edge (104,107)")
	// Adjacent base tiles outside the ellipse keep base data (not pond).
	for _, p := range [][2]int{{99, 104}, {104, 100}, {104, 108}} {
		t, ok := grassAt(p[0], p[1])
		check(ok && string(t.Data) != "29",
			fmt.Sprintf("base kept near pond (%d,%d) (got data=%s ok=%v)", p[0], p[1], string(t.Data), ok))
	}
	for i, x := range []int{98, 100, 102, 104, 106} {
		checkTile(x, 98, "3907", false, fmt.Sprintf("demo t-test-%d tile (%d,98)", i+1, x))
	}
	// Grid corners must be walkable grass too (no region expansion needed).
	// Note: the last row holds only 8 slots (232 = 14x16 + 8), so the final
	// occupied slot is (110,138); (126,138) is untouched colliding base (403).
	checkTile(96, 110, "3907", false, "grid NW corner (96,110)")
	checkTile(126, 110, "3907", false, "grid NE corner (126,110)")
	checkTile(96, 138, "3907", false, "grid SW corner (96,138)")
	checkTile(110, 138, "3907", false, "grid last slot (110,138)")
	checkTile(126, 138, "403", true, "untouched colliding base (126,138)")
	lastFrames = nil

	send(conn, `[9,{"regionsLoaded":true,"userAgent":"check"}]`)
	fmt.Println("ready:", drain(conn, 4*time.Second)) // Spawns (3 bulks)
	tally(nil)

	// --- Spawn checks ---
	s, ok := spawns["p2"]
	check(ok && s.x == 101 && s.y == 96 && s.key == "base",
		fmt.Sprintf("guest p2 at 101,96 (got %+v)", s))
	for _, gone := range []string{"m1", "t1", "t-oak-1", "m-test-1", "m-test-8"} {
		_, ok := spawns[gone]
		check(!ok, "legacy gone: "+gone)
	}
	wantRes := map[string]struct {
		x, y, typ int
		key       string
	}{
		"t-test-1": {98, 98, 10, "oak"},
		"t-test-2": {100, 98, 10, "oak"},
		"t-test-3": {102, 98, 11, "coalrock"},
		"t-test-4": {104, 98, 13, "shrimpspot"},
		"t-test-5": {106, 98, 12, "blueberrybush"},
	}
	for inst, want := range wantRes {
		got, ok := spawns[inst]
		check(ok && got.x == want.x && got.y == want.y && got.key == want.key && got.typ == want.typ,
			fmt.Sprintf("spawn %s type=%d key=%s at %d,%d (got %+v)", inst, want.typ, want.key, want.x, want.y, got))
	}

	// Showcase grid: 156 mobs + 76 NPCs, alphabetical, exact grid slots.
	mKeys, nKeys := []string{}, []string{}
	for inst, si := range spawns {
		switch {
		case strings.HasPrefix(inst, "m-show-"):
			mKeys = append(mKeys, si.key)
		case strings.HasPrefix(inst, "n-show-"):
			nKeys = append(nKeys, si.key)
		}
	}
	check(len(mKeys) == 156, fmt.Sprintf("156 showcase mobs (got %d)", len(mKeys)))
	check(len(nKeys) == 76, fmt.Sprintf("76 showcase NPCs (got %d)", len(nKeys)))
	// Alphabetical: collect per-index keys and verify sorted order.
	mByIdx := map[int]string{}
	dupM := false
	seenM := map[string]bool{}
	for inst, si := range spawns {
		if !strings.HasPrefix(inst, "m-show-") {
			continue
		}
		var n int
		fmt.Sscanf(inst, "m-show-%d", &n)
		if _, dup := mByIdx[n]; dup {
			dupM = true
		}
		mByIdx[n] = si.key
		if seenM[si.key] {
			dupM = true
		}
		seenM[si.key] = true
	}
	ordered := make([]string, 0, len(mByIdx))
	for i := 1; i <= len(mByIdx); i++ {
		ordered = append(ordered, mByIdx[i])
	}
	check(!dupM, "mob keys unique, instances unique")
	check(sort.StringsAreSorted(ordered),
		fmt.Sprintf("mobs alphabetical m-show-1=%s m-show-156=%s", ordered[0], ordered[len(ordered)-1]))
	nByIdx := map[int]string{}
	seenN := map[string]bool{}
	dupN := false
	for inst, si := range spawns {
		if !strings.HasPrefix(inst, "n-show-") {
			continue
		}
		var n int
		fmt.Sscanf(inst, "n-show-%d", &n)
		if _, dup := nByIdx[n]; dup {
			dupN = true
		}
		nByIdx[n] = si.key
		if seenN[si.key] {
			dupN = true
		}
		seenN[si.key] = true
	}
	nOrdered := make([]string, 0, len(nByIdx))
	for i := 1; i <= len(nByIdx); i++ {
		nOrdered = append(nOrdered, nByIdx[i])
	}
	check(!dupN, "npc keys unique, instances unique")
	check(sort.StringsAreSorted(nOrdered),
		fmt.Sprintf("npcs alphabetical n-show-1=%s n-show-76=%s", nOrdered[0], nOrdered[len(nOrdered)-1]))

	// Grid geometry + walkability: exact slots, all grass c:false, pond clear.
	minX, maxX, minY, maxY := 1<<30, -1, 1<<30, -1
	badTiles := []string{}
	pondHits := []string{}
	for inst, si := range spawns {
		if !strings.HasPrefix(inst, "m-show-") && !strings.HasPrefix(inst, "n-show-") {
			continue
		}
		var n, base int
		if strings.HasPrefix(inst, "m-show-") {
			fmt.Sscanf(inst, "m-show-%d", &n)
			base = n - 1
		} else {
			fmt.Sscanf(inst, "n-show-%d", &n)
			base = 156 + n - 1
		}
		wantX, wantY := 96+(base%16)*2, 110+(base/16)*2
		if si.x != wantX || si.y != wantY {
			badTiles = append(badTiles, fmt.Sprintf("%s at %d,%d want %d,%d", inst, si.x, si.y, wantX, wantY))
		}
		if si.x < minX {
			minX = si.x
		}
		if si.x > maxX {
			maxX = si.x
		}
		if si.y < minY {
			minY = si.y
		}
		if si.y > maxY {
			maxY = si.y
		}
		t, ok := byXY[[2]int{si.x, si.y}]
		if !ok || t.C || string(t.Data) != "3907" {
			badTiles = append(badTiles, fmt.Sprintf("%s not walkable grass: %+v ok=%v", inst, t, ok))
		}
		if isPond(si.x, si.y) {
			pondHits = append(pondHits, inst)
		}
		// Type check: Mob=3, NPC=1.
		wantType := 3
		if strings.HasPrefix(inst, "n-show-") {
			wantType = 1
		}
		if si.typ != wantType {
			badTiles = append(badTiles, fmt.Sprintf("%s type=%d want %d", inst, si.typ, wantType))
		}
	}
	check(len(badTiles) == 0, fmt.Sprintf("grid slots exact + grass c:false + types (bad=%v)", badTiles))
	check(minX == 96 && maxX == 126 && minY == 110 && maxY == 138,
		fmt.Sprintf("grid extent x96-126 y110-138 (got x%d-%d y%d-%d)", minX, maxX, minY, maxY))
	check(len(pondHits) == 0, fmt.Sprintf("pond clear of grid (hits=%v)", pondHits))
	// No spawn at all (incl. demos) inside the pond.
	for inst, si := range spawns {
		if isPond(si.x, si.y) {
			pondHits = append(pondHits, inst)
		}
	}
	check(len(pondHits) == 0, fmt.Sprintf("pond clear of ALL spawns (hits=%v)", pondHits))

	// Demo players with equipment.
	for _, d := range []struct {
		inst, weapon string
		x, y         int
	}{{"p-show-1", "ironsword", 98, 108}, {"p-show-2", "goldsword", 100, 108}} {
		got, ok := spawns[d.inst]
		check(ok && got.x == d.x && got.y == d.y && got.key == "base" && got.hasEquip,
			fmt.Sprintf("demo %s at %d,%d equipped (got %+v)", d.inst, d.x, d.y, got))
	}

	totalSpawns := len(spawns)
	check(totalSpawns == 240, fmt.Sprintf("240 total spawns (p2+5 demo resources+156+76+2 demos, got %d)", totalSpawns))

	// --- Collision: pond Step rejected, resource Step rejected, grass Step silent ---
	drain(conn, 1*time.Second)
	lastFrames = nil
	send(conn, `[11,{"opcode":2,"playerX":100,"playerY":96,"nextGridX":104,"nextGridY":104}]`)
	_ = drain(conn, 2*time.Second)
	st, tp := stops()
	check(st >= 1 && tp >= 1,
		fmt.Sprintf("pond Step rejected (stop=%d teleport=%d)", st, tp))
	send(conn, `[11,{"opcode":2,"playerX":100,"playerY":96,"nextGridX":98,"nextGridY":98}]`)
	_ = drain(conn, 2*time.Second)
	st, tp = stops()
	check(st >= 1 && tp >= 1,
		fmt.Sprintf("resource Step rejected (stop=%d teleport=%d)", st, tp))
	send(conn, `[11,{"opcode":2,"playerX":100,"playerY":96,"nextGridX":101,"nextGridY":96}]`)
	_ = drain(conn, 2*time.Second)
	st, tp = stops()
	check(st == 0 && tp == 0,
		fmt.Sprintf("grass Step silent (stop=%d teleport=%d)", st, tp))

	// --- Gather works: Target Object = 1 swing each; loop each demo resource
	// until its probabilistic exhaust depletes it (foraging depletes on the
	// first swing). 800ms drains keep clear of the 600ms per-instance debounce.
	swing := func(inst string) {
		send(conn, `[14,[3,"`+inst+`"]]`)
		tally(drain(conn, 800*time.Millisecond))
	}
	before := anim["t-test-1"]
	swing("t-test-1")
	check(anim["t-test-1"]-before == 1,
		fmt.Sprintf("Target Object = 1 swing (got %d)", anim["t-test-1"]-before))
	for _, inst := range []string{"t-test-1", "t-test-2", "t-test-3", "t-test-4", "t-test-5"} {
		for i := 0; i < 60 && res1[inst] == 0; i++ {
			swing(inst)
		}
		check(res1[inst] >= 1, inst+" depleted via Target loop")
	}
	extra := anim["t-test-5"]
	swing("t-test-5")
	check(anim["t-test-5"]-extra == 0, "extra swing on depleted t-test-5 ignored")

	// --- Respawn: 15s fallback table timers -> all 5 return to state 0 ---
	fmt.Println("waiting for resource respawn (~15s fallback)...")
	tally(drain(conn, 20*time.Second))
	for _, inst := range []string{"t-test-1", "t-test-2", "t-test-3", "t-test-4", "t-test-5"} {
		check(res0[inst] >= 1, inst+" respawned (state 0)")
	}

	// --- Showcase anims: wait for a 5s tick -> 10 atk + 5 idle ---
	fmt.Println("waiting for showcase anim tick (~5s)...")
	tally(drain(conn, 8*time.Second))
	atkTotal := 0
	for _, c := range showAtk {
		atkTotal += c
	}
	idleTotal := 0
	for _, c := range showIdle {
		idleTotal += c
	}
	check(atkTotal >= 10, fmt.Sprintf(">=10 showcase Attack broadcasts (got %d)", atkTotal))
	check(idleTotal >= 5, fmt.Sprintf(">=5 showcase Idle broadcasts (got %d)", idleTotal))
	check(len(showAtk) >= 10, fmt.Sprintf(">=10 distinct mobs attacked (got %d)", len(showAtk)))

	fmt.Printf("RESULT spawns=%d mobs=%d npcs=%d anim=%v depleted=%v showAtk=%d showIdle=%d\n",
		totalSpawns, len(mKeys), len(nKeys), anim, res1, atkTotal, idleTotal)
	if len(failures) > 0 {
		fmt.Printf("CHECK FAILED (%d failures)\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("CHECK PASSED")
}
