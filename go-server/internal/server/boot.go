// Minimal Kaetram-compatible WebSocket stub.
//
// Listens on ws://127.0.0.1:9001 (client server PORT, HUB disabled).
// Boot: S Connected -> C Handshake{gVer} -> S Handshake{type:client} ->
// C Login -> S Welcome + Map -> C Ready{regionsLoaded,userAgent} ->
// S Spawn* (SPEC.md section 2).
// Every RX/TX packet is logged to stdout for manual verification
// against the stock client at http://127.0.0.1:9000.
package server

import (
	"bytes"
	"compress/gzip"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/app"
	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/meta"
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/player"
	"rpg-world-server/internal/sim"
	"rpg-world-server/internal/version"
	worldcore "rpg-world-server/internal/world"
	"rpg-world-server/internal/worldmap"
)

// E9b split notes (behavior frozen):
//   - boot/env (TESTMAP/CLEAN/COMBAT, PORT addr, DUMMY_* tuning) parses via
//     internal/app + internal/world; the root vars below keep their exact
//     names/defaults because frozen m5-m13.go and *_wire.go files read them.
//   - movement/anticheat verify math delegates to internal/world (pure).
//   - the 20Hz tick loop runs subsystems through an internal/world Engine.
//   - showcase/combat-scene layout math delegates to internal/sim (pure).
//   - D2a: transport (upgrader/subs/broadcast/send/accept-gate) is owned by
//     internal/net (Hub + Conn/Session types); the entity registry
//     (entities/players maps, positions, region interest, disconnect fanout)
//     is owned by internal/world (Registry). Root keeps NO transport
//     globals: playerConn embeds *net.Conn, and every former
//     broadcast/send/setEntityPos/entityPos/removeClient/connByInstance call
//     site goes through the package APIs with identical frames/logs.
//     handleConn dispatch + tick + boot stay in package main (D2b moves
//     those). internal/world.Store backs the entity positions; the combat
//     brain and boot sequence stay here.
// Lock order: net outbox/Conn.mu -> world Registry.mu -> persist store
// (dbMu) -> subsystem state (see internal/net + internal/world docs).

// addr resolves the listen address: PORT env (e.g. PORT=9002 for a side-by-side
// run while the TS dev server occupies 9001) or the client-server default 9001.
func addr() string {
	return app.ListenAddr(os.Getenv("PORT"))
}

// The WS upgrader lives in the net Hub (D2a); the root keeps no transport
// globals.

func intp(v int) *int { return &v }

func boolp(v bool) *bool { return &v }

// bufferSize emulates Utils.getBufferSize (util/utils.ts:257-258):
// encodeURI(JSON.stringify(data)).split(/%..|./).length - 1. Every ASCII
// char counts 1 and every UTF-8 byte of non-ASCII encodes as one %XX, so
// the result equals the UTF-8 byte length of the JSON string.
func bufferSize(jsonBytes []byte) int {
	return len(jsonBytes)
}

// TESTMAP mode (default ON; TESTMAP=0/false/off/no or --testmap=false /
// --notestmap selects the 9 real regions pure): CLONE of the 9 real regions
// around spawn as the base map, plus overlays: center pond + 5-resource demo
// line + showcase grid stamped over the base (see getTestRegionData).
//
// Tile IDs (repo read-only recon):
//   - grass testGrassTile=3907: base layer of the layered [3907,9787] grass
//     under the hero spawn (100,100); neither id is in world.json
//     `collisions`/`objects`, so buildTile flags it c:false (walkable).
//   - water testWaterTile=29: in world.json `collisions` (c:true) AND in the
//     client map animations dict (map.json "29": 28->29->30->31 water ripple),
//     so it renders animated and blocks movement.
const (
	testGrassTile = 3907
	testWaterTile = 29

	// Center pond: ellipse centered (pondCX,pondCY) radii pondRX x pondRY.
	// Covers x100-108, y101-107; demo line y=98 and spawns y96 stay on grass.
	pondCX, pondCY = 104, 104
	pondRX, pondRY = 4, 3
)

// bootModes is the E9b-parsed TESTMAP/CLEAN/COMBAT selection (internal/world
// truth tables, identical to the inline parsing below it replaced). The
// individual vars keep their names for the frozen root files.
var bootModes = worldcore.ParseModes(os.Getenv, os.Args[1:])

// testMode defaults ON for now; env wins, then CLI flags.
var testMode = bootModes.Test

// cleanMode serves pure original terrain with zero overlays plus ONE
// fully-equipped adventurer showcase. Select with CLEAN=1/true/on/yes or
// --clean (CLEAN=0/false/off/no or --clean=false/--noclean turns it off;
// default OFF). It reuses the TESTMAP=0 pure path for tiles (getRegionData,
// no pond/grass stamps) and additionally drops ALL test entities (demo line,
// showcase grid, demos, guest p2, rat) and the showcase anim ticker — only
// the Welcome hero + one adventurer Spawn + one Equipment Batch remain.
// Collision/movement/gather handlers stay wired but dormant (no resources).
var cleanMode = bootModes.Clean

// combatMode serves pure original terrain (like CLEAN) plus a combat test
// scene: a boss dummy + a 4-bot party (warrior, archer, mage, support) that
// beats on it. Select with COMBAT=1/true/on/yes or --combat (COMBAT=
// 0/false/off/no or --combat=false/--nocombat turns it off; default OFF).
// It reuses the TESTMAP=0 pure path for tiles (getRegionData, no pond/grass
// stamps) and drops ALL test entities (demo line, showcase grid, demos,
// guest p2, rat); only the Welcome hero + 4 bot Spawns + Boss Spawn remain.
// The party brain (startCombat) ticks independently of clients; broadcasts
// are no-ops with no subscribers.
var combatMode = bootModes.Combat

// isTestWater reports whether (x,y) is pond water (ellipse test).
func isTestWater(x, y int) bool {
	return sim.IsWater(x, y, pondCX, pondCY, pondRX, pondRY)
}

// isTestGrass reports whether (x,y) is a forced-walkable overlay tile in
// test mode: under each of the 5 demo resources, under every showcase grid slot, and
// under the 2 paperdoll demos. getTestRegionData stamps grass 3907 c:false
// there (replacing any colliding base tile); tileBlocked mirrors it.
func isTestGrass(x, y int) bool {
	if y == 98 {
		for _, ox := range []int{98, 100, 102, 104, 106} {
			if x == ox {
				return true
			}
		}
	}
	if (x == 98 && y == 108) || (x == 100 && y == 108) {
		return true
	}
	// Showcase-grid slots are forced walkable (grid math in internal/sim).
	if sim.InGrid(x, y, showOX, showOY, showCols, showStep, len(showMobs)+len(showNPCs)) {
		return true
	}
	return false
}

// getTestRegionData clones the 9 real regions around spawn (100,100) as the
// base map, then stamps overlays over the base (replacing tiles, adding where
// the base was empty): center pond (water 29 c:true), grass 3907 c:false
// under each demo resource + every showcase grid slot + demo spots (so all 232 stand
// on walkable). RegionTile shape and [4,base64gzip,bufSize] framing are
// identical to the real path.
func getTestRegionData() map[int][]RegionTile {
	data := getRegionData(100, 100)
	posIdx := make(map[int]map[[2]int]int, len(data))
	for rid, tiles := range data {
		m := make(map[[2]int]int, len(tiles))
		for i, t := range tiles {
			m[[2]int{t.X, t.Y}] = i
		}
		posIdx[rid] = m
	}
	set := func(x, y int, tile RegionTile) {
		rid := (y/mapDivisionSize)*sideLen + (x / mapDivisionSize)
		m := posIdx[rid]
		if m == nil {
			m = make(map[[2]int]int)
			posIdx[rid] = m
		}
		if i, ok := m[[2]int{x, y}]; ok {
			tiles := data[rid]
			tiles[i].Data = tile.Data
			tiles[i].C = tile.C
			tiles[i].O = false
			tiles[i].Cur = ""
			data[rid] = tiles
			return
		}
		data[rid] = append(data[rid], tile)
		m[[2]int{x, y}] = len(data[rid]) - 1
	}
	// Grass overlays first so the pond always wins on any overlap.
	for _, ox := range []int{98, 100, 102, 104, 106} {
		set(ox, 98, RegionTile{X: ox, Y: 98, Data: testGrassTile})
	}
	for i := 0; i < len(showMobs)+len(showNPCs); i++ {
		gx, gy := showPos(i)
		set(gx, gy, RegionTile{X: gx, Y: gy, Data: testGrassTile})
	}
	for _, p := range [][2]int{{98, 108}, {100, 108}} {
		set(p[0], p[1], RegionTile{X: p[0], Y: p[1], Data: testGrassTile})
	}
	pondWater := 0
	for y := pondCY - pondRY; y <= pondCY+pondRY; y++ {
		for x := pondCX - pondRX; x <= pondCX+pondRX; x++ {
			if isTestWater(x, y) {
				set(x, y, RegionTile{X: x, Y: y, Data: testWaterTile, C: true})
				pondWater++
			}
		}
	}
	base := 0
	for _, tiles := range data {
		base += len(tiles)
	}
	log.Printf("testmap: base+overlay tiles=%d regions=%d pondWater=%d", base, len(data), pondWater)
	return data
}

// buildMapFrame serves the 9 real regions around spawn (100,100): region 50
// (x96-143 y96-143) + neighbours 25,26,27,49,51,73,74,75 (regions.ts
// getSurroundingRegions; sideLength = 1152/48 = 24). Tiles are extracted
// from world.json (index = y*width+x, number|[n..] layered) with empty
// (0/[]) skipped and data>=1 kept, mirroring getRegionTileData
// (regions.ts:541-563). Framing stays [4,base64gzip,bufSize] with gzip
// (Utils.compress default) + encodeURI byte length. Built once and cached.
// This is the STATIC base: per-player dynamic re-skins layer on top via
// buildMapFrameFor (dynmap.go) without changing this frame.
func buildMapFrame() []any {
	mapOnce.Do(func() {
		data := mapBaseData()
		mapFrame = encodeMapFrame(data)
		log.Printf("map frame: cleanMode=%v testMode=%v %d regions", cleanMode, testMode, len(data))
	})
	return mapFrame
}

// encodeMapFrame gzips + base64-framing for region data: the shared
// [4,base64gzip,bufSize] constructor for the static boot frame and the
// per-player dynamic variants (identical framing, dynmap.go).
func encodeMapFrame(data map[int][]RegionTile) []any {
	regionsJSON, err := json.Marshal(data)
	if err != nil {
		log.Fatalf("marshal regions: %v", err)
	}

	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(regionsJSON); err != nil {
		log.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		log.Fatalf("gzip close: %v", err)
	}

	return mapPkt(base64.StdEncoding.EncodeToString(buf.Bytes()), bufferSize(regionsJSON))
}

var (
	mapOnce  sync.Once
	mapFrame []any
)

// Region plumbing (mirrors server map/regions.ts + map/map.ts).
const (
	mapDivisionSize = 48 // Modules.Constants.MAP_DIVISION_SIZE (modules.ts:630)

	diagonalFlag = 0x20000000 // Modules.MapFlags (modules.ts:706-708)
	flipMask     = 0x20000000 | 0x40000000 | 0x80000000
)

func worldPath() string {
	if p := os.Getenv("WORLD_JSON"); p != "" {
		return p
	}
	// Fresh-clone portable default: ../packages/server/data/map/world.json
	// relative to the server working directory (go-server/). Fall back to
	// the legacy absolute path only if the relative candidate is missing.
	rel := filepath.Join("..", "packages", "server", "data", "map", "world.json")
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	for _, alt := range []string{
		filepath.Join("..", "..", "packages", "server", "data", "map", "world.json"),
		filepath.Join("packages", "server", "data", "map", "world.json"),
	} {
		if _, err := os.Stat(alt); err == nil {
			return alt
		}
	}
	return "/Users/appfuxion/repo/rpg-world-sim/packages/server/data/map/world.json"
}

// Canonical world data lives in internal/worldmap (World/LoadDefault); the
// root keeps thin compat aliases so m8/m10 (`world == nil`), m13
// (`world.Width`), and bus_impl/regionOf/getTestRegionData (`sideLen`)
// compile unchanged.
var (
	worldOnce sync.Once
	world     *worldmap.World
	sideLen   int
)

func loadWorld() {
	w, err := worldmap.LoadDefault(worldPath())
	if err != nil {
		log.Fatalf("%v", err)
	}
	worldOnce.Do(func() {
		world = w
		sideLen = w.SideLen
		log.Printf("world loaded: %dx%d sideLen=%d tiles=%d collisions=%d objects=%d",
			w.Width, w.Height, sideLen, len(w.Data), len(w.Collisions), len(w.Objects))
	})
}

// unflipTile strips Tiled flip bitmasks (map.ts:353-361).
func unflipTile(id int) int {
	return worldmap.UnflipTile(id)
}

// buildTile delegates to the canonical worldmap store (regions.ts:610-646).
// The RegionTile conversion stays here (RegionTile lives in package main via
// packets.go, which worldmap cannot import).
func buildTile(x, y int) (RegionTile, bool) {
	loadWorld()
	t, ok := world.BuildTile(x, y)
	if !ok {
		return RegionTile{}, false
	}
	return RegionTile{X: t.X, Y: t.Y, Data: t.Data, C: t.C, O: t.O, Cur: t.Cur}, true
}

// surroundingRegions delegates to the canonical worldmap store
// (regions.ts:711-766), region first then neighbours.
func surroundingRegions(region int) []int {
	loadWorld()
	return world.SurroundingRegions(region)
}

// getRegionData delegates to the canonical worldmap store (regions.ts:501-529)
// for a static spawn, converting Tiles to RegionTiles; empty regions dropped.
func getRegionData(px, py int) map[int][]RegionTile {
	loadWorld()
	raw := world.RegionData(px, py)
	data := make(map[int][]RegionTile, len(raw))
	for rid, tiles := range raw {
		out := make([]RegionTile, 0, len(tiles))
		for _, t := range tiles {
			out = append(out, RegionTile{X: t.X, Y: t.Y, Data: t.Data, C: t.C, O: t.O, Cur: t.Cur})
		}
		data[rid] = out
	}
	return data
}

// newPlayerInstance mints a random per-connection player id (p-<12 hex).
// Guest/scenario bots keep their stable scenario IDs (p2, p-warbot, ...);
// only the Welcome self player is random per connection.
func newPlayerInstance() string {
	var b [6]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("p-%d", time.Now().UnixNano()%0xffffff)
	}
	return "p-" + hex.EncodeToString(b[:])
}

// welcomePlayer is the self player delivered via Welcome (PlayerData,
// impl/player.ts:15-28). Key 'base' resolves to sprite player/base.
// Orientation is the required PlayerData-level field (always emitted).
// The instance is random per connection (M2 registry); scenario bots keep
// their stable IDs.
func welcomePlayer(instance string) PlayerData {
	// Look up the player's level for HP/mana scaling (formulas.ts parity).
	level := 1
	if c, ok := worldcore.Find[*playerConn](instance); ok && c != nil {
		st := playerStateFor(c.Username)
		pstateMu.Lock()
		level = st.Level
		pstateMu.Unlock()
	}
	maxHP := meta.HeroMaxHPForLevel(level)
	maxMana := meta.HeroMaxManaForLevel(level)
	return PlayerData{
		EntityData: EntityData{
			Instance:      instance,
			Type:          EntityPlayer,
			Key:           "base",
			Name:          "hero",
			X:             100,
			Y:             96,
			Level:         intp(level),
			HitPoints:     intp(maxHP),
			MaxHitPoints:  intp(maxHP),
			MovementSpeed: intp(220),
			AttackRange:   intp(1),
		},
		Orientation: OrientationDown,
		Rank:        0,
		Pvp:         false,
		Mana:        intp(maxMana),
		MaxMana:     intp(maxMana),
		Equipments:  []any{},
	}
}

// M4 resource tables (packages/server/data/*.json, same shape as
// ResourceInfo in common/types/resource.d.ts). Loaded once at boot via
// resourceDataPath (relative ../packages/server/data like worldPath, WORLD_JSON
// sibling envs override per table). Counts logged at boot.
//   - trees.json    -> Tree entities     (type 10, client prefix trees/)
//   - rocks.json    -> Rock entities     (type 11, prefix rocks/)
//   - fishing.json  -> FishSpot entities (type 13, prefix fishspots/)
//   - foraging.json -> Foraging entities (type 12, prefix bushes/)
//
// Every key in each table is spawnable; the TESTMAP demo line below uses one
// key per type (all level-1 so the stub skill level 1 can harvest them).
type resourceInfo struct {
	Name             string `json:"name"`
	LevelRequirement int    `json:"levelRequirement"`
	Experience       int    `json:"experience"`
	Difficulty       int    `json:"difficulty"`
	Item             string `json:"item"`
	RespawnTime      int    `json:"respawnTime"` // ms, optional per entry
}

var (
	resourcesOnce  sync.Once
	resourceTables = map[string]map[string]*resourceInfo{}
	resourceCounts = map[string]int{}
)

