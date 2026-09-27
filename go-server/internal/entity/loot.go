// Package entity (loot) holds the M5 drops/loot slice, extracted
// behavior-frozen from the root m5.go adapter (task D2b item 1).
//
// TS sources (see the root adapter for the full tour): mob.getDrops /
// getRandomItem (personal roll + one roll per dropTables entry, chance vs
// DROP_PROBABILITY 100000), Item (type 2) / LootBag (type 8, take-all)
// entities, ItemDefaults blink/despawn timers (shortened to 20s/30s so loot
// never outlives the e2e windows), the coins+log fallback for empty rolls,
// and quest/achievement-gated entries (Node filters them from the pool first
// via fullfillsQuest — the gate check happens before the uniform pick).
//
// Everything transport/world related stays with the root adapter and is
// reached only through the LootWorld seam below (root helpers in
// parentheses):
//
//	SetEntityPos -> worldcore.SetEntityPos (loot position registry)
//	EntityPos    -> worldcore.EntityPos (Who-path payload rebuild)
//	RemoveEntity -> worldcore.RemoveEntity (expiry/pickup teardown)
//	Broadcast    -> worldcore.Broadcast (Spawn/Blink/Despawn frames)
//
// Everything data/gating related is injected through LootDeps (root helpers
// in parentheses):
//
//	Walkable    -> !blocked(x, y) (near-walkable spiral on spawn)
//	DoubleDrops -> worldDoubleDrops (double-drops event duplicates the roll)
//	Gate        -> m11DropGated (quest/achievement gate per entry)
//	DataPath    -> resourceDataPath (tables/mobs/items JSON lookup)
//
// Packet shapes, probabilities, timers, curves and log strings are frozen:
// frames are built with internal/protocol (the same constructors the root
// pkt/pktOp shims wrap), so wire bytes are identical. The "m5:" log prefix
// is kept verbatim so the e2e server-log greps keep matching.
package entity

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"rpg-world-server/internal/protocol"
)

// DropProbability is Modules.Constants.DROP_PROBABILITY (modules.ts:625).
const DropProbability = 100000

// DropJSON is one drop-table entry (tables.json / mobs.json drops lists).
type DropJSON struct {
	Key         string `json:"key"`
	Chance      int    `json:"chance"`
	Count       int    `json:"count"`
	Quest       string `json:"quest"`
	Status      string `json:"status"`
	Achievement string `json:"achievement"`
}

// Loot reads the Drops + DropTables subset of the canonical entity
// MobProfile (mob.go, mirroring the Node Mob.loadData entry), loaded here
// from the same mobs.json file.

// Drop is one rolled loot item.
type Drop struct {
	Key   string
	Count int
}

// Loot is one live loot entity (Item type 2 / LootBag type 8).
type Loot struct {
	Instance string
	Bag      bool
	Items    []Drop
	X, Y     int
	Owner    string
	blinkT   *time.Timer
	destroyT *time.Timer
	seq      int // spawn order (FindLootAt oldest-pick parity)
}

// LootWorld is the transport/world seam implemented by the root adapter.
type LootWorld interface {
	SetEntityPos(inst string, x, y int)
	EntityPos(inst string) (x, y int, ok bool)
	RemoveEntity(inst string)
	Broadcast(frames ...[]any)
}

// LootDeps bundles the loot seams for one call. Configure once at boot
// (before the first spawn); zero value keeps every behavior dormant except
// the pure roll math.
type LootDeps struct {
	World LootWorld
	// Walkable reports whether a tile can hold loot (!blocked parity).
	Walkable func(x, y int) bool
	// DoubleDrops duplicates a mob roll while the double-drops event is
	// active (worldDoubleDrops parity).
	DoubleDrops func([]Drop) []Drop
	// Gate reports whether a gated drop entry passes for the killer
	// (m11DropGated parity). Nil = gates closed for non-empty usernames,
	// matching the pre-split behavior of filtering with a deny-all gate.
	Gate func(username, quest, achievement, status string) bool
	// DataPath resolves a data-table path (resourceDataPath parity:
	// RES_<name> override, then ../packages/server/data/<name>.json).
	// Nil selects the default resolution below.
	DataPath func(name string) string
}

