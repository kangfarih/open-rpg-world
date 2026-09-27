package server

import (
	"encoding/json"
	"testing"

	worldcore "rpg-world-server/internal/world"
)

// Marker dispatch table (entities.ts getEntityType parity): known keys
// classify to their spawn kind in TS order (item -> npc -> mob -> tree ->
// rock -> fishspot -> foraging).
func TestClassifyMarkerDispatch(t *testing.T) {
	cases := []struct {
		key  string
		want markerType
	}{
		{"ironsword", markerItem},      // items.json only
		{"sorcerer", markerNPCKind},    // npcs.json (store: sorcerer)
		{"king", markerNPCKind},        // npcs.json (plain talk)
		{"rat", markerMob},             // mobs.json
		{"goblin", markerMob},          // mobs.json
		{"oak", markerTree},            // trees.json
		{"nisocrock", markerRock},      // rocks.json
		{"shrimpspot", markerFishSpot}, // fishing.json
		{"peachbush", markerForaging},  // foraging.json
		{"blueberrybush", markerForaging},
	}
	for _, c := range cases {
		if got := classifyMarker(c.key); got != c.want {
			t.Fatalf("classifyMarker(%q) = %d, want %d", c.key, got, c.want)
		}
	}
}

// Unknown marker keys (oak6, inibti, miniiceknight, empty, ...) skip like
// TS would (getEntityType -1 matches no spawn case).
func TestClassifyMarkerUnknownSkipped(t *testing.T) {
	for _, key := range []string{"oak6", "inibti", "miniiceknight", "", "greenstoremannpc", "empty"} {
		if got := classifyMarker(key); got != markerUnknown {
			t.Fatalf("classifyMarker(%q) = %d, want unknown", key, got)
		}
	}
}

// TESTMAP (default) population is a no-op: demo line + showcase grid stay
// exactly as today, no marker spawns anywhere.
func TestMarkerPopulateTestmapNoop(t *testing.T) {
	savedTest, savedClean, savedCombat := testMode, cleanMode, combatMode
	testMode, cleanMode, combatMode = true, false, false
	t.Cleanup(func() { testMode, cleanMode, combatMode = savedTest, savedClean, savedCombat })

	baseRes := len(resourceSpawns)
	baseReg := worldcore.EntityCount()
	baseMobs := len(mobs)

	got := populateMarkers()
	if got.mobs != 0 || got.npcs != 0 || got.trees != 0 || got.unknown != 0 {
		t.Fatalf("TESTMAP populate = %+v, want all zero", got)
	}
	if len(resourceSpawns) != baseRes {
		t.Fatalf("resourceSpawns grew in TESTMAP (%d -> %d)", baseRes, len(resourceSpawns))
	}
	if worldcore.EntityCount() != baseReg {
		t.Fatalf("registry grew in TESTMAP (%d -> %d)", baseReg, worldcore.EntityCount())
	}
	if len(mobs) != baseMobs {
		t.Fatalf("mob registry grew in TESTMAP (%d -> %d)", baseMobs, len(mobs))
	}
	if len(markerNPCs) != 0 || len(markerMobSet) != 0 {
		t.Fatal("marker registries non-empty after TESTMAP populate")
	}
}

// Real-mode boot population: all 4,226 markers dispatch synchronously and
// the world holds >= 4000 spawned entities afterwards.
func TestMarkerBootCountRealMode(t *testing.T) {
	savedTest, savedClean, savedCombat := testMode, cleanMode, combatMode
	testMode, cleanMode, combatMode = false, false, false
	savedSync := markerSpawnSync
	markerSpawnSync = true
	t.Cleanup(func() {
		resetMarkers()
		testMode, cleanMode, combatMode = savedTest, savedClean, savedCombat
		markerSpawnSync = savedSync
	})

	loadWorld()
	if world == nil {
		t.Fatal("world not loaded")
	}
	total := len(world.Entities)
	if total != 4226 {
		t.Fatalf("world markers = %d, want 4226", total)
	}

	got := populateMarkers()
	t.Logf("markers: %+v", got)
	spawned := got.mobs + got.npcs + got.trees + got.rocks + got.fish + got.foraging + got.items
	if spawned < 4000 {
		t.Fatalf("spawned = %d, want >= 4000", spawned)
	}
	if spawned+got.unknown != total {
		t.Fatalf("spawned(%d)+unknown(%d) != %d markers", spawned, got.unknown, total)
	}
	// Breakdown parity with the world.json census.
	if got.mobs != 2258 {
		t.Fatalf("mobs = %d, want 2258", got.mobs)
	}
	if got.trees != 1279 {
		t.Fatalf("trees = %d, want 1279", got.trees)
	}
	if got.foraging != 344 {
		t.Fatalf("foraging = %d, want 344", got.foraging)
	}
	if got.rocks != 91 {
		t.Fatalf("rocks = %d, want 91", got.rocks)
	}
	if got.fish != 91 {
		t.Fatalf("fish = %d, want 91", got.fish)
	}
	if got.npcs != 71 {
		t.Fatalf("npcs = %d, want 71", got.npcs)
	}
	// Every queued mob drained synchronously into the engine.
	if remaining := len(markerCounts_.mobQueue); remaining != 0 {
		t.Fatalf("mob queue remaining = %d, want 0 (sync drain)", remaining)
	}
	if len(mobs) < got.mobs {
		t.Fatalf("engine mobs = %d, want >= %d", len(mobs), got.mobs)
	}

	// Sorcerer marker (tile 117058 -> 706,101) resolves as an NPC Who
	// payload in showcase shape, so talk/store/bank works.
	payload, ok := spawnPayload("mk-n-117058")
	if !ok {
		t.Fatal("Who mk-n-117058 (sorcerer) unresolved")
	}
	raw, _ := json.Marshal(payload)
	var probe struct {
		Type int    `json:"type"`
		Key  string `json:"key"`
		Name string `json:"name"`
		X    int    `json:"x"`
		Y    int    `json:"y"`
	}
	_ = json.Unmarshal(raw, &probe)
	if probe.Type != EntityNPC || probe.Key != "sorcerer" {
		t.Fatalf("sorcerer payload = type %d key %q, want NPC/sorcerer", probe.Type, probe.Key)
	}
	if probe.X != 706 || probe.Y != 101 {
		t.Fatalf("sorcerer at %d,%d, want 706,101", probe.X, probe.Y)
	}
	if key := resolveNPCKey(nil, "mk-n-117058"); key != "sorcerer" {
		t.Fatalf("ResolveNPCKey(mk-n-117058) = %q, want sorcerer", key)
	}

	// A marker oak is a live gatherable resource in the same registries as
	// the demo line (blocking + hitResource-visible).
	resMu.Lock()
	_, oakState := resources["mk-t-115372"]
	resMu.Unlock()
	if !oakState {
		t.Fatal("marker oak mk-t-115372 missing from the resource registry")
	}
	if inst := resourceAt(172, 100); inst != "mk-t-115372" {
		t.Fatalf("resourceAt(172,100) = %q, want mk-t-115372", inst)
	}
	if !blocked(172, 100) {
		t.Fatal("marker oak tile not blocking (resource-occupant rule)")
	}

	// A marker mob resolves through Who with its live engine profile.
	var mobInst string
	markerMu.Lock()
	for inst := range markerMobSet {
		mobInst = inst
		break
	}
	markerMu.Unlock()
	if mobInst == "" {
		t.Fatal("no marker mobs registered")
	}
	mp, ok := spawnPayload(mobInst)
	if !ok {
		t.Fatalf("Who %s unresolved", mobInst)
	}
	raw, _ = json.Marshal(mp)
	var mprobe struct {
		Type int    `json:"type"`
		Key  string `json:"key"`
	}
	_ = json.Unmarshal(raw, &mprobe)
	m := markerMobFor(mobInst)
	if m == nil {
		t.Fatalf("%s not in the marker mob set", mobInst)
	}
	m.mu.Lock()
	liveKey := m.key
	m.mu.Unlock()
	if mprobe.Type != EntityMob || mprobe.Key != liveKey {
		t.Fatalf("Who %s = type %d key %q, want Mob/%q (live profile, never the rat fallback)",
			mobInst, mprobe.Type, mprobe.Key, liveKey)
	}
}