func resourceDataPath(name string) string {
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

func loadResources() {
	resourcesOnce.Do(func() {
		for _, name := range []string{"trees", "rocks", "fishing", "foraging"} {
			raw, err := os.ReadFile(resourceDataPath(name))
			if err != nil {
				log.Fatalf("read %s.json: %v", name, err)
			}
			var tbl map[string]*resourceInfo
			if err := json.Unmarshal(raw, &tbl); err != nil {
				log.Fatalf("parse %s.json: %v", name, err)
			}
			resourceTables[name] = tbl
			resourceCounts[name] = len(tbl)
		}
		log.Printf("resources loaded: trees=%d rocks=%d fishing=%d foraging=%d",
			resourceCounts["trees"], resourceCounts["rocks"],
			resourceCounts["fishing"], resourceCounts["foraging"])
	})
}

// resourceKind maps a TESTMAP/real spawn key to its table + entity type +
// gathering skill. Skill names match Modules.Skills (lumberjacking, mining,
// fishing, foraging).
func resourceKind(entityType int) (table, skill string) {
	switch entityType {
	case EntityTree:
		return "trees", "lumberjacking"
	case EntityRock:
		return "rocks", "mining"
	case EntityFishSpot:
		return "fishing", "fishing"
	case EntityForaging:
		return "foraging", "foraging"
	}
	return "", ""
}

// resourceSpawns lists every harvestable resource entity. TESTMAP mode: mixed
// demo line at y=98 (x=98,100,102,104,106) — 2 oaks + 1 rock + 1 fishspot +
// 1 bush — all level-1 (harvestable at stub skill 1), each blocking via the
// resource-occupant rule. Real mode: the legacy demo oak t1 plus one per real
// world.json `entities` oak marker nearest spawn (tileIndex->key,
// map.ts forEachEntity -> entities.ts spawnTree; key "oak" renders trees/oak
// via the entities.ts Type->prefix rule). All stand on walkable (c:false) tiles
// with walkable neighbours (verified against `collisions`/`objects`), and each
// blocks movement via the resource-occupant rule below (server map.isColliding
// hasEntityAt + isResource, map.ts:246-254).
var resourceSpawns = func() []ResourceEntityData {
	if cleanMode || combatMode {
		return nil // CLEAN/COMBAT: pure terrain, no resources (handlers dormant).
	}
	if testMode {
		return []ResourceEntityData{
			{EntityData: EntityData{Instance: "t-test-1", Type: EntityTree, Key: "oak", Name: "Oak", X: 98, Y: 98}, State: intp(0)},
			{EntityData: EntityData{Instance: "t-test-2", Type: EntityTree, Key: "oak", Name: "Oak", X: 100, Y: 98}, State: intp(0)},
			{EntityData: EntityData{Instance: "t-test-3", Type: EntityRock, Key: "coalrock", Name: "Coal", X: 102, Y: 98}, State: intp(0)},
			{EntityData: EntityData{Instance: "t-test-4", Type: EntityFishSpot, Key: "shrimpspot", Name: "Shrimp Fishing Spot", X: 104, Y: 98}, State: intp(0)},
			{EntityData: EntityData{Instance: "t-test-5", Type: EntityForaging, Key: "blueberrybush", Name: "Blueberry Bush", X: 106, Y: 98}, State: intp(0)},
		}
	}
	return []ResourceEntityData{
		{EntityData: EntityData{Instance: "t1", Type: EntityTree, Key: "oak", Name: "Oak", X: 105, Y: 100}, State: intp(0)},
		{EntityData: EntityData{Instance: "t-oak-1", Type: EntityTree, Key: "oak", Name: "Oak", X: 105, Y: 106}, State: intp(0)}, // entities idx 122217
		{EntityData: EntityData{Instance: "t-oak-2", Type: EntityTree, Key: "oak", Name: "Oak", X: 118, Y: 105}, State: intp(0)}, // idx 121078
		{EntityData: EntityData{Instance: "t-oak-3", Type: EntityTree, Key: "oak", Name: "Oak", X: 122, Y: 109}, State: intp(0)}, // idx 125690
		{EntityData: EntityData{Instance: "t-oak-4", Type: EntityTree, Key: "oak", Name: "Oak", X: 126, Y: 109}, State: intp(0)}, // idx 125694
		{EntityData: EntityData{Instance: "t-oak-5", Type: EntityTree, Key: "oak", Name: "Oak", X: 102, Y: 122}, State: intp(0)}, // idx 140646
		{EntityData: EntityData{Instance: "t-oak-6", Type: EntityTree, Key: "oak", Name: "Oak", X: 132, Y: 112}, State: intp(0)}, // idx 129156
		{EntityData: EntityData{Instance: "t-oak-7", Type: EntityTree, Key: "oak", Name: "Oak", X: 147, Y: 102}, State: intp(0)}, // idx 117651 (region 51)
	}
}()

// Showcase grid: EVERY mobs/* key from sprites.json that has a PNG (156;
// mobs/ghostrider has no PNG and is skipped, logged below) spawned as Mob
// entities + every npcs/* key (76 unique — sprites.json lists
// npcs/redbikinigirlnpc twice) as NPC entities. 232 total, all STATIC (no
// roam broadcasts) so the formation holds for screenshots. Alphabetical
// within each group (mobs first at indices 0-155, NPCs at 156-231), spacing
// 2 tiles from origin (showOX,showOY): extent x96-126, y110-138 — all inside
// test region 50 (x96-143 y96-143), clear of the pond (y101-107) and the demo
// row (y=98), every tile walkable grass (c:false in test mode).
const (
	showOX, showOY     = 96, 110
	showCols, showStep = 16, 2
)

var showMobs = []string{
	"adherer", "ancientwizard", "angel", "ant", "babyspider", "bat", "beetle",
	"blackpirateskeleton", "blackwizard", "blazespider", "bluepreta", "brownmouse",
	"cactus", "card", "card2", "cat", "clam", "cobra", "cowwarrior", "crab",
	"crystalscorpion", "cursedhahoemask", "cursedjangseung", "darkogre", "darkregion",
	"darkregionillusion", "darkscorpion", "darkskeleton", "darkwolf", "deathknight",
	"desertscorpion", "devilkazya", "earthworm", "eliminator", "enel", "eye",
	"firespider", "flaredeathknight", "fluffy", "forestdragon", "frog", "frostqueen",
	"ghost", "goblin", "goldgolem", "golem", "greencockroach", "greenfish",
	"greenpirateskeleton", "guardmace", "guardsword", "hellhound", "hellspider",
	"hermitcrab", "hobgoblin", "hongcheol", "icebat", "icecrab", "icegoblin",
	"icegolem", "iceknight", "icerat", "icevulture", "icewizard", "infectedguard",
	"infectedvillager", "ironogre", "jirisanmoonbear", "kaonashi", "lavaslime",
	"lightningguardian", "livingarmor", "mantis", "mermaid", "mimic", "minidragon",
	"miniemperor", "miniiceknight1", "miniiceknight2", "miniiceknight3",
	"miniiceknight4", "miniiceknight5", "miniknight1", "miniknight2", "miniknight3",
	"miniknight4", "miniknight5", "miniknight6", "miniseadragon", "moleking",
	"nightmareregion", "ogre", "ogrelord", "oldogre", "orc", "paladin", "penguin",
	"picklemob", "pierrot", "pinkelf", "piratecaptain", "pirateking", "pirateskeleton",
	"poisonspider", "preta", "purplepreta", "queenant", "queenspider", "rabbitman",
	"rat", "redcockroach", "redelf", "redfish", "redguard", "redmouse",
	"redpirateskeleton", "regionhenchman", "rhaphidophoridae", "rooster", "rudolf",
	"santa", "santaelf", "seadragon", "shadowregion", "skeleton", "skeleton2",
	"skeletonking", "skydinosaur", "skyelf", "slime", "smalldevil", "snek",
	"snowelf", "snowlady", "snowman", "snowman2", "snowrabbit", "snowwolf",
	"soldierant", "soybeanbug", "spectre", "spider", "squid", "suicideghost",
	"vulture", "whitebear", "whitemouse", "whitetiger", "windguardian", "wizard",
	"wolf", "yellowbat", "yellowfish", "yellowmouse", "yellowpreta", "zombie",
}

var showNPCs = []string{
	"agent", "ancientmanumentnpc", "angelnpc", "beachnpc", "blacksmith",
	"blacksmith2", "bluebikinigirlnpc", "bluestoremannpc", "boxingman", "coder",
	"desertnpc", "doctor", "elfnpc", "fairynpc", "fairynpc2", "firstsonangelnpc",
	"fisherman", "forestnpc", "greenstoremannpc", "guard", "guard2", "guildnpc",
	"herbalist", "iamverycoldnpc", "iceelfnpc", "king", "king2", "lavanpc",
	"madscientist", "mermaidnpc", "miner", "miner2", "mojojojonpc", "momangelnpc",
	"nyan", "octocat", "octopus", "oddeyecat", "oldlady", "oldlady2", "picklenpc",
	"pirategirlnpc", "priest", "prisoner", "purplestoremannpc", "ratnpc",
	"redbikinigirlnpc", "redstoremannpc", "rick", "rickgf", "royalguard",
	"royalguard2", "royalguard3", "santaelfnpc", "scientist", "secondsonangelnpc",
	"shepherdboy", "snowshepherdboy", "soldier", "sorcerer", "sponge",
	"superiorangelnpc", "vampire", "vendingmachine", "villagegirl", "villagegirl2",
	"villagegirl3", "villagegirl4", "villagegirl5", "villagegirl6", "villagegirl7",
	"villager", "villager2", "villager3", "villager4", "zombiegf",
}

// showPos returns the grid tile for showcase index i (mobs 0-155, NPCs
// continue at 156-231). Layout math lives in internal/sim.
func showPos(i int) (int, int) {
	return sim.GridPos(i, showOX, showOY, showCols, showStep)
}

// showAnimTick broadcasts the rotating showcase anims: Animation{action:Attack}
// for a window of 10 mobs + Idle for a window of 5 NPCs, so screenshots catch
// attack frames. The grid itself never moves (no Movement broadcasts).
var showTick uint64

func showAnimTick() {
	frames := make([][]any, 0, 15)
	start := int(showTick*10) % len(showMobs)
	for k := 0; k < 10; k++ {
		idx := (start + k) % len(showMobs)
		frames = append(frames, pkt(PacketAnimation, animationData{
			Instance: fmt.Sprintf("m-show-%d", idx+1), Action: ActionAttack,
		}))
	}
	ns := int(showTick*5) % len(showNPCs)
	for k := 0; k < 5; k++ {
		idx := (ns + k) % len(showNPCs)
		frames = append(frames, pkt(PacketAnimation, animationData{
			Instance: fmt.Sprintf("n-show-%d", idx+1), Action: ActionIdle,
		}))
	}
	showTick++
	worldcore.Broadcast(frames...)
	log.Printf("showcase anim tick %d: 10 atk (m-show-%d..) + 5 idle", showTick, start+1)
}

// startShowcase launches the 5s anim loop (once; broadcasts are no-ops
// with no subscribers).
var showOnce sync.Once

func startShowcase() {
	if cleanMode || combatMode {
		return // CLEAN/COMBAT: no grid, no anim ticker.
	}
	showOnce.Do(func() {
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for range t.C {
				showAnimTick()
			}
		}()
	})
}

// demoPlayers spawns 2 paperdoll demo players just north of the grid (grass,
// clear of pond/demo line). Weapon/helmet keys verified in server items.json
// (ironsword/ironhelmet, goldsword/goldhelmet) and sprites PNGs.
func demoPlayers() []PlayerData {
	// Demo players are level 1.
	level := 1
	maxHP := meta.HeroMaxHPForLevel(level)
	maxMana := meta.HeroMaxManaForLevel(level)
	mk := func(inst, name string, x, y int, weapon, helmet string) PlayerData {
		return PlayerData{
			EntityData: EntityData{
				Instance: inst, Type: EntityPlayer, Key: "base", Name: name,
				X: x, Y: y, Orientation: intp(OrientationDown),
				Level: intp(level), HitPoints: intp(maxHP), MaxHitPoints: intp(maxHP),
				MovementSpeed: intp(220), AttackRange: intp(1),
			},
			Orientation: OrientationDown, Rank: 0, Pvp: false,
			Mana: intp(maxMana), MaxMana: intp(maxMana),
			Equipments: []any{
				map[string]any{"type": EquipmentWeapon, "key": weapon, "count": 1, "enchantments": map[string]any{}},
				map[string]any{"type": EquipmentHelmet, "key": helmet, "count": 1, "enchantments": map[string]any{}},
			},
		}
	}
	return []PlayerData{
		mk("p-show-1", "ironknight", 98, 108, "ironsword", "ironhelmet"),
		mk("p-show-2", "goldknight", 100, 108, "goldsword", "goldhelmet"),
	}
}

// cleanAdventurerEquipments is the full paperdoll set for the CLEAN-mode
// adventurer showcase. Every key verified in packages/server/data/items.json
// with a client sprite at player/<slot>/<key>.png (boots have no player/
// folder — player.ts getType returns ” so the client falls back to
// items/<key>.png, which exists for ironboots). Slots per Modules.Equipment.
func cleanAdventurerEquipments() []any {
	mk := func(slot int, key string) any {
		return map[string]any{"type": slot, "key": key, "count": 1, "enchantments": map[string]any{}}
	}
	return []any{
		mk(EquipmentHelmet, "ironhelmet"),
		mk(EquipmentChestplate, "ironchestplate"),
		mk(EquipmentWeapon, "ironsword"),
		mk(EquipmentShield, "ironshield"),
		mk(EquipmentLegplates, "ironlegplates"),
		mk(EquipmentCape, "wings"),
		mk(EquipmentBoots, "ironboots"),
	}
}

// cleanAdventurer is the ONE fully-equipped Player near the hero at (102,96):
// walkable base grass 3907 (the suggested 102,100 is colliding tile 9975 in
// world.json). Type Player=0 so the client resolves the base sprite via the
// entities.ts prefix rule; paperdoll layers come from Equipments (player.ts
// load -> equip) plus the Batch below.
func cleanAdventurer() PlayerData {
	// Clean adventurer is level 1.
	level := 1
	maxHP := meta.HeroMaxHPForLevel(level)
	maxMana := meta.HeroMaxManaForLevel(level)
	return PlayerData{
		EntityData: EntityData{
			Instance: "p-adv-1", Type: EntityPlayer, Key: "base", Name: "adventurer",
			X: 102, Y: 96, Orientation: intp(OrientationDown),
			Level: intp(level), HitPoints: intp(maxHP), MaxHitPoints: intp(maxHP),
			MovementSpeed: intp(220), AttackRange: intp(1),
		},
		Orientation: OrientationDown, Rank: 0, Pvp: false,
		Mana: intp(maxMana), MaxMana: intp(maxMana),
		Equipments: cleanAdventurerEquipments(),
	}
}

// cleanEquipmentBatch mirrors server handler.ts handleEquipment:
// [8, Batch=0, {data:{equipments:[...]}}] (EquipmentPacket serialize with
// opcode; impl/equipment.ts SerializedEquipment). Sent right after the
// adventurer Spawn so paperdoll layers render even for clients that only
// apply Batch frames.
func cleanEquipmentBatch() []any {
	return pktOp(PacketEquipment, EquipmentBatch, map[string]any{
		"data": map[string]any{"equipments": cleanAdventurerEquipments()},
	})
}

