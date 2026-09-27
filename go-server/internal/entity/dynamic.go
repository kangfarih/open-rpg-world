// Dynamic per-player mapping for package entity: the area.ts
// fulfillsRequirement + getMappedTile port behind the map.ts:234-244 dynamic
// branch in isColliding.
//
// TS rule: when resolving a collision for a player, if the tile sits in a
// dynamic area whose quest (or achievement) that player finished, the
// collision is evaluated at the MAPPED tile (the area's `mapping`
// counterpart, offset by the tile's position relative to the area).
//
// The per-player gate is a Progression the caller supplies (the server wires
// it to quest state). Collision callers use DynamicRemap as the remap
// function for worldmap.IsBlockedRemapped. Client tile SERVING is per-player
// too (server dynmap.go buildMapFrameFor re-skins served tiles through
// DynamicRemap + MappedTile, regions.ts buildDynamicTile parity).
package entity

// Progression is one player's quest/achievement finish state (served by the
// quest engine; the server adapts quest.StateFor to it).
type Progression interface {
	QuestFinished(key string) bool
	AchFinished(key string) bool
}

// FulfillsRequirement ports area.ts fulfillsRequirement: a quest gate
// checks the quest finished, else an achievement gate checks the achievement
// finished; areas with neither gate never fulfill (TS returns false).
func FulfillsRequirement(area *Area, p Progression) bool {
	if area == nil || p == nil {
		return false
	}
	if area.Quest != "" {
		return p.QuestFinished(area.Quest)
	}
	if area.Achievement != "" {
		return p.AchFinished(area.Achievement)
	}
	return false
}

// DynamicAt reports the dynamic MAPPING area containing (x,y): the first
// dynamic area that contains the tile and has a linked mapped counterpart
// (dynamic.ts link(); only the original side carries `mapping`, same as
// TS region.getDynamicArea which resolves areas with mappings).
func DynamicAt(x, y int) *Area {
	areasMu.Lock()
	defer areasMu.Unlock()
	for _, a := range dynamicAreas {
		if a.mappedArea != nil && a.Inside(x, y) {
			return a
		}
	}
	return nil
}

// MappedTile ports area.ts getMappedTile: the tile relative to the area
// re-based onto the mapped counterpart. ok=false when there is no mapping.
// The mapped link + origins are snapshotted under areasMu so a concurrent
// LoadAreas link cannot race the read.
func MappedTile(area *Area, x, y int) (mx, my int, ok bool) {
	if area == nil {
		return 0, 0, false
	}
	areasMu.Lock()
	mapped, ax, ay := area.mappedArea, area.X, area.Y
	areasMu.Unlock()
	if mapped == nil {
		return 0, 0, false
	}
	relX := x - ax
	relY := y - ay
	return mapped.X + relX, mapped.Y + relY, true
}

// DynamicRemap resolves the collision tile for a player standing at (x,y):
// the mapped tile when a dynamic mapping area contains the tile AND the
// player fulfills its quest/achievement requirement, else no remap
// (map.ts:234-244 dynamic branch parity). Pure over the registry + p.
func DynamicRemap(x, y int, p Progression) (mx, my int, ok bool) {
	area := DynamicAt(x, y)
	if area == nil || !FulfillsRequirement(area, p) {
		return 0, 0, false
	}
	return MappedTile(area, x, y)
}

// MappedAnimTile ports area.ts getMappedAnimationTile: the animation tile
// relative to the area re-based onto the mapped-animation counterpart.
// ok=false when there is no animation mapping (the common case — no
// world.json dynamic area currently sets one; the server tile overlay
// omits the animation field then, client loadRegionTileData parity).
// The mapped link + origins are snapshotted under areasMu like MappedTile.
func MappedAnimTile(area *Area, x, y int) (ax, ay int, ok bool) {
	if area == nil {
		return 0, 0, false
	}
	areasMu.Lock()
	mapped, ox, oy := area.mappedAnimation, area.X, area.Y
	areasMu.Unlock()
	if mapped == nil {
		return 0, 0, false
	}
	relX := x - ox
	relY := y - oy
	return mapped.X + relX, mapped.Y + relY, true
}

// DynamicAreas returns a copy of the dynamic-area group (test/introspection).
func DynamicAreas() []*Area {
	areasMu.Lock()
	defer areasMu.Unlock()
	out := make([]*Area, len(dynamicAreas))
	copy(out, dynamicAreas)
	return out
}
