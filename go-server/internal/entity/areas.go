// Area system (M10) for package entity: the game/map/areas/* port
// (camera, music, pvp, overlay, chest and dynamic areas) extracted
// behavior-frozen from the root m10.go adapter.
//
// Per-type behavior ports (areas/impl/* + player.ts updates):
//   - camera:      Camera frame [42, opcode] on enter (lockX/lockY/player)
//     and FreeFlow on exit; change-detected per player.
//   - music:       Music frame [30, null, song] on song change (null stays
//     null — [30,null] is the valid "stop music" frame).
//   - pvp:         PVP frame [37, null, {state}] + IN/NOT_IN_PVP_ZONE
//     notify on state flip; player.pvp feeds Spawn PlayerData.
//   - overlay:     Overlay [41, Set, {image,colour}] on enter, [41, Remove]
//     on exit; colour = rgba(rgb…) or rgba(0,0,0,darkness).
//     type "freezing" areas apply the Freezing status effect
//     (Effect [47, Add, {instance, effect:17}]).
//   - chest:       chest-AREA flow: each mob spawn adopts its containing
//     chest area; clearing the area of mobs spawns a reward
//     chest (Spawn frame, type Chest4 key "chest") at spawnX/Y
//     guarded by the mob respawn delay (Utils.timePassed);
//     the next mob spawn removes the unlooted chest. Opening
//     rolls the item list (key:count:probability, Node
//     Utils.randomInt inclusive) and spawns the drop as an M5
//     loot entity so pickup lands in the inventory.
//   - dynamic:     pairs linked by the `mapping` id (mappedArea /
//     mappedAnimation by id) — the tile-remap endpoint for
//     full-world streaming; parsed + linked here, no per-tile
//     work in the stub (documented divergence).
//
// Chest entity OPEN flow (entities.ts spawnChest onOpen): open despawns the
// chest, rolls one item and spawns it at the chest tile (persistent — no
// blink expiry, matching the isStatic=false spawnItem in Node's chest flow).
// A mimic-flagged chest additionally spawns a 'mimic' mob at the chest tile
// when opened by a player (spawnMob('mimic'), non-respawnable, linked so its
// death re-spawns THIS chest after CHEST_RESPAWN via the KillMob mimic
// hook); the item roll still runs. The chest's own achievement (static
// chests only — the area flow never sets one) finishes for the opener on
// open (entities.ts onOpen finish). The AREA achievement fires at CLEAR time
// (RemoveChestMob, chest.ts onEmpty parity), never on open.
//
// Static chests (world.json areas.chest, singular — e.g. id 304 at 271,731
// with mimic:true) spawn once at boot (SpawnStaticChests) and live outside
// the chest-area mob cycle; area chests (areas.chests, plural) spawn on
// area clear. Both share the Chest type and the open flow. Late joiners
// miss the boot Spawn broadcast (broadcast-only entities, same standing
// divergence as area-reward chests).
//
// Everything transport/world related stays with the root adapter and is
// reached only through the AreaWorld seam (root helpers in parentheses):
//
//	Notify            -> m6Notify (pvp zone flips)
//	SendPVP           -> S->C PVP [37, null, {state}] to self
//	SendOverlaySet    -> S->C Overlay [41, Set, {image,colour}] to self
//	SendOverlayRemove -> S->C Overlay [41, Remove] to self
//	SendCamera        -> S->C Camera [42, opcode] to self
//	SendMusic         -> S->C Music [30, null, song] to self
//	SendEffect        -> S->C Effect [47, opcode, {instance, effect}] to self
//	FreezeApply       -> abFreezeApply (freezing status tracker bridge)
//	FreezeClear       -> abFreezeClear (area exit + disconnect)
//	SetEntityPos      -> setEntityPos (chest entity registry)
//	Despawn           -> S->C Despawn [13] broadcast (chest remove/open)
//	SpawnChestFrame   -> S->C Spawn [5] broadcast (reward chest)
//	SpawnMimic        -> m9SpawnMob('mimic') + S->C Spawn [5] broadcast
//
// The persistent chest-item drop additionally uses ChestLoot
// (m5NearWalkable + m5RegisterLoot + the Item Spawn frame).
package entity

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Opcodes (common/network/opcodes.ts) + frame constants.
// ---------------------------------------------------------------------------

// Opcodes.Overlay: Set0 Remove1 Lamp2 RemoveLamps3 Darkness4.
const (
	// OverlaySet is Opcodes.Overlay.Set.
	OverlaySet = 0
	// OverlayRemove is Opcodes.Overlay.Remove.
	OverlayRemove = 1
)

// Opcodes.Camera: LockX0 LockY1 FreeFlow2 Player3.
const (
	// CameraLockX is Opcodes.Camera.LockX.
	CameraLockX = 0
	// CameraLockY is Opcodes.Camera.LockY.
	CameraLockY = 1
	// CameraFreeFlow is Opcodes.Camera.FreeFlow.
	CameraFreeFlow = 2
	// CameraPlayer is Opcodes.Camera.Player.
	CameraPlayer = 3
)