// Combat scene: boss dummy + 4-bot party (COMBAT mode only).
//
// BossDummy: Mob `golem` (sprite mobs/golem.png verified) at (106,96), tile
// 5094 walkable (verified vs world.json collisions/objects), name
// "BossDummy", 5000 HP, static (no roam ticker, no retaliate — the brain
// only ever broadcasts bot->boss frames).
//
// Party (all facing the boss, orientation Right=3; boss faces Left=2):
//   - WarBot  Lv20 warrior at (102,96) tile 3907 walkable, full gold paperdoll
//     inline in Spawn (goldhelmet 0, goldchestplate 3, goldsword 4, goldshield
//     5, goldlegplates 9, goldboots 11 — all sprites verified). Autos 1200ms
//     15-25, Sunder 40/5s (Critical + Boulder + skills).
//   - ArchBot archer at (100,98) tile 3969 walkable, leather set
//     (leatherhelmet 0, leatherchest 3, leatherleggings 9, leatherboots 11) +
//     woodenbow (weaponarcher 4, sprite player/weapon/woodenbow verified) +
//     arrow (arrows slot 2, items/arrow + projectiles/arrow verified).
//     Autos 1600ms 12-20 ranged, Volley 3x30 Critical/6s, Spawn
//     attackRange 8 (ARCHER_ATTACK_RANGE default).
//   - MageBot mage at (102,98) tile 3394 walkable, leather set + naturestaff
//     (weaponmagic 4, sprite player/weapon/naturestaff verified; level 17 in
//     items.json — the spec "naturestaff17" is this item). Autos 2000ms
//     18-28 ranged (purplebolt/greenbolt flavour), Storm 45/7s
//     (Critical + Fireball), Spawn attackRange 9 (items.json).
//   - SupBot support at (100,96) tile 3394 walkable, leather set, NO weapon
//     (no priest player sprites exist — only npcs/priest — so the unarmed
//     leather healer is the priest-look approximation). NEVER attacks: every
//     6s heals the lowest-HP bot if anyone is hurt (Idle anim + Heal packet
//   - Healing FX + Points, amount clamped to missing HP, skipped at full),
//     every 12s party buff (Effect Add DefenseBuff/StrengthSuperBuff on all
//     4 bots).
//
// Ranged combat is real (M3 slice 1): archer/mage autos, Volley arrows and
// Storm bolts spawn Projectile entities (type 5, ownerInstance +
// targetInstance + hit, arrow/greenbolt keys from items.json) that travel
// distance*90ms like projectile.ts, then impact into the Combat Hit +
// (Storm Fireball) Effect + Points pipeline with hit.ranged=true, matching
// Hit.serialize. A kill mid-flight drops the impact (no phantom damage on
// the respawned life); a kill at launch fizzles the cast.
// Damage uses the formulas.ts port (see the M3 block above for divergences):
// autos roll (bonus+20)*1.25 [+5 player] [*slash 1.1] via rand^accuracy with
// 5% natural crits (ceilings war 37/52, archer 41/59, mage 62/91); Sunder 40
// / Volley 3x30 / Storm 45 stay fixed ability power under the shared 1s GCD.
// Leash demo: one rat mob (m-rat-1, 104,94, mobs.json rat profile) chases
// the nearest player within 6 tiles, leashes home beyond 10, respawns 10s
// after death; the BossDummy tracks its nearest attacker (log-only, no
// retaliate). No Equipment Batch is sent: Batch equips game.player (the
// hero), while inline Spawn equipments are applied per-entity via
// player.load -> equip.
const (
	combatDummyInstance  = "m-dummy"
	combatBotInstance    = "p-warbot"
	combatArcherInstance = "p-archer"
	combatMageInstance   = "p-mage"
	combatSupInstance    = "p-support"
	combatDummyX         = 106
	combatDummyY         = 96
	combatBotX           = 102
	combatBotY           = 96
	combatArcherX        = 100
	combatArcherY        = 98
	combatMageX          = 102
	combatMageY          = 98
	combatSupX           = 100
	combatSupY           = 96

	combatAutoMs      = 1200 // warrior player attack cycle (player attackAnimationSpeed=120)
	combatSkillMs     = 5000 // Sunder attempt cadence
	combatAutoMin     = 15
	combatAutoMax     = 25
	combatSkillDamage = 40

	combatArcherAutoMs  = 1600 // archer cycle
	combatArcherSkillMs = 6000 // Volley attempt cadence
	combatArcherAutoMin = 12
	combatArcherAutoMax = 20
	combatVolleyHits    = 3
	combatVolleyDamage  = 30

	combatMageAutoMs  = 2000 // mage cycle
	combatMageSkillMs = 7000 // Storm attempt cadence
	combatMageAutoMin = 18
	combatMageAutoMax = 28
	combatStormDamage = 45

	combatHealMs     = 6000 // support heal cadence (+40 HP)
	combatHealAmount = 40
	combatBuffMs     = 12000 // support party-buff cadence
)

const combatGlobalCD = time.Second // 1s GLOBAL CD shared across skills

// M3 slice 1 — real combat core (formula port, projectiles, aggro/leash).
//
// Damage formula port of packages/server/src/info/formulas.ts getDamage /
// getMaxDamage (melee/archery/magic): maxDamage = (damageBonus + skillLevel)
// * 1.25 [+50% on crit] [+5 player bonus] [*attack-style multiplier], rolled
// with randomWeightedInt(0, max, accuracy) = floor(rand^accuracy * (max+1))
// and clamped to the target's remaining HP. Accuracy mirrors the truth
// (MAX_ACCURACY 0.45 + bonus term + level term + target defense term +
// stat-weight term, -0.15 on crit).
// Documented divergences: (a) no potion/buff/terror accuracy or damage
// modifiers (support buffs are FX-only); (b) target is the stub BossDummy
// (Lv1, zero defense stats, defense level 1, damage reduction 1.0) instead
// of the golem-38 profile; (c) accuracy clamped to [0.7, 2.0] so demo DPS
// stays playable (faithful high-level accuracy would skew most hits to
// single digits); (d) no triangle-advantage step (dummy has no defense
// identity); (e) infinite arrows/mana (no inventory yet); (f) crit is a
// flat 5% roll (base rate; no critical-enchantment gear on the bots).
// Bot profiles below use items.json values: goldsword (str bonus 3,
// acc 6, slash 10), woodenbow+arrow (archery bonus 2+7=9, archery stat 2),
// naturestaff (magic bonus 26, magic stat 36).
type combatBotStat struct {
	weapon        string
	attackStyle   string // "slash" or "" (none)
	damageBonus   int
	accuracyBonus int
	accuracyLevel int
	damageLevel   int
	archer        bool
	magic         bool
	crush         int
	slash         int
	stab          int
	magicStat     int
	archery       int
}

var combatBotStats = map[string]*combatBotStat{
	combatBotInstance: {
		weapon: "goldsword", attackStyle: "slash",
		damageBonus: 3, accuracyBonus: 6, accuracyLevel: 20, damageLevel: 20,
		crush: 6, slash: 10, stab: 7,
	},
	combatArcherInstance: {
		weapon:      "woodenbow",
		damageBonus: 9, accuracyBonus: 9, accuracyLevel: 20, damageLevel: 20,
		archer: true, crush: 1, slash: 2, stab: 1, archery: 2,
	},
	combatMageInstance: {
		weapon:      "naturestaff",
		damageBonus: 26, accuracyBonus: 26, accuracyLevel: 20, damageLevel: 20,
		magic: true, crush: 2, slash: 4, stab: 4, magicStat: 36,
	},
}

// combatWeaponAttackRate is the data-driven attackRate lookup (player
// getAttackRate reads weapon.attackRate; bows fall back to
// ARCHER_ATTACK_RANGE-adjacent class cadences). items.json carries no
// attackRate for goldsword/woodenbow/naturestaff, so all three fall back to
// the class defaults: warrior 1200ms, archer 1600ms, mage 2000ms.
var combatWeaponAttackRate = map[string]int{}

func combatAutoRate(bot string) time.Duration {
	if ms := combatWeaponAttackRate[combatBotStats[bot].weapon]; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	switch bot {
	case combatArcherInstance:
		return combatArcherAutoMs * time.Millisecond
	case combatMageInstance:
		return combatMageAutoMs * time.Millisecond
	default:
		return combatAutoMs * time.Millisecond
	}
}

func combatMaxDamageFloat(bot string, critical bool) float64 {
	st := combatBotStats[bot]
	return meta.MaxDamageFloat(st.damageBonus, st.damageLevel, st.attackStyle, critical)
}

// combatAccuracyWeight ports getAccuracyWeight for a zero-defense dummy:
// archers/mages use their own school ((stat-0)/3, floored at 1), melee sums
// the positive schools. Resulting maxDamage ceilings: war 37 (crit 52),
// archer 41 (crit 59), mage 62 (crit 91).
func combatAccuracyWeight(bot string) float64 {
	st := combatBotStats[bot]
	return meta.AccuracyWeight(st.archer, st.magic, st.crush, st.slash, st.stab, st.magicStat, st.archery)
}

func combatAccuracy(bot string, critical bool) float64 {
	st := combatBotStats[bot]
	return meta.Accuracy(ModulesMaxAccuracy, ModulesMaxLevel, st.accuracyBonus, st.accuracyLevel, 1, combatAccuracyWeight(bot), critical)
}

// combatRollLocked rolls one formula hit for bot (caller holds combatMu;
// clamped to remaining HP like the truth). combatRollCrit is the base 5%
// crit chance (no gear bonus on the bots).
func combatRollLocked(bot string, critical bool) int {
	max := combatMaxDamageFloat(bot, critical)
	acc := combatAccuracy(bot, critical)
	return meta.RollDamage(max, acc, rand.Float64(), combatHP)
}

func combatRollCrit() bool { return meta.RollCrit(rand.Float64()) }

// Modules constants mirrored from packages/common/network/modules.ts.
const (
	ModulesMaxAccuracy = 0.45
	ModulesMaxLevel    = 120
)

// dummyMaxHP/dummyRespawnDelay are env-overridable for fast scripted checks
// (DUMMY_HP/DUMMY_RESPAWN seconds); defaults are the spec values (BossDummy
// 5000 HP, 15s respawn). Parsing lives in internal/app.
var dummyMaxHP = app.ParseDummyHP(os.Getenv("DUMMY_HP"))

var dummyRespawnDelay = app.ParseDummyRespawn(os.Getenv("DUMMY_RESPAWN"))

// Bot MaxHP (spawn full; support heals cap here). Boss deals no damage, so
// heals are observable as Heal + Healing-FX + Points packets.
var combatBotMaxHP = map[string]int{
	combatBotInstance:    400,
	combatArcherInstance: 300,
	combatMageInstance:   300,
	combatSupInstance:    300,
}

// combatBotOrder is the spawn/roster order and the heal tiebreak order.
var combatBotOrder = []string{
	combatBotInstance,
	combatArcherInstance,
	combatMageInstance,
	combatSupInstance,
}

var (
	combatOnce sync.Once
	combatMu   sync.Mutex
	combatHP   int
	combatDead bool
	// combatLastSkill is the per-bot 1s GLOBAL skill CD (skills only; autos
	// never consult it). combatBotHP tracks live bot HP for the healer.
	combatLastSkill map[string]time.Time
	combatBotHP     map[string]int
	combatBuffCycle int
)

// warbotEquipments is the full gold paperdoll (slots per Modules.Equipment).
func warbotEquipments() []any {
	mk := func(slot int, key string) any {
		return map[string]any{"type": slot, "key": key, "count": 1, "enchantments": map[string]any{}}
	}
	return []any{
		mk(EquipmentHelmet, "goldhelmet"),
		mk(EquipmentChestplate, "goldchestplate"),
		mk(EquipmentWeapon, "goldsword"),
		mk(EquipmentShield, "goldshield"),
		mk(EquipmentLegplates, "goldlegplates"),
		mk(EquipmentBoots, "goldboots"),
	}
}

func warbot() PlayerData {
	return PlayerData{
		EntityData: EntityData{
			Instance: combatBotInstance, Type: EntityPlayer, Key: "base", Name: "WarBot",
			X: combatBotX, Y: combatBotY, Orientation: intp(OrientationRight),
			Level: intp(20), HitPoints: intp(400), MaxHitPoints: intp(400),
			MovementSpeed: intp(220), AttackRange: intp(1),
		},
		Orientation: OrientationRight, Rank: 0, Pvp: false,
		Mana: intp(100), MaxMana: intp(100),
		Equipments: warbotEquipments(),
	}
}

// dummyData builds the dummy Spawn payload with live HP (omitted when dead;
// the respawn broadcast re-spawns it).
func dummyData() (EntityData, bool) {
	combatMu.Lock()
	defer combatMu.Unlock()
	if combatDead {
		return EntityData{}, false
	}
	return EntityData{
		Instance: combatDummyInstance, Type: EntityMob, Key: "golem", Name: "BossDummy",
		X: combatDummyX, Y: combatDummyY, Orientation: intp(OrientationLeft),
		Level: intp(1), HitPoints: intp(combatHP), MaxHitPoints: intp(dummyMaxHP),
		MovementSpeed: intp(150), AttackRange: intp(1),
	}, true
}

// leatherSet is the shared leather paperdoll base (helmet 0, chestplate 3,
// legplates 9, boots 11 — all keys in items.json with client sprites).
func leatherSet() []any {
	mk := func(slot int, key string) any {
		return map[string]any{"type": slot, "key": key, "count": 1, "enchantments": map[string]any{}}
	}
	return []any{
		mk(EquipmentHelmet, "leatherhelmet"),
		mk(EquipmentChestplate, "leatherchest"),
		mk(EquipmentLegplates, "leatherleggings"),
		mk(EquipmentBoots, "leatherboots"),
	}
}

func archerbotEquipments() []any {
	mk := func(slot int, key string) any {
		return map[string]any{"type": slot, "key": key, "count": 1, "enchantments": map[string]any{}}
	}
	return append(leatherSet(),
		mk(EquipmentWeapon, "woodenbow"),
		mk(EquipmentArrows, "arrow"),
	)
}

func magebotEquipments() []any {
	mk := func(slot int, key string) any {
		return map[string]any{"type": slot, "key": key, "count": 1, "enchantments": map[string]any{}}
	}
	return append(leatherSet(),
		mk(EquipmentWeapon, "naturestaff"),
	)
}

func supbotEquipments() []any { return leatherSet() }

// combatBotData builds one party bot Spawn. attackRange is per-class from the
// real server: bows default to ARCHER_ATTACK_RANGE=8 when items.json carries
// no attackRange (e.g. woodenbow; modules.ts:644 + item.ts
// getDefaultAttackRange), staves carry attackRange 9 in items.json (e.g.
// naturestaff), melee/unarmed stay 1 (goldsword has no attackRange, support
// is unarmed).
func combatBotData(instance, name string, x, y, level, hp, maxHP, attackRange int, equip []any) PlayerData {
	return PlayerData{
		EntityData: EntityData{
			Instance: instance, Type: EntityPlayer, Key: "base", Name: name,
			X: x, Y: y, Orientation: intp(OrientationRight),
			Level: intp(level), HitPoints: intp(hp), MaxHitPoints: intp(maxHP),
			MovementSpeed: intp(220), AttackRange: intp(attackRange),
		},
		Orientation: OrientationRight, Rank: 0, Pvp: false,
		Mana: intp(100), MaxMana: intp(100),
		Equipments: equip,
	}
}

func archerbot() PlayerData {
	return combatBotData(combatArcherInstance, "ArchBot",
		combatArcherX, combatArcherY, 20,
		combatBotMaxHP[combatArcherInstance], combatBotMaxHP[combatArcherInstance],
		8,
		archerbotEquipments())
}

func magebot() PlayerData {
	return combatBotData(combatMageInstance, "MageBot",
		combatMageX, combatMageY, 20,
		combatBotMaxHP[combatMageInstance], combatBotMaxHP[combatMageInstance],
		9,
		magebotEquipments())
}

func supbot() PlayerData {
	return combatBotData(combatSupInstance, "SupBot",
		combatSupX, combatSupY, 20,
		combatBotMaxHP[combatSupInstance], combatBotMaxHP[combatSupInstance],
		1,
		supbotEquipments())
}

func combatSpawns() [][]any {
	frames := [][]any{
		pkt(PacketSpawn, warbot()),
		pkt(PacketSpawn, archerbot()),
		pkt(PacketSpawn, magebot()),
		pkt(PacketSpawn, supbot()),
	}
	if d, ok := dummyData(); ok {
		frames = append(frames, pkt(PacketSpawn, d))
	}
	// M9: the rat is an engine mob — spawn frame reflects its live state.
	if rat := ratEntity(); rat != nil {
		frames = append(frames, pkt(PacketSpawn, rat))
	}
	return frames
}

// combatGen invalidates in-flight projectiles across a boss death: a
// projectile launched before the kill never damages the respawned boss
// (truth: target.hit on a dead target is a no-op via the isDead guard).
var combatGen int

// bossTarget/bossAttackers is the slice-1 aggro demo: the dummy tracks its
// attackers and retargets the nearest one (mob handler.ts handleHit adds the
// attacker; combat picks findNearestTarget). No retaliate yet — the boss
// never emits Combat packets, the target switch is log-observable only.
var (
	bossTarget    string
	bossAttackers = map[string]bool{}
)

func botTile(instance string) (int, int) {
	switch instance {
	case combatArcherInstance:
		return combatArcherX, combatArcherY
	case combatMageInstance:
		return combatMageX, combatMageY
	case combatSupInstance:
		return combatSupX, combatSupY
	default:
		return combatBotX, combatBotY
	}
}

func bossAggroNote(attacker string) {
	bossAttackers[attacker] = true
	best, bestD := attacker, -1
	for a := range bossAttackers {
		ax, ay := botTile(a)
		dx, dy := ax-combatDummyX, ay-combatDummyY
		if dx < 0 {
			dx = -dx
		}
		if dy < 0 {
			dy = -dy
		}
		if d := dx + dy; bestD < 0 || d < bestD {
			best, bestD = a, d
		}
	}
	if best != bossTarget {
		bossTarget = best
		log.Printf("combat: boss aggro -> %s (nearest attacker, no retaliate)", best)
	}
}

// applyBossHitLocked is the shared damage pipeline (server combat.ts
// sendAttack + character.handleHitPoints shape): optional Animation,
// Combat Hit splat, optional Effect impact, then Points (boss HP bar).
// Caller holds combatMu; no-op when boss dead. At 0 HP the boss despawns
// (mob death path: Despawn[13], not the player Death[29] scroll packet)
// and a respawn timer restores full HP + re-spawns it.
func applyBossHitLocked(attacker string, dmg, typ int, skills []string, ranged bool, effect int, withAnim bool) {
	if combatDead {
		return
	}
	if attacker != "" {
		bossAggroNote(attacker)
	}
	combatHP -= dmg
	if combatHP < 0 {
		combatHP = 0
	}
	hit := HitData{Type: typ, Damage: dmg, Skills: skills}
	if ranged {
		hit.Ranged = boolp(true)
	}
	if withAnim {
		worldcore.Broadcast(pkt(PacketAnimation, animationData{
			Instance: attacker, Action: ActionAttack,
		}))
	}
	worldcore.Broadcast(pktOp(PacketCombat, CombatHit, combatData{
		Instance: attacker, Target: combatDummyInstance, Hit: hit,
	}))
	if effect >= 0 {
		worldcore.Broadcast(pktOp(PacketEffect, EffectAdd, effectData{
			Instance: combatDummyInstance, Effect: effect,
		}))
	}
	worldcore.Broadcast(pkt(PacketPoints, pointsData{
		Instance:  combatDummyInstance,
		HitPoints: intp(combatHP), MaxHitPoints: intp(dummyMaxHP),
	}))
	log.Printf("combat: %s hit boss type=%d dmg=%d hp=%d/%d", attacker, typ, dmg, combatHP, dummyMaxHP)
	if combatHP <= 0 {
		combatDead = true
		combatGen++
		bossTarget = ""
		bossAttackers = map[string]bool{}
		worldcore.Broadcast(pkt(PacketDespawn, despawnData{Instance: combatDummyInstance}))
		log.Printf("combat: boss died -> despawned, respawn in %v", dummyRespawnDelay)
		// M5: boss death rolls the golem drop tables (attacker owns the loot).
		spawnLoot("golem", combatDummyX, combatDummyY, attacker)
		time.AfterFunc(dummyRespawnDelay, combatRespawn)
	}
}

