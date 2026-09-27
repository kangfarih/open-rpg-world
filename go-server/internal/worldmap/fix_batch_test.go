package worldmap

import (
	"testing"
)

func TestSurroundingRegionsDegenerateNil(t *testing.T) {
	var nilW *World
	if got := nilW.SurroundingRegions(0); got != nil {
		t.Fatalf("nil SurroundingRegions = %v, want nil", got)
	}
	if got := nilW.RegionData(0, 0); len(got) != 0 {
		t.Fatalf("nil RegionData = %v, want empty", got)
	}
	zero := &World{Width: 0, Height: 0, SideLen: 0}
	if got := zero.SurroundingRegions(0); got != nil {
		t.Fatalf("degenerate SurroundingRegions = %v, want nil (no div-by-zero)", got)
	}
	if got := zero.RegionData(10, 10); len(got) != 0 {
		t.Fatalf("degenerate RegionData = %v, want empty", got)
	}
	// Width smaller than a division: SideLen 0 via Load math.
	narrow := &World{Width: 10, Height: 10, SideLen: 10 / MapDivisionSize}
	if got := narrow.SurroundingRegions(0); got != nil {
		t.Fatalf("narrow SurroundingRegions = %v, want nil", got)
	}
}

func TestLoadDefaultResetForTests(t *testing.T) {
	ResetDefaultForTests()
	if Default() != nil || DefaultErr() != nil {
		t.Fatal("after reset Default must be nil")
	}
	w, err := LoadDefault(worldmapTestDataPath())
	if err != nil {
		t.Skipf("world.json unavailable: %v", err)
	}
	if Default() != w {
		t.Fatal("Default must return cached world after LoadDefault")
	}
	ResetDefaultForTests()
	if Default() != nil {
		t.Fatal("second reset must clear cache")
	}
}
