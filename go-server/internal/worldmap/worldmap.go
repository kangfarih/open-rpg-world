// Package worldmap is an additive, dependency-free extraction of the world/
// region plumbing in main.go (region plumbing block: world load,
// worldPath/loadWorld/unflipTile/buildTile/surroundingRegions/getRegionData
// plus consts mapDivisionSize/diagonalFlag/flipMask and the worldFile shape).
//
// RegionTile currently lives in package main (packets.go), which cannot be
// imported, so this package defines its own Tile struct with identical JSON
// tags plus pure helpers. Load is the uncached single-read primitive (the
// caller owns caching); LoadDefault is the shared canonical store with
// sync.Once discipline that the root delegates to.
package worldmap

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
)

// Map division + Tiled flip-bit constants (mirrors main.go, which mirrors
// Modules.Constants.MAP_DIVISION_SIZE and Modules.MapFlags).
const (
	MapDivisionSize = 48 // Modules.Constants.MAP_DIVISION_SIZE (modules.ts:630)

	DiagonalFlag = 0x20000000 // Modules.MapFlags (modules.ts:706-708)
	FlipMask     = 0x20000000 | 0x40000000 | 0x80000000
)

// Config carries caller-owned world-file location. The caller owns caching;
// Load does a single uncached read per call.
type Config struct {
	WorldJSONPath string
}

// Tile mirrors RegionTile in packets.go (which mirrors RegionTileData in
// types/map.d.ts:17-25). Data is Tile (number | number[]) straight from
// world.json. C is always emitted (false = walkable grass, true =
// collision); O/Cur only when set. Defined locally because package main
// cannot be imported.
type Tile struct {
	X    int    `json:"x"`
	Y    int    `json:"y"`
	Data any    `json:"data"`
	C    bool   `json:"c"`
	O    bool   `json:"o,omitempty"`
	Cur  string `json:"cur,omitempty"`
}

// World is the loaded world.json plus derived lookup sets. SideLen is
// Width / MapDivisionSize, computed by Load (mirrors main.go loadWorld).
// Plateau maps tile index (y*Width+x) to plateau level (map.ts
// getPlateauLevel; TS `map.plateau`, 26325 entries). Tiles absent from the
// table are plateau 0. The `high` array (1421 entries) is rendering-only
// (foreground layer select) and is intentionally not loaded.
// Entities maps tile index (y*Width+x) to the static entity marker key
// (world.json `entities`, 4226 entries; TS `map.entities` consumed by
// map.ts forEachEntity -> entities.ts load). Absent tiles carry no marker.
type World struct {
	Width      int
	Height     int
	Data       []json.RawMessage
	Collisions map[int]bool
	Objects    map[int]bool
	Cursors    map[int]string
	Plateau    map[int]int
	Entities   map[int]string
	SideLen    int
}

// worldFile mirrors the worldFile struct in main.go (the on-disk shape of
// world.json).
type worldFile struct {
	Width      int               `json:"width"`
	Height     int               `json:"height"`
	Data       []json.RawMessage `json:"data"`
	Collisions []int             `json:"collisions"`
	Objects    []int             `json:"objects"`
	Cursors    map[string]string `json:"cursors"`
	Plateau    map[string]int    `json:"plateau"`
	Entities   map[string]string `json:"entities"`
}

// Load reads and parses the world JSON at path, building the collision,
// object, and cursor lookup sets. One call = one read; the caller owns
// caching (no globals, no sync.Once). Returns an error on missing or
// invalid files (main.go log.Fatalf becomes a returned error here).
func Load(path string) (*World, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read world.json: %w", err)
	}
	var w worldFile
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("parse world.json: %w", err)
	}
	collSet := make(map[int]bool, len(w.Collisions))
	for _, id := range w.Collisions {
		collSet[id] = true
	}
	objSet := make(map[int]bool, len(w.Objects))
	for _, id := range w.Objects {
		objSet[id] = true
	}
	curSet := make(map[int]string, len(w.Cursors))
	for k, v := range w.Cursors {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		curSet[id] = v
	}
	plateau := make(map[int]int, len(w.Plateau))
	for k, v := range w.Plateau {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		plateau[id] = v
	}
	entities := make(map[int]string, len(w.Entities))
	for k, v := range w.Entities {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		entities[id] = v
	}
	return &World{
		Width:      w.Width,
		Height:     w.Height,
		Data:       w.Data,
		Collisions: collSet,
		Objects:    objSet,
		Cursors:    curSet,
		Plateau:    plateau,
		Entities:   entities,
		SideLen:    w.Width / MapDivisionSize,
	}, nil
}