func strikeBossLocked(attacker string, dmg, typ int, skills []string, ranged bool, effect int) {
	applyBossHitLocked(attacker, dmg, typ, skills, ranged, effect, true)
}

// Projectile entity registry for in-flight ranged shots (Who/List
// resolution between Spawn and impact Despawn).
var (
	projMu       sync.Mutex
	projSeq      int
	projPayloads = map[string]EntityData{}
)

func projKey(owner string) string {
	if owner == combatMageInstance {
		return "greenbolt" // naturestaff projectileName (items.json)
	}
	return "arrow" // arrow item projectileName (woodenbow has none)
}

// spawnRangedStrikeLocked ports combat.ts sendRangedAttack + entities
// spawnProjectile: the swing Animation + a Projectile Spawn (type 5,
// ownerInstance + targetInstance + hit) go out instantly, then damage lands
// on impact after distance*90ms of travel (projectile.ts), when the Combat
// Hit + Effect + Points pipeline fires. A stale/mid-flight kill drops the
// impact (instant-fallback: no phantom damage on the new life).
func spawnRangedStrikeLocked(owner string, dmg, typ int, skills []string, effect int) {
	if combatDead {
		log.Printf("combat: %s ranged fizzles (boss dead)", owner)
		return
	}
	bossAggroNote(owner)
	gen := combatGen
	ox, oy := botTile(owner)
	// Flight time follows the projectile.ts rule (distance*90ms, internal/sim).
	travel := sim.TravelBetween(ox, oy, combatDummyX, combatDummyY)
	hit := HitData{Type: typ, Damage: dmg, Skills: skills, Ranged: boolp(true)}
	projSeq++
	inst := fmt.Sprintf("pr-%d", projSeq)
	key := projKey(owner)
	p := EntityData{
		Instance: inst, Type: EntityProjectile, Key: key, Name: key,
		X: ox, Y: oy, OwnerInstance: owner, TargetInstance: combatDummyInstance,
		Hit: &hit,
	}
	projMu.Lock()
	projPayloads[inst] = p
	projMu.Unlock()
	worldcore.SetEntityPos(inst, ox, oy)
	worldcore.Broadcast(pkt(PacketAnimation, animationData{Instance: owner, Action: ActionAttack}))
	worldcore.Broadcast(pkt(PacketSpawn, p))
	log.Printf("combat: %s launched %s (%s) travel=%v dmg=%d", owner, inst, key, travel, dmg)
	time.AfterFunc(travel, func() {
		combatMu.Lock()
		defer combatMu.Unlock()
		worldcore.RemoveEntity(inst)
		projMu.Lock()
		delete(projPayloads, inst)
		projMu.Unlock()
		worldcore.Broadcast(pkt(PacketDespawn, despawnData{Instance: inst}))
		if combatDead || gen != combatGen {
			log.Printf("combat: %s impact dropped (target died mid-flight, fallback)", inst)
			return
		}
		applyBossHitLocked(owner, dmg, typ, skills, true, effect, false)
	})
}

// trySkill consumes one bot's 1s GLOBAL skill CD and runs fn. Returns false
// when the GCD rejects it (shared per-bot across that bot's skills).
// Caller must hold combatMu (every skill path locks first; autos never
// consult it).
func trySkill(bot string, fn func()) bool {
	now := time.Now()
	if now.Sub(combatLastSkill[bot]) < combatGlobalCD {
		log.Printf("combat: %s skill rejected by GCD (Δ=%v since last skill)", bot, now.Sub(combatLastSkill[bot]))
		return false
	}
	combatLastSkill[bot] = now
	fn()
	return true
}

// Autos NEVER consult the skill GCD; each class ticks at its own
// data-driven rate (combatAutoRate). Damage uses the M3 formula port with a
// 5% natural crit; archer/mage autos fly as real projectiles.
func combatAuto() {
	combatMu.Lock()
	defer combatMu.Unlock()
	crit := combatRollCrit()
	typ := HitsNormal
	if crit {
		typ = HitsCritical
	}
	strikeBossLocked(combatBotInstance, combatRollLocked(combatBotInstance, crit), typ, nil, false, -1)
}

func archerAuto() {
	combatMu.Lock()
	defer combatMu.Unlock()
	crit := combatRollCrit()
	typ := HitsNormal
	if crit {
		typ = HitsCritical
	}
	spawnRangedStrikeLocked(combatArcherInstance, combatRollLocked(combatArcherInstance, crit), typ, nil, -1)
}

func mageAuto() {
	combatMu.Lock()
	defer combatMu.Unlock()
	crit := combatRollCrit()
	typ := HitsNormal
	if crit {
		typ = HitsCritical
	}
	spawnRangedStrikeLocked(combatMageInstance, combatRollLocked(combatMageInstance, crit), typ, nil, -1)
}

// combatSunder attempts one Sunder (Critical + Boulder, 40 dmg). Returns
// false when the 1s GLOBAL CD rejects it (shared across skills via
// combatLastSkill); true when cast (or fizzled on a dead boss — the GCD is
// still consumed).
func combatSunder() bool {
	combatMu.Lock()
	defer combatMu.Unlock()
	return trySkill(combatBotInstance, func() {
		if combatDead {
			log.Printf("combat: sunder fizzles (boss dead)")
			return
		}
		strikeBossLocked(combatBotInstance, combatSkillDamage, HitsCritical, []string{"sunder"}, false, EffectBoulder)
	})
}

// combatVolley attempts one Volley: 3x Critical 30 arrow projectiles
// (skills ["volley"]) under a single GCD consumption. Returns false on GCD reject.
func combatVolley() bool {
	combatMu.Lock()
	defer combatMu.Unlock()
	return trySkill(combatArcherInstance, func() {
		if combatDead {
			log.Printf("combat: volley fizzles (boss dead)")
			return
		}
		for i := 0; i < combatVolleyHits; i++ {
			spawnRangedStrikeLocked(combatArcherInstance, combatVolleyDamage, HitsCritical, []string{"volley"}, -1)
		}
	})
}

// combatStorm attempts one Storm (Critical 45 greenbolt projectile + Fireball
// on impact, skills ["storm"]). Returns false on GCD reject.
func combatStorm() bool {
	combatMu.Lock()
	defer combatMu.Unlock()
	return trySkill(combatMageInstance, func() {
		if combatDead {
			log.Printf("combat: storm fizzles (boss dead)")
			return
		}
		spawnRangedStrikeLocked(combatMageInstance, combatStormDamage, HitsCritical, []string{"storm"}, EffectFireball)
	})
}

// combatHeal heals the lowest-HP bot, clamped to its missing HP (skipped
// while all bots are full — the BossDummy never retaliates, so the healer
// stays silent until something can actually wound the party), and broadcasts
// Heal (green +HP splat via client handleHeal) + Healing FX + Points (bot HP
// bar). The heal anim is Idle: no weapon swing. The support NEVER attacks
// the boss.
func combatHeal() {
	combatMu.Lock()
	defer combatMu.Unlock()
	target := combatBotOrder[0]
	lowest := combatBotHP[target] - combatBotMaxHP[target]
	for _, b := range combatBotOrder[1:] {
		if d := combatBotHP[b] - combatBotMaxHP[b]; d < lowest {
			lowest, target = d, b
		}
	}
	missing := combatBotMaxHP[target] - combatBotHP[target]
	if missing <= 0 {
		log.Printf("combat: support heal skipped (all bots full)")
		return
	}
	amount := combatHealAmount
	if amount > missing {
		amount = missing
	}
	hp := combatBotHP[target] + amount
	combatBotHP[target] = hp
	worldcore.Broadcast(pkt(PacketAnimation, animationData{
		Instance: combatSupInstance, Action: ActionIdle,
	}))
	worldcore.Broadcast(pkt(PacketHeal, healData{
		Instance: target, Type: "hitpoints", Amount: amount,
	}))
	worldcore.Broadcast(pktOp(PacketEffect, EffectAdd, effectData{
		Instance: target, Effect: EffectHealing,
	}))
	worldcore.Broadcast(pkt(PacketPoints, pointsData{
		Instance: target, HitPoints: intp(hp), MaxHitPoints: intp(combatBotMaxHP[target]),
	}))
	log.Printf("combat: support healed %s +%d hp=%d/%d", target, amount, hp, combatBotMaxHP[target])
}

// combatBuffTick attempts the buff under the support's GCD; when the 6s heal
// tick wins the race in the same second, retry once after the GCD clears so
// no buff cycle is ever silently dropped.
func combatBuffTick() {
	combatMu.Lock()
	ok := trySkill(combatSupInstance, func() { combatBuffNoLock() })
	combatMu.Unlock()
	if !ok {
		time.AfterFunc(combatGlobalCD+100*time.Millisecond, func() {
			combatMu.Lock()
			defer combatMu.Unlock()
			trySkill(combatSupInstance, func() { combatBuffNoLock() })
		})
	}
}

// combatBuffNoLock is the single party-buff body (Effect Add on all 4 bots,
// alternating DefenseBuff / StrengthSuperBuff each cycle). Caller must hold
// combatMu; the buff ticker (combatBuffTick) is its only caller.

func combatBuffNoLock() {
	effect := EffectDefenseBuff
	name := "DefenseBuff"
	if combatBuffCycle%2 == 1 {
		effect = EffectStrengthSuperBuf
		name = "StrengthSuperBuff"
	}
	combatBuffCycle++
	for _, b := range combatBotOrder {
		worldcore.Broadcast(pktOp(PacketEffect, EffectAdd, effectData{
			Instance: b, Effect: effect,
		}))
	}
	log.Printf("combat: support party buff %s on 4 bots", name)
}

// combatSkillTick fires every 5s: one real Sunder attempt plus an immediate
// probe that the GCD must reject (observable in logs as exactly one sunder
// emission per window).
func combatSkillTick() {
	combatSunder()
	combatSunder()
}

// volleySkillTick fires every 6s: one Volley plus a GCD-rejected probe.
func volleySkillTick() {
	combatVolley()
	combatVolley()
}

// stormSkillTick fires every 7s: one Storm plus a GCD-rejected probe.
func stormSkillTick() {
	combatStorm()
	combatStorm()
}

// Leash-demo rat (M3 slice 1 aggro/AI proof): one rat mob at (104,94),
// mobs.json movementSpeed 450, that chases the nearest player within 6
// tiles (mob.ts canAggro/isNear shape, aggroRange demo value 6), leashes
// back to spawn beyond 10 tiles (mob.ts sendToSpawn/outsideRoaming shape),
// and respawns 10s after death (rat respawnDelay in mobs.json is 10s).
// Slice 1 keeps the party scene stable: the rat never attacks and nothing
// targets it (no damage source yet), so death/respawn below is implemented
// but idle until later slices add real damage.
const (
	combatRatInstance = "m-rat-1"
	combatRatX        = 104
	combatRatY        = 94
	combatRatMaxHP    = 20 // mobs.json rat hitPoints
	combatRatAggro    = 6
	combatRatLeash    = 10
	combatRatTickMs   = 500
)

var combatRatRespawnDelay = 10 * time.Second

// M9: the leash-demo rat's AI moved to the engine (m9.go). The legacy
// per-instance rat state (ratMu/ratX/ratHP/ratTick/teleport/kill/respawn)
// and the nearestPlayerTile helper are retired; combatSpawns() reads the
// live engine mob instead (ratEntity), and the rat keeps its demo
// semantics (forced chase, no strikes) via mobOverrides in adoptExistingMobs.

func combatRespawn() {
	combatMu.Lock()
	combatHP = dummyMaxHP
	combatDead = false
	combatMu.Unlock()
	if d, ok := dummyData(); ok {
		worldcore.Broadcast(pkt(PacketSpawn, d))
	}
	log.Printf("combat: boss respawned full HP=%d", dummyMaxHP)
}

// startCombat launches the party brain (once): warrior auto 1200ms + Sunder
// 5s, archer auto 1600ms + Volley 6s, mage auto 2000ms + Storm 7s, support
// heal 6s + buff 12s, rat AI 500ms. Auto cadences resolve via
// combatAutoRate (weapon attackRate where items.json carries one, else the
// class defaults above). Broadcasts are no-ops with no subscribers.
func startCombat() {
	combatOnce.Do(func() {
		combatMu.Lock()
		combatHP = dummyMaxHP
		combatLastSkill = make(map[string]time.Time)
		combatBotHP = make(map[string]int, len(combatBotOrder))
		for _, b := range combatBotOrder {
			combatBotHP[b] = combatBotMaxHP[b]
		}
		combatMu.Unlock()
		// M9: the leash-demo rat lives in the engine now (adoptExistingMobs at
		// boot); no per-instance reset or 500ms AI ticker here.
		go func() {
			warAutoT := time.NewTicker(combatAutoRate(combatBotInstance))
			defer warAutoT.Stop()
			skillT := time.NewTicker(combatSkillMs * time.Millisecond)
			defer skillT.Stop()
			archAutoT := time.NewTicker(combatAutoRate(combatArcherInstance))
			defer archAutoT.Stop()
			volleyT := time.NewTicker(combatArcherSkillMs * time.Millisecond)
			defer volleyT.Stop()
			mageAutoT := time.NewTicker(combatAutoRate(combatMageInstance))
			defer mageAutoT.Stop()
			stormT := time.NewTicker(combatMageSkillMs * time.Millisecond)
			defer stormT.Stop()
			healT := time.NewTicker(combatHealMs * time.Millisecond)
			defer healT.Stop()
			buffT := time.NewTicker(combatBuffMs * time.Millisecond)
			defer buffT.Stop()
			for {
				select {
				case <-warAutoT.C:
					combatAuto()
				case <-skillT.C:
					combatSkillTick()
				case <-archAutoT.C:
					archerAuto()
				case <-volleyT.C:
					volleySkillTick()
				case <-mageAutoT.C:
					mageAuto()
				case <-stormT.C:
					stormSkillTick()
				case <-healT.C:
					combatHeal()
				case <-buffT.C:
					combatBuffTick()
				}
			}
		}()
	})
}

// spawnFrames returns Spawn frames: guest p2, every resource, the 232-entity
// showcase grid (156 m-show-* Mobs + 76 n-show-* NPCs, Type Mob=3 / NPC=1 so
// the client resolves sprites mobs/<key> / npcs/<key> via the entities.ts
// prefix rule), and 2 paperdoll demo players. Levels/HP are flat defaults
// (mobs + NPCs 1/50). NPCs idle statically; mobs never roam so the formation
// holds for screenshots. Resource State reflects the LIVE depleted state
// (reconnects during a depletion window see the stump, not a phantom tree).
func spawnFrames() [][]any {
	if combatMode {
		// COMBAT: hero arrives via Welcome; 4 bots + Boss Spawns only.
		return combatSpawns()
	}
	if cleanMode {
		// CLEAN: hero arrives via Welcome; exactly one adventurer Spawn
		// plus the Equipment Batch (total 2 players, zero overlays).
		return [][]any{
			pkt(PacketSpawn, cleanAdventurer()),
			cleanEquipmentBatch(),
		}
	}
	gx, gy := 102, 98
	if testMode {
		gx, gy = 101, 96
	}
	// Guest player is level 1.
	guestLevel := 1
	guestMaxHP := meta.HeroMaxHPForLevel(guestLevel)
	other := EntityData{
		Instance:      "p2",
		Type:          EntityPlayer,
		Key:           "base",
		Name:          "guest",
		X:             gx,
		Y:             gy,
		Orientation:   intp(OrientationDown),
		Level:         intp(guestLevel),
		HitPoints:     intp(guestMaxHP),
		MaxHitPoints:  intp(guestMaxHP),
		MovementSpeed: intp(220),
		AttackRange:   intp(1),
	}
	rat := EntityData{
		Instance:      "m1",
		Type:          EntityMob,
		Key:           "rat",
		Name:          "Rat",
		X:             104,
		Y:             104,
		Orientation:   intp(OrientationDown),
		Level:         intp(1),
		HitPoints:     intp(30),
		MaxHitPoints:  intp(30),
		MovementSpeed: intp(220),
		AttackRange:   intp(1),
	}
	frames := [][]any{
		pkt(PacketSpawn, other),
	}
	if !testMode {
		frames = append(frames, pkt(PacketSpawn, rat))
	} else {
		for i, key := range showMobs {
			x, y := showPos(i)
			frames = append(frames, pkt(PacketSpawn, EntityData{
				Instance:      fmt.Sprintf("m-show-%d", i+1),
				Type:          EntityMob,
				Key:           key,
				Name:          key,
				X:             x,
				Y:             y,
				Orientation:   intp(OrientationDown),
				Level:         intp(1),
				HitPoints:     intp(50),
				MaxHitPoints:  intp(50),
				MovementSpeed: intp(150),
				AttackRange:   intp(1),
			}))
		}
		for i, key := range showNPCs {
			x, y := showPos(len(showMobs) + i)
			frames = append(frames, pkt(PacketSpawn, EntityData{
				Instance:      fmt.Sprintf("n-show-%d", i+1),
				Type:          EntityNPC,
				Key:           key,
				Name:          key,
				X:             x,
				Y:             y,
				Orientation:   intp(OrientationDown),
				Level:         intp(1),
				HitPoints:     intp(50),
				MaxHitPoints:  intp(50),
				MovementSpeed: intp(150),
				AttackRange:   intp(1),
			}))
		}
		for _, p := range demoPlayers() {
			p := p
			frames = append(frames, pkt(PacketSpawn, p))
		}
	}
	resMu.Lock()
	defer resMu.Unlock()
	// Marker resources ride resourceSpawns (same gather/exhaust/respawn
	// path) but stay out of the Ready burst: legacy demo entries only,
	// markers are discovered region-scoped via List + Who (TS parity).
	for _, res := range legacyResourceSpawns() {
		res := res
		if st, ok := resources[res.Instance]; ok && st.depleted {
			res.State = intp(ResourceStateDepleted)
		} else {
			res.State = intp(ResourceStateDefault)
		}
		frames = append(frames, pkt(PacketSpawn, res))
	}
	return frames
}

