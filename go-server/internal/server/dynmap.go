// Per-player dynamic tile serving (regions.ts buildDynamicTile parity).
//
// DESIGN (why this shape):
//
// TS builds the Map frame PER PLAYER on every sendRegion: static tiles from
// the region cache plus dynamic tiles re-skinned through buildDynamicTile
// (original tile when the player fails the area's quest/achievement gate,
// mapped tile when fulfilled), and re-pushes it on quest finish
// (quests.ts handleProgress -> player.updateRegion), achievement finish
// (achievements.ts finishCallback -> updateRegion) and Ready/login
// (incoming.ts handleReady -> updateRegion).
//
// Go instead builds ONE static frame at boot (mapOnce/mapFrame, always the 9
// spawn regions) and sends it at Login. Rebuilding per player per send would
// discard that cache and risk regressions for the common case, so:
//
//   - The cached static frame stays the base for everyone (TESTMAP/CLEAN/
//     COMBAT overlays unchanged — the variant is built on top of the same
//     mode-selected base via mapBaseData).
//   - Players whose progression fulfills NO dynamic gate in the served scope
//     get the byte-identical cached frame (buildMapFrameFor aliases it —
//     zero regression risk, zero extra bytes).
//   - Players who DO fulfill a gate get a per-player variant: the same base
//     with fulfilled dynamic tiles substituted by their mapped content
//     (same x,y; Data/C/O/Cur read at the mapped tile, optional animation
//     from the mapped-animation tile — buildDynamicTile parity). Variants
//     are cached keyed by a compact progression signature (sorted fulfilled
//     gate keys, e.g. "a:queenant"), so per-player builds are bounded: at
//     most 2^(in-scope gates) entries (on the current map exactly one gate,
//     queenant, intersects the spawn scope), hard-capped at dynCacheCap with
//     a full clear on overflow.
//   - Live updates mirror TS's updateRegion push with the same [4,base64gzip,
//     bufSize] framing (no packet-shape change): maybePushDynamicMap sends
//     the player's current frame only when its signature differs from what
//     that connection last saw. It is called at Ready (covers returning
//     players whose quest finished while offline — TS serves dynamic tiles
//     on the Ready updateRegion) and after every live quest/achievement
//     stage mutation (finish AND reset-revert, so the client never shows
//     stale mapped tiles). The client merges Map frames per tile
//     (client map.ts loadRegionTileData overwrites data/collision/cursor/
//     objects per tile), so a full-frame resend converges to the same end
//     state as TS's dynamic-only diff for already-loaded regions; the resend
//     fires only on signature transitions, so steady-state traffic is
//     unchanged.
//   - Scope note: Go serves the fixed 9 spawn regions (no full-world region
//     streaming on movement — standing divergence). Only dynamic tiles inside
//     that served scope are substituted; gates fulfilled elsewhere change
//     nothing in the served frame and produce no push.
//
// Collision already agrees: blockedForPlayer evaluates movement at the same
// mapped tile (IsBlockedRemapped/DynamicRemap), so served tiles and
// walkability now match per player.
package server

import (
	"log"
	"sort"
	"sync"

	"rpg-world-server/internal/entity"
	gnet "rpg-world-server/internal/net"
)

// dynCacheCap bounds the per-signature frame cache (see the package doc:
// realistic size is 2^(in-scope gates); the cap is a backstop).
const dynCacheCap = 64

var (
	dynMu sync.Mutex
	// dynCache maps mode|progression-signature -> encoded Map frame.
	dynCache = map[string][]any{}
	// dynSent maps connection instance -> signature last pushed to it.
	// Absent means the static frame (Login always sends the static
	// buildMapFrame, whose signature is "").
	dynSent = map[string]string{}
)

// dynStaticFrame sources the shared static frame (variable for tests).
var dynStaticFrame = func() []any { return buildMapFrame() }

// ---------------------------------------------------------------------------
// Dynamic-area view (hermetic seam over entity.Area).
// ---------------------------------------------------------------------------

// dynAreaView is the dynamic-area surface the tile overlay needs. The
// production adapter wraps *entity.Area; tests use fakes, so the remap
// matrix needs no registry or world state.
type dynAreaView interface {
	// EachTile iterates candidate outer tiles (rect bounds ∩ Inside).
	EachTile(fn func(x, y int))
	// GateKey is the effective "q:key"/"a:key" gate (area.ts
	// fulfillsRequirement if/else order: quest first, else achievement).
	GateKey() (string, bool)
	// Fulfills reports the per-player gate (area.ts fulfillsRequirement).
	Fulfills(prog entity.Progression) bool
	// MapTo resolves the mapped tile (area.ts getMappedTile).
	MapTo(x, y int) (mx, my int, ok bool)
	// AnimTo resolves the mapped-animation tile (getMappedAnimationTile).
	AnimTo(x, y int) (ax, ay int, ok bool)
}