// EffectFreezing is Modules.Effects.Freezing (area.ts addPlayer). The
// freezing status area applies Effects.Freezing (17).
const EffectFreezing = 17

// ChestRespawnStatic is Modules.Constants.CHEST_RESPAWN (50s chest respawn
// for statically spawned chests). Chest AREAS use the mob respawnDelay as
// the spawn guard (area.ts spawnDelay from the first added mob).
const ChestRespawnStatic = 50 * time.Second

// AreaWorld is the area-system half of the World seam (root helpers in
// parentheses — see the package doc; packet shapes stay frozen in root).
type AreaWorld interface {
	SetEntityPos(instance string, x, y int)
	Despawn(instance string)
	Notify(instance, msg string)
	SendPVP(instance string, state bool)
	SendOverlaySet(instance, image, colour string)
	SendOverlayRemove(instance string)
	SendCamera(instance string, opcode int)
	SendMusic(instance, song string)
	SendEffect(instance string, add bool, effect int)
	FreezeApply(instance string)
	FreezeClear(instance string)
	SpawnChestFrame(c ChestSpawn)
	// SpawnMimic spawns a 'mimic' mob at (x, y), non-respawnable
	// (TS mimic.respawnable = false via the NoRespawn override). Reports
	// the mob instance, or ok=false when the spawn failed (unknown key —
	// the chest link is skipped, TS `if (mimic)` parity).
	SpawnMimic(x, y int) (instance string, ok bool)
	// FinishAchievement finishes an achievement for the player behind
	// instance (quest.Finish path: static-chest open rewards, chest
	// clear rewards, door achievements).
	FinishAchievement(instance, key string)
}

// ChestSpawn is the Spawn-frame descriptor for a reward chest
// (type Chest4, key "chest").
type ChestSpawn struct {
	Instance string
	X, Y     int
}

// ---------------------------------------------------------------------------
// Area model (areas/area.ts + areas/areas.ts base + ProcessedArea fields).
// ---------------------------------------------------------------------------