// Static vs entity trees: STATIC map trees are colliding tiles (a layer id in
// world.json `collisions`, flagged c:true by buildTile above). The client
// blocks those itself: grid defaults to 1, loadRegionTileData clears only on
// !tile.c (map.ts:169-172), and handleRequestPath refuses colliding targets
// (player/handler.ts:56). ENTITY resources (our demo line) instead stand on a
// WALKABLE tile (c:false); nothing in the tile grid blocks them. The real
// server blocks them in map.isColliding via the entity grid — hasEntityAt +
// isResource (map.ts:246-254) — and enforces it in setPosition/verifyCollision
// (player.ts:1579-1580,693-716: teleport back + cheat score). `high` is NOT a
// collision source: the server never consults it for `c`, it only drives the
// foreground render layer (canopy drawn over the player). The stub mirrors
// this split: tiles carry c, entities are tracked below.

// resourceEntities are the blocking entity-grid occupants: instance -> x,y.
// Only RESOURCES block (server isColliding checks entity.isResource());
// players/mobs are Characters and never block movement. Derived from
// resourceSpawns at init so tiles and entities can never drift apart.
var resourceEntities = func() map[string][2]int {
	m := make(map[string][2]int, len(resourceSpawns))
	for _, o := range resourceSpawns {
		m[o.Instance] = [2]int{o.X, o.Y}
	}
	return m
}()

// tileBlocked mirrors server map.isCollisionIndex + isOutOfBounds
// (map.ts:170,201-208): OOB or empty data blocks; otherwise any layer whose
// unflipped id is in `collisions` blocks. Objects are also treated as
// blocking here to match the c flag buildTile emits (client refuses those
// requests itself anyway). Dynamic per-player remap applies in blockedForPlayer;
// doors trigger on Step landing (handleDoorStep); noclip stays admin-only.
// In test mode the same real-terrain rule applies on top of the cloned base,
// with overlays: pond water always blocks, forced-grass tiles (demo resources, showcase
// grid, demos) always walk.
func tileBlocked(x, y int) bool {
	if testMode && !cleanMode && !combatMode {
		if x < 0 || y < 0 {
			return true
		}
		if isTestWater(x, y) {
			return true
		}
		if isTestGrass(x, y) {
			return false
		}
	}
	loadWorld()
	return world.IsBlocked(x, y)
}

// resourceAt returns the instance occupying (x,y), if it is a resource.
func resourceAt(x, y int) string {
	for inst, pos := range resourceEntities {
		if pos[0] == x && pos[1] == y {
			return inst
		}
	}
	return ""
}

// blocked reports whether (x,y) rejects movement: static collision or a
// resource-entity occupant (server map.isColliding:246-254).
func blocked(x, y int) bool {
	return tileBlocked(x, y) || resourceAt(x, y) != "" || chestItemsAt(x, y)
}

// session tracks one connection's player grid pos (from Welcome spawn,
// updated by Started/Step/Stop reports) for teleport-back on reject, plus
// the last resource target (Request/Started/Follow/Entity carry
// targetInstance; Step does not) so Step can apply the same gather-approach
// exception as Request. LastStep + CheatScore implement the M2 speed
// anticheat (movementSpeed ms/tile, 2-tile grace, teleport-back, >15
// disconnect — cf. player.ts handleMovementRequest/Step + handler.ts:809).
// The session type lives in internal/net (gnet.Session, D2a); the root
// addresses it through the embedded Conn record.

// stopPlayer mirrors server stopMovement/teleport-back: halt the client
// entity (connection.ts:461-463 entity.stop()) and snap it to the last
// valid tile (Teleport [12,{instance,x,y}], connection.ts:478-501),
// plus the authoritative List.Positions reply (regions.ts
// sendEntityPositions) so the client resyncs on mismatch.
func stopPlayer(c *playerConn) {
	s := &c.Sess
	_ = gnet.Send(c.Conn, pktOp(PacketMovement, MovementStop, serverMovement{Instance: c.Instance}))
	_ = gnet.Send(c.Conn, pkt(PacketTeleport, teleportData{Instance: c.Instance, X: s.PlayerX, Y: s.PlayerY}))
	_ = gnet.Send(c.Conn, pktOp(PacketList, ListPositions, map[string]any{
		"positions": map[string]any{c.Instance: map[string]any{"x": s.PlayerX, "y": s.PlayerY}},
	}))
}

// List opcodes (Opcodes.List in opcodes.ts): Spawns0 Positions1.
const (
	ListSpawns    = 0
	ListPositions = 1
)

// targetsResource reports whether any of the given target instances is the
// resource occupying (x,y). Empty occupant never matches, so static
// collisions are always rejected even with no target set.
func targetsResource(x, y int, targets ...string) bool {
	return worldcore.TargetsOccupant(resourceAt(x, y), targets...)
}

// rejectLocked records one cheat/collision strike: teleport-back + Positions
// reply; over 15 disconnects the conn (handler.ts cheatScore gate).
// Returns true when the caller should drop the connection.
//
// Enhanced with TS parity (player.ts:662-671):
//   - bypassAntiCheat flag: skip increment during teleports
//   - combat bypass: skip increment during active combat
func rejectLocked(c *playerConn, reason string) bool {
	// Teleport bypass flag (player.ts:663).
	if c.Sess.BypassAntiCheat {
		return false
	}

	// Combat bypass (player.ts:664): skip cheat score during active combat.
	if inCombat(c.Instance) {
		return false
	}

	c.Sess.CheatScore++
	n := c.Sess.CheatScore
	stopPlayer(c)
	worldcore.UpdateRegion(c, c.Sess.PlayerX, c.Sess.PlayerY)
	log.Printf("anticheat: %s instance=%s score=%d", reason, c.Instance, n)
	if n > 15 {
		log.Printf("anticheat: disconnecting %s (score %d > 15)", c.Instance, n)
		return true
	}
	return false
}

// checkSpeed enforces max tiles/sec vs movementSpeed with the main.go
// semantics verbatim: 220ms/tile default, first step after idle (>2s)
// always passes, 5% margin per tile (verifyMovement), sliding lastStep
// window advanced even on reject. Returns true when the step is too fast
// (caller rejects).
//
// Enhanced with TS parity (player.ts:726-746):
//   - latency compensation: subtract client latency from step timing
//   - region change exemption: 1.5s grace after region change
//   - door exemption: skip speed check on door tiles
//   - dynamic speed calculation: boots, running, hot sauce, freezing, cheater
func checkSpeed(c *playerConn, tiles int) bool {
	s := &c.Sess

	// Calculate dynamic movement speed (TS getMovementSpeed parity).
	mods := worldcore.SpeedModifiers{
		Override:   -1, // use default unless admin override
		IsCheater:  isCheater(c.Username),
		HasRunning: abilities.HasStatusEffect(c.Instance, fxRunning),
		HasHotSauce: abilities.HasStatusEffect(c.Instance, fxHotSauce),
		HasFreezing: abilities.HasStatusEffect(c.Instance, fxFreezing),
		HasSnowPotion: abilities.HasStatusEffect(c.Instance, fxSnowPotion),
	}

	// Admin /ms override.
	if override := movementSpeedOverride(c.Username); override > 0 {
		mods.Override = override
	}

	// Boots movement modifier from equipment.
	if bootMods := bootsMovementModifier(c.Username); bootMods > 0 && bootMods != 1.0 {
		mods.BootsModifier = bootMods
	}

	// Calculate and sync speed if changed.
	newSpeed := worldcore.CalculateMovementSpeed(mods)
	if s.MovementSpeed != newSpeed {
		s.MovementSpeed = newSpeed
		// Sync speed to other players in region (TS setMovementSpeed parity).
		syncMovementSpeed(c, newSpeed)
	}

	// Region change grace (1.5s).
	regionGrace := time.Duration(0)
	if !s.LastRegionChange.IsZero() {
		elapsed := time.Since(s.LastRegionChange)
		if elapsed < 1500*time.Millisecond {
			regionGrace = 1500*time.Millisecond - elapsed
		}
	}

	// Door exemption.
	isDoor := entity.IsDoor(s.PlayerX, s.PlayerY)

	// Latency: client-reported from movement packet timestamps (TS
	// player.ts:726-746 parity). Updated by handleMovement on each packet
	// that carries a Timestamp field; defaults to 0 until the first
	// timestamped packet arrives.
	latencyMs := s.LatencyMs

	st := worldcore.SpeedState{MovementSpeed: s.MovementSpeed, LastStep: s.LastStep}
	violation := worldcore.CheckSpeed(&st, tiles, time.Now(), latencyMs, regionGrace, isDoor)
	s.MovementSpeed, s.LastStep = st.MovementSpeed, st.LastStep
	return violation
}

// isCheater reports whether the player has the Cheater rank (TS isCheater parity).
func isCheater(username string) bool {
	st := playerStateFor(username)
	return st != nil && st.Rank == 6 // Modules.Ranks.Cheater = 6
}

// inCombat reports whether the player is in active combat (TS combat.started parity).
func inCombat(instance string) bool {
	return econVitals{}.InCombat(instance)
}

// movementSpeedOverride returns the admin /ms override for a player (0 = none).
func movementSpeedOverride(username string) int {
	return cmdMovementSpeed(username)
}

// bootsMovementModifier returns the boots equipment movement modifier
// (TS player.ts getMovementSpeed: boots.movementModifier). Reads the
// equipped boots slot from the player state and looks up the item's
// movementModifier in the items.json catalogue. Returns 1.0 (no modifier)
// when no boots are equipped or the item has no modifier.
func bootsMovementModifier(username string) float64 {
	st := playerStateFor(username)
	if st == nil || EquipmentBoots >= len(st.Equip) {
		return 1.0
	}
	key := st.Equip[EquipmentBoots].Key
	if key == "" {
		return 1.0
	}
	info := controller.ItemInfoFor(key)
	if info == nil || info.MovementModifier <= 0 || info.MovementModifier == 1.0 {
		return 1.0
	}
	return info.MovementModifier
}

// syncMovementSpeed broadcasts the player's movement speed to nearby regions
// (TS setMovementSpeed parity: MovementPacket with Speed opcode).
func syncMovementSpeed(c *playerConn, speed int) {
	// Movement opcode 2 = Speed (Opcodes.Movement.Speed).
	_ = gnet.Send(c.Conn, pktOp(PacketMovement, 2, map[string]any{
		"instance":      c.Instance,
		"movementSpeed": speed,
	}))
}

// markRegionChange records a region change for the 1.5s speed exemption.
func markRegionChange(c *playerConn) {
	c.Sess.LastRegionChange = time.Now()
}

// withTeleportBypass runs fn with the anti-cheat bypass flag armed (TS
// bypassAntiCheat parity: teleport sets the flag, then clears it).
func withTeleportBypass(c *playerConn, fn func()) {
	c.Sess.BypassAntiCheat = true
	defer func() { c.Sess.BypassAntiCheat = false }()
	fn()
}

// movementBlocked mirrors the player.ts:1122 gate (isStunned() ||
// teleporting). isStunned() is status.has(Modules.Effects.Stun): weapon-stun
// procs land Effects.Stun (= fxStun 4, combat_procs applyHitStatus) and the
// /toggle debug path lands the same ID (CmdEffectStun 4) in the abilities
// tracker, so HasStatusEffect covers both. The teleport half mirrors
// character.teleport's 500ms window via markTeleported (set on every
// server-side teleport funnel). TS note: player.teleport/setPosition never
// arm teleporting for players (player.ts:1579 setPosition's third parameter
// is `forced`, not withTeleport — only mobs arm it, mob.ts:306), so the TS
// gate is near-dead code for players; the Go flag arms it on server
// teleports instead, which is the behavior the gate exists for.
func movementBlocked(c *playerConn) bool {
	if c == nil {
		return false
	}
	if abilities.HasStatusEffect(c.Instance, fxStun) {
		return true
	}
	return c.isTeleporting()
}

// isTeleporting reports the teleport grace window (sessMu-guarded: read on
// the conn goroutine, cleared on a timer goroutine).
func (c *playerConn) isTeleporting() bool {
	if c == nil {
		return false
	}
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	return c.teleporting
}

// markTeleported arms the teleporting flag, cleared 500ms later
// (character.teleport setTimeout parity). Called by every server-side
// teleport funnel (teleport, minigameTeleport, worldApplyTeleport). Respawn is
// excluded: it delivers a Respawn packet (not Teleport) that already resets
// client pathing, matching TS player.respawn (player.teleport, no flag).
func (c *playerConn) markTeleported() {
	if c == nil {
		return
	}
	c.sessMu.Lock()
	c.teleporting = true
	c.sessMu.Unlock()
	time.AfterFunc(500*time.Millisecond, func() {
		c.sessMu.Lock()
		c.teleporting = false
		c.sessMu.Unlock()
	})
}

// clearMovementState mirrors the player.ts movement clears: the container
// gate (canAccessContainer=false, :1125), the open loot bag
// (activeLootBag=”, :1126), the crafting interface
// (activeCraftingInterface=-1, :1127) and the talk cursor (resetTalk,
// handleMovementStep:1224). Trade sessions are untouched — the crafting
// Iface lives beside, not inside, trade state. resetTalk clears
// talkNPC/talkIndex, the cursor shared by NPC talk and the world sign
// TalkWith paging path (world_wire worldSignTalk operates on the same
// fields via withTalk).
func clearMovementState(c *playerConn) {
	clearContainerAccess(c)
	entity.ClearBagOpener(c.Instance)
	controller.ClearCraftingIface(c)
	c.resetTalk()
}

