package sim

import (
	"testing"
	"time"
)

func TestGridPos(t *testing.T) {
	// main.go layout: origin (96,110), 16 cols, step 2.
	if x, y := GridPos(0, 96, 110, 16, 2); x != 96 || y != 110 {
		t.Fatalf("slot 0 = %d,%d", x, y)
	}
	if x, y := GridPos(16, 96, 110, 16, 2); x != 96 || y != 112 {
		t.Fatalf("slot 16 = %d,%d", x, y)
	}
	if x, y := GridPos(231, 96, 110, 16, 2); x != 110 || y != 138 {
		t.Fatalf("slot 231 = %d,%d", x, y)
	}
	if !InGrid(96, 110, 96, 110, 16, 2, 232) || InGrid(97, 110, 96, 110, 16, 2, 232) {
		t.Fatal("InGrid mismatch")
	}
	if InGrid(96, 110, 96, 110, 16, 2, 0) {
		t.Fatal("empty scene has no slots")
	}
}

func TestIsWater(t *testing.T) {
	// Pond (104,104) rx4 ry3 covers x100-108, y101-107.
	if !IsWater(104, 104, 104, 104, 4, 3) {
		t.Fatal("center must be water")
	}
	if IsWater(100, 96, 104, 104, 4, 3) {
		t.Fatal("spawn must not be water")
	}
	if IsWater(108, 104, 104, 104, 4, 3+0) && !IsWater(109, 104, 104, 104, 4, 3) {
		// rx edge inclusive, outside exclusive — documents the boundary.
	} else {
		t.Fatal("rx boundary mismatch")
	}
}

func TestTravel(t *testing.T) {
	if got := TravelTime(0); got != 90*time.Millisecond {
		t.Fatalf("clamp = %v", got)
	}
	// Archer (100,98) -> dummy (106,96): manhattan=|6|+|2|=8 tiles -> 720ms.
	if got := TravelBetween(100, 98, 106, 96); got != 720*time.Millisecond {
		t.Fatalf("archer travel = %v", got)
	}
}