// Area is one world.json area entry. A single struct carries the union of
// the per-type impl fields exactly like Node's Area base class does.
// JSON tags match world.json (file shape frozen).
type Area struct {
	ID     int    `json:"id"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Type   string `json:"type"` // camera mode / overlay kind
	Song   string `json:"song"` // music areas

	Darkness float64 `json:"darkness"` // overlay
	Fog      string  `json:"fog"`      // overlay fog image
	RGB      string  `json:"rgb"`      // overlay "r,g,b"

	Entities int    `json:"entities"` // chest area: mob count to clear
	Items    string `json:"items"`    // chest area: comma list (key:count:prob)
	SpawnX   int    `json:"spawnX"`   // chest spawn tile
	SpawnY   int    `json:"spawnY"`

	// Mimic flags a mimic chest (world.json areas.chest static entries,
	// e.g. id 304 at 271,731): opening spawns a 'mimic' mob whose death
	// re-spawns the chest (entities.ts spawnChest onOpen). Chest AREAS
	// never set it (chest.ts passes no mimic to spawnChest).
	Mimic bool `json:"mimic"`

	Mapping   int `json:"mapping"`   // dynamic: mapped counterpart area id
	Animation int `json:"animation"` // dynamic: mapped animation area id

	// Quest/Achievement gate dynamic collision remaps (area.ts
	// fulfillsRequirement) and names the chest clear reward (chest.ts
	// onEmpty attacker achievement).
	Quest       string `json:"quest"`
	Achievement string `json:"achievement"`

	Polygon []struct {
		X int `json:"x"`
		Y int `json:"y"`
	} `json:"polygon"`
	Ignore bool `json:"ignore"` // omitted from walk callbacks (area.ts)

	// ---- runtime (not serialized) ----
	mobs       []string      // chest area: instances of mobs inside (area.entities)
	mobMu      sync.Mutex    // guards mobs + chest + lastSpawn
	chest      *Chest        // live chest entity (nil = none)
	lastSpawn  int64         // last chest spawn ms (Utils.timePassed guard)
	spawnDelay time.Duration // taken from the first mob's respawnDelay

	// linked dynamic counterparts (dynamic.ts link())
	mappedArea      *Area
	mappedAnimation *Area
}

// Inside ports Area.contains/inRectangularArea (x <= px < x+width). The
// polygon branch exists in Node for music areas; the ray-cast port covers
// them (InPolygon).
func (a *Area) Inside(x, y int) bool {
	if a == nil || a.Ignore {
		return false // Area.contains: ignorable areas are skipped
	}
	if len(a.Polygon) > 0 {
		return a.InPolygon(x, y)
	}
	return x >= a.X && x < a.X+a.Width && y >= a.Y && y < a.Y+a.Height
}

// InPolygon ports Area.inPolygon (ray cast, same winding, float math like
// Node — integer division would truncate edge crossings).
func (a *Area) InPolygon(x, y int) bool {
	inside := false
	j := len(a.Polygon) - 1
	fx, fy := float64(x), float64(y)
	for i := 0; i < len(a.Polygon); i++ {
		xi, yi := float64(a.Polygon[i].X), float64(a.Polygon[i].Y)
		xj, yj := float64(a.Polygon[j].X), float64(a.Polygon[j].Y)
		if (yi > fy) != (yj > fy) && fx < (xj-xi)*(fy-yi)/(yj-yi)+xi {
			inside = !inside
		}
		j = i
	}
	return inside
}

// IsStatusArea ports Area.isStatusArea: overlay areas of type "freezing".
func (a *Area) IsStatusArea() bool {
	return a.Type == "freezing"
}

// RGBList parses the "r,g,b" string (overlay.ts: split(',').map(Number),
// whitespace-tolerant).
func (a *Area) RGBList() []int {
	if strings.TrimSpace(a.RGB) == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(a.RGB, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ItemsList splits the chest items CSV (chest.ts: rawData.items.split(',')).
func (a *Area) ItemsList() []string {
	if strings.TrimSpace(a.Items) == "" {
		return nil
	}
	parts := strings.Split(a.Items, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// OverlayColour builds the overlay colour string (updateOverlay).
func (a *Area) OverlayColour() string {
	rgb := a.RGBList()
	colour := fmt.Sprintf("rgba(0, 0, 0, %v)", a.Darkness)
	if len(rgb) >= 3 {
		colour = fmt.Sprintf("rgba(%d, %d, %d, %v)", rgb[0], rgb[1], rgb[2], a.Darkness)
	}
	return colour
}

// OverlayImage picks the overlay fog image (updateOverlay: fog || 'blank').
func (a *Area) OverlayImage() string {
	if a.Fog == "" {
		return "blank"
	}
	return a.Fog
}

// LiveChest reports the area's live chest entity (nil = none).
func (a *Area) LiveChest() *Chest {
	a.mobMu.Lock()
	defer a.mobMu.Unlock()
	return a.chest
}

// MappedArea reports the dynamic `mapping` counterpart (dynamic.ts link).
// The link is written once under areasMu in LoadAreas and never mutated
// afterwards, so a lock-free read is safe after load (callers holding
// areasMu read the field directly; see DynamicAt).
func (a *Area) MappedArea() *Area {
	if a == nil {
		return nil
	}
	return a.mappedArea
}

// MappedAnimation reports the dynamic `animation` counterpart.
func (a *Area) MappedAnimation() *Area {
	if a == nil {
		return nil
	}
	return a.mappedAnimation
}

func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// ---------------------------------------------------------------------------
// Registries (areas.ts subclass instances -> one registry per group).
// ---------------------------------------------------------------------------

var (
	areasMu      sync.Mutex
	cameraAreas  []*Area
	musicAreas   []*Area
	pvpAreas     []*Area
	overlayAreas []*Area
	chestAreas   []*Area
	// staticChests holds the world.json areas.chest (singular) static
	// entries — one-shot chests (some flagged mimic) living outside the
	// chest-area mob cycle. Kept apart from chestAreas so mob adoption,
	// the clear flow and the m10test chest echo never see them.
	staticChests []*Area
	dynamicAreas []*Area
	dynamicByID  = map[int]*Area{}
	areasLoaded  bool
	doors        = map[int]*Door{}
	areasWidth   int

	// playerAreas mirrors player.overlayArea/cameraArea/currentSong/pvp —
	// keyed by connection instance (player.ts change-detection fields).
	stateMu     sync.Mutex
	overlayArea = map[string]*Area{}
	cameraArea  = map[string]*Area{}
	songState   = map[string]string{}
	pvpState    = map[string]bool{}
	frozenState = map[string]bool{} // active Freezing effect

	// mimicChests links live mimic mobs to the chest their death re-spawns
	// (mob.chest parity, keyed by mob instance; popped on death).
	mimicMu     sync.Mutex
	mimicChests = map[string]*Chest{}

	chestSeq uint64 // atomic chest-entity sequence (no time-based collisions)
)

// resetAreas clears every registry (tests only; the server loads once).
func resetAreas() {
	areasMu.Lock()
	cameraAreas, musicAreas, pvpAreas, overlayAreas = nil, nil, nil, nil
	chestAreas, dynamicAreas = nil, nil
	staticChests = nil
	dynamicByID = map[int]*Area{}
	doors = map[int]*Door{}
	areasWidth = 0
	areasLoaded = false
	areasMu.Unlock()
	atomic.StoreUint64(&chestSeq, 0)
	stateMu.Lock()
	overlayArea = map[string]*Area{}
	cameraArea = map[string]*Area{}
	songState = map[string]string{}
	pvpState = map[string]bool{}
	frozenState = map[string]bool{}
	stateMu.Unlock()
	mimicMu.Lock()
	mimicChests = map[string]*Chest{}
	mimicMu.Unlock()
}

// Chest is the chest entity spawned by a cleared chest area
// (entities.ts spawnChest -> Chest entity) or once at boot for a static
// world.json areas.chest entry (SpawnStaticChests). Mimic/Achievement ride
// along so the mimic-death hook re-spawns THIS chest unchanged.
type Chest struct {
	Instance    string
	X, Y        int
	Items       []string
	Achievement string
	Mimic       bool
	Area        *Area
}

// LoadAreas parses a world.json document's `areas` groups (Node world.ts
// constructor builds one Areas subclass per group from map.areas). The
// root adapter reads the file (worldPath); a nil/empty document marks the
// registry loaded and returns (world.json absent — same as the old stub).
func LoadAreas(doc []byte) {
	areasMu.Lock()
	defer areasMu.Unlock()
	if areasLoaded {
		return
	}
	areasLoaded = true
	if len(doc) == 0 {
		return
	}
	var parsed struct {
		Width int `json:"width"`
		Areas struct {
			Camera  []Area    `json:"camera"`
			Music   []Area    `json:"music"`
			PVP     []Area    `json:"pvp"`
			Overlay []Area    `json:"overlay"`
			Chests  []Area    `json:"chests"`
			Chest   []Area    `json:"chest"`
			Dynamic []Area    `json:"dynamic"`
			Doors   []rawDoor `json:"doors"`
		} `json:"areas"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		log.Printf("m10: parse areas: %v", err)
		return
	}
	cp := func(src []Area) []*Area {
		out := make([]*Area, 0, len(src))
		for i := range src {
			out = append(out, &src[i])
		}
		return out
	}
	cameraAreas = cp(parsed.Areas.Camera)
	musicAreas = cp(parsed.Areas.Music)
	pvpAreas = cp(parsed.Areas.PVP)
	overlayAreas = cp(parsed.Areas.Overlay)
	chestAreas = cp(parsed.Areas.Chests)
	staticChests = cp(parsed.Areas.Chest)
	dynamicAreas = cp(parsed.Areas.Dynamic)

	// map.ts loadDoors parity: link areas.doors by destination id, keyed by
	// entry tile index (needs the map width from the same document).
	areasWidth = parsed.Width
	doors = loadDoors(parsed.Areas.Doors, parsed.Width)

	// dynamic.ts link(): map `mapping`/`animation` ids to their areas.
	for _, a := range dynamicAreas {
		dynamicByID[a.ID] = a
	}
	for _, a := range dynamicAreas {
		if a.Mapping != 0 {
			a.mappedArea = dynamicByID[a.Mapping]
		}
		if a.Animation != 0 {
			a.mappedAnimation = dynamicByID[a.Animation]
		}
	}

	log.Printf("m10: areas loaded camera=%d music=%d pvp=%d overlay=%d chests=%d static=%d dynamic=%d doors=%d",
		len(cameraAreas), len(musicAreas), len(pvpAreas), len(overlayAreas), len(chestAreas), len(staticChests), len(dynamicAreas), len(doors))
}

