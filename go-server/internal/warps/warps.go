// Package warps is an additive, transport-free extraction of the warp
// trigger data in packages/server/src/game/map/map.ts
// (map.areas.warps) with the rectangular hit rule from
// game/map/areas/area.ts (inRectangularArea).
//
// Source shapes:
//   - world.json areas.warps entries: {id, name, x, y, width, height,
//     level?, quest?, achievement?} (see ProcessedArea in
//     packages/common/types/map.d.ts:39-108).
//   - Menu-driven warping itself (level/quest/achievement/cooldown checks,
//     random landing point, Teleport packet {instance, x, y,
//     withAnimation?}) lives in controllers/warps.ts and
//     common/network/impl/teleport.ts and is intentionally NOT modelled
//     here; this package is pure trigger geometry + file loading.
//
// Divergences / verification notes:
//   - controllers/warps.ts does NOT cover doors, so per the task condition
//     this package carries no Doors handling. Doors are owned by map.ts
//     loadDoors/isDoor/getDoor: exact-tile lookup keyed by
//     coordToIndex(x, y) (y*width+x) with destination coords resolved by
//     linking door.destination ids at load time. That is a different
//     (point, not rect) lookup and lives outside warps scope.
//   - TS warps are menu-driven, not step-on triggers: nothing in the TS
//     server calls At()/contains() on warp areas during movement (only
//     doors trigger on step, player.ts:1277). At() still mirrors the
//     canonical rect rule (area.ts:228-230) so callers get faithful
//     geometry: x >= X && y >= Y && x < X+W && y < Y+H (half-open,
//     first match in file order, like areas.ts inArea).
//   - world.json warp entries carry no destination coordinates, so DestX
//     and DestY load as 0 unless the entry carries optional destX/destY
//     keys. In TS the landing point is picked at warp time as a random
//     tile inside the destination warp rect
//     (Utils.randomInt(x, x+width-1), warps.ts:63-69), not from stored
//     coords; see the Warp field docs.
package warps

import (
	"encoding/json"
	"fmt"
	"os"
)

// Warp is one rectangular warp trigger from world.json areas.warps.
// ID/X/Y/W/H are the entry identity and rect; DestX/DestY is the
// step-on destination. world.json warp entries carry no destination
// fields (TS resolves the landing point at warp time to a random tile
// inside the target rect, warps.ts:63-69), so DestX/DestY stay 0 unless
// the entry carries optional destX/destY keys.
type Warp struct {
	ID    int `json:"id"`
	X     int `json:"x"`
	Y     int `json:"y"`
	W     int `json:"width"`
	H     int `json:"height"`
	DestX int `json:"destX,omitempty"`
	DestY int `json:"destY,omitempty"`
}

// Contains reports whether the grid point (x, y) lies inside the warp
// rect. Mirrors area.ts inRectangularArea (half-open upper bound).
func (w Warp) Contains(x, y int) bool {
	return x >= w.X && y >= w.Y && x < w.X+w.W && y < w.Y+w.H
}

// Registry is the ordered list of warps from one world.json load.
// Order is file order; At returns the first match like areas.ts inArea.
type Registry struct {
	Warps []Warp
}

// worldFile is the minimal on-disk shape needed here.
type worldFile struct {
	Areas map[string][]Warp `json:"areas"`
}

// Load reads world.json at path and returns its areas.warps entries.
// A missing areas.warps key yields an empty registry (map.ts uses
// `map.areas.warps || []`); a missing file or invalid JSON is an error.
func Load(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read world file: %w", err)
	}
	var wf worldFile
	if err := json.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("parse world file: %w", err)
	}
	return &Registry{Warps: wf.Areas["warps"]}, nil
}

// At returns a copy of the first warp whose rect contains (x, y), or nil.
// Pure lookup; mirrors the first-match order of areas.ts inArea. The copy
// (not an interior pointer) keeps registry state immutable to callers.
func (r *Registry) At(x, y int) *Warp {
	if r == nil {
		return nil
	}
	for i := range r.Warps {
		if r.Warps[i].Contains(x, y) {
			cp := r.Warps[i]
			return &cp
		}
	}
	return nil
}
