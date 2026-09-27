// Package sim is the E9b home for the TESTMAP/COMBAT-gated demo scene math:
// the showcase-grid layout, the center-pond ellipse predicate, and the
// ranged-projectile travel timing. All functions are pure and take their
// layout constants as parameters so the canonical values stay exactly once
// in package main (frozen m5-m13.go and *_wire.go files reference the root
// combat/showcase symbols, so the constants cannot move for E9b). main.go
// delegates to these helpers; packet shapes, positions and cadences are
// unchanged.
package sim

import (
	"time"
)

// GridPos returns the showcase-grid tile for index i given the origin,
// column count and step (main.go showPos with showOX/showOY/showCols/
// showStep). Mobs occupy indices 0..len(showMobs)-1, NPCs continue after.
func GridPos(i, ox, oy, cols, step int) (x, y int) {
	return ox + (i%cols)*step, oy + (i/cols)*step
}

// InGrid reports whether (x,y) is a showcase-grid slot for a scene of
// mobCount+npcCount entities (main.go isTestGrass grid branch).
func InGrid(x, y, ox, oy, cols, step, total int) bool {
	if x < ox || y < oy || (x-ox)%step != 0 || (y-oy)%step != 0 {
		return false
	}
	col := (x - ox) / step
	row := (y - oy) / step
	if col >= cols {
		return false
	}
	return row*cols+col < total
}

// IsWater reports whether (x,y) is pond water: the ellipse test from
// main.go isTestWater (center cx,cy radii rx,ry).
func IsWater(x, y, cx, cy, rx, ry int) bool {
	dx := float64(x - cx)
	dy := float64(y - cy)
	return (dx*dx)/float64(rx*rx)+(dy*dy)/float64(ry*ry) <= 1
}

// TravelTime converts a tile distance to projectile flight time: the
// projectile.ts rule (distance*90ms) used by main.go
// spawnRangedStrikeLocked. Distances below 1 tile clamp to 1.
func TravelTime(dist int) time.Duration {
	if dist < 1 {
		dist = 1
	}
	return time.Duration(dist*90) * time.Millisecond
}

// TravelBetween returns the flight time between two tiles (Manhattan
// distance, then TravelTime), mirroring the projectile.ts getDistance rule
// (entity.ts:120-122: Math.abs(dx) + Math.abs(dy)).
func TravelBetween(x0, y0, x1, y1 int) time.Duration {
	dist := Manhattan(x0, y0, x1, y1)
	return TravelTime(dist)
}

// Manhattan returns the Manhattan distance between two grid points
// (entity.ts getDistance: Math.abs(dx) + Math.abs(dy)).
func Manhattan(x0, y0, x1, y1 int) int {
	dx := x1 - x0
	dy := y1 - y0
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	return dx + dy
}