// AreasLoaded reports whether LoadAreas has run (single boot load).
func AreasLoaded() bool {
	areasMu.Lock()
	defer areasMu.Unlock()
	return areasLoaded
}

// InArea ports Areas.inArea over one group: first containing area.
func InArea(group []*Area, x, y int) *Area {
	for _, a := range group {
		if a.Inside(x, y) {
			return a
		}
	}
	return nil
}

// ChestAreas returns a copy of the chest-area group (test/introspection).
func ChestAreas() []*Area {
	areasMu.Lock()
	defer areasMu.Unlock()
	out := make([]*Area, len(chestAreas))
	copy(out, chestAreas)
	return out
}

// FirstChestArea returns the first chest area (the M10TEST chestmob
// harness adopts it), or nil when the group is empty.
func FirstChestArea() *Area {
	areasMu.Lock()
	defer areasMu.Unlock()
	if len(chestAreas) == 0 {
		return nil
	}
	return chestAreas[0]
}

// ChestAreaAt ports Mob.addToChestArea (mob.ts: chestAreas.inArea).
func ChestAreaAt(x, y int) *Area {
	areasMu.Lock()
	defer areasMu.Unlock()
	return InArea(chestAreas, x, y)
}

// AddChestMob ports Area.addEntity (mob.ts addToChestArea). Records the
// mob in the area and (first mob only) adopts its respawn delay as the
// chest spawn guard; a live unlooted chest is removed (chest.ts onSpawn ->
// removeChest).
func AddChestMob(area *Area, instance string, respawnDelay time.Duration, w GameWorld) {
	if area == nil || instance == "" || w == nil {
		return
	}
	area.mobMu.Lock()
	for _, m := range area.mobs {
		if m == instance {
			area.mobMu.Unlock()
			return
		}
	}
	area.mobs = append(area.mobs, instance)
	if area.spawnDelay == 0 {
		area.spawnDelay = respawnDelay
	}
	c := area.chest
	if c != nil {
		area.chest = nil
	}
	area.mobMu.Unlock()
	if c != nil {
		// chest.ts removeChest path for area chests (non-static): despawn.
		w.Despawn(c.Instance)
		log.Printf("m10: chest %s removed (area %d repopulated)", c.Instance, area.ID)
	}
}