// Load loads the world at the configured path. Convenience wrapper so a
// caller holding a Config can load without threading the path separately.
func (c Config) Load() (*World, error) {
	return Load(c.WorldJSONPath)
}

// UnflipTile strips Tiled flip bitmasks (map.ts:353-361). Pure function.
func UnflipTile(id int) int {
	if id > DiagonalFlag {
		return id &^ FlipMask
	}
	return id
}

// BuildTile mirrors regions.ts:610-646: skip empty, keep data>=1 (arrays are
// always kept), flag collisions/objects/cursors per layer. Walkable tiles
// keep C=false. Returns false for skipped tiles.
func (w *World) BuildTile(x, y int) (Tile, bool) {
	idx := y*w.Width + x
	if idx < 0 || idx >= len(w.Data) {
		return Tile{}, false
	}
	raw := w.Data[idx]
	if string(raw) == "0" || string(raw) == "[]" || string(raw) == "null" {
		return Tile{}, false
	}
	var layers []int
	var data any
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		if num < 1 {
			return Tile{}, false
		}
		layers = []int{int(num)}
		data = layers[0]
	} else {
		var arr []float64
		if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
			return Tile{}, false
		}
		layers = make([]int, len(arr))
		for i, v := range arr {
			layers[i] = int(v)
		}
		data = layers
	}

	tile := Tile{X: x, Y: y, Data: data}
	for _, id := range layers {
		u := UnflipTile(id)
		if w.Objects[u] {
			tile.O = true
			tile.C = true
		} else if w.Collisions[u] {
			tile.C = true
		}
		if cur, ok := w.Cursors[u]; ok {
			tile.Cur = cur
		}
	}
	return tile, true
}

// SurroundingRegions mirrors getSurroundingRegions (regions.ts:711-766),
// region first then neighbours (9 for interior regions like 50).
// Nil worlds and degenerate (SideLen<=0) worlds return nil instead of
// dividing by zero.
func (w *World) SurroundingRegions(region int) []int {
	if w == nil || w.SideLen <= 0 || w.Width <= 0 || w.Height <= 0 {
		return nil
	}
	total := (w.Width / MapDivisionSize) * (w.Height / MapDivisionSize)
	if region < 0 || region > total-1 {
		return nil
	}
	out := []int{region}
	left := region%w.SideLen == 0
	right := region%w.SideLen == w.SideLen-1
	top := region < w.SideLen
	bottom := region > total-w.SideLen-1

	switch {
	case left:
		out = append(out, region+1)
	case right:
		out = append(out, region-1)
	default:
		out = append(out, region-1, region+1)
	}

	switch {
	case top || bottom:
		rel := region + w.SideLen
		if !top {
			rel = region - w.SideLen
		}
		out = append(out, rel)
		switch {
		case rel%w.SideLen == 0:
			out = append(out, rel+1)
		case rel%w.SideLen == w.SideLen-1:
			out = append(out, rel-1)
		default:
			out = append(out, rel-1, rel+1)
		}
	case left:
		out = append(out, region-w.SideLen, region-w.SideLen+1, region+w.SideLen, region+w.SideLen+1)
	case right:
		out = append(out, region-w.SideLen, region-w.SideLen-1, region+w.SideLen, region+w.SideLen-1)
	default:
		out = append(out, region+w.SideLen, region-w.SideLen,
			region+w.SideLen-1, region+w.SideLen+1, region-w.SideLen-1, region-w.SideLen+1)
	}
	return out
}

// RegionData mirrors getRegionData (regions.ts:501-529) for a static spawn:
// region of (px,py) plus all surrounding regions, empty ones dropped.
// Nil or degenerate (SideLen<=0) worlds yield an empty map.
func (w *World) RegionData(px, py int) map[int][]Tile {
	data := make(map[int][]Tile)
	if w == nil || w.SideLen <= 0 {
		return data
	}
	region := (py/MapDivisionSize)*w.SideLen + (px / MapDivisionSize)
	for _, rid := range w.SurroundingRegions(region) {
		x0 := (rid % w.SideLen) * MapDivisionSize
		y0 := (rid / w.SideLen) * MapDivisionSize
		var tiles []Tile
		for y := y0; y < y0+MapDivisionSize; y++ {
			for x := x0; x < x0+MapDivisionSize; x++ {
				if t, ok := w.BuildTile(x, y); ok {
					tiles = append(tiles, t)
				}
			}
		}
		if len(tiles) > 0 {
			data[rid] = tiles
		}
	}
	return data
}