// dynAreaAdapter adapts *entity.Area to dynAreaView.
type dynAreaAdapter struct{ a *entity.Area }

func (d dynAreaAdapter) EachTile(fn func(x, y int)) {
	a := d.a
	for y := a.Y; y < a.Y+a.Height; y++ {
		for x := a.X; x < a.X+a.Width; x++ {
			if a.Inside(x, y) {
				fn(x, y)
			}
		}
	}
}

func (d dynAreaAdapter) GateKey() (string, bool) {
	if d.a.Quest != "" {
		return "q:" + d.a.Quest, true
	}
	if d.a.Achievement != "" {
		return "a:" + d.a.Achievement, true
	}
	return "", false
}

func (d dynAreaAdapter) Fulfills(prog entity.Progression) bool {
	return entity.FulfillsRequirement(d.a, prog)
}

func (d dynAreaAdapter) MapTo(x, y int) (int, int, bool) {
	return entity.MappedTile(d.a, x, y)
}

func (d dynAreaAdapter) AnimTo(x, y int) (int, int, bool) {
	return entity.MappedAnimTile(d.a, x, y)
}

// entityDynAreas adapts the linked dynamic registry, skipping counterpart
// sides without their own mapping (region.getDynamicArea parity — only the
// original side carries `mapping`, same filter as entity.DynamicAt).
func entityDynAreas() []dynAreaView {
	raw := entity.DynamicAreas()
	out := make([]dynAreaView, 0, len(raw))
	for _, a := range raw {
		if a == nil || a.MappedArea() == nil {
			continue
		}
		out = append(out, dynAreaAdapter{a: a})
	}
	return out
}

// ---------------------------------------------------------------------------
// Overlay: fulfilled dynamic tiles -> substituted RegionTiles.
// ---------------------------------------------------------------------------

// dynamicOverlayTiles ports regions.ts buildDynamicTile over one player's
// progression: for every in-scope tile of a fulfilled mapping area, the
// substituted tile keeps the ORIGINAL x,y but carries the MAPPED tile's
// content (Data/C/O/Cur from fetch at the mapped coords, index override
// parity: buildTile(x, y, mappedIndex)). The mapped-animation raw data rides
// along when the area links one (client loadRegionTileData parity).
// Unfulfilled tiles are absent (the base keeps serving the original, same as
// TS returning `original`). A mapped tile with no content yields a data:0
// tile (TS buildTile(x, y, index) with empty data parity — no fallback to
// the original). First area wins on overlap (entity.DynamicAt order).
// Returns the overlay plus the compact progression signature (sorted
// fulfilled gate keys of contributing areas; "" when nothing substituted).
func dynamicOverlayTiles(
	areas []dynAreaView,
	prog entity.Progression,
	fetch func(x, y int) (RegionTile, bool),
	inScope func(x, y int) bool,
) (map[[2]int]RegionTile, string) {
	overlay := map[[2]int]RegionTile{}
	gates := map[string]bool{}
	for _, area := range areas {
		if area == nil || !area.Fulfills(prog) {
			continue
		}
		gate, ok := area.GateKey()
		if !ok {
			continue
		}
		area.EachTile(func(x, y int) {
			if !inScope(x, y) {
				return
			}
			pos := [2]int{x, y}
			if _, done := overlay[pos]; done {
				return
			}
			mx, my, ok := area.MapTo(x, y)
			if !ok {
				return
			}
			mt, ok := fetch(mx, my)
			var tile RegionTile
			if !ok {
				tile = RegionTile{X: x, Y: y, Data: 0}
			} else {
				tile = mt
				tile.X, tile.Y = x, y
				if ax, ay, ok := area.AnimTo(x, y); ok {
					if at, ok := fetch(ax, ay); ok {
						tile.Animation = at.Data
					}
				}
			}
			overlay[pos] = tile
			gates[gate] = true
		})
	}
	keys := make([]string, 0, len(gates))
	for g := range gates {
		keys = append(keys, g)
	}
	sort.Strings(keys)
	sig := ""
	for i, g := range keys {
		if i > 0 {
			sig += ","
		}
		sig += g
	}
	return overlay, sig
}

