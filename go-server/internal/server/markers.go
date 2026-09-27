// Marker population: boot spawns for the world.json `entities` markers.
//
// TS reference: packages/server/src/controllers/entities.ts load (71-142):
// map.forEachEntity dispatches each tileIndex->key marker through
// getEntityType (items.json -> npcs.json -> mobs.json -> trees.json ->
// rocks.json -> fishing.json -> foraging.json, first match wins; unknown
// keys match nothing and are silently ignored) into spawnItem / spawnNPC /
// spawnMob / spawnTree / spawnRock / spawnFishSpots / spawnForaging, then
// the static map.chest loop spawns area chests. The static-chest half is
// NOT duplicated here: world.json areas.chest already spawns through the
// mimic batch (entity.SpawnStaticChests, called just above this hook).
//
// Mode convention (shared with resourceSpawns + adoptExistingMobs in
// boot.go/m9.go): TESTMAP keeps its demo line + showcase grid, CLEAN and
// COMBAT keep their scenes — marker population runs on the REAL path
// only, i.e. !testMode && !cleanMode && !combatMode. Entities are separate
// from tiles, so the testmap tile clone (getTestRegionData) is untouched;
// this gate only controls marker spawning.
//
// Delivery is List + Who (TS updateEntityList parity: the client diffs the
// region-scoped List against its spawned set and fetches missing instances
// via Who). Marker entities are NOT added to the Ready spawnFrames burst:
// TS never Ready-bursts the whole world, and 4000+ Spawn frames per
// connection would dwarf the legacy real-mode burst (p2 + rat + 8 demo
// resources, which stays exactly as today via legacyResourceSpawns).
//
// Per-type spawn paths (all reuse the existing registries/loaders — no
// file is re-read, no drop-table/XP/formula or packet-shape change):
//   - mobs:      spawnMobQuiet — the full spawnMob path (mobs.json +
//     spawns.json per-instance overrides, plateau bind, chest-area
//     adoption) minus the Spawn broadcast (zero subscribers at boot; late
//     joiners resolve through List + the marker Who branch below).
//     STAGGERED: 2258 mobs never spawn on one tick — the queue drains at
//     MARKER_MOB_BATCH (default 200) instances per MARKER_MOB_MS (default
//     50ms) tick, i.e. ~4000/s, the full set in ~0.6s. (There is no
//     pre-existing engine spawn queue to reuse — the engine only owns the
//     500ms AI tick — so the drain loop below is the documented cadence.)
//     Tests and MARKER_SYNC=1 drain synchronously.
//   - NPCs:      markerNPCs registry + position index; Who serves the same
//     EntityData shape as the showcase NPC spawns (Type NPC), so talk,
//     store, bank and quest-talk resolve through the unchanged
//     resolveNPCKey -> spawnPayload path.
//   - resources: appended to resourceSpawns (+ the resources state map and
//     the resourceEntities blocker), the same ResourceEntityData shape as
//     the demo line, so gather/exhaust/respawn/List/Who work unchanged.
//   - items:     entity loot registry (RegisterLoot, persistent like chest
//     items), the same Item shape as loot so Target/Step pickup works.
//     (Zero item markers exist in the current data; TS static-item respawn
//     on pickup is not ported — picked-up marker items stay gone.)
//
// Perf notes (verified, not changed): position insertion is Store.Set, an
// O(1) amortized map upsert (the "entity grid" here is the Registry map —
// there is no per-tile grid structure to keep O(1)); List delivery filters
// the snapshot by the requester's 9-region interest set (handleList, still
// region-scoped); mob AI still ticks every live mob each 500ms (TS parity
// — the per-mob step is lock + profile copy + player scan, sub-ms for
// 2k+ mobs) with roaming gated to the 17s RoamInterval plus the
// entities.ts >30-players far-skip (SkipFarRoam in m9.go/mob.go).
package server

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/entity"
	worldcore "rpg-world-server/internal/world"
)

// Marker entity kinds (entities.ts getEntityType dispatch order).
type markerType int

const (
	markerUnknown markerType = iota
	markerItem
	markerNPCKind
	markerMob
	markerTree
	markerRock
	markerFishSpot
	markerForaging
)

// markerNPC is one marker-spawned NPC (showcase Spawn shape parity).
type markerNPC struct {
	key  string
	name string
	x, y int
}

// markerMobSpawn is one queued marker mob spawn (staggered drain).
type markerMobSpawn struct {
	instance string
	key      string
	x, y     int
}