// RemoveChestMob ports Area.removeEntity + onEmpty (mob handler death
// path): drop the mob from the area; when the last one leaves, spawn the
// reward chest guarded by the respawn delay (chest.ts spawnChest +
// Utils.timePassed) and award the area achievement to the clearing attacker
// (chest.ts onEmpty attacker achievement finish). attackerInstance is "" for
// killerless clears (no award, TS attacker-undefined parity).
func RemoveChestMob(area *Area, instance, attackerInstance string, w GameWorld) {
	if area == nil || instance == "" {
		return
	}
	area.mobMu.Lock()
	idx := -1
	for i, m := range area.mobs {
		if m == instance {
			idx = i
			break
		}
	}
	if idx < 0 {
		area.mobMu.Unlock()
		return
	}
	area.mobs = append(area.mobs[:idx], area.mobs[idx+1:]...)
	empty := len(area.mobs) == 0
	delay := area.spawnDelay
	if delay <= 0 {
		delay = RespawnDelay // Node fallback: MobDefaults.RESPAWN_DELAY
	}
	canSpawn := empty && time.Now().UnixMilli()-area.lastSpawn >= delay.Milliseconds()
	achievement := area.Achievement
	if canSpawn {
		area.lastSpawn = time.Now().UnixMilli()
	}
	area.mobMu.Unlock()

	if !canSpawn {
		return
	}
	spawnChestEntity(area, w)
	if empty && achievement != "" && attackerInstance != "" {
		w.FinishAchievement(attackerInstance, achievement)
	}
}

// KillHookForMob fires the chest-area death path for a killed mob
// (handler.ts: mob.area?.removeEntity(mob, attacker) -> onEmpty chest
// spawn). Called from KillMob. killerInstance is "" for killerless kills.
func KillHookForMob(mobX, mobY int, mobInstance, killerInstance string, w GameWorld) {
	area := ChestAreaAt(mobX, mobY)
	if area == nil {
		return
	}
	RemoveChestMob(area, mobInstance, killerInstance, w)
}

// spawnChestEntity ports entities.ts spawnChest for the chest-AREA flow:
// Spawn frame (type Chest4, key "chest") + registry entry. The area flow
// passes neither achievement nor mimic (chest.ts spawnChest), so both stay
// zero — area achievements fire at CLEAR (RemoveChestMob), never on open.
// The instance carries an atomic sequence so two clears in the same
// millisecond never collide.
func spawnChestEntity(area *Area, w GameWorld) {
	seq := atomic.AddUint64(&chestSeq, 1)
	inst := fmt.Sprintf("chest-%d-%d", area.ID, seq)
	c := &Chest{Instance: inst, X: area.SpawnX, Y: area.SpawnY, Items: area.ItemsList(), Area: area}

	area.mobMu.Lock()
	area.chest = c
	area.mobMu.Unlock()

	w.SetEntityPos(inst, c.X, c.Y)
	w.SpawnChestFrame(ChestSpawn{Instance: inst, X: c.X, Y: c.Y})
	log.Printf("m10: chest %s spawned at %d,%d (area %d cleared, items=%v)",
		inst, c.X, c.Y, area.ID, c.Items)
}

// StaticChests returns a copy of the static-chest def group (test/introspection).
func StaticChests() []*Area {
	areasMu.Lock()
	defer areasMu.Unlock()
	out := make([]*Area, len(staticChests))
	copy(out, staticChests)
	return out
}

// SpawnStaticChests spawns one live chest per world.json areas.chest entry
// (entities.ts constructor loop: spawnChest(items, x, y, isStatic=true,
// achievement, mimic)). Idempotent: entries with a live chest are skipped,
// so a boot re-run or a mimic-death respawn race never doubles a chest.
// The instance is deterministic per entry id (chest-static-<id>) so a
// scripted client can address it.
func SpawnStaticChests(w GameWorld) {
	areasMu.Lock()
	defs := make([]*Area, len(staticChests))
	copy(defs, staticChests)
	areasMu.Unlock()
	for _, def := range defs {
		def.mobMu.Lock()
		if def.chest != nil {
			def.mobMu.Unlock()
			continue
		}
		c := &Chest{
			Instance:    fmt.Sprintf("chest-static-%d", def.ID),
			X:           def.X,
			Y:           def.Y,
			Items:       def.ItemsList(),
			Achievement: def.Achievement,
			Mimic:       def.Mimic,
			Area:        def,
		}
		def.chest = c
		def.mobMu.Unlock()

		w.SetEntityPos(c.Instance, c.X, c.Y)
		w.SpawnChestFrame(ChestSpawn{Instance: c.Instance, X: c.X, Y: c.Y})
		log.Printf("m10: static chest %s spawned at %d,%d (entry %d, items=%v mimic=%v)",
			c.Instance, c.X, c.Y, def.ID, c.Items, c.Mimic)
	}
}

// ChestFor finds a live chest entity by instance.
func ChestFor(instance string) *Chest {
	areasMu.Lock()
	defer areasMu.Unlock()
	for _, area := range chestAreas {
		if c := area.LiveChest(); c != nil && c.Instance == instance {
			return c
		}
	}
	for _, def := range staticChests {
		if c := def.LiveChest(); c != nil && c.Instance == instance {
			return c
		}
	}
	return nil
}

// ChestAt reports the chest occupying a tile (movement-block check).
func ChestAt(x, y int) bool {
	areasMu.Lock()
	defer areasMu.Unlock()
	for _, area := range chestAreas {
		if c := area.LiveChest(); c != nil && c.X == x && c.Y == y {
			return true
		}
	}
	for _, def := range staticChests {
		if c := def.LiveChest(); c != nil && c.X == x && c.Y == y {
			return true
		}
	}
	return false
}