// handleMovement enforces collisions the client grid cannot: resource-entity
// tiles (walkable c:false, e.g. demo oak) plus a backstop for static collisions.
// Request (the client already refuses static targets itself, handler.ts:56):
// reject only when the destination is blocked AND the player is not
// targeting the occupying resource (targeted approaches path adjacent via
// ignores, handler.ts:83). Step applies the SAME exception (chop approach):
// the client already walked locally, so an illegal next tile gets Stop +
// Teleport-back like verifyCollision (player.ts:693-716) — except a step
// onto the currently-targeted resource tile, which Request also allows.
// Request far jumps (>2 tiles, noclip) and too-fast Steps (speed check)
// bump cheatScore with teleport-back; >15 disconnects. Anything else silent.
func handleMovement(c *playerConn, mv clientMovement) bool {
	s := &c.Sess
	if mv.Opcode == nil {
		return false
	}

	// Client-reported latency (TS player.ts:726-746 parity): the client
	// includes performance.now() in each movement packet. We compute
	// one-way latency as server_now_ms - client_timestamp. Since the
	// client and server clocks may have different epochs, we sanity-check
	// the result: values >10s are treated as clock-skew and ignored.
	if mv.Timestamp != nil && *mv.Timestamp > 0 {
		raw := int(time.Now().UnixMilli()) - *mv.Timestamp
		if raw >= 0 && raw < 10000 {
			s.LatencyMs = raw
		}
	}

	if mv.TargetInstance != "" {
		s.Target = mv.TargetInstance
	}
	// player.ts:1119-1127 order (Request) + Step:1199-1224: target-clear
	// above, then the stun/teleport halt, then the container/bag/crafting/
	// talk clears — on every move request, before the door/combat
	// early-returns and verify. Request returns after the halt; Step falls
	// through (TS has no return after the Step stop) and still clears.
	if op := *mv.Opcode; op == MovementRequest || op == MovementStep {
		if movementBlocked(c) {
			// Halt: Stop + teleport-back + Positions, same frames as
			// the rejectLocked path (no cheatScore: TS does not
			// increment on the Request block).
			stopPlayer(c)
			worldcore.UpdateRegion(c, s.PlayerX, s.PlayerY)
			if op == MovementRequest {
				return false
			}
		}
		clearMovementState(c)
	} else {
		// M6: any movement closes the store UI and revokes bank access
		// (stores.ts storeOpen=none + player.ts canAccessContainer=false
		// on move). Request/Step clear it via clearMovementState above.
		clearContainerAccess(c)
	}
	switch *mv.Opcode {
	case MovementRequest:
		if mv.RequestX == nil || mv.RequestY == nil {
			return false
		}
		dx := abs(*mv.RequestX - s.PlayerX)
		dy := abs(*mv.RequestY - s.PlayerY)
		if worldcore.JumpTooFar(dx, dy) && !noclipAllowed(c.Username) {
			// Noclip jump (player.ts handleMovementRequest diff>2). m13:
			// player.noclip bypasses the jump check (movement.ts noclip).
			return rejectLocked(c, fmt.Sprintf("noclip request %d,%d->%d,%d", s.PlayerX, s.PlayerY, *mv.RequestX, *mv.RequestY))
		}
		if checkSpeed(c, dx+dy) && !noclipAllowed(c.Username) {
			return rejectLocked(c, "speed request")
		}
		if blockedForPlayer(c, *mv.RequestX, *mv.RequestY) && !targetsResource(*mv.RequestX, *mv.RequestY, mv.TargetInstance) && !noclipAllowed(c.Username) {
			stopPlayer(c)
			worldcore.UpdateRegion(c, s.PlayerX, s.PlayerY)
		}
	case MovementStarted:
		if mv.PlayerX != nil && mv.PlayerY != nil {
			dx := abs(*mv.PlayerX - s.PlayerX)
			dy := abs(*mv.PlayerY - s.PlayerY)
			if worldcore.JumpTooFar(dx, dy) {
				// Started mismatch: silent resync only (no cheatScore).
				stopPlayer(c)
				worldcore.UpdateRegion(c, s.PlayerX, s.PlayerY)
				return false
			}
			s.PlayerX, s.PlayerY = *mv.PlayerX, *mv.PlayerY
			worldcore.SetEntityPos(c.Instance, s.PlayerX, s.PlayerY)
			worldcore.UpdateRegion(c, s.PlayerX, s.PlayerY)
			trackPos(c)
		}
	case MovementStep:
		if mv.NextGridX != nil && mv.NextGridY != nil {
			dx := abs(*mv.NextGridX - s.PlayerX)
			dy := abs(*mv.NextGridY - s.PlayerY)
			if dx+dy > 0 && checkSpeed(c, dx+dy) {
				return rejectLocked(c, "speed step")
			}
		}
		if mv.PlayerX != nil && mv.PlayerY != nil {
			s.PlayerX, s.PlayerY = *mv.PlayerX, *mv.PlayerY
			worldcore.SetEntityPos(c.Instance, s.PlayerX, s.PlayerY)
			worldcore.UpdateRegion(c, s.PlayerX, s.PlayerY)
			trackPos(c)
			minigamePositionUpdate(c)  // M8: lobby area enter/exit callbacks
			mobOnPlayerMoved(c)     // M9: aggro scan on position update (Node detectAggro)
			areaPositionUpdate(c) // M10: detectAreas parity (pvp/overlay/camera/music)
			handleDoorStep(c)      // doors fire on stopping on a door tile (player.ts:1280)
		}
		if mv.NextGridX != nil && mv.NextGridY != nil &&
			blockedForPlayer(c, *mv.NextGridX, *mv.NextGridY) &&
			!targetsResource(*mv.NextGridX, *mv.NextGridY, mv.TargetInstance, s.Target) {
			stopPlayer(c)
			worldcore.UpdateRegion(c, s.PlayerX, s.PlayerY)
		} else if mv.NextGridX != nil && mv.NextGridY != nil {
			// M5: stepping onto a loot tile picks it up.
			pickupAtTile(c, *mv.NextGridX, *mv.NextGridY)
		}
	case MovementFollow:
		// Log-only: a Follow carrying a resource targetInstance is just the
		// client pathing adjacent to the resource (approach). The single
		// gather swing comes from the explicit click-on-arrival Target packet.
		log.Printf("movement follow target=%s", mv.TargetInstance)
	case MovementEntity:
		log.Printf("movement entity target=%s", mv.TargetInstance)
	}
	return false
}

func abs(v int) int { return worldcore.Abs(v) }

// handleTarget accepts the click-to-interact packet the client sends on
// arrival: [14, [opcode, instance, x?, y?]] (player/handler.ts handleStopPathing
// -> getTargetType returns Object(3) for resources). ONLY Object(3) on a known
// resource counts as a gather swing (explicit click-on-arrival): walking to
// the resource (Request/Started/Step/Follow/Entity) is approach only and
// never gathers. The 600ms per-instance debounce stays as a safety net.
func handleTarget(c *playerConn, frame clientFrame) {
	if c == nil || c.Conn == nil || len(frame) < 2 {
		return
	}
	var msg []json.RawMessage
	if err := json.Unmarshal(frame[1], &msg); err != nil || len(msg) < 2 {
		return
	}
	var opcode int
	var instance string
	if err := json.Unmarshal(msg[0], &opcode); err != nil {
		return
	}
	if err := json.Unmarshal(msg[1], &instance); err != nil {
		return
	}
	log.Printf("target opcode=%d instance=%s", opcode, instance)
	// M5: Target on a loot entity picks it up (in addition to Step).
	if isLoot(instance) {
		// Bags additionally emit the Open frame (lootbag.open parity):
		// the stock client never Targets bags (getTargetType -> None), so
		// this only fires for scripted clients — take-all stays because
		// the combat harness requires it. A denied bag (someone else's)
		// returns early with the notify and never reaches take-all.
		if entity.IsBag(instance) {
			l, ok := entity.FindLoot(instance)
			if !ok {
				return
			}
			if lootBagOwnerDenied(c, l.Owner) {
				return
			}
			sendLootBagOpen(c, instance)
		}
		pickup(c, instance)
		return
	}
	// M6: Target Talk(0) on an NPC -> store open / bank / talk text.
	if opcode == TargetTalk && isNPCInstance(instance) {
		handleNPCTarget(c, instance)
		return
	}
	// M10: Target Talk(0) on a chest entity -> openChest (player/incoming.ts
	// handleTarget Talk branch: isChest() -> chest.openChest(player)).
	if opcode == TargetTalk && !isNPCInstance(instance) {
		if chest := chestFor(instance); chest != nil {
			openChest(c, chest)
			return
		}
		// World: Target Talk on a sign position -> Bubble text (sign.talk).
		if worldSignTalk(c, instance) {
			return
		}
	}
	if opcode == TargetObject && isResourceInstance(instance) {
		hitResource(c.Instance, instance)
	}
}

// isNPCInstance reports whether the instance resolves to an npcs.json NPC
// (showcase n-show-N keys map positionally onto showNPCs).
func isNPCInstance(instance string) bool {
	return resolveNPCKey(nil, instance) != ""
}

// handleCombatReq routes one hero swing at a killable mob (M5); anything
// else stays log-only (combat frames never gather).
func handleCombatReq(c *playerConn, frame clientFrame) {
	var data json.RawMessage
	switch {
	case len(frame) >= 3:
		data = frame[2]
	case len(frame) >= 2:
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(frame[1], &probe); err != nil {
			return
		}
		data = frame[1]
	default:
		return
	}
	var cd struct {
		Instance string `json:"instance"`
		Target   string `json:"target"`
	}
	if err := json.Unmarshal(data, &cd); err != nil {
		return
	}
	log.Printf("combat instance=%s target=%s", cd.Instance, cd.Target)
	// M9: any engine-registered mob is attackable; the legacy BossDummy
	// path stays for the COMBAT party scene.
	if mobFor(cd.Target) != nil || cd.Target == combatDummyInstance {
		handlePlayerAttack(c, cd.Target)
		petMirrorSwing(c, cd.Target) // pet same-target swing (no-op with no pet)
	}
}

// handleAnimationReq is log-only: animation echoes never gather (only Target
// Object counts as a gather swing).
func handleAnimationReq(frame clientFrame) {
	if len(frame) < 2 {
		return
	}
	var ad struct {
		Instance         string `json:"instance"`
		ResourceInstance string `json:"resourceInstance"`
	}
	if err := json.Unmarshal(frame[1], &ad); err != nil {
		return
	}
	log.Printf("animation instance=%s resourceInstance=%s", ad.Instance, ad.ResourceInstance)
}

// resourceState tracks the hit/shake/depleted/respawn cycle for ONE resource
// instance (any of Tree/Rock/FishSpot/Foraging). Real server flow
// (resourceskill.ts interact loop, SKILL_LOOP=1000ms): each loop tick sends S
// Animation{instance:player, action:Attack, resourceInstance} (client shakes
// the resource + plays the gather sound, connection.ts handleAnimation) via
// player.sendToRegion (resourceskill.ts:104-110); each tick rolls
// canExhaustResource and on success awards item + XP, then shouldDeplete()
// depletes (always, except fishing spots which use a 1/10 random depletion).
// deplete() fires onStateChange -> Regions push of S Resource{instance,
// state:Depleted} (entities.ts:212-220, resource.ts:36-48) — both REGION
// broadcasts, never unicast. Respawn re-sends state Default the same way.
// Stub mapping (documented divergences): one C Target Object(3) click = one
// loop tick (no 1s auto-loop; the client only sends Target on click); the
// canExhaust roll runs per swing with stub skill/tool levels (M5 adds real
// Skills/inventory — XP is a log hook only); fishing spots deplete on first
// success like all types (no 1/10 randomDepletion). Respawn per instance from
// the table respawnTime (ms) with 15s fallback (RESOURCE_RESPAWN is 30s
// upstream; M4_RESPAWN_MS env overrides all for fast tests). lastHit
// implements the per-instance 600ms debounce as a safety net against
// duplicate Target arrivals for one physical click.
const (
	resourceRespawnFallback = 15 * time.Second
	resourceDebounce        = 600 * time.Millisecond
)

type resourceState struct {
	swings   int
	depleted bool
	timer    *time.Timer
	lastHit  time.Time
}

var (
	resMu     sync.Mutex
	resources = func() map[string]*resourceState {
		m := make(map[string]*resourceState, len(resourceSpawns))
		for _, o := range resourceSpawns {
			m[o.Instance] = &resourceState{}
		}
		return m
	}()
)

// isResourceInstance reports whether id is a known harvestable resource.
func isResourceInstance(id string) bool {
	resMu.Lock()
	defer resMu.Unlock()
	_, ok := resources[id]
	return ok
}

// playerSkillLevel returns the attacker's real gathering skill level from
// their playerState (resourceskill.ts `level`; gates resource.data.levelRequirement
// > level with a notify). Falls back to 1 when the player is offline or the
// skill is untrained (matching TS Skill default). Env M4_SKILL_<NAME> overrides
// one skill, M4_SKILL overrides all (test/debug hook).
func playerSkillLevel(attackerInstance, skill string) int {
	// Env overrides for testing (preserve the M4 debug knobs).
	// Skill name is uppercased for the env var: M4_SKILL_MINING, M4_SKILL_LUMBERJACKING, etc.
	skillUpper := strings.ToUpper(skill)
	if v := os.Getenv("M4_SKILL_" + skillUpper); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	if v := os.Getenv("M4_SKILL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	// Look up the player's real skill level.
	c, ok := worldcore.Find[*playerConn](attackerInstance)
	if !ok || c == nil {
		return 1
	}
	st := playerStateFor(c.Username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	skillID, found := map[string]int{
		"lumberjacking": player.SkillLumberjacking,
		"mining":        player.SkillMining,
		"fishing":       player.SkillFishing,
		"foraging":      player.SkillForaging,
	}[skill]
	if !found {
		return 1
	}
	if sd, ok := st.Skills[skillID]; ok && sd != nil {
		return sd.Level
	}
	return 1 // untrained skill defaults to level 1
}

// playerToolLevel returns the tier of the attacker's equipped tool for the
// given gathering skill (weapon.lumberjacking/mining/fishing from items.json).
// Returns 0 when no tool is equipped or the weapon is not a tool for that
// skill (triggers the INVALID_WEAPON deny in hitResource). Env M4_TOOL_<SKILL>
// overrides (test/debug hook).
func playerToolLevel(attackerInstance, skill string) int {
	// Env override for testing (skill name uppercased: M4_TOOL_MINING, etc.).
	skillUpper := strings.ToUpper(skill)
	if v := os.Getenv("M4_TOOL_" + skillUpper); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	// Look up the player's equipped weapon and its tool tier.
	c, ok := worldcore.Find[*playerConn](attackerInstance)
	if !ok || c == nil {
		return 1 // fallback: assume basic tool when offline
	}
	w := heroEquipSlot(c.Username, EquipmentWeapon)
	if w.Key == "" {
		return 0 // no weapon equipped
	}
	return toolTier(w.Key, skill)
}

// resourceRespawnDelay resolves the respawn timer for one table entry:
// M4_RESPAWN_MS env wins (fast tests), then the entry respawnTime (ms),
// then the 15s fallback.
func resourceRespawnDelay(info *resourceInfo) time.Duration {
	if v := os.Getenv("M4_RESPAWN_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	if info != nil && info.RespawnTime > 0 {
		return time.Duration(info.RespawnTime) * time.Millisecond
	}
	return resourceRespawnFallback
}

// canExhaustResource mirrors resourceskill.ts canExhaustResource verbatim:
// probability = difficulty - weaponLevel*skillLevel, clamped to >= 2, success
// iff randomInt(0, probability) == 2 (Utils.randomInt is inclusive, so
// P = 1/(probability+1)). Foraging overrides to always true (foraging.ts).
func canExhaustResource(skill string, weaponLevel, skillLevel int, info *resourceInfo) bool {
	if skill == "foraging" {
		return true
	}
	probability := info.Difficulty - weaponLevel*skillLevel
	if probability < 2 {
		probability = 2
	}
	return rand.Intn(probability+1) == 2
}

// Connection + registry state moved to packages (D2a): the net Hub owns
// subs/sockets/writeMu/accept-gate, the world Registry owns the
// entities/players maps. The root keeps no transport globals.

// playerConn is the per-connection record (M2): the transport core embeds
// *gnet.Conn (socket, identity, session, outbox, interest set — owned by
// internal/net) plus the game session fields owned by the root slices.
type playerConn struct {
	*gnet.Conn

	// sessMu guards the M6 store/bank/NPC-talk session fields below. The
	// conn goroutine writes them (controller ClearAccess/OpenStore path,
	// bank/talk updates, movement clear) while the 20s store ticker reads
	// storeOpen off-goroutine (econPeers.WithStoreOpen); every access —
	// including the EconomyConn seam in m6.go — goes through sessMu so
	// -race stays clean under e2e/m6 load.
	sessMu sync.RWMutex
	// M6 store/bank/NPC-talk session state (stores.ts/handler.ts parity).
	storeOpen          string // key of the currently open store ("" = none)
	canAccessContainer bool   // banker-granted bank access (cleared on move)
	talkNPC            string // last plain-NPC key talked to (talkIndex reset)
	talkIndex          int    // current npc.talk() index for talkNPC
	// Teleport grace (character.teleport parity): armed by every
	// server-side teleport funnel, cleared 500ms later via markTeleported
	// (the setTimeout(() => teleporting=false, 500) parity). While armed,
	// movement Requests halt like a stun (player.ts:1122). sessMu-guarded
	// like the M6 fields above: the conn goroutine reads it on movement
	// while the timer goroutine clears it.
	teleporting bool

	// Loiter tracking (player.ts loiter/isLoiteringThreshold parity): last
	// authoritative region id + ms epoch of the last region change,
	// stamped by markLoiterRegion from the UpdateRegion hook (loiter.go).
	// sessMu-guarded: the conn goroutine writes on region updates while
	// the engine loiter sweep reads. loiterInit false = no baseline yet
	// (first sighting stamps, mirroring entity.region -1).
	loiterInit   bool
	loiterRegion int
	loiterSince  int64

	// Region streaming (TS regions.ts handle/sendRegion parity): tracks
	// which region IDs this connection has already received static tile
	// data for. On every region change, newly visible surrounding regions
	// not in this set are built and sent as a Map packet, then added.
	// Seeded with the 9 spawn regions at login; reset on teleport.
	regionsLoaded map[int]bool

	// M7 chat session state (player.chat parity).
	rank int        // Modules.Ranks value (seeded for e2e only)
	chat *chatState // rate limiter + global cooldown + rank cache

	// M8 minigame session state (player.minigame/team/coursing* parity).
	mgGame   string // "coursing"|"teamwar" when playing (player.minigame)
	mgTeam   int    // Team enum value for the active game
	mgScore  int    // coursingScore mirror (score packets + persistence)
	mgTarget string // coursingTarget (pointer entity)

	// Connection lifecycle gates (conn goroutine only — no lock needed):
	// handshakeDone tracks a successful C Handshake so Login without a
	// prior Handshake is rejected; loggedIn rejects a second Login
	// identity switch on the same socket.
	handshakeDone bool
	loggedIn      bool
}

// Entity is the central registry record (M2) alias: canonical owner is the
// world Store (worldcore.Entry).
type Entity = worldcore.Entry

// setEntityPos upserts the registry position for an instance.
// Registry + region-interest primitives moved to packages (D2a):
// worldcore.SetEntityPos/EntityPos/UpdateRegion/ClientInterested/TileRegion
// (internal/world Registry) and gnet.RegionScoped/FrameInstance
// (internal/net). The old root funcs (setEntityPos/entityPos/
// updateClientRegion/clientInterested/regionScoped/frameInstance) are gone;
// call sites use the package APIs directly.

// Unicast + fan-out mechanics moved to packages (D2a): gnet.Send/SendDirect
// (internal/net Hub, TX logging + enqueue + immediate writes) and
// worldcore.Broadcast (internal/world Registry, region routing). The old
// root funcs (enqueueTo/enqueueGlobal/sendDirect/send/broadcast) are gone;
// call sites use the package APIs directly.

// startTickLoop launches the central 20Hz flush loop (one ticker per
// process): every FlushInterval each conn's queued frames flush as a single
// bulk write (5s write deadline; dead conns dropped + Despawn broadcast).
// Subsystem ticks run through the internal/world Engine in frozen order
// (abilities -> regen -> pets -> events -> loiter); the Engine only sequences
// the existing entry points owned by the frozen *_wire.go files (plus the
// loiter sweep in loiter.go).
var tickOnce sync.Once

// tickEngine sequences the per-flush subsystem ticks (E9b orchestrator seam;
// same functions, same order as the inline calls it replaces).
var tickEngine = &worldcore.Engine{Subs: []worldcore.Subsystem{
	{Name: "abilities", Tick: abStatusTick}, // DoT ticks -> Points, expiries -> EffectRemove
	{Name: "regen", Tick: combatRegenTick},  // passive +1/7s regen (heroes + engine mobs)
	{Name: "pets", Tick: petTick},           // pet follow steps / teleports (no-op with no pets)
	{Name: "events", Tick: worldEventTick},  // event rotation -> global notices (no-op when none due)
	{Name: "loiter", Tick: loiterTick},      // loitering XP + cheatScore forgiveness every ~19.2s (throttled)
}}

func startTickLoop() {
	tickOnce.Do(func() {
		go func() {
			t := time.NewTicker(worldcore.FlushInterval)
			defer t.Stop()
			for range t.C {
				tickEngine.Tick()
				for _, c := range gnet.Flush(worldcore.Conns()) {
					worldcore.RemoveClient(c.WS)
				}
			}
		}()
	})
}

// Disconnect fanout moved to the world Registry (D2a): worldcore.RemoveClient
// runs the registry cleanup + Despawn broadcast + the hooks registered here
// (same order as the old root removeClient) + close + log line. Register at
// boot only (main() calls registerDisconnectHooks before serving).
func registerDisconnectHooks() {
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// M8: leave the minigame (disconnect() kicks to lobby position).
		minigameDisconnect(c)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// M9: drop HP state + release any mob targeting this player.
		mobPlayerLeave(c)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// M10: drop per-player area state (pvp/overlay/camera/song/freezing).
		forgetAreaPlayer(c.Instance)
		// Plateau: drop the tracked plateauLevel.
		plateauForget(c.Instance)
		// Dynmap: drop the last-pushed map signature.
		dynMapForget(c.Instance)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// Abilities: drop mana/target/fx state + freeze-tracker keys.
		abForgetPlayer(c)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// Pets: despawn the companion (disconnect removePet parity).
		petForgetPlayer(c)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// World: drop per-conn lamp state.
		worldForgetPlayer(c)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// Social: hub unregister + friends flush + offline fanout (all three).
		socOnDisconnect(c)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// R2: forget the refresh-banner session record (next login re-arms).
		bannerForget(c.Username)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// Trade: notify the open peer with Trade Close + clear both sides
		// (player.ts disconnect trade.close() parity; no-op without a session).
		disconnectTrade(c)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// Statistics: accumulate session time before the persist flush.
		statsRecordDisconnect(c.Username)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// M5: synchronous persist on disconnect (plus the 10s dirty flush).
		savePlayerSync(c.Username)
	})
	worldcore.OnDisconnect(func(v any) {
		c, ok := v.(*playerConn)
		if !ok || c == nil {
			return
		}
		// M11: quest/achievement rows persist on disconnect (same path).
		persistQuests(c.Username)
	})
}