// applyDynamicTiles clones the base region data and substitutes overlay
// tiles by position (replacing the base tile, appending when the base was
// empty there — getTestRegionData.set parity). Pure over its inputs.
func applyDynamicTiles(
	base map[int][]RegionTile,
	overlay map[[2]int]RegionTile,
	ridOf func(x, y int) int,
) map[int][]RegionTile {
	out := make(map[int][]RegionTile, len(base))
	posIdx := make(map[int]map[[2]int]int, len(base))
	for rid, tiles := range base {
		cp := make([]RegionTile, len(tiles))
		copy(cp, tiles)
		out[rid] = cp
		m := make(map[[2]int]int, len(tiles))
		for i, t := range tiles {
			m[[2]int{t.X, t.Y}] = i
		}
		posIdx[rid] = m
	}
	for pos, tile := range overlay {
		rid := ridOf(pos[0], pos[1])
		m := posIdx[rid]
		if m == nil {
			m = make(map[[2]int]int)
			posIdx[rid] = m
		}
		if i, ok := m[pos]; ok {
			tiles := out[rid]
			tiles[i] = tile
			out[rid] = tiles
			continue
		}
		out[rid] = append(out[rid], tile)
		m[pos] = len(out[rid]) - 1
	}
	return out
}

// ---------------------------------------------------------------------------
// Per-player frames.
// ---------------------------------------------------------------------------

// servedScope returns the in-scope predicate for the served spawn regions
// (region of 100,100 plus surrounding regions — the same 9 regions the
// static frame covers). The region set resolves lazily so overlay paths
// that never see a tile never touch the world store.
func servedScope() func(x, y int) bool {
	var once sync.Once
	var served map[int]bool
	return func(x, y int) bool {
		once.Do(func() {
			loadWorld()
			region := (100/mapDivisionSize)*sideLen + (100 / mapDivisionSize)
			served = make(map[int]bool)
			for _, rid := range surroundingRegions(region) {
				served[rid] = true
			}
		})
		return served[(y/mapDivisionSize)*sideLen+(x/mapDivisionSize)]
	}
}

// dynCacheKey namespaces the signature by boot mode (each mode has its own
// base: TESTMAP overlays vs pure terrain).
func dynCacheKey(sig string) string {
	mode := "real"
	switch {
	case cleanMode:
		mode = "clean"
	case combatMode:
		mode = "combat"
	case testMode:
		mode = "test"
	}
	return mode + "|" + sig
}

// buildMapFrameFor returns the Map frame for one player: the byte-identical
// cached static frame when their progression remaps nothing in the served
// scope (remapped=false), else the cached-per-signature variant
// (remapped=true). questProg reuses the collision gate's progression read
// (plateau.go), so served tiles and IsBlockedRemapped always agree.
func buildMapFrameFor(username string) (frame []any, sig string, remapped bool) {
	overlay, sig := dynamicOverlayTiles(
		entityDynAreas(),
		questProg{username: username},
		func(mx, my int) (RegionTile, bool) { return buildTile(mx, my) },
		servedScope(),
	)
	if len(overlay) == 0 {
		return dynStaticFrame(), sig, false
	}
	key := dynCacheKey(sig)
	dynMu.Lock()
	if f, ok := dynCache[key]; ok {
		dynMu.Unlock()
		return f, sig, true
	}
	dynMu.Unlock()
	data := applyDynamicTiles(
		mapBaseData(),
		overlay,
		func(x, y int) int { return (y/mapDivisionSize)*sideLen + (x / mapDivisionSize) },
	)
	frame = encodeMapFrame(data)
	log.Printf("dynmap: built variant sig=%q regions=%d", sig, len(data))
	dynMu.Lock()
	if len(dynCache) >= dynCacheCap {
		dynCache = map[string][]any{}
	}
	dynCache[key] = frame
	dynMu.Unlock()
	return frame, sig, true
}

// mapBaseData is the mode-selected static base both the boot frame and the
// per-player variants share (TESTMAP/CLEAN/COMBAT overlays unchanged).
func mapBaseData() map[int][]RegionTile {
	if cleanMode || combatMode {
		return getRegionData(100, 100)
	}
	if testMode {
		return getTestRegionData()
	}
	return getRegionData(100, 100)
}

