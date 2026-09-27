// Scripted WS check for the M6 slice (TESTMAP=1 default server): full economy
// loop — Target Talk(0) opens the store (forester) / bank (redstoremannpc
// banker) via the showcase NPC grid, gather an oak (Target Object = 1 swing,
// probabilistic exhaust), sell the logs to the store (getTotalCost curve),
// buy arrows (stock decrement + currency spend), Store Select quote,
// movement closes the store, bank Container Select deposit/withdraw,
// inventory swap, and SQLite persistence across a relogin (container batches).
// Not part of the stub build (underscore dirs are ignored by the go tool).
// Usage: go run ./e2e/m6 (stub must run with TESTMAP=1, the default).
package main

import (
	"encoding/json"
	"fmt"
	"os"
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
			// Never close(incoming): the relogin leg shares the channel and a
			// closed channel would kill the second reader.
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
func drain(d time.Duration) map[int]int {
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

// frameOpcode reads the S->C opcode element f[1] of [id, opcode, data] frames
// (pktOp server shape); -1 when the frame carries no opcode.
func frameOpcode(f []json.RawMessage) int {
	if len(f) >= 3 {
		var op int
		_ = json.Unmarshal(f[1], &op)
		return op
	}
	return -1
}

// containerMsg is one parsed S->C Container frame ([21, opcode, {type,slot}]).
type containerMsg struct {
	Opcode int
	Type   int
	Slot   *invSlot
}

func parseContainer(f []json.RawMessage) (containerMsg, bool) {
	var m containerMsg
	if len(f) < 3 {
		return m, false
	}
	_ = json.Unmarshal(f[1], &m.Opcode)
	var data struct {
		Type int      `json:"type"`
		Slot *invSlot `json:"slot"`
	}
	if json.Unmarshal(f[2], &data) == nil {
		m.Type, m.Slot = data.Type, data.Slot
		return m, true
	}
	return m, false
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

// curX/curY track the server-confirmed position across walkTo calls (the
// server position follows our Step reports exactly as long as no step is
// rejected, so consecutive walks stay desync-free).
var curX, curY = 100, 96

// walkTo steps to (tx,ty) with 1-tile Steps (opcode 2) paced at legal speed
// (movementSpeed=220ms/tile with a 5% margin) so the anticheat never fires,
// pausing for region updates along the way.
func walkTo(conn *websocket.Conn, tx, ty int) {
	px, py := curX, curY
	dir := func(cur, target int) int {
		if target > cur {
			return 1
		}
		if target < cur {
			return -1
		}
		return 0
	}
	for px != tx || py != ty {
		nx, ny := px, py
		switch {
		case px != tx:
			nx += dir(px, tx)
		case py != ty:
			ny += dir(py, ty)
		}
		// Report PlayerX/PlayerY as the DESTINATION tile: the server updates
		// its position from PlayerX, then speed-checks nextGrid against the
		// previous position — so PlayerX=next keeps every step a clean 1 tile.
		send(conn, fmt.Sprintf(`[11,{"opcode":2,"playerX":%d,"playerY":%d,"nextGridX":%d,"nextGridY":%d}]`, nx, ny, nx, ny))
		time.Sleep(320 * time.Millisecond) // > 220ms/tile (5% margin) so the speed anticheat never fires
		drain(60 * time.Millisecond)
		lastFrames = nil
		px, py = nx, ny
	}
	curX, curY = tx, ty
	time.Sleep(300 * time.Millisecond)
	drain(200 * time.Millisecond)
	lastFrames = nil
}

// invSlot is one serialized container slot.
type invSlot struct {
	Index int    `json:"index"`
	Key   string `json:"key"`
	Count int    `json:"count"`
}

func batchSlots(t json.RawMessage) map[int]invSlot {
	out := map[int]invSlot{}
	var cd struct {
		Type int `json:"type"`
		Data struct {
			Slots []invSlot `json:"slots"`
		} `json:"data"`
	}
	if json.Unmarshal(t, &cd) == nil {
		for _, s := range cd.Data.Slots {
			out[s.Index] = s
		}
	}
	return out
}

func lastOf(frames [][]json.RawMessage, id int) json.RawMessage {
	var found json.RawMessage
	for _, f := range frames {
		var fid int
		_ = json.Unmarshal(f[0], &fid)
		if fid == id {
			found = frameData(f)
		}
	}
	return found
}

type storeFrame struct {
	Key      string `json:"key"`
	Currency string `json:"currency"`
	Items    []struct {
		Key   string `json:"key"`
		Name  string `json:"name"`
		Count int    `json:"count"`
		Price int    `json:"price"`
	} `json:"items"`
	Item *struct {
		Key   string `json:"key"`
		Count int    `json:"count"`
		Price int    `json:"price"`
		Index *int   `json:"index"`
	} `json:"item"`
}

// npcPos is the showcase grid tile for NPC index i. NPCs continue AFTER the
// 156 mobs: showPos(len(showMobs)+i) = 96 + ((156+i)%16)*2, 110 + ((156+i)/16)*2
// (verified against the server's spawn log: n-show-1 -> 120,128).
func npcPos(i int) (int, int) {
	g := 156 + i
	return 96 + (g%16)*2, 110 + (g/16)*2
}

// dialPort resolves the target port: PORT env (matching the server's own
// override) or the default 9001.
func dialPort() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "9001"
}

func main() {
	agentX, agentY := npcPos(0)
	// Honor PORT (the server's own override) so a side-by-side run works
	// while the default 9001 is occupied.
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

	// Deterministic economy: seedGold=2000 tops the account up to 2000 gold
	// server-side (TESTMAP e2e hook; Node e2e accounts start pre-loaded).
	send(conn, `[2,{"opcode":0,"username":"m6tester","password":"x","seedGold":2000,"seedArrow":15,"seedRank":2}]`)
	fmt.Println("login:", drain(2*time.Second))
	// The seedArrow hook echoes a Container Add for the fresh arrow stack;
	// track its slot index for the equipment checks below.
	arrowIdx := -1
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 1 && m.Slot != nil && m.Slot.Key == "arrow" {
			arrowIdx = m.Slot.Index
		}
	}
	check(arrowIdx >= 0, fmt.Sprintf("seedArrow -> arrow stack at inventory slot %d", arrowIdx))
	lastFrames = nil

	// --- M11 claim release: NPC talk order is quest -> achievement -> store
	// (handler.handleTalkToNPC parity), and forestnpc fronts the foresting
	// quest plus the ratinfestation achievement until both finish — they
	// would swallow the forester store leg below. Finish both server-side.
	for _, rel := range []string{
		`[46,{"m11test":"setstage","key":"foresting","stage":3}]`,
		`[46,{"m11test":"setach","key":"ratinfestation","stage":21}]`,
	} {
		send(conn, rel)
		drain(700 * time.Millisecond)
	}
	lastFrames = nil

	// --- Walk to the showcase NPC grid (agent = n-show-1) and talk. ---
	fmt.Println("walking to agent (n-show-1)...")
	walkTo(conn, agentX, agentY) // agent tile; adjacent counts (lenient talk gate)

	// Plain NPC talk: NPC Talk(31,0) bubble text, advancing per talk.
	var talkF struct {
		Instance string `json:"instance"`
		Text     string `json:"text"`
	}
	firstTalk := ""
	for i := 0; i < 2; i++ {
		lastFrames = nil
		send(conn, `[14,[0,"n-show-1"]]`)
		drain(900 * time.Millisecond)
		talkF = struct {
			Instance string `json:"instance"`
			Text     string `json:"text"`
		}{}
		_ = json.Unmarshal(lastOf(lastFrames, 31), &talkF)
		if i == 0 {
			check(talkF.Instance == "n-show-1" && talkF.Text != "",
				fmt.Sprintf("agent talk 1: %q", talkF.Text))
			firstTalk = talkF.Text
		} else {
			check(talkF.Text != firstTalk,
				fmt.Sprintf("agent talk advances (%q -> %q)", firstTalk, talkF.Text))
		}
	}

	// --- Forester (n-show-18): store open via Target Talk(0). ---
	fmt.Println("walking to forester (n-show-18)...")
	fx, fy := npcPos(17)
	walkTo(conn, fx, fy)
	lastFrames = nil
	send(conn, `[14,[0,"n-show-18"]]`)
	counts := drain(1200 * time.Millisecond)
	check(counts[40] == 1, fmt.Sprintf("forester talk -> Store Open (got %d store frames)", counts[40]))
	var sf storeFrame
	_ = json.Unmarshal(lastOf(lastFrames, 40), &sf)
	check(sf.Key == "forester" && sf.Currency == "gold" && len(sf.Items) > 0,
		fmt.Sprintf("store open %s currency=%s items=%d", sf.Key, sf.Currency, len(sf.Items)))
	var bronzePrice int
	for _, it := range sf.Items {
		if it.Key == "bronzeaxe" {
			bronzePrice = it.Price
		}
	}
	check(bronzePrice == 1000, fmt.Sprintf("forester bronzeaxe price 1000 (got %d)", bronzePrice))

	// --- Gather the demo oak t-test-1 (98,98) for logs. The gather path is
	// lenient (Target Object from anywhere counts as one swing), matching the
	// M4 slice. Swing until exhaust (difficulty 10 -> 1/9 per swing at tool 1
	// + skill 1, so cap at 200 swings).	fmt.Println("chopping oak t-test-1...")
	lastFrames = nil
	logsBefore, logsIdx := 0, -1
	for i := 0; i < 200; i++ {
		send(conn, `[14,[3,"t-test-1"]]`)
		drain(120 * time.Millisecond)
		for _, f := range lastFrames {
			var id int
			_ = json.Unmarshal(f[0], &id)
			if id != 21 {
				continue
			}
			if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 1 && m.Slot != nil && m.Slot.Key == "logs" {
				logsBefore += m.Slot.Count
				logsIdx = m.Slot.Index
			}
		}
		lastFrames = nil
		if logsBefore > 0 {
			break
		}
	}
	check(logsBefore > 0, fmt.Sprintf("oak exhausted -> +logs (got %d)", logsBefore))

	// --- Sell the logs to the forester (allowedItems includes logs). ---
	fmt.Println("selling logs to forester...")
	walkTo(conn, fx, fy) // back to the forester (move closes the store)
	lastFrames = nil
	send(conn, `[14,[0,"n-show-18"]]`)
	drain(900 * time.Millisecond)
	lastFrames = nil
	// Sell the logs at their actual slot index (seeded gold occupies 0; the
	// server rightly refuses selling the store currency itself).
	send(conn, fmt.Sprintf(`[40,{"opcode":3,"key":"forester","index":%d,"count":1}]`, logsIdx))
	drain(900 * time.Millisecond)
	sawGold := false
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 1 && m.Slot != nil && m.Slot.Key == "gold" {
			sawGold = true
		}
	}
	check(sawGold, "sell logs -> Container Add gold (65 each at max stock)")
	lastFrames = nil

	// --- Miner2 (n-show-32): BUY check with the seeded wallet. ---
	fmt.Println("walking to miner2 (n-show-32) for the buy check...")
	mx, my := npcPos(31)
	walkTo(conn, mx, my)
	lastFrames = nil
	send(conn, `[14,[0,"n-show-32"]]`)
	counts = drain(1200 * time.Millisecond)
	check(counts[40] == 1, "miner2 talk -> Store Open")
	lastFrames = nil
	send(conn, `[40,{"opcode":2,"key":"miner","index":0,"count":1}]`) // buy 1 coal
	drain(900 * time.Millisecond)
	bought, coalIdx := false, -1
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 1 && m.Slot != nil && m.Slot.Key == "coal" {
			bought = true
			coalIdx = m.Slot.Index
		}
	}
	check(bought, "buy 1 coal -> Container Add coal (50 gold)")

	// --- Store Select quote (sell-side preview on the logs slot). ---
	lastFrames = nil
	send(conn, fmt.Sprintf(`[40,{"opcode":5,"key":"miner","index":%d,"count":1}]`, logsIdx))
	drain(700 * time.Millisecond)
	var sel storeFrame
	haveSel := false
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		// Store Select is [40, 5, {key, item:{...}}] — opcode 5, data at f[2].
		if id == 40 && frameOpcode(f) == 5 && json.Unmarshal(f[2], &sel) == nil && sel.Item != nil {
			haveSel = true
		}
	}
	// The logs slot was reused by the purchased coal, so the quote must be
	// for coal: Node getTotalCost for a stocked item = 20% of its price
	// (coal 50 -> 10).
	check(haveSel && sel.Item != nil && sel.Item.Key == "coal" && sel.Item.Price == 10,
		fmt.Sprintf("select slot -> Store Select quote %v (20%% curve)", sel.Item))

	// --- Movement closes the store + revokes bank access. ---
	fmt.Println("stepping away from miner2...")
	walkTo(conn, mx+2, my) // one legal step: server clears storeOpen
	lastFrames = nil
	send(conn, `[40,{"opcode":3,"key":"miner","index":0,"count":1}]`)
	counts = drain(600 * time.Millisecond)
	check(counts[40] == 0, "sell after movement ignored (storeOpen cleared)")

	// --- Banker (n-show-48 redstoremannpc): NPC Bank batch + deposit/withdraw. ---
	fmt.Println("walking to banker (n-show-48)...")
	bx, by := npcPos(47)
	walkTo(conn, bx, by)
	lastFrames = nil
	send(conn, `[14,[0,"n-show-48"]]`)
	counts = drain(1200 * time.Millisecond)
	check(counts[21] == 1, fmt.Sprintf("banker talk -> Container Batch bank (got %d container frames)", counts[21]))
	var bankBatch struct {
		Type int `json:"type"`
		Data struct {
			Slots []invSlot `json:"slots"`
		} `json:"data"`
	}
	_ = json.Unmarshal(lastOf(lastFrames, 21), &bankBatch)
	check(bankBatch.Type == 0, fmt.Sprintf("bank batch type 0 (got %d, %d slots)", bankBatch.Type, len(bankBatch.Data.Slots)))

	// --- Deposit the coal (tracked slot) for the relogin leg: proves bank ->
	// SQLite -> login Container Batch round-trip. Done FIRST so the round-trip
	// below cannot shift dense indices underneath the stash.
	lastFrames = nil
	send(conn, fmt.Sprintf(`[21,{"opcode":3,"type":0,"fromContainer":1,"fromIndex":%d,"toContainer":0}]`, coalIdx))
	drain(900 * time.Millisecond)
	stashedOK := false
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 0 && m.Slot != nil && m.Slot.Key == "coal" {
			stashedOK = true
		}
	}
	check(stashedOK, "coal stashed in bank for relogin")

	// --- Round-trip: deposit the gold stack (slot 0 — coal is already in the
	// bank so a removal here cannot reshuffle it) and withdraw it back.
	// Bank selects carry type:0 (ContainerType.Bank) like the real client
	// (menu.ts handleBankSelect) — type:1 is the EQUIP trigger on the server.
	lastFrames = nil
	send(conn, `[21,{"opcode":3,"type":0,"fromContainer":1,"fromIndex":0,"toContainer":0}]`)
	drain(900 * time.Millisecond)
	depOK := false
	var depSlot invSlot
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 0 && m.Slot != nil {
			depOK, depSlot = true, *m.Slot
		}
	}
	check(depOK, fmt.Sprintf("deposit inv[0] -> bank add %+v", depSlot))

	// Withdraw it back (bank slot index = depSlot.Index).
	lastFrames = nil
	send(conn, fmt.Sprintf(`[21,{"opcode":3,"type":0,"fromContainer":0,"fromIndex":%d,"toContainer":1}]`, depSlot.Index))
	drain(900 * time.Millisecond)
	wdOK := false
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 1 && m.Slot != nil && m.Slot.Key == depSlot.Key {
			wdOK = true
		}
	}
	check(wdOK, "withdraw bank slot -> inventory add")

	// --- Inventory swap: reorder two slots in place (no server echo). ---
	lastFrames = nil
	send(conn, `[21,{"opcode":4,"type":1,"fromIndex":0,"value":1}]`)
	counts = drain(600 * time.Millisecond)
	check(counts[21] == 0, "inventory swap sends no packets (client-side)")
	if counts[21] != 0 {
		for _, f := range lastFrames {
			fmt.Printf("DEBUG swap-window frame: id=%s data=%.200s\n", string(f[0]), string(f[1]))
		}
		fmt.Printf("DEBUG swap counts: %v\n", counts)
	}

	// --- Equipment: bluestoremannpc (n-show-8) sells arrows @5g infinite —
	// buy a second stack so the equip-swap has a real echo, then walk the
	// equip -> swap -> unequip -> re-equip flow.
	fmt.Println("walking to clerk (n-show-8, startshop)...")
	cx, cy := npcPos(7)
	walkTo(conn, cx, cy)
	lastFrames = nil
	send(conn, `[14,[0,"n-show-8"]]`)
	counts = drain(1200 * time.Millisecond)
	check(counts[40] == 1, "clerk talk -> Store Open (startshop)")
	lastFrames = nil
	send(conn, `[40,{"opcode":2,"key":"startshop","index":0,"count":10}]`) // buy 10 arrows
	drain(900 * time.Millisecond)
	arrows2, arrows2Idx := 0, -1
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 1 && m.Slot != nil && m.Slot.Key == "arrow" {
			arrows2 += m.Slot.Count
			arrows2Idx = m.Slot.Index
		}
	}
	check(arrows2 == 10, fmt.Sprintf("buy 10 arrows -> stack of %d at slot %d", arrows2, arrows2Idx))

	// Equip the fresh stack (Container Select type Inventory = equip trigger).
	lastFrames = nil
	send(conn, fmt.Sprintf(`[21,{"opcode":3,"type":1,"fromIndex":%d}]`, arrows2Idx))
	drain(900 * time.Millisecond)
	equipped := false
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id == 8 && frameOpcode(f) == 1 {
			equipped = true
		}
	}
	check(equipped, "Container Select on arrows -> Equipment Equip (Arrows slot)")

	// Selecting an empty/invalid slot must be a silent no-op (Node:
	// handleContainerSelect Inventory -> getItem(get(index)) nil -> return).
	// (A true two-stack equip-swap is untestable with arrows: they merge —
	// max stack 9999 — so the seeded and bought stacks are one.)
	lastFrames = nil
	send(conn, `[21,{"opcode":3,"type":1,"fromIndex":20}]`)
	counts = drain(700 * time.Millisecond)
	check(counts[8] == 0 && counts[21] == 0, "equip select on empty slot is silent (Node !item return)")

	// Movement revokes bank/store access but NOT equipment.
	walkTo(conn, cx+2, cy)
	lastFrames = nil
	send(conn, `[8,{"opcode":2,"type":2}]`) // Unequip the Arrows slot
	drain(900 * time.Millisecond)
	unequipped := false
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id == 8 && frameOpcode(f) == 2 {
			unequipped = true
		}
	}
	check(unequipped, "Equipment Unequip works after movement (equipment unaffected by move-clear)")
	// The unequip echo Add carries the slot the stack returned to (dense
	// indices shifted through the bank round-trip, so the echo is the
	// reliable index source).
	var arrows3Idx int = -1
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if m, ok := parseContainer(f); ok && m.Opcode == 1 && m.Type == 1 && m.Slot != nil && m.Slot.Key == "arrow" {
			arrows3Idx = m.Slot.Index
		}
	}
	// Unequip on the now-empty slot must be a silent no-op.
	lastFrames = nil
	send(conn, `[8,{"opcode":2,"type":2}]`)
	counts = drain(600 * time.Millisecond)
	check(counts[8] == 0, "unequip on empty slot is silent (Node stops at !equipment.key)")

	// Re-equip the stack for the persistence leg.
	lastFrames = nil
	if arrows3Idx >= 0 {
		send(conn, fmt.Sprintf(`[21,{"opcode":3,"type":1,"fromIndex":%d}]`, arrows3Idx))
		drain(900 * time.Millisecond)
		reeq := false
		for _, f := range lastFrames {
			var id int
			_ = json.Unmarshal(f[0], &id)
			if id == 8 && frameOpcode(f) == 1 {
				reeq = true
			}
		}
		check(reeq, "arrows re-equipped for the persistence leg")
	} else {
		check(false, "unequip echo returned the arrow stack to a tracked slot")
	}

	// --- Logout (server persists synchronously) and relogin. ---
	lastFrames = nil
	conn.Close()
	time.Sleep(300 * time.Millisecond)

	conn2, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+dialPort()+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL (relogin):", err)
		os.Exit(1)
	}
	defer conn2.Close()
	go reader(conn2)

	drain(1500 * time.Millisecond)
	lastFrames = nil
	send(conn2, `[1,{"gVer":"0.5.5-beta"}]`)
	drain(1500 * time.Millisecond)
	lastFrames = nil
	send(conn2, `[2,{"opcode":0,"username":"m6tester","password":"x","seedRank":2}]`)
	drain(2500 * time.Millisecond)
	// Persistence proof: a bank Container Batch (type 0) arrives on login
	// whenever the bank was non-empty at disconnect — it isn't (we withdrew
	// everything), so instead prove the INVENTORY batch persisted.
	var invBatch struct {
		Type int `json:"type"`
		Data struct {
			Slots []invSlot `json:"slots"`
		} `json:"data"`
	}
	invOK, invKeys := false, map[string]bool{}
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		// Login batches are [21, 0, {type, data:{slots}}] — data at f[2].
		if len(f) >= 3 && json.Unmarshal(f[2], &invBatch) == nil && invBatch.Type == 1 {
			invOK = true
			for _, s := range invBatch.Data.Slots {
				invKeys[s.Key] = true
			}
		}
	}
	check(invOK, fmt.Sprintf("relogin -> inventory batch (%v)", keys(invKeys)))
	check(invKeys["gold"], "gold persisted across relogin")

	// Bank batch must restore the stashed coal (bank -> SQLite -> login).
	bankOK, bankCoal := false, false
	var bankRestored struct {
		Type int `json:"type"`
		Data struct {
			Slots []invSlot `json:"slots"`
		} `json:"data"`
	}
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id != 21 {
			continue
		}
		if len(f) >= 3 && json.Unmarshal(f[2], &bankRestored) == nil && bankRestored.Type == 0 {
			bankOK = true
			for _, s := range bankRestored.Data.Slots {
				if s.Key == "coal" {
					bankCoal = true
				}
			}
		}
	}
	check(bankOK && bankCoal, fmt.Sprintf("relogin -> bank batch with coal (type 0 batch=%v)", bankOK))

	// Equipment persistence: the login sequence must carry an Equipment
	// Batch frame re-equipping the arrows (equipment -> SQLite -> login).
	eqBatch, eqArrow := false, false
	var eqData struct {
		Equipments []struct {
			Type  int    `json:"type"`
			Key   string `json:"key"`
			Count int    `json:"count"`
		} `json:"equipments"`
	}
	for _, f := range lastFrames {
		var id int
		_ = json.Unmarshal(f[0], &id)
		if id == 8 && frameOpcode(f) == 0 {
			if json.Unmarshal(frameData(f), &eqData) == nil {
				eqBatch = true
				for _, e := range eqData.Equipments {
					if e.Key == "arrow" && e.Type == 2 {
						eqArrow = true
					}
				}
			}
		}
	}
	check(eqBatch && eqArrow, fmt.Sprintf("relogin -> Equipment Batch with arrows (batch=%v)", eqBatch))

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("FAILED: %d checks\n", len(failures))
		for _, f := range failures {
			fmt.Println(" -", f)
		}
		os.Exit(1)
	}
	fmt.Println("M6 E2E PASSED (stores + bank + NPC talk + persistence)")
}

func keys(m map[string]bool) string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	if len(out) == 0 {
		return "(empty)"
	}
	return strings.Join(out, ",")
}