var lootDeps LootDeps

// ConfigureLoot installs the loot seams (called once from the root boot,
// before the first spawn; timers capture this config).
func ConfigureLoot(d LootDeps) { lootDeps = d }

func lootDataPath(name string) string {
	if lootDeps.DataPath != nil {
		return lootDeps.DataPath(name)
	}
	if p := os.Getenv("RES_" + name); p != "" {
		return p
	}
	rel := filepath.Join("..", "packages", "server", "data", name+".json")
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	alt := filepath.Join("..", "..", "packages", "server", "data", name+".json")
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	return "/Users/appfuxion/repo/rpg-world-sim/packages/server/data/" + name + ".json"
}

// ---------------------------------------------------------------------------
// Drop tables (packages/server/data/tables.json + mobs.json + items.json
// stackable flags).
// ---------------------------------------------------------------------------

var (
	lootTablesOnce sync.Once
	lootTables     = map[string][]DropJSON{}
	lootMobs       = map[string]*MobProfile{}
	lootStackable  = map[string]bool{}
)

// LoadLootTables loads tables/mobs/items once (m5 loadM5Tables verbatim,
// identical log line).
func LoadLootTables() {
	lootTablesOnce.Do(func() {
		raw, err := os.ReadFile(lootDataPath("tables"))
		if err != nil {
			log.Fatalf("read tables.json: %v", err)
		}
		var tbl map[string]struct {
			Drops []DropJSON `json:"drops"`
		}
		if err := json.Unmarshal(raw, &tbl); err != nil {
			log.Fatalf("parse tables.json: %v", err)
		}
		for k, v := range tbl {
			lootTables[k] = v.Drops
		}
		raw, err = os.ReadFile(lootDataPath("mobs"))
		if err != nil {
			log.Fatalf("read mobs.json: %v", err)
		}
		if err := json.Unmarshal(raw, &lootMobs); err != nil {
			log.Fatalf("parse mobs.json: %v", err)
		}
		raw, err = os.ReadFile(lootDataPath("items"))
		if err != nil {
			log.Fatalf("read items.json: %v", err)
		}
		var items map[string]struct {
			Stackable *bool `json:"stackable"`
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			log.Fatalf("parse items.json: %v", err)
		}
		for k, v := range items {
			if v.Stackable != nil && *v.Stackable {
				lootStackable[k] = true
			}
		}
		log.Printf("m5: tables=%d mobs=%d stackable=%d", len(lootTables), len(lootMobs), len(lootStackable))
	})
}

// Stackable reports the items.json stackable flag (m5AddItem parity).
func Stackable(key string) bool {
	LoadLootTables()
	return lootStackable[key]
}

// Profile returns the mob profile for a mob key (m5MobMaxHP parity).
func Profile(mobKey string) (*MobProfile, bool) {
	LoadLootTables()
	p, ok := lootMobs[mobKey]
	return p, ok
}

// RollEntry ports mob.getRandomItem: pick one entry uniformly, fix counts
// for gold/flask/arrow/feather, then roll chance vs DROP_PROBABILITY.
// Quest/achievement-gated entries roll only when the gate passes — the gate
// check happens before the uniform pick (see RollEntryGated).
func RollEntry(entries []DropJSON, level int) (key string, count int, ok bool) {
	avail := entries
	if len(avail) == 0 {
		return "", 0, false
	}
	if level < 1 {
		level = 1
	}
	drop := avail[rand.Intn(len(avail))]
	count = drop.Count
	if count <= 0 {
		count = 1
	}
	switch drop.Key {
	case "gold":
		count = rand.Intn(level*10-level+1) + level
	case "flask":
		count = rand.Intn(3) + 1
	case "arrow":
		count = rand.Intn(level) + 1
	case "firearrow":
		count = rand.Intn((level+1)/2) + 1
	case "feather":
		count = rand.Intn(level) + 1
	}
	chance := drop.Chance
	if chance > DropProbability {
		chance = DropProbability
	}
	if rand.Intn(DropProbability+1) < chance {
		return drop.Key, count, true
	}
	return "", 0, false
}