// markerCounts tallies one population run (logged + returned).
type markerCounts struct {
	mobs, npcs, trees, rocks, fish, foraging, items, unknown int
	// unknownKeys aggregates skipped keys (distinct key -> occurrences).
	unknownKeys map[string]int
	// run records what to undo in resetMarkers (tests only).
	mobInstances []string
	npcInstances []string
	resBase      int
	resInstances []string
	itemInsts    []string
	// mobQueue is the pending staggered batch (async drain only).
	mobQueue []markerMobSpawn
}

var (
	markerMu      sync.Mutex
	markersDone   bool
	markerCounts_ markerCounts
	markerNPCs    = map[string]*markerNPC{}
	markerMobSet  = map[string]bool{}
	// markerLegacyResources snapshots len(resourceSpawns) before the first
	// population so spawnFrames keeps serving exactly the legacy demo
	// entries on Ready (-1 = unpopulated; markers append after the bound).
	markerLegacyResources = -1
	// markerSpawnSync drains the mob queue synchronously (tests +
	// MARKER_SYNC=1); production drains on the batch ticker.
	markerSpawnSync = os.Getenv("MARKER_SYNC") == "1"
)

// classifyMarker maps a marker key to its spawn kind in the exact
// entities.ts getEntityType order (item -> npc -> mob -> tree -> rock ->
// fishspot -> foraging). All tables are the already-loaded singletons —
// nothing is re-read. Unknown keys (oak6, inibti, miniiceknight, empty,
// greenstoremannpc, ...) report markerUnknown for skip+log.
func classifyMarker(key string) markerType {
	loadMobTables()
	loadResources()
	if key != "" && controller.ItemInfoFor(key) != nil {
		return markerItem
	}
	if controller.IsNPCKey(key) {
		return markerNPCKind
	}
	mobMu.Lock()
	_, mobOK := mobProf[key]
	mobMu.Unlock()
	if mobOK {
		return markerMob
	}
	if resourceTables["trees"] != nil && resourceTables["trees"][key] != nil {
		return markerTree
	}
	if resourceTables["rocks"] != nil && resourceTables["rocks"][key] != nil {
		return markerRock
	}
	if resourceTables["fishing"] != nil && resourceTables["fishing"][key] != nil {
		return markerFishSpot
	}
	if resourceTables["foraging"] != nil && resourceTables["foraging"][key] != nil {
		return markerForaging
	}
	return markerUnknown
}

// markerResourceKind maps a resource marker kind to its table, entity type
// and instance prefix (resourceKind covers entity types for the gather
// path; the prefix keeps marker instances distinct from legacy ones).
func markerResourceKind(t markerType) (table string, entityType int, prefix string) {
	switch t {
	case markerTree:
		return "trees", EntityTree, "mk-t"
	case markerRock:
		return "rocks", EntityRock, "mk-r"
	case markerFishSpot:
		return "fishing", EntityFishSpot, "mk-f"
	case markerForaging:
		return "foraging", EntityForaging, "mk-g"
	}
	return "", 0, ""
}