// LinkMimicChest records the chest a live mimic belongs to (mob.chest
// parity, set in OpenChest when the mimic spawn succeeds).
func LinkMimicChest(mobInstance string, c *Chest) {
	mimicMu.Lock()
	defer mimicMu.Unlock()
	mimicChests[mobInstance] = c
}

// TakeMimicChest pops the chest linked to a dead mimic (nil,false when the
// mob is not a mimic).
func TakeMimicChest(mobInstance string) (*Chest, bool) {
	mimicMu.Lock()
	defer mimicMu.Unlock()
	c, ok := mimicChests[mobInstance]
	if ok {
		delete(mimicChests, mobInstance)
	}
	return c, ok
}

// handleMimicDeath ports the handler.ts death tail `mob.chest?.respawn()`:
// a dead mimic drops from the registry (TS destroy) and its chest re-spawns
// after CHEST_RESPAWN (chest.respawn setTimeout parity), reusing the same
// chest (items/achievement/mimic flag). Non-mimics are a no-op. The slot
// guard (no overwrite of a live chest) only fires in a repopulation race
// the static flow cannot otherwise produce.
func handleMimicDeath(mobInstance string, w GameWorld) {
	c, ok := TakeMimicChest(mobInstance)
	if !ok {
		return
	}
	w.RemoveMob(mobInstance)
	w.AfterDelay(ChestRespawnStatic, func() {
		area := c.Area
		if area == nil {
			return
		}
		area.mobMu.Lock()
		if area.chest != nil {
			area.mobMu.Unlock()
			log.Printf("m10: mimic chest %s respawn suppressed (area %d occupied)", c.Instance, area.ID)
			return
		}
		area.chest = c
		area.mobMu.Unlock()

		w.SetEntityPos(c.Instance, c.X, c.Y)
		w.SpawnChestFrame(ChestSpawn{Instance: c.Instance, X: c.X, Y: c.Y})
		log.Printf("m10: mimic chest %s re-spawned at %d,%d (area %d)", c.Instance, c.X, c.Y, area.ID)
	})
	log.Printf("m10: mimic %s died -> chest %s respawns in %v", mobInstance, c.Instance, ChestRespawnStatic)
}

// OpenChest ports entities.ts spawnChest onOpen (remove -> mimic spawn ->
// item roll -> loot spawn -> opener achievement), in that exact order:
// despawn the chest, spawn the mimic mob when flagged and opened by a
// player (non-respawnable, linked so its death re-spawns this chest), roll
// one entry (key:count:probability, Utils.randomInt inclusive) and spawn
// the item at the chest tile as a persistent M5 loot entity, then finish
// the chest's own achievement for the opener when set (static chests;
// the area flow never sets one — TS area spawnChest passes no achievement).
// The AREA achievement is NOT awarded here: TS awards it in the onEmpty
// clear callback (RemoveChestMob), not on open.
// Reports false (no-op) when the chest was already removed — the double-open
// guard: only the live registry entry may be opened.
func OpenChest(chest *Chest, openerInstance, openerUsername string, w GameWorld) bool {
	if chest == nil || w == nil {
		return false
	}
	if chest.Area != nil {
		// Remove the chest first (onOpen -> this.remove(chest)).
		chest.Area.mobMu.Lock()
		if chest.Area.chest != chest {
			chest.Area.mobMu.Unlock()
			return false
		}
		chest.Area.chest = nil
		// Node's onEmpty fired once already; opening does NOT re-clear mobs —
		// lastSpawn is NOT reset here (matches chest.ts removeChest).
		chest.Area.mobMu.Unlock()
	}

	w.Despawn(chest.Instance)
	log.Printf("m10: %s opened chest %s at %d,%d", openerUsername, chest.Instance, chest.X, chest.Y)

	// Mimic spawn (entities.ts onOpen `if (player && mimic)`): a failed
	// spawn skips the link (TS `if (mimic)`), the item flow still runs.
	if openerUsername != "" && chest.Mimic {
		if inst, ok := w.SpawnMimic(chest.X, chest.Y); ok {
			LinkMimicChest(inst, chest)
			log.Printf("m10: mimic %s spawned at %d,%d (chest %s)", inst, chest.X, chest.Y, chest.Instance)
		} else {
			log.Printf("m10: mimic spawn failed (chest %s at %d,%d)", chest.Instance, chest.X, chest.Y)
		}
	}

	item := RollChestItem(chest.Items)
	if item == nil {
		return true
	}
	// Chest item spawns are persistent (isStatic=false, no blink expiry in
	// the Node chest flow) — spawn via the M5 loot path with no timers.
	lx, ly := w.NearWalkable(chest.X, chest.Y)
	seq := atomic.AddUint64(&chestSeq, 1)
	inst := fmt.Sprintf("chestitem-%d", seq)
	w.RegisterLoot(inst, item.Key, item.Count, lx, ly, openerUsername)
	w.SpawnLootItem(LootItem{Instance: inst, Key: item.Key, Count: item.Count, X: lx, Y: ly})
	log.Printf("m10: chest item %s (x%d) spawned at %d,%d", item.Key, item.Count, lx, ly)

	// Static-chest open reward (entities.ts onOpen finish): skipped with
	// the early return above when no item rolled, exactly like Node.
	if openerUsername != "" && openerInstance != "" && chest.Achievement != "" {
		w.FinishAchievement(openerInstance, chest.Achievement)
	}
	return true
}