// RollEntryGated filters quest/achievement-gated entries by the killer's
// progression (mob.getRandomItem receives the player), then rolls uniformly
// over the survivors (RollEntry body). Empty username = gates closed (gated
// entries never pass without a killer context).
func RollEntryGated(username string, entries []DropJSON, level int) (string, int, bool) {
	gate := lootDeps.Gate
	filtered := make([]DropJSON, 0, len(entries))
	for _, e := range entries {
		pass := e.Quest == "" && e.Achievement == ""
		if !pass && username != "" && gate != nil {
			pass = gate(username, e.Quest, e.Achievement, e.Status)
		}
		if pass {
			filtered = append(filtered, e)
		}
	}
	entries = filtered
	return RollEntry(entries, level)
}

// GetDrops ports mob.getDrops for a mob key: personal roll + one roll per
// drop table. Empty result falls back to coins + log so every kill in the
// slice is observable (spec-authorized fallback).
func GetDrops(mobKey string) []Drop {
	return GetDropsFor(mobKey, "")
}

// GetDropsFor ports mob.getDrops with a killer context: M11 quest gates
// evaluate against the killer's progression (mob.getDrops takes the player).
// Empty result falls back to coins + log so every kill in the slice is
// observable (spec-authorized fallback).
func GetDropsFor(mobKey, username string) []Drop {
	LoadLootTables()
	prof := lootMobs[mobKey]
	level := 1
	var out []Drop
	if prof != nil {
		level = prof.Level
		if level < 1 {
			level = 1
		}
		if k, c, ok := RollEntryGated(username, prof.Drops, level); ok {
			out = append(out, Drop{Key: k, Count: c})
		}
		for _, t := range prof.DropTables {
			entries, ok := lootTables[t]
			if !ok {
				log.Printf("m5: mob %s has invalid drop table %s", mobKey, t)
				continue
			}
			if k, c, ok := RollEntryGated(username, entries, level); ok {
				out = append(out, Drop{Key: k, Count: c})
			}
		}
	}
	if len(out) == 0 {
		out = []Drop{
			{Key: "gold", Count: rand.Intn(level) + 1},
			{Key: "logs", Count: 1},
		}
		log.Printf("m5: %s rolls empty -> fallback coins+log", mobKey)
	}
	return out
}

// ---------------------------------------------------------------------------
// Loot entities (Item type 2 / LootBag type 8) + blink/destroy timers.
// ---------------------------------------------------------------------------

var (
	lootDespawnDelay = func() time.Duration {
		if v := os.Getenv("LOOT_DESPAWN_MS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return time.Duration(n) * time.Millisecond
			}
		}
		return 30 * time.Second
	}()
	lootBlinkDelay = func() time.Duration {
		if v := os.Getenv("LOOT_BLINK_MS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return time.Duration(n) * time.Millisecond
			}
		}
		if d := lootDespawnDelay - 10*time.Second; d > 0 {
			return d
		}
		return lootDespawnDelay / 2
	}()
)

var (
	lootMu  sync.Mutex
	loots   = map[string]*Loot{}
	lootSeq int
)

// NearWalkable finds the nearest walkable tile to (x, y) (m5NearWalkable
// verbatim: self, then the r=1..3 spiral perimeter).
func NearWalkable(x, y int) (int, int) {
	walkable := lootDeps.Walkable
	if walkable == nil {
		return x, y
	}
	if walkable(x, y) {
		return x, y
	}
	for r := 1; r <= 3; r++ {
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				if dx != r && dx != -r && dy != r && dy != -r {
					continue // perimeter only (interior already probed)
				}
				if nx, ny := x+dx, y+dy; walkable(nx, ny) {
					return nx, ny
				}
			}
		}
	}
	return x, y
}

func lootIntp(v int) *int { return &v }