// hitResource registers one gather swing on the given resource instance:
// level gate (deny + log, like the INVALID_LEVEL notify), tool gate (deny +
// log, like the INVALID_WEAPON notify; foraging needs no tool), then enqueue
// S Animation (client shake + gather sound) and roll canExhaustResource — on
// success log the XP hook (M5 awards real Skill XP) and enqueue S
// Resource{state:Depleted} (exhausted frame) + start its own respawn timer ->
// enqueue S Resource{state:Default}. Enqueue (not direct write) matches
// resourceskill sendToRegion + entities onStateChange Regions push; the tick
// loop flushes. Unknown or depleted instances are ignored (logged); the
// per-instance 600ms debounce stays as a safety net; each instance
// depletes/respawns independently. ONLY handleTarget (TargetObject) calls
// this — walking to the resource (Request/Step/Follow/Entity) never gathers,
// and Combat/Animation echoes never gather either.
func hitResource(attacker, instance string) {
	loadResources()
	resMu.Lock()
	st, ok := resources[instance]
	resMu.Unlock()
	if !ok {
		return
	}
	var desc *ResourceEntityData
	for i := range resourceSpawns {
		if resourceSpawns[i].Instance == instance {
			desc = &resourceSpawns[i]
			break
		}
	}
	if desc == nil {
		return
	}
	table, skill := resourceKind(desc.Type)
	info := resourceTables[table][desc.Key]
	if info == nil {
		log.Printf("resource %s (%s) has no table entry, swing ignored", instance, desc.Key)
		return
	}
	skillLevel := playerSkillLevel(attacker, skill)
	if info.LevelRequirement > skillLevel {
		log.Printf("resource %s denied: %s level %d < required %d (key %s)",
			instance, skill, skillLevel, info.LevelRequirement, desc.Key)
		return
	}
	toolLevel := 0
	if skill != "foraging" {
		toolLevel = playerToolLevel(attacker, skill)
		if toolLevel <= 0 {
			log.Printf("resource %s denied: missing %s tool for %s (key %s)",
				instance, skill, desc.Key, desc.Key)
			return
		}
	}
	resMu.Lock()
	if st.depleted {
		log.Printf("resource %s swing ignored (depleted)", instance)
		resMu.Unlock()
		return
	}
	now := time.Now()
	if !st.lastHit.IsZero() && now.Sub(st.lastHit) < resourceDebounce {
		log.Printf("resource %s swing debounced (%v since last hit)", instance, now.Sub(st.lastHit))
		resMu.Unlock()
		return
	}
	st.lastHit = now
	st.swings++
	swings := st.swings
	resMu.Unlock()

	worldcore.Broadcast(pkt(PacketAnimation, animationData{
		Instance:         attacker,
		Action:           ActionAttack,
		ResourceInstance: instance,
	}))

	if !canExhaustResource(skill, toolLevel, skillLevel, info) {
		log.Printf("resource %s swung (%s sound), no exhaust (swing %d, key %s)",
			instance, skill, swings, desc.Key)
		return
	}
	log.Printf("resource %s exhausted: +%d %s xp (M5 hook, key %s item %s)",
		instance, info.Experience, skill, desc.Key, info.Item)
	// M6 (resourceskill.ts:114-118 order): the table item lands in the
	// inventory BEFORE the skill XP — a full inventory would swallow the XP.
	if ci, _ := worldcore.Find[*playerConn](attacker); ci != nil && info.Item != "" {
		yield := 1
		if worldHarvestDouble(skill) {
			yield = 2 // world: lumberjacking/mining double-yield events
		}
		idx := addItem(ci.Username, info.Item, yield)
		_ = gnet.Send(ci.Conn, pktOp(PacketContainer, ContainerAdd, containerData{
			Type: ContainerTypeInventory,
			Slot: &slotData{Index: idx, Key: info.Item, Count: yield, Enchantments: map[string]any{}},
		}))
		markDirty(ci.Username)
	}
	// M5: table experience lands on the real gathering skill.
	gatherXP(attacker, skill, info.Experience)
	// Statistics: successful exhausts count toward gather milestones
	// (resourceskill.ts:121 handleSkill parity — after item + XP land).
	attackerConn, _ := worldcore.Find[*playerConn](attacker)
	statsHandleSkill(attackerConn, skill)
	// M11: quest resource stages fire on exhaust (quest.ts resourceCallback
	// from resourceskill.ts:131 — after the item + XP land).
	questResource(attackerConn, skill, desc.Key)
	resMu.Lock()
	st.depleted = true
	delay := resourceRespawnDelay(info)
	resMu.Unlock()
	worldcore.Broadcast(pkt(PacketResource, resourceData{Instance: instance, State: ResourceStateDepleted}))
	log.Printf("resource %s depleted -> exhausted frame, respawn in %v", instance, delay)
	resMu.Lock()
	st.timer = time.AfterFunc(delay, func() {
		resMu.Lock()
		st.swings = 0
		st.depleted = false
		st.timer = nil
		resMu.Unlock()
		worldcore.Broadcast(pkt(PacketResource, resourceData{Instance: instance, State: ResourceStateDefault}))
		log.Printf("resource %s respawned (state 0)", instance)
	})
	resMu.Unlock()
}

// clientFrame is a generic C->S frame: [packetId, data?] (socket.ts send).
type clientFrame []json.RawMessage

// handleList answers a C->S List request (packet 6, no payload used —
// incoming.ts routes it to updateEntityList): S List Spawns lists every
// entity instance in the requester's regions, S List Positions carries the
// authoritative grid pos of each Character-like entity there (regions.ts
// sendEntities/sendEntityPositions). The client diffs Spawns vs its spawned
// set and asks for the missing ones via Who.
func handleList(c *playerConn) {
	if c == nil || c.Conn == nil {
		return
	}
	regions := c.Conn.Regions()
	regionSet := make(map[int]bool, len(regions))
	for _, r := range regions {
		regionSet[r] = true
	}
	var ids []string
	positions := make(map[string]any)
	for _, e := range worldcore.EntitySnapshot() {
		if regionSet[worldcore.TileRegion(e.X, e.Y)] {
			ids = append(ids, e.Instance)
			positions[e.Instance] = map[string]any{"x": e.X, "y": e.Y}
		}
	}
	if ids == nil {
		ids = []string{}
	}
	_ = gnet.Send(c.Conn, pktOp(PacketList, ListSpawns, map[string]any{"entities": ids}))
	_ = gnet.Send(c.Conn, pktOp(PacketList, ListPositions, map[string]any{"positions": positions}))
	log.Printf("list reply instance=%s entities=%d", c.Instance, len(ids))
}

// handleWho answers C->S Who (packet 7, [ids]): one S Spawn per known live
// entity (incoming.ts handleWho). Unknown ids are ignored (logged). Replies
// are scoped to the requester's 9-region interest set (List parity): ids
// outside it are skipped so a client cannot enumerate the whole world.
func handleWho(c *playerConn, frame clientFrame) {
	if c == nil || c.Conn == nil || len(frame) < 2 {
		return
	}
	var ids []string
	if err := json.Unmarshal(frame[1], &ids); err != nil {
		log.Printf("who parse fail: %v", err)
		return
	}
	for _, id := range ids {
		payload, ok := spawnPayload(id)
		if !ok {
			log.Printf("who unknown instance=%s", id)
			continue
		}
		if x, y, found := worldcore.EntityPos(id); !found || !worldcore.ClientInterested(c, x, y) {
			log.Printf("who out-of-interest instance=%s", id)
			continue
		}
		_ = gnet.Send(c.Conn, pkt(PacketSpawn, payload))
	}
	log.Printf("who reply instances=%d", len(ids))
}

// handleSyncReq forwards a C->S Sync (packet 10, PlayerData) to the other
// players whose interest includes the sender (other-player equip/appearance
// broadcast, connection.ts handleSync). Unicast echo is skipped. The
// instance is always the sender's own (spoofed instances are overwritten)
// so a client cannot impersonate another entity's Sync.
func handleSyncReq(c *playerConn, frame clientFrame) {
	if c == nil || c.Conn == nil || len(frame) < 2 {
		return
	}
	var data map[string]any
	if err := json.Unmarshal(frame[1], &data); err != nil {
		return
	}
	inst := c.Instance
	data["instance"] = inst
	x, y, found := worldcore.EntityPos(inst)
	if !found {
		x, y = c.Sess.PlayerX, c.Sess.PlayerY
	}
	raw, _ := json.Marshal(data)
	var msg json.RawMessage = raw
	for _, o := range worldcore.AllOf[*playerConn]() {
		if o == c {
			continue
		}
		if worldcore.ClientInterested(o, x, y) {
			if !gnet.TryEnqueue(o.Conn, []any{PacketSync, msg}) {
				o.Conn.BumpDropped()
			}
		}
	}
	log.Printf("sync forward instance=%s", inst)
}

// spawnPayload rebuilds the Spawn payload for a known instance: live resources
// honour depleted state; players echo their Welcome shape at the registry
// pos; statics echo their scenario definition.
func spawnPayload(instance string) (any, bool) {
	x, y, found := worldcore.EntityPos(instance)
	if !found {
		return nil, false
	}
	projMu.Lock()
	if p, ok := projPayloads[instance]; ok {
		projMu.Unlock()
		p := p
		p.X, p.Y = x, y
		return p, true
	}
	projMu.Unlock()
	// M5: live loot entities resolve here for Who.
	if p, ok := lootPayload(instance); ok {
		return p, true
	}
	resMu.Lock()
	_, isRes := resources[instance]
	resMu.Unlock()
	if isRes {
		resMu.Lock()
		depleted := resources[instance].depleted
		resMu.Unlock()
		st := ResourceStateDefault
		if depleted {
			st = ResourceStateDepleted
		}
		for _, o := range resourceSpawns {
			if o.Instance == instance {
				o := o
				o.X, o.Y = x, y
				o.State = intp(st)
				return o, true
			}
		}
	}
	for _, c := range worldcore.AllOf[*playerConn]() {
		if c.Instance == instance {
			ph := welcomePlayer(instance)
			ph.X, ph.Y = x, y
			// M10: Spawn PlayerData.pvp mirrors the live PVP state
			// (player.ts serialize).
			ph.Pvp = pvpState(instance)
			return ph, true
		}
	}
	if d, ok := petPayloadByInstance(instance); ok {
		return d, true
	}
	// Marker NPCs (real-mode world.json `entities` markers): showcase Spawn
	// shape, so NPC talk/store/bank resolve through resolveNPCKey.
	if d, ok := markerNPCPayload(instance); ok {
		return d, true
	}
	// Marker mobs (real-mode): live engine state at the registry pos (scoped
	// to marker instances so m1/m-rat-1/m9test Who paths stay identical).
	if m := markerMobFor(instance); m != nil {
		m.mu.Lock()
		d := m.data()
		m.mu.Unlock()
		if x, y, found := worldcore.EntityPos(instance); found {
			d.X, d.Y = x, y
		}
		return d, true
	}
	if d, ok := staticPayload(instance); ok {
		return d, true
	}
	// Last resort: minimal entity shape so the client can spawn something.
	return EntityData{Instance: instance, Type: EntityMob, Key: "rat", Name: instance, X: x, Y: y}, true
}