// ChestItem is one rolled drop (chest.ts getItem return).
type ChestItem struct {
	Key   string
	Count int
}

// RollChestItem ports Chest.getItem: random entry, "key:count:probability"
// suffix parsing, probability roll with inclusive randomInt(0,100).
func RollChestItem(items []string) *ChestItem {
	if len(items) == 0 {
		return nil
	}
	item := items[rand.Intn(len(items))] // randomInt(0, len-1) inclusive
	count, probability := 1, 100
	// Entries look like "bronzesword" or "gold:25:50" — parse ':' segments.
	if indexOfByte(item, ':') >= 0 {
		segs := splitColon(item)
		item = segs[0]
		if len(segs) > 1 {
			if n, err := strconv.Atoi(segs[1]); err == nil {
				count = n
			}
		}
		if len(segs) > 2 {
			if n, err := strconv.Atoi(segs[2]); err == nil {
				probability = n
			}
		}
	}
	if item == "" {
		return nil
	}
	// Roll: randomInt(0,100) > probability -> no drop.
	if rand.Intn(101) > probability {
		return nil
	}
	return &ChestItem{Key: item, Count: count}
}

func indexOfByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func splitColon(s string) []string {
	parts := strings.Split(s, ":")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// ---------------------------------------------------------------------------
// Per-player area detection (player.ts updatePVP/Overlay/Camera/Music +
// handler.detectAreas). Fired from the movement path + teleports.
// ---------------------------------------------------------------------------

// OnPositionUpdate is the area hook on the movement path (Node
// handleMovement -> detectAreas). Change-detection is per player per group.
func OnPositionUpdate(instance, username string, x, y int, w GameWorld) {
	areasMu.Lock()
	pvpArea := InArea(pvpAreas, x, y)
	overlayArea := InArea(overlayAreas, x, y)
	cameraArea := InArea(cameraAreas, x, y)
	musicArea := InArea(musicAreas, x, y)
	areasMu.Unlock()

	UpdatePVP(instance, username, pvpArea != nil, w)
	UpdateOverlay(instance, username, overlayArea, w)
	UpdateCamera(instance, username, cameraArea, w)
	UpdateMusic(instance, username, musicArea, w)
}

// UpdatePVP ports player.updatePVP: notify + PVP packet on state flip.
func UpdatePVP(instance, username string, inPVP bool, w GameWorld) {
	if w == nil || instance == "" {
		return
	}
	stateMu.Lock()
	if pvpState[instance] == inPVP {
		stateMu.Unlock()
		return
	}
	pvpState[instance] = inPVP
	stateMu.Unlock()

	if !inPVP {
		w.Notify(instance, "misc:NOT_IN_PVP_ZONE")
	} else {
		w.Notify(instance, "misc:IN_PVP_ZONE")
	}
	// PVPPacket serializes [37, undefined, data] -> [37, null, {state}]
	// (opcode element present as null — packet.ts serialize).
	w.SendPVP(instance, inPVP)
	log.Printf("m10: %s pvp=%v", username, inPVP)
}

// UpdateOverlay ports player.updateOverlay: Overlay Set/Remove on area
// change; freezing status areas apply/remove the Freezing effect.
func UpdateOverlay(instance, username string, area *Area, w GameWorld) {
	if w == nil {
		return
	}
	stateMu.Lock()
	if overlayArea[instance] == area {
		stateMu.Unlock()
		return
	}
	prev := overlayArea[instance]
	overlayArea[instance] = area
	stateMu.Unlock()

	if area == nil {
		if prev != nil && prev.IsStatusArea() {
			SetFreezing(instance, false, w)
		}
		w.SendOverlayRemove(instance)
		return
	}

	w.SendOverlaySet(instance, area.OverlayImage(), area.OverlayColour())
	if area.IsStatusArea() {
		SetFreezing(instance, true, w)
	} else if prev != nil && prev.IsStatusArea() {
		SetFreezing(instance, false, w)
	}
	log.Printf("m10: %s overlay area %d (%s)", username, area.ID, area.Type)
}

// SetFreezing applies/removes the Freezing status effect
// (Area.addPlayer/removePlayer -> player.status Effects.Freezing). The
// tracker bridge feeds EFFECT_RATE cold damage through the Points pipeline
// (character.ts handleColdDamage); the visual Effect frames stay here.
func SetFreezing(instance string, on bool, w GameWorld) {
	if w == nil || instance == "" {
		return
	}
	stateMu.Lock()
	if frozenState[instance] == on {
		stateMu.Unlock()
		return
	}
	frozenState[instance] = on
	stateMu.Unlock()

	if on {
		w.FreezeApply(instance)
	} else {
		w.FreezeClear(instance)
	}

	w.SendEffect(instance, on, EffectFreezing)
}

// UpdateCamera ports player.updateCamera: mode opcode on enter, FreeFlow
// on exit (change-detected).
func UpdateCamera(instance, username string, area *Area, w GameWorld) {
	if w == nil || instance == "" {
		return
	}
	stateMu.Lock()
	if cameraArea[instance] == area {
		stateMu.Unlock()
		return
	}
	cameraArea[instance] = area
	stateMu.Unlock()

	if area == nil {
		w.SendCamera(instance, CameraFreeFlow)
		return
	}
	switch area.Type {
	case "lockX":
		w.SendCamera(instance, CameraLockX)
	case "lockY":
		w.SendCamera(instance, CameraLockY)
	case "player":
		w.SendCamera(instance, CameraPlayer)
	}
	log.Printf("m10: %s camera %s (area %d)", username, area.Type, area.ID)
}

// UpdateMusic ports player.updateMusic: Music frame on song change; an
// empty song sends [30,null] explicitly (Node sends newSong=undefined —
// serialized as a null element).
func UpdateMusic(instance, username string, area *Area, w GameWorld) {
	if w == nil || instance == "" {
		return
	}
	song := ""
	if area != nil {
		song = area.Song
	}
	stateMu.Lock()
	if songState[instance] == song {
		stateMu.Unlock()
		return
	}
	songState[instance] = song
	stateMu.Unlock()

	// MusicPacket serializes [30, undefined, song] -> [30, null, song]
	// (opcode element present as null).
	w.SendMusic(instance, song)
	log.Printf("m10: %s music %q", username, song)
}

// PVPState reports the player's current pvp flag (Spawn PlayerData.pvp).
func PVPState(instance string) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	return pvpState[instance]
}