// spawnLocked registers one loot entity and emits its Spawn frame. Caller
// holds no locks; timers are armed under the registry lock before return
// so a concurrent Destroy cannot slip between registration and arming.
func spawnLocked(drops []Drop, cx, cy int, owner, logPrefix string) string {
	lx, ly := NearWalkable(cx, cy)
	lootMu.Lock()
	lootSeq++
	inst := fmt.Sprintf("loot-%d", lootSeq)
	bag := len(drops) > 1
	l := &Loot{Instance: inst, Bag: bag, Items: drops, X: lx, Y: ly, Owner: owner, seq: lootSeq}
	loots[inst] = l
	l.blinkT = time.AfterFunc(lootBlinkDelay, func() { BlinkLoot(inst) })
	l.destroyT = time.AfterFunc(lootDespawnDelay, func() { DestroyLoot(inst, "expired") })
	lootMu.Unlock()
	if lootDeps.World != nil {
		lootDeps.World.SetEntityPos(inst, lx, ly)
		var payload protocol.EntityData
		if bag {
			payload = protocol.EntityData{Instance: inst, Type: protocol.EntityLootBag, Key: "lootbag", Name: "Loot Bag", X: lx, Y: ly}
		} else {
			payload = protocol.EntityData{Instance: inst, Type: protocol.EntityItem, Key: drops[0].Key, Name: drops[0].Key, X: lx, Y: ly, Count: lootIntp(drops[0].Count)}
		}
		lootDeps.World.Broadcast(protocol.Pkt(protocol.PacketSpawn, payload))
	}
	// M11: multi-drop bags log their contents (Node logs the roll set; the
	// bag itself only carries the keys server-side, so the log is the only
	// observable record — e2e asserts on this line).
	keys := make([]string, 0, len(drops))
	for _, d := range drops {
		keys = append(keys, d.Key)
	}
	log.Printf("%s loot %s spawned (%s x%d) at %d,%d owner=%s bag=%v items=%v",
		logPrefix, inst, drops[0].Key, drops[0].Count, lx, ly, owner, bag, keys)
	return inst
}

// SpawnLoot drops the roll at the corpse: single -> Item, multi -> LootBag
// (take-all on Target/Step; lootbag menu Open flow deferred, logged).
func SpawnLoot(mobKey string, cx, cy int, owner string) string {
	drops := GetDropsFor(mobKey, owner)
	if lootDeps.DoubleDrops != nil {
		drops = lootDeps.DoubleDrops(drops) // world: double-drops event duplicates the roll
	}
	if len(drops) == 0 {
		return ""
	}
	return spawnLocked(drops, cx, cy, owner, "m5:")
}

// SpawnLootBag creates a loot entity with the given exact items (used by
// /drop and /lootbag; TS spawns Item/LootBag entities directly). The m13
// log prefix is kept verbatim.
func SpawnLootBag(owner string, cx, cy int, items []Drop) string {
	if len(items) == 0 {
		return ""
	}
	return spawnLocked(items, cx, cy, owner, "m13:")
}

// SpawnLootAt wraps SpawnLootBag with an explicit tile (the /drop path
// spawns without a killer gate — no drop-table roll, the exact key).
func SpawnLootAt(owner, key string, count, x, y int) string {
	return SpawnLootBag(owner, x, y, []Drop{{Key: key, Count: count}})
}

// BlinkLoot frees the loot for all (owner cleared + Blink frame).
func BlinkLoot(inst string) {
	lootMu.Lock()
	l, ok := loots[inst]
	if ok {
		l.Owner = ""
	}
	lootMu.Unlock()
	if !ok {
		return
	}
	if lootDeps.World != nil {
		lootDeps.World.Broadcast(protocol.Pkt(protocol.PacketBlink, inst))
	}
	log.Printf("m5: loot %s blinking (free for all, destroy in %v)", inst, lootDespawnDelay-lootBlinkDelay)
}

// DestroyLoot tears a loot entity down (expiry or pickup).
func DestroyLoot(inst, why string) {
	lootMu.Lock()
	l, ok := loots[inst]
	if ok {
		delete(loots, inst)
		if l.blinkT != nil {
			l.blinkT.Stop()
		}
		if l.destroyT != nil {
			l.destroyT.Stop()
		}
	}
	lootMu.Unlock()
	if !ok {
		return
	}
	clearBagOpeners(inst)
	if lootDeps.World != nil {
		lootDeps.World.RemoveEntity(inst)
		if l.Bag {
			// LootBag.destroy() parity: close() broadcasts Close so every
			// client with the menu open hides it (then the entity despawns).
			lootDeps.World.Broadcast(protocol.PktOp(protocol.PacketLootBag, protocol.LootBagClose, map[string]any{}))
		}
		lootDeps.World.Broadcast(protocol.Pkt(protocol.PacketDespawn, protocol.DespawnData{Instance: inst}))
	}
	log.Printf("m5: loot %s destroyed (%s)", inst, why)
}