// Default world store: the canonical holder mirroring the old root sync.Once
// discipline. Load stays the uncached single-read primitive; LoadDefault is
// the shared cached path the root delegates to. The first path wins; later
// calls return the cached world. No logging here (the root keeps its
// fatal/log lines).
var (
	defaultOnce  sync.Once
	defaultWorld *World
	defaultErr   error
)

// LoadDefault loads the world at path once and returns the cached result
// (nil world plus error when the first load failed).
func LoadDefault(path string) (*World, error) {
	defaultOnce.Do(func() {
		defaultWorld, defaultErr = Load(path)
	})
	return defaultWorld, defaultErr
}

// Default returns the cached world (nil until LoadDefault succeeds).
func Default() *World { return defaultWorld }

// DefaultErr returns the cached load error, if any.
func DefaultErr() error { return defaultErr }

// ResetDefaultForTests clears the LoadDefault cache (tests only). The
// production path never resets: the first path wins for the process
// lifetime. Tests that need distinct worlds must call this between cases.
func ResetDefaultForTests() {
	defaultOnce = sync.Once{}
	defaultWorld = nil
	defaultErr = nil
}

// MarkerAt returns the static entity marker key at tile index idx
// (y*Width+x), or ("", false) when the tile carries no marker (map.ts
// forEachEntity source for entities.ts load).
func (w *World) MarkerAt(idx int) (string, bool) {
	if w == nil {
		return "", false
	}
	key, ok := w.Entities[idx]
	return key, ok
}

// MarkerCoord converts a tile index to its (x, y) tile (map.ts
// indexToCoord: x = idx % width, y = idx / width).
func (w *World) MarkerCoord(idx int) (x, y int) {
	if w == nil || w.Width <= 0 {
		return 0, 0
	}
	return idx % w.Width, idx / w.Width
}

// ForEachMarker iterates every static entity marker in tile-index order
// (map.ts forEachEntity parity; Go map iteration is unordered, so indices
// are sorted for deterministic boot spawns).
func (w *World) ForEachMarker(callback func(x, y int, key string)) {
	if w == nil {
		return
	}
	indices := make([]int, 0, len(w.Entities))
	for idx := range w.Entities {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		x, y := w.MarkerCoord(idx)
		callback(x, y, w.Entities[idx])
	}
}

// PlateauLevel returns the plateau level at (x,y) (map.ts
// getPlateauLevel/coordToIndex over `map.plateau`). Pure function: tiles
// absent from the table (and OOB coordinates) are plateau 0.
func (w *World) PlateauLevel(x, y int) int {
	if w == nil || x < 0 || y < 0 || x >= w.Width || y >= w.Height {
		return 0
	}
	if lvl, ok := w.Plateau[y*w.Width+x]; ok {
		return lvl
	}
	return 0
}

// IsBlocked mirrors the real-terrain half of the root tileBlocked
// (map.ts:170,201-208): OOB or empty data blocks; otherwise any layer whose
// unflipped id is in collisions/objects blocks. TESTMAP pond/grass overlays
// stay with the root caller.
func (w *World) IsBlocked(x, y int) bool {
	if x < 0 || y < 0 || x >= w.Width || y >= w.Height {
		return true
	}
	idx := y*w.Width + x
	if idx < 0 || idx >= len(w.Data) {
		return true
	}
	raw := w.Data[idx]
	if string(raw) == "0" || string(raw) == "[]" || string(raw) == "null" {
		return true
	}
	var layers []int
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		if num < 1 {
			return true
		}
		layers = []int{int(num)}
	} else {
		var arr []float64
		if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
			return true
		}
		layers = make([]int, len(arr))
		for i, v := range arr {
			layers[i] = int(v)
		}
	}
	for _, id := range layers {
		u := UnflipTile(id)
		if w.Collisions[u] || w.Objects[u] {
			return true
		}
	}
	return false
}

// IsBlockedRemapped is the per-player gated collision variant (map.ts:234-244
// dynamic branch in isColliding via getMappedTile): when remap resolves (x,y)
// to a mapped tile, the collision is evaluated at the mapped tile; otherwise
// the static tile applies. A nil remap is the static IsBlocked. The caller
// supplies the per-player dynamic-area resolution (quest/achievement-gated);
// tile serving itself stays static (documented in the caller).
func (w *World) IsBlockedRemapped(x, y int, remap func(x, y int) (int, int, bool)) bool {
	if remap != nil {
		if mx, my, ok := remap(x, y); ok {
			return w.IsBlocked(mx, my)
		}
	}
	return w.IsBlocked(x, y)
}