// ForgetPlayer drops per-player area state on disconnect.
func ForgetPlayer(instance string, w GameWorld) {
	if instance == "" {
		return
	}
	stateMu.Lock()
	delete(overlayArea, instance)
	delete(cameraArea, instance)
	delete(songState, instance)
	delete(pvpState, instance)
	delete(frozenState, instance)
	stateMu.Unlock()
	if w == nil {
		return
	}
	w.FreezeClear(instance)
}

// ---------------------------------------------------------------------------
// TESTMAP synthetic areas (the stub world has no real camera/pvp/overlay
// tiles). Injected once when running the default TESTMAP mode so the e2e can
// drive every area type on the synthetic map (mirrors the showcase grid
// convention). Layout (TESTMAP pond at 112..120 x 93..99):
//   music   x98..107  y100..103   song "beach"
//   overlay x110..120 y100..103   dark 0.75
//   pvp     x98..108  y105..108
//   chests  x110..120 y105..107   chest spawn (114,106)
//   chest-mob band x110..120 y100..103 (mobs adopted via m9test spawn)
//   camera  x98..107  y105..108   lockX (world.json has 0 camera areas; the
//                                  type value is exercised for parity)
// ---------------------------------------------------------------------------

// InjectTestAreas ensures the TESTMAP synthetic bands. The mode gate
// (TESTMAP-only, no clean/combat) stays in the root adapter; injection is
// per-group (only groups the loaded world does NOT provide — the real
// world.json carries music/pvp/overlay/chests, so injecting over them
// would shadow the e2e's real-area legs; camera is the group it lacks).
func InjectTestAreas() {
	areasMu.Lock()
	defer areasMu.Unlock()
	// Per-group injection: only groups the loaded world does NOT provide
	// (the real world.json carries music/pvp/overlay/chests — injecting over
	// them would shadow the e2e's real-area legs; camera is the group it
	// lacks, so that band always lands under TESTMAP).
	rect := func(id, x, y, w, h int) *Area {
		return &Area{ID: id, X: x, Y: y, Width: w, Height: h}
	}
	if len(musicAreas) == 0 {
		musicAreas = append(musicAreas, &Area{ID: 9001, X: 98, Y: 100, Width: 10, Height: 4, Song: "beach"})
	}
	if len(overlayAreas) == 0 {
		overlayAreas = append(overlayAreas, &Area{ID: 9002, X: 110, Y: 100, Width: 11, Height: 4, Type: "dark", Darkness: 0.75})
	}
	if len(pvpAreas) == 0 {
		pvpAreas = append(pvpAreas, rect(9003, 98, 105, 11, 4))
	}
	if len(cameraAreas) == 0 {
		cameraAreas = append(cameraAreas, &Area{ID: 9004, X: 98, Y: 105, Width: 10, Height: 4, Type: "lockX"})
	}
	if len(chestAreas) == 0 {
		chest := rect(9005, 110, 105, 11, 3)
		chest.Items = "bronzesword"
		chest.SpawnX, chest.SpawnY = 114, 106
		chestAreas = append(chestAreas, chest)
	}
	log.Printf("m10: TESTMAP synthetic area bands ensured (camera=%d music=%d pvp=%d overlay=%d chests=%d)",
		len(cameraAreas), len(musicAreas), len(pvpAreas), len(overlayAreas), len(chestAreas))
}