// RegisterLoot adds a pre-built loot entry to the registry without any
// timers (M10 chest drops: persistent items with no blink/expiry — Node
// chest items never expire on their own). Pickup routing is shared.
func RegisterLoot(inst, key string, count, x, y int, owner string) {
	lootMu.Lock()
	lootSeq++
	loots[inst] = &Loot{Instance: inst, Bag: false, Items: []Drop{{Key: key, Count: count}}, X: x, Y: y, Owner: owner, seq: lootSeq}
	lootMu.Unlock()
	if lootDeps.World != nil {
		lootDeps.World.SetEntityPos(inst, x, y)
	}
}

// TakeLoot atomically claims + removes one loot entity (single-Item
// Find+Destroy duplication gap: Find then Destroy lets two pickups both
// succeed). Reports false when the loot is already gone. Teardown mirrors
// DestroyLoot (timers stopped, openers cleared, Remove + Despawn frames).
func TakeLoot(inst string) (Loot, bool) {
	lootMu.Lock()
	l, ok := loots[inst]
	if !ok {
		lootMu.Unlock()
		return Loot{}, false
	}
	delete(loots, inst)
	if l.blinkT != nil {
		l.blinkT.Stop()
	}
	if l.destroyT != nil {
		l.destroyT.Stop()
	}
	cp := *l
	cp.Items = append([]Drop(nil), l.Items...)
	lootMu.Unlock()
	clearBagOpeners(inst)
	if lootDeps.World != nil {
		lootDeps.World.RemoveEntity(inst)
		if l.Bag {
			lootDeps.World.Broadcast(protocol.PktOp(protocol.PacketLootBag, protocol.LootBagClose, map[string]any{}))
		}
		lootDeps.World.Broadcast(protocol.Pkt(protocol.PacketDespawn, protocol.DespawnData{Instance: inst}))
	}
	log.Printf("m5: loot %s taken", inst)
	return cp, true
}

// ---------------------------------------------------------------------------
// LootBag Open/Take lifecycle (objects/lootbag.ts parity).
//
// TS flow: stopping on a bag opens it (activeLootBag + LootBag Open
// {items}); the client menu sends LootBag {Take, index} per item; take()
// validates opener + ownership + distance, transfers one stack, and either
// destroys the emptied bag (close + despawn) or broadcasts Take {index} so
// open menus drop the row. Slots are stable: take() deletes the container
// entry (a hole), surviving slots keep their indices — mirrored here with
// Drop{Key:""} tombstones, which the take-all path skips.
// ---------------------------------------------------------------------------

// BagSlot is one live lootbag slot for the Open payload (SlotData parity).
type BagSlot struct {
	Index int
	Key   string
	Count int
}

var (
	bagMu      sync.Mutex
	bagOpeners = map[string]string{} // player instance -> open bag instance
)

// OpenBag records player as the opener of a live bag
// (player.activeLootBag parity). Reports false when inst is not a bag.
// The live-bag check and the opener record happen under the registry lock
// (lootMu outer, bagMu inner) so a concurrent Destroy cannot slip between
// validation and recording.
func OpenBag(player, inst string) bool {
	lootMu.Lock()
	defer lootMu.Unlock()
	l, ok := loots[inst]
	if !ok || !l.Bag {
		return false
	}
	bagMu.Lock()
	bagOpeners[player] = inst
	bagMu.Unlock()
	return true
}

// ActiveBag reports the bag a player currently has open.
func ActiveBag(player string) (string, bool) {
	bagMu.Lock()
	defer bagMu.Unlock()
	inst, ok := bagOpeners[player]
	return inst, ok
}