// markerMobBatchSize caps one stagger tick (MARKER_MOB_BATCH, default 200).
func markerMobBatchSize() int {
	if v := os.Getenv("MARKER_MOB_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 200
}

// markerMobInterval is the stagger tick cadence (MARKER_MOB_MS,
// default 50ms).
func markerMobInterval() time.Duration {
	if v := os.Getenv("MARKER_MOB_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 50 * time.Millisecond
}

// populateMarkers spawns every world.json `entities` marker on the REAL
// path only (mode gate above). It runs from the static-chest boot hook
// (loadAreas, right after SpawnStaticChests) — before mobEngine and
// initEntities, so the registry seed picks the marker resources up. NPCs,
// resources and items spawn synchronously (cheap registry inserts); mobs
// drain staggered per the cadence above. Idempotent: repeat calls return
// the recorded counts without respawning (tests reset via resetMarkers).
func populateMarkers() markerCounts {
	markerMu.Lock()
	if markersDone {
		c := markerCounts_
		markerMu.Unlock()
		return c
	}
	markerMu.Unlock()

	var zero markerCounts
	if testMode || cleanMode || combatMode {
		return zero
	}
	loadWorld()
	loadResources()
	loadMobTables()
	if world == nil {
		log.Printf("markers: world not loaded, skipping population")
		return zero
	}

	markerMu.Lock()
	markerLegacyResources = len(resourceSpawns)
	resBase := len(resourceSpawns)
	markerMu.Unlock()

	var run markerCounts
	run.unknownKeys = map[string]int{}
	run.resBase = resBase

	world.ForEachMarker(func(x, y int, key string) {
		t := classifyMarker(key)
		switch t {
		case markerMob:
			idx := y*world.Width + x
			run.mobQueue = append(run.mobQueue, markerMobSpawn{
				instance: fmt.Sprintf("mk-m-%d", idx), key: key, x: x, y: y,
			})
		case markerNPCKind:
			idx := y*world.Width + x
			spawnMarkerNPC(fmt.Sprintf("mk-n-%d", idx), key, x, y, &run)
		case markerTree, markerRock, markerFishSpot, markerForaging:
			idx := y*world.Width + x
			spawnMarkerResource(t, idx, key, x, y, &run)
		case markerItem:
			idx := y*world.Width + x
			spawnMarkerItem(fmt.Sprintf("mk-i-%d", idx), key, x, y, &run)
		default:
			run.unknown++
			run.unknownKeys[key]++
		}
	})

	run.mobs = len(run.mobQueue)
	// Publish the run (queue included — the drain owns it), then spawn
	// mobs synchronously (tests / MARKER_SYNC=1) or start the stagger
	// ticker for the async production drain.
	markerMu.Lock()
	for _, q := range run.mobQueue {
		markerMobSet[q.instance] = true
	}
	run.mobInstances = append(run.mobInstances, mobInstancesOf(run.mobQueue)...)
	markerCounts_ = run
	markersDone = true
	pending := len(run.mobQueue) > 0
	syncDrain := markerSpawnSync
	markerMu.Unlock()

	if syncDrain {
		drainMarkerMobs(0)
	} else if pending {
		startMarkerDrain()
	}

	for key, n := range run.unknownKeys {
		log.Printf("markers: unknown key %q skipped (%d markers, no table entry)", key, n)
	}
	log.Printf("markers: spawned %d mobs, %d npcs, %d trees, %d rocks, %d fish, %d foraging, %d items; skipped %d unknown",
		run.mobs, run.npcs, run.trees, run.rocks, run.fish, run.foraging, run.items, run.unknown)
	return run
}

func mobInstancesOf(queue []markerMobSpawn) []string {
	out := make([]string, 0, len(queue))
	for _, q := range queue {
		out = append(out, q.instance)
	}
	return out
}

// spawnMarkerNPC registers one marker NPC (showcase Spawn shape: Type NPC,
// level 1, 50 HP, speed 150). Talk/store/bank/quest-talk resolve through
// the unchanged m6 path (spawnPayload Who branch below).
func spawnMarkerNPC(instance, key string, x, y int, run *markerCounts) {
	name := key
	if info := controller.NPCFor(key); info != nil && info.Name != "" {
		name = info.Name
	}
	markerMu.Lock()
	markerNPCs[instance] = &markerNPC{key: key, name: name, x: x, y: y}
	markerMu.Unlock()
	worldcore.SetEntityPos(instance, x, y)
	run.npcs++
	run.npcInstances = append(run.npcInstances, instance)
}

// spawnMarkerResource appends one marker resource to resourceSpawns plus
// the live state and blocker entries (demo-line shape), so gather,
// exhaust, respawn, List and Who work unchanged.
func spawnMarkerResource(t markerType, idx int, key string, x, y int, run *markerCounts) {
	table, entityType, prefix := markerResourceKind(t)
	info := resourceTables[table][key]
	if info == nil {
		run.unknown++
		run.unknownKeys[key]++
		return
	}
	inst := fmt.Sprintf("%s-%d", prefix, idx)
	res := ResourceEntityData{
		EntityData: EntityData{
			Instance: inst, Type: entityType, Key: key, Name: info.Name,
			X: x, Y: y,
		},
		State: intp(0),
	}
	resourceSpawns = append(resourceSpawns, res)
	resMu.Lock()
	resources[inst] = &resourceState{}
	resMu.Unlock()
	resourceEntities[inst] = [2]int{x, y}
	run.resInstances = append(run.resInstances, inst)
	switch t {
	case markerTree:
		run.trees++
	case markerRock:
		run.rocks++
	case markerFishSpot:
		run.fish++
	case markerForaging:
		run.foraging++
	}
}

// spawnMarkerItem registers one ground item (persistent loot shape, like
// chest items) so Target/Step pickup works unchanged.
func spawnMarkerItem(instance, key string, x, y int, run *markerCounts) {
	registerLoot(instance, key, 1, x, y, "")
	run.items++
	run.itemInsts = append(run.itemInsts, instance)
}

// drainMarkerMobs spawns up to n queued marker mobs (n <= 0 drains all).
// Called by the stagger ticker and synchronously by tests/MARKER_SYNC.
// Returns the remaining queue length.
func drainMarkerMobs(n int) int {
	for {
		markerMu.Lock()
		if len(markerCounts_.mobQueue) == 0 {
			markerMu.Unlock()
			return 0
		}
		q := markerCounts_.mobQueue[0]
		markerCounts_.mobQueue = markerCounts_.mobQueue[1:]
		remaining := len(markerCounts_.mobQueue)
		markerMu.Unlock()

		if !spawnMobQuiet(q.instance, q.key, q.x, q.y, mobOverrides{}) {
			log.Printf("markers: mob %s (%s) at %d,%d skipped (no profile)", q.instance, q.key, q.x, q.y)
		}
		if n > 0 {
			n--
			if n == 0 {
				return remaining
			}
		}
	}
}

// startMarkerDrain launches the stagger ticker (once): each tick spawns up
// to MARKER_MOB_BATCH queued mobs until the queue is empty.
var markerDrainOnce sync.Once

func startMarkerDrain() {
	markerDrainOnce.Do(func() {
		go func() {
			t := time.NewTicker(markerMobInterval())
			defer t.Stop()
			for range t.C {
				if drainMarkerMobs(markerMobBatchSize()) == 0 {
					return
				}
			}
		}()
	})
}

// legacyResourceSpawns bounds the Ready spawnFrames burst to the pre-marker
// demo entries (real-mode Ready shape unchanged by population; markers are
// discovered region-scoped via List + Who).
func legacyResourceSpawns() []ResourceEntityData {
	markerMu.Lock()
	defer markerMu.Unlock()
	if markerLegacyResources < 0 || markerLegacyResources > len(resourceSpawns) {
		return resourceSpawns
	}
	return resourceSpawns[:markerLegacyResources]
}

// markerNPCPayload serves Who for one marker NPC (showcase Spawn shape).
func markerNPCPayload(instance string) (any, bool) {
	markerMu.Lock()
	npc, ok := markerNPCs[instance]
	markerMu.Unlock()
	if !ok {
		return nil, false
	}
	x, y, found := worldcore.EntityPos(instance)
	if !found {
		x, y = npc.x, npc.y
	}
	return EntityData{
		Instance: instance, Type: EntityNPC, Key: npc.key, Name: npc.name,
		X: x, Y: y,
		Orientation:   intp(OrientationDown),
		Level:         intp(1),
		HitPoints:     intp(50),
		MaxHitPoints:  intp(50),
		MovementSpeed: intp(150),
		AttackRange:   intp(1),
	}, true
}

// markerMobFor resolves a MARKER mob only (scoped so m1/m-rat-1 and m9test
// Who paths stay byte-identical): live engine state at the registry pos.
func markerMobFor(instance string) *mob {
	markerMu.Lock()
	_, ok := markerMobSet[instance]
	markerMu.Unlock()
	if !ok {
		return nil
	}
	return mobFor(instance)
}

// resetMarkers undoes one populateMarkers run (tests only): drops every
// spawned entity and restores the pre-run registries.
func resetMarkers() {
	markerMu.Lock()
	run := markerCounts_
	markerCounts_ = markerCounts{}
	markersDone = false
	legacy := markerLegacyResources
	markerLegacyResources = -1
	markerNPCs = map[string]*markerNPC{}
	markerMobSet = map[string]bool{}
	markerMu.Unlock()

	for _, inst := range run.mobInstances {
		worldcore.RemoveEntity(inst)
		removeMob(inst)
	}
	for _, inst := range run.npcInstances {
		worldcore.RemoveEntity(inst)
	}
	if legacy >= 0 && legacy <= len(resourceSpawns) {
		resourceSpawns = resourceSpawns[:legacy]
	}
	resMu.Lock()
	for _, inst := range run.resInstances {
		delete(resources, inst)
	}
	resMu.Unlock()
	for _, inst := range run.resInstances {
		delete(resourceEntities, inst)
	}
	for _, inst := range run.itemInsts {
		entity.DestroyLoot(inst, "marker reset")
	}
}