// Marker NPC talk: standing next to the sorcerer marker opens its store
// through the unchanged M6 path (walk-to-NPC parity at the handler level;
// live walking 600 tiles from spawn is infeasible, so the conn is placed
// adjacent like a completed walk).
func TestMarkerNPCTalkOpensStore(t *testing.T) {
	savedTest, savedClean, savedCombat := testMode, cleanMode, combatMode
	testMode, cleanMode, combatMode = false, false, false
	savedSync := markerSpawnSync
	markerSpawnSync = true
	t.Cleanup(func() {
		resetMarkers()
		testMode, cleanMode, combatMode = savedTest, savedClean, savedCombat
		markerSpawnSync = savedSync
	})
	populateMarkers()

	c, _ := deathConn(t, "talk-hero", "talk-hero-user")
	c.Sess.PlayerX, c.Sess.PlayerY = 705, 101 // adjacent to sorcerer 706,101
	worldcore.SetEntityPos(c.Instance, 705, 101)
	drainOutbox(c)

	handleNPCTarget(c, "mk-n-117058")
	if got := c.StoreOpen(); got != "sorcerer" {
		t.Fatalf("storeOpen = %q, want sorcerer", got)
	}
	found := false
	for _, f := range drainOutbox(c) {
		if len(f) == 3 {
			if id, ok := f[0].(int); ok && id == PacketStore {
				if op, ok := f[1].(int); ok && op == StoreOpen {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("no Store Open frame after talking to the sorcerer marker")
	}
}

// Stagger drain: batches of N per call (the MARKER_MOB_BATCH cadence).
func TestMarkerStaggerDrainBatches(t *testing.T) {
	savedSync := markerSpawnSync
	markerSpawnSync = true
	t.Cleanup(func() {
		markerSpawnSync = savedSync
		resetMarkers()
	})
	loadMobTables()

	seeds := []markerMobSpawn{
		{instance: "mk-test-1", key: "rat", x: 100, y: 96},
		{instance: "mk-test-2", key: "rat", x: 101, y: 96},
		{instance: "mk-test-3", key: "rat", x: 102, y: 96},
	}
	markerMu.Lock()
	markerCounts_.mobQueue = append(markerCounts_.mobQueue, seeds...)
	for _, q := range seeds {
		markerMobSet[q.instance] = true
	}
	markerMu.Unlock()

	if remaining := drainMarkerMobs(2); remaining != 1 {
		t.Fatalf("drain(2) remaining = %d, want 1", remaining)
	}
	if mobFor("mk-test-1") == nil || mobFor("mk-test-2") == nil {
		t.Fatal("first batch missing from the engine")
	}
	if mobFor("mk-test-3") != nil {
		t.Fatal("third mob spawned before its batch")
	}
	if remaining := drainMarkerMobs(0); remaining != 0 {
		t.Fatalf("drain all remaining = %d, want 0", remaining)
	}
	if mobFor("mk-test-3") == nil {
		t.Fatal("third mob missing after full drain")
	}
	// Stagger cadence defaults (documented): 200 mobs per 50ms tick.
	if markerMobBatchSize() != 200 {
		t.Fatalf("batch default = %d, want 200", markerMobBatchSize())
	}
}