// staticPayload returns the scenario Spawn definition for a static instance.
func staticPayload(instance string) (any, bool) {
	for _, f := range spawnFrames() {
		if len(f) < 2 {
			continue
		}
		raw, err := json.Marshal(f[1])
		if err != nil {
			continue
		}
		var probe struct {
			Instance string `json:"instance"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue
		}
		if probe.Instance == instance {
			var v any
			if err := json.Unmarshal(raw, &v); err == nil {
				return v, true
			}
			return f[1], true
		}
	}
	return nil, false
}

// initEntities seeds the central registry with every static spawn (resources,
// showcase grid, demos, guest, bots, dummy, adventurer) so List/Who and
// region routing resolve before any client connects.
func initEntities() {
	loadWorld()
	loadResources()
	gx, gy := 102, 98
	if testMode {
		gx, gy = 101, 96
	}
	worldcore.SetEntityPos("p2", gx, gy)
	if !testMode && !cleanMode && !combatMode {
		worldcore.SetEntityPos("m1", 104, 104)
	}
	for _, o := range resourceSpawns {
		worldcore.SetEntityPos(o.Instance, o.X, o.Y)
	}
	if testMode && !cleanMode && !combatMode {
		for i, key := range showMobs {
			_ = key
			x, y := showPos(i)
			worldcore.SetEntityPos(fmt.Sprintf("m-show-%d", i+1), x, y)
		}
		for i, key := range showNPCs {
			_ = key
			x, y := showPos(len(showMobs) + i)
			worldcore.SetEntityPos(fmt.Sprintf("n-show-%d", i+1), x, y)
		}
		for _, p := range demoPlayers() {
			worldcore.SetEntityPos(p.Instance, p.X, p.Y)
		}
	}
	if cleanMode {
		worldcore.SetEntityPos("p-adv-1", 102, 96)
	}
	if combatMode {
		worldcore.SetEntityPos(combatBotInstance, combatBotX, combatBotY)
		worldcore.SetEntityPos(combatArcherInstance, combatArcherX, combatArcherY)
		worldcore.SetEntityPos(combatMageInstance, combatMageX, combatMageY)
		worldcore.SetEntityPos(combatSupInstance, combatSupX, combatSupY)
		worldcore.SetEntityPos(combatDummyInstance, combatDummyX, combatDummyY)
		worldcore.SetEntityPos(combatRatInstance, combatRatX, combatRatY)
	}
	n := worldcore.EntityCount()
	log.Printf("entity registry seeded: %d statics", n)
}

func handleConn(conn *websocket.Conn) {
	gnet.AddSub(conn)

	// Per-connection player record: random Welcome instance + queued outbox.
	inst := newPlayerInstance()
	c := &playerConn{Conn: gnet.NewConn(conn, inst)}
	worldcore.AddPlayer(conn, c)
	worldcore.SetEntityPos(inst, 100, 96)
	worldcore.UpdateRegion(c, 100, 96)
	defer worldcore.RemoveClient(conn)

	// S Connected [0,null]: client answers with Handshake{gVer}.
	if err := gnet.Send(c.Conn, pkt(PacketConnected, nil)); err != nil {
		log.Printf("write connected: %v", err)
		return
	}

	spawnsSent := false
	sendSpawns := func() {
		if spawnsSent {
			return
		}
		spawnsSent = true
		// Perf guard: 240 Spawn frames in one bulk would be a ~40KB message;
		// split into 3 bulks (same Ready flow, back-to-back sends) and log
		// the total JSON bytes so the client OOM risk stays visible.
		all := spawnFrames()
		total := 0
		for i := 0; i < len(all); i += 80 {
			end := i + 80
			if end > len(all) {
				end = len(all)
			}
			chunk := all[i:end]
			raw, _ := json.Marshal(chunk)
			total += len(raw)
			if err := gnet.SendDirect(conn, chunk...); err != nil {
				log.Printf("write spawns chunk %d: %v", i/80, err)
				return
			}
		}
		log.Printf("spawns sent: %d frames in %d bulks, %d bytes JSON", len(all), (len(all)+79)/80, total)
	}

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			log.Printf("read: %v", err)
			return
		}
		fmt.Printf("RX %s\n", raw)

		// Accept single [id,data] and bulk [[id,data],...] inbound.
		var outer []json.RawMessage
		if err := json.Unmarshal(raw, &outer); err != nil || len(outer) == 0 {
			continue // non-[ messages are ignored, like the client side
		}
		var frames []clientFrame
		var probe int
		if err := json.Unmarshal(outer[0], &probe); err == nil {
			frames = []clientFrame{outer}
		} else if err := json.Unmarshal(raw, &frames); err != nil {
			continue
		}

		for _, frame := range frames {
			if len(frame) == 0 {
				continue
			}
			// Ops limiter: drop inbound frames over the per-conn msg budget.
			if !gnet.AllowMsg(gnet.AddrID(conn)) {
				log.Printf("ops: drop frame over msg budget instance=%s", c.Instance)
				continue
			}
			var id int
			if err := json.Unmarshal(frame[0], &id); err != nil {
				continue
			}

			switch id {
			case PacketHandshake: // C Handshake{gVer} -> S Handshake{type:client} (gVer-gated, R1)
				if gv, ok := gverGatePass(frame); !ok {
					// Version contract failed: notice on an existing opcode
					// (Notification Text + hub redirect payload, no wire
					// change) then close (ban-path parity). GVER_STRICT=0
					// disables the gate for dev. The deferred RemoveClient
					// runs the disconnect fanout (idempotent — no explicit
					// call here).
					sendGVerReject(conn, gv)
					return
				}
				reply := HandshakeData{
					Type:       "client",
					Instance:   c.Instance,
					ServerID:   1,
					ServerTime: time.Now().UnixMilli(),
				}
				if err := gnet.Send(c.Conn, pkt(PacketHandshake, reply)); err != nil {
					log.Printf("write handshake: %v", err)
					return
				}
				c.handshakeDone = true
			case PacketLogin: // C Login (opcode lives inside data) -> Welcome + Map only
				// Login requires a prior successful Handshake (reject
				// handshake-less logins that skip the gVer contract).
				if !c.handshakeDone {
					log.Printf("login rejected instance=%s (no prior handshake)", c.Instance)
					return
				}
				// Double Login would switch identity mid-socket (c.Username
				// overwrite + second Welcome); reject it.
				if c.loggedIn {
					log.Printf("login rejected instance=%s (already logged in as %s)", c.Instance, c.Username)
					return
				}
				var login struct {
					Username  string `json:"username"`
					SeedGold  int    `json:"seedGold,omitempty"`
					SeedArrow int    `json:"seedArrow,omitempty"`
					SeedRank  int    `json:"seedRank,omitempty"`
					SeedPos   []int  `json:"seedPos,omitempty"`
				}
				if len(frame) >= 2 {
					_ = json.Unmarshal(frame[1], &login)
				}
				// M13 login gate BEFORE any login state creation (Node
				// login.go database loader -> connection.reject): banned
				// users get the 'ban' text frame and a close with no
				// pstates/SetEntityPos/dirty/seed side effects.
				if login.Username != "" && checkBan(login.Username) {
					_ = gnet.WriteText(conn, []byte("ban"), 2*time.Second)
					return
				}
				// M5: Welcome from DB when the login username is known, else
				// fresh; Container/Skill batches restore visible state.
				ph, extra := loginWelcome(c, login.Username)
				// M6 e2e hook (TESTMAP only): seedGold tops the account up to N
				// gold, folding the seeded wallet into the single inventory
				// Container Batch (no double-Batch: the welcome Batch is
				// replaced in place when present).
				if login.SeedGold > 0 && testMode && !cleanMode && !combatMode {
					seedGold(c.Username, login.SeedGold)
					seeded := pktOp(PacketContainer, ContainerBatch, containerData{
						Type: ContainerTypeInventory,
						Data: &containerBatch{Slots: invSlots(c.Username)},
					})
					replaced := false
					for i, f := range extra {
						if len(f) == 3 && f[0] == PacketContainer && f[1] == ContainerBatch {
							if cd, ok := f[2].(containerData); ok && cd.Type == ContainerTypeInventory {
								extra[i] = seeded
								replaced = true
								break
							}
						}
					}
					if !replaced {
						extra = append(extra, seeded)
					}
				}
				// M6 equipment e2e hook (TESTMAP only): seedArrow appends a
				// fresh arrow stack so the harness gets a deterministic slot
				// index (arrows are Equipment.Arrows-equippable).
				if login.SeedArrow > 0 && testMode && !cleanMode && !combatMode {
					arrowIdx := seedItem(c.Username, "arrow", login.SeedArrow)
					extra = append(extra, pktOp(PacketContainer, ContainerAdd, containerData{
						Type: ContainerTypeInventory,
						Slot: &slotData{Index: arrowIdx, Key: "arrow", Count: login.SeedArrow, Enchantments: map[string]any{}},
					}))
				}
				// M7 e2e hook (TESTMAP only): seedRank grants a rank so the
				// harness can exercise the mod/admin command tables without
				// persistent account plumbing.
				if login.SeedRank > 0 && testMode && !cleanMode && !combatMode {
					chatStateFor(c).rank = login.SeedRank
					c.rank = login.SeedRank
				}
				// M8 e2e hook (TESTMAP only): seedPos teleports the freshly
				// logged-in player to a tile (usually inside a minigame lobby
				// area) so the harness skips the long walk from spawn.
				// Tracked via trackPos (same helper walked movement uses:
				// st.X/Y + dirty + plateau), so saves persist the seeded
				// tile rather than the stale spawn tile.
				if len(login.SeedPos) == 2 && testMode && !cleanMode && !combatMode {
					x, y := login.SeedPos[0], login.SeedPos[1]
					c.Sess.PlayerX, c.Sess.PlayerY = x, y
					worldcore.SetEntityPos(c.Instance, x, y)
					worldcore.UpdateRegion(c, x, y)
					trackPos(c)
					ph.X, ph.Y = x, y
					extra = append(extra, pkt(PacketTeleport, teleportData{Instance: c.Instance, X: x, Y: y}))
					// M8: the position change may cross a lobby area boundary
					// (onEnter parity for the seeded tile).
					minigamePositionUpdate(c)
					// M10: seed area state (pvp/overlay/camera/song) so the
					// Welcome Spawn carries the right pvp flag.
					areaPositionUpdate(c)
				}
				// M13 mspeed override applies to the session before the first
				// movement check (ban already gated above, pre-state).
				if ms := cmdMovementSpeed(c.Username); ms > 0 {
					c.Sess.MovementSpeed = ms
				}
				frames := append([][]any{pkt(PacketWelcome, ph), buildMapFrame()}, extra...)
				// M11: restore + batch quest/achievement state after the login
				// extras (handler.ts:69-70 onLoaded → handleQuests/
				// handleAchievements Batch frames; loadPlayerState precedent).
				ensureQuestTables()
				loadQuests(c.Username)
				frames = append(frames, questLoginBatches(c.Username)...)
				// Abilities: restore unlocks + queue the Ability Batch
				// (handler.ts onLoaded ability serialize).
				abLoadAbilities(c.Username)
				frames = append(frames, abLoginBatch(c.Username))
				// Social: friends table DDL (boot already ran it; cheap
				// re-ensure like m11), restore + batch the Friends List and
				// the guild Login/Update when guilded, fan presence out.
				socEnsureTables()
				frames = append(frames, socOnLogin(c)...)
				if err := gnet.Send(c.Conn, frames...); err != nil {
					log.Printf("write welcome/map: %v", err)
					return
				}
				c.loggedIn = true
				resetRegionsLoaded(c) // region streaming: seed loaded-set from actual pos
				worldPushLights(c) // world: login region-enter Lamp fan-out
				maybeBannerConn(c) // R2: refresh banner when behind preferred (hub-gated no-op)
			case PacketReady: // C Ready{regionsLoaded,userAgent} -> Spawn* (only here)
				// Dynmap: TS handleReady re-sends the per-player region
				// (updateRegion) so returning progress applies to tiles.
				// No-remap players are signature-clean: no extra frame.
				maybePushDynamicMap(c)
				sendSpawns()
			case PacketList: // C List request -> Spawns + Positions
				handleList(c)
			case PacketWho: // C Who [newIds] -> Spawn each known
				handleWho(c, frame)
			case PacketSync: // C Sync PlayerData -> forward to region neighbours
				handleSyncReq(c, frame)
			case PacketChat: // C Chat [text] -> sanitize, commands, region bubble (M7)
				handleChat(c, frame)
			case PacketMinigame: // C Minigame {m8test|m9test|m10test} -> test hooks (M8-M10 TESTMAP)
				handleMinigameTest(c, frame)
				handleAreaTest(c, frame)
				if len(frame) >= 2 {
					var probe map[string]json.RawMessage
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["m9test"] != nil {
						handleMobTest(c, frame[1])
					}
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["m11test"] != nil {
						handleQuestTest(c, frame[1])
					}
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["m12test"] != nil {
						handleTradeTest(c, frame[1])
					}
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["m13test"] != nil {
						handleCommandTest(c, frame[1])
					}
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["abtest"] != nil {
						abTestHandler(c, frame[1])
					}
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["pettest"] != nil {
						petTestHandler(c, frame[1])
					}
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["socialtest"] != nil {
						socTestHandler(c, frame[1])
					}
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["handofftest"] != nil {
						handoffTestHandler(c, frame[1])
					}
					if err := json.Unmarshal(frame[1], &probe); err == nil && probe["worldtest"] != nil {
						worldTestHandler(c, frame[1])
					}
				}
			case PacketEquipment: // C Equipment {opcode,type} -> Unequip (M6)
				handleEquipment(c, frame)
			case PacketQuest: // C Quest {key} -> accept the start prompt (M11)
				if len(frame) >= 2 {
					handleQuestAccept(c, frame[1])
				}
			case PacketAbility: // C Ability {opcode,key,index} -> use / quickslot
				if len(frame) >= 2 {
					abHandleAbility(c, frame[1])
				}
			case PacketPet: // C Pet {opcode} -> Pickup(0) returns the pet (pets wire)
				petHandlePacket(c, frame)
			case PacketWarp: // C Warp {id} -> menu-driven warp (world wire)
				if len(frame) >= 2 {
					worldHandleWarp(c, frame[1])
				}
			case PacketFriends: // C Friends {opcode,username} -> Add/Remove (social wire)
				if len(frame) >= 2 {
					socHandleFriends(c, frame[1])
				}
			case PacketGuild: // C Guild {opcode,...} -> Create/Join/Leave/List/Chat/... (social wire)
				if len(frame) >= 2 {
					socHandleGuild(c, frame[1])
				}
			case PacketTrade: // C Trade (M12)
				if len(frame) >= 2 {
					handleTrade(c, frame[1])
				}
			case PacketEnchant: // C Enchant Select/Confirm (M12)
				if len(frame) >= 2 {
					handleEnchant(c, frame[1])
				}
			case PacketCrafting: // C Crafting Select/Craft (M12)
				if len(frame) >= 2 {
					handleCrafting(c, frame[1])
				}
			case PacketRespawn: // C Respawn [] -> player.respawn (M9)
				handleMobRespawn(c)
			case PacketMovement: // C [11,{opcode,...}] -> Stop/Teleport on blocked tiles
				if len(frame) < 2 {
					continue
				}
				var mv clientMovement
				if err := json.Unmarshal(frame[1], &mv); err != nil {
					continue
				}
				if disconnect := handleMovement(c, mv); disconnect {
					return
				}
			case PacketTarget: // C Target [opcode, instance] -> gather / loot / NPC talk
				handleTarget(c, frame)
			case PacketStore: // C Store {opcode,key,index,count} -> Buy/Sell/Select (M6)
				handleStore(c, frame)
			case PacketContainer: // C Container {opcode,...} -> bank moves/swap/drop (M6)
				handleContainer(c, frame)
			case PacketCombat: // C Combat {instance,target} -> hero swing on killables
				handleCombatReq(c, frame)
			case PacketExamine: // C Examine [instance] -> description notify (mob/item)
				handleExamineReq(c, frame)
			case PacketLootBag: // C LootBag {Take,index} -> single-stack take
				handleLootBagReq(c, frame)
			case PacketAnimation: // C Animation {resourceInstance} -> chop on that oak
				handleAnimationReq(frame)
			default:
				log.Printf("unknown opcode id=%d instance=%s (dropped)", id, c.Instance)
			}
		}
	}
}

// Boot runs the frozen boot sequence (E9b order, see app.BootOrder):
// persist + registries + schedulers + tick loop + entity seed + showcase +
// combat brains + API/console starts. Called once by app.Run via Steps.
func Boot() {
	initPlayerState()
	startStoreTicker() // M6: stores.json registry + 20s stock refresh
	ensureQuestTables()    // M11: quests/achievements SQLite tables (schema up-front)
	ensureCmdTables()    // M13: mute/ban/jail/noclip flags table (schema up-front)
	abEnsureTables()     // abilities: unlock table (schema up-front)
	socEnsureTables()    // social: guilds + friends tables (schema up-front)
	socLoadGuilds()      // social: rebuild the guild registry from the tables
	worldBoot()          // world: warps registry + globals + event scheduler
	loadMinigames()        // M8: world.json minigame areas + 1s tick engines
	loadAreas()       // M10: world.json camera/music/pvp/overlay/chest/dynamic areas
	injectTestAreas() // M10: TESTMAP synthetic area bands for the e2e
	mobEngine()           // M9: mob AI engine (mobs.json/spawns.json, 500ms tick)
	startTickLoop()
	initEntities()
	// D2a: region geometry + region-enter hook for the world Registry (the
	// world is loaded by initEntities above, so sideLen is final), then the
	// disconnect fanout hooks owned by the Registry.
	worldcore.Configure(sideLen, mapDivisionSize, surroundingRegions, func(v any) {
		if pc, ok := v.(*playerConn); ok {
			streamNewRegions(pc) // region streaming: send newly visible regions
			worldPushLights(pc) // world: region-enter Lamp fan-out (deduped, no-op when none new)
			markLoiterRegion(pc, worldcore.TileRegion(pc.Sess.PlayerX, pc.Sess.PlayerY))
			markRegionChange(pc) // anti-cheat: 1.5s speed exemption after region change
		}
	})
	registerDisconnectHooks()
	startShowcase()
	if combatMode {
		startCombat()
	}
	opsStartAPI()     // API_PORT set => serve REST; unset => off; busy port => log + continue
	opsStartConsole() // TTY stdin only; CONSOLE=0 or piped stdin => off
}

// Handler serves the WS endpoint (accept + handleConn + release) on a
// fresh mux (the old DefaultServeMux registration carried exactly this
// one pattern). GET /healthz reports the drain-lifecycle snapshot
// {state, load, buildId, gVer} (new R1 surface; 503 once DRAINING).
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Live load (PlayerCount), lifecycle state: 200 RUNNING, 503 once
		// DRAINING/SHUTDOWN so balancers stop new sessions.
		st := app.Default.State()
		code := http.StatusOK
		if st != version.StateRunning {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": st, "load": worldcore.PlayerCount(),
			"buildId": version.BuildID, "gVer": version.GVer,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, ok := gnet.Accept(w, r)
		if !ok {
			return
		}
		handleConn(conn)
		gnet.Release(conn)
	})
	return mux
}

// modesLine formats the boot mode log line (flag help included).
func modesLine() string {
	return fmt.Sprintf("cleanMode=%v testMode=%v combatMode=%v (CLEAN/COMBAT env or --clean/--combat; TESTMAP env or --testmap flag, default ON)", cleanMode, testMode, combatMode)
}

// Steps builds the canonical boot driver input for app.Run (called by both
// entry points: the root shim and cmd/server).
func Steps() app.Steps {
	return app.Steps{Init: Boot, Addr: addr(), Modes: modesLine(), Handler: Handler()}
}