// maybePushDynamicMap is the TS player.updateRegion parity for the served
// map: push the player's current Map frame when its progression signature
// differs from what this connection last saw (finish, reset-revert, or
// Ready with pre-existing progress). No-remap players never differ from the
// Login static frame, so they receive nothing extra. Nil-safe (quest paths
// with no live conn are no-ops, like the progress frames they accompany).
func maybePushDynamicMap(c *playerConn) {
	// c.Conn first: Username promotes through the embedded *gnet.Conn,
	// so it must only be read with a live transport.
	if c == nil || c.Conn == nil || c.Username == "" {
		return
	}
	frame, sig, remapped := buildMapFrameFor(c.Username)
	dynMu.Lock()
	if dynSent[c.Instance] == sig {
		dynMu.Unlock()
		return
	}
	dynSent[c.Instance] = sig
	dynMu.Unlock()
	_ = gnet.Send(c.Conn, frame)
	if remapped {
		log.Printf("dynmap: pushed variant to %s instance=%s sig=%q", c.Username, c.Instance, sig)
	} else {
		log.Printf("dynmap: reverted to static for %s instance=%s", c.Username, c.Instance)
	}
}

// dynMapForget drops the per-connection last-sent signature on disconnect
// (plateauLevels parity — instances are never reused, this just bounds the
// map over churn).
func dynMapForget(instance string) {
	dynMu.Lock()
	delete(dynSent, instance)
	dynMu.Unlock()
}

// ---------------------------------------------------------------------------
// Full-world region streaming (TS regions.ts handle/sendRegion parity).
// ---------------------------------------------------------------------------

// streamNewRegions sends tile data for any surrounding regions the player
// hasn't received yet (TS regions.ts sendRegion: on region change, compute
// new 9-region interest set, send regions not in player.regionsLoaded).
// Returns true if any new regions were sent. The caller must have updated
// c.Sess.PlayerX/PlayerY before calling.
func streamNewRegions(c *playerConn) bool {
	if c == nil || c.Conn == nil {
		return false
	}
	loadWorld()
	px, py := c.Sess.PlayerX, c.Sess.PlayerY
	region := (py/mapDivisionSize)*sideLen + (px / mapDivisionSize)
	surrounding := surroundingRegions(region)

	// Find regions the player hasn't loaded yet.
	var newRegions []int
	for _, rid := range surrounding {
		if !c.regionsLoaded[rid] {
			newRegions = append(newRegions, rid)
		}
	}
	if len(newRegions) == 0 {
		return false
	}

	// Build region data for the new regions only.
	data := make(map[int][]RegionTile)
	for _, rid := range newRegions {
		x0 := (rid % sideLen) * mapDivisionSize
		y0 := (rid / sideLen) * mapDivisionSize
		var tiles []RegionTile
		for y := y0; y < y0+mapDivisionSize; y++ {
			for x := x0; x < x0+mapDivisionSize; x++ {
				if t, ok := buildTile(x, y); ok {
					tiles = append(tiles, t)
				}
			}
		}
		if len(tiles) > 0 {
			data[rid] = tiles
		}
	}
	if len(data) == 0 {
		// Mark as loaded even if empty (no tiles to send).
		for _, rid := range newRegions {
			c.regionsLoaded[rid] = true
		}
		return false
	}

	// Apply dynamic overlay for this player's progression (same as the
	// boot frame path). The overlay only substitutes tiles within the
	// served scope; for streaming, the scope is the new regions.
	overlay, _ := dynamicOverlayTiles(
		entityDynAreas(),
		questProg{username: c.Username},
		func(mx, my int) (RegionTile, bool) { return buildTile(mx, my) },
		func(x, y int) bool {
			rid := (y/mapDivisionSize)*sideLen + (x / mapDivisionSize)
			for _, r := range newRegions {
				if r == rid {
					return true
				}
			}
			return false
		},
	)
	if len(overlay) > 0 {
		data = applyDynamicTiles(
			data,
			overlay,
			func(x, y int) int { return (y/mapDivisionSize)*sideLen + (x / mapDivisionSize) },
		)
	}

	frame := encodeMapFrame(data)
	_ = gnet.Send(c.Conn, frame)

	// Mark new regions as loaded.
	for _, rid := range newRegions {
		c.regionsLoaded[rid] = true
	}
	log.Printf("dynmap: streamed %d new regions to %s (region %d)", len(data), c.Username, region)
	return true
}

// resetRegionsLoaded clears the loaded-region set and re-seeds with the
// current position's surrounding regions (called on teleport).
func resetRegionsLoaded(c *playerConn) {
	if c == nil {
		return
	}
	c.regionsLoaded = make(map[int]bool)
	loadWorld()
	px, py := c.Sess.PlayerX, c.Sess.PlayerY
	region := (py/mapDivisionSize)*sideLen + (px / mapDivisionSize)
	for _, rid := range surroundingRegions(region) {
		c.regionsLoaded[rid] = true
	}
}