// ClearBagOpener forgets a player's open bag (open another / disconnect).
func ClearBagOpener(player string) {
	bagMu.Lock()
	delete(bagOpeners, player)
	bagMu.Unlock()
}

// clearBagOpeners forgets every opener of a destroyed bag.
func clearBagOpeners(inst string) {
	bagMu.Lock()
	for player, open := range bagOpeners {
		if open == inst {
			delete(bagOpeners, player)
		}
	}
	bagMu.Unlock()
}

// BagSlots lists the live slots of a bag with stable indices (getItems
// parity: taken slots are holes, survivors keep their indices).
func BagSlots(inst string) ([]BagSlot, bool) {
	lootMu.Lock()
	defer lootMu.Unlock()
	l, ok := loots[inst]
	if !ok || !l.Bag {
		return nil, false
	}
	var out []BagSlot
	for i, d := range l.Items {
		if d.Key == "" {
			continue // taken slot (hole)
		}
		out = append(out, BagSlot{Index: i, Key: d.Key, Count: d.Count})
	}
	return out, true
}

// TakeBagItem removes slot index from a bag (lootbag.take parity): the
// slot becomes a hole (tombstone, stable surviving indices). Returns the
// taken drop and the remaining live count. ok=false when the bag or slot
// is gone (already-taken slots included).
func TakeBagItem(inst string, index int) (taken Drop, remaining int, ok bool) {
	lootMu.Lock()
	defer lootMu.Unlock()
	l, found := loots[inst]
	if !found || !l.Bag || index < 0 || index >= len(l.Items) {
		return Drop{}, 0, false
	}
	d := l.Items[index]
	if d.Key == "" {
		return Drop{}, 0, false
	}
	l.Items[index] = Drop{}
	for _, r := range l.Items {
		if r.Key != "" {
			remaining++
		}
	}
	return d, remaining, true
}

// IsLoot reports whether id is a live loot entity.
func IsLoot(id string) bool {
	lootMu.Lock()
	defer lootMu.Unlock()
	_, ok := loots[id]
	return ok
}

// IsBag reports whether id is a live lootbag (vs a single Item).
func IsBag(id string) bool {
	lootMu.Lock()
	defer lootMu.Unlock()
	l, ok := loots[id]
	return ok && l.Bag
}

// FindLoot returns a detached copy of the loot record (pickup path).
func FindLoot(inst string) (Loot, bool) {
	lootMu.Lock()
	defer lootMu.Unlock()
	l, ok := loots[inst]
	if !ok {
		return Loot{}, false
	}
	cp := *l
	cp.Items = append([]Drop(nil), l.Items...)
	return cp, true
}

// FindLootAt returns the id of the loot lying on (x, y) (Step path).
// When several share a tile the oldest spawn (lowest seq) wins so the
// result is deterministic across map iteration order.
func FindLootAt(x, y int) (string, bool) {
	lootMu.Lock()
	defer lootMu.Unlock()
	best := ""
	bestSeq := 0
	found := false
	for id, l := range loots {
		if l.X == x && l.Y == y {
			if !found || l.seq < bestSeq {
				best, bestSeq, found = id, l.seq, true
			}
		}
	}
	if !found {
		return "", false
	}
	return best, true
}

// LootPayload rebuilds the Spawn payload for a loot instance (Who path).
func LootPayload(inst string) (any, bool) {
	lootMu.Lock()
	l, ok := loots[inst]
	lootMu.Unlock()
	if !ok {
		return nil, false
	}
	x, y := l.X, l.Y
	if lootDeps.World != nil {
		if px, py, found := lootDeps.World.EntityPos(inst); found {
			x, y = px, py
		}
	}
	if l.Bag {
		return protocol.EntityData{Instance: inst, Type: protocol.EntityLootBag, Key: "lootbag", Name: "Loot Bag", X: x, Y: y}, true
	}
	if len(l.Items) == 0 {
		return nil, false
	}
	return protocol.EntityData{Instance: inst, Type: protocol.EntityItem, Key: l.Items[0].Key, Name: l.Items[0].Key, X: x, Y: y, Count: lootIntp(l.Items[0].Count)}, true
}
