package world

import (
	"testing"
	"time"
)

func getenvOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseModesDefaults(t *testing.T) {
	m := ParseModes(getenvOf(nil), nil)
	if !m.Test || m.Clean || m.Combat {
		t.Fatalf("defaults = %+v, want test-only", m)
	}
}

func TestParseModesEnvWins(t *testing.T) {
	m := ParseModes(getenvOf(map[string]string{
		"TESTMAP": "0", "CLEAN": "1", "COMBAT": "yes",
	}), []string{"--testmap", "--noclean", "--nocombat"})
	if m.Test || !m.Clean || !m.Combat {
		t.Fatalf("env-wins = %+v", m)
	}
}

func TestParseModesFlags(t *testing.T) {
	m := ParseModes(getenvOf(nil), []string{"--notestmap", "--clean", "--combat"})
	if m.Test || !m.Clean || !m.Combat {
		t.Fatalf("flags = %+v", m)
	}
	// Falsy env spellings stay exact (case-sensitive, like main.go).
	for _, v := range []string{"0", "false", "off", "no"} {
		if ParseModes(getenvOf(map[string]string{"TESTMAP": v}), nil).Test {
			t.Fatalf("TESTMAP=%q should be off", v)
		}
		if !ParseModes(getenvOf(map[string]string{"CLEAN": "True" + v}), nil).Clean {
			t.Fatalf("CLEAN=True%s should be on (unknown truthy)", v)
		}
	}
}

func TestRegionOf(t *testing.T) {
	if got := RegionOf(100, 96, 0, 48); got != 0 {
		t.Fatalf("unloaded sideLen = %d, want 0", got)
	}
	// (96/48)*sideLen + (100/48) with sideLen=24 -> region 50.
	if got := RegionOf(100, 96, 24, 48); got != 50 {
		t.Fatalf("region = %d, want 50", got)
	}
	if !InterestHit(50, []int{25, 50, 75}) || InterestHit(51, []int{25, 50}) {
		t.Fatal("InterestHit mismatch")
	}
}

func TestCheckSpeed(t *testing.T) {
	now := time.Now()
	st := &SpeedState{}
	if CheckSpeed(st, 1, now, 0, 0, false) {
		t.Fatal("first step must pass")
	}
	// Immediate second step at 1 tile is too fast (220ms*0.95 allowance).
	if !CheckSpeed(st, 1, now.Add(10*time.Millisecond), 0, 0, false) {
		t.Fatal("10ms step must be too fast")
	}
	// Legal pace passes.
	st2 := &SpeedState{MovementSpeed: 220, LastStep: now}
	if CheckSpeed(st2, 1, now.Add(300*time.Millisecond), 0, 0, false) {
		t.Fatal("300ms step must pass")
	}
	// Idle >2s always passes.
	st3 := &SpeedState{MovementSpeed: 220, LastStep: now}
	if CheckSpeed(st3, 9, now.Add(3*time.Second), 0, 0, false) {
		t.Fatal("post-idle step must pass")
	}
	// tiles<1 clamps to 1; non-positive speed defaults to 220.
	st4 := &SpeedState{}
	if CheckSpeed(st4, 0, now.Add(3*time.Second), 0, 0, false) {
		t.Fatal("clamped idle step must pass")
	}
	if st4.MovementSpeed != 220 {
		t.Fatalf("speed default = %d, want 220", st4.MovementSpeed)
	}
	// Door exemption: even an impossibly fast step passes on a door tile.
	st5 := &SpeedState{MovementSpeed: 220, LastStep: now}
	if CheckSpeed(st5, 1, now.Add(1*time.Millisecond), 0, 0, true) {
		t.Fatal("door tile step must pass")
	}
	// Region grace: step within grace window after region change passes.
	st6 := &SpeedState{MovementSpeed: 220, LastStep: now}
	if CheckSpeed(st6, 1, now.Add(10*time.Millisecond), 0, 2*time.Second, false) {
		t.Fatal("region grace step must pass")
	}
	// Latency compensation: high latency + small step passes.
	st7 := &SpeedState{MovementSpeed: 220, LastStep: now}
	if CheckSpeed(st7, 1, now.Add(10*time.Millisecond), 50, 0, false) {
		t.Fatal("high-latency small step must pass")
	}
}

func TestCalculateMovementSpeed(t *testing.T) {
	// Default: no modifiers, 220ms/tile.
	if got := CalculateMovementSpeed(SpeedModifiers{Override: -1}); got != 220 {
		t.Fatalf("default speed = %d, want 220", got)
	}
	// Admin override replaces default.
	if got := CalculateMovementSpeed(SpeedModifiers{Override: 150}); got != 150 {
		t.Fatalf("override speed = %d, want 150", got)
	}
	// Running: 10% faster (0.9x).
	if got := CalculateMovementSpeed(SpeedModifiers{Override: -1, HasRunning: true}); got != 198 {
		t.Fatalf("running speed = %d, want 198", got)
	}
	// HotSauce: 20% faster (0.8x).
	if got := CalculateMovementSpeed(SpeedModifiers{Override: -1, HasHotSauce: true}); got != 176 {
		t.Fatalf("hotsauce speed = %d, want 176", got)
	}
	// Freezing: 25% slower (1.25x).
	if got := CalculateMovementSpeed(SpeedModifiers{Override: -1, HasFreezing: true}); got != 275 {
		t.Fatalf("freezing speed = %d, want 275", got)
	}
	// Freezing + SnowPotion: exemption, stays at 220.
	if got := CalculateMovementSpeed(SpeedModifiers{Override: -1, HasFreezing: true, HasSnowPotion: true}); got != 220 {
		t.Fatalf("freezing+snow speed = %d, want 220", got)
	}
	// Cheater: 2x slower.
	if got := CalculateMovementSpeed(SpeedModifiers{Override: -1, IsCheater: true}); got != 440 {
		t.Fatalf("cheater speed = %d, want 440", got)
	}
	// Boots modifier stacks: 0.8x boots.
	if got := CalculateMovementSpeed(SpeedModifiers{Override: -1, BootsModifier: 0.8}); got != 176 {
		t.Fatalf("boots speed = %d, want 176", got)
	}
	// Stacked: boots + running + freezing (no snow).
	mods := SpeedModifiers{Override: -1, BootsModifier: 0.8, HasRunning: true, HasFreezing: true}
	// 220 * 0.8 = 176, * 0.9 = 158, * 1.25 = 197
	if got := CalculateMovementSpeed(mods); got != 197 {
		t.Fatalf("stacked speed = %d, want 197", got)
	}
}

func TestTargetsOccupant(t *testing.T) {
	if TargetsOccupant("", "t-test-1") {
		t.Fatal("empty occupant must never match")
	}
	if !TargetsOccupant("t-test-1", "", "t-test-1") {
		t.Fatal("exact target must match")
	}
	if TargetsOccupant("t-test-1", "t-test-2") {
		t.Fatal("other target must not match")
	}
}

func TestStore(t *testing.T) {
	s := NewStore()
	s.Set("p1", 100, 96)
	s.Set("p1", 101, 96) // upsert
	if x, y, ok := s.Pos("p1"); !ok || x != 101 || y != 96 {
		t.Fatalf("pos = %d,%d,%v", x, y, ok)
	}
	if _, _, ok := s.Pos("nope"); ok {
		t.Fatal("unknown must be missing")
	}
	s.Set("m1", 104, 104)
	if s.Count() != 2 || len(s.Snapshot()) != 2 {
		t.Fatal("count/snapshot mismatch")
	}
	s.Remove("p1")
	if s.Count() != 1 {
		t.Fatal("remove failed")
	}
}

func TestEngineOrder(t *testing.T) {
	var got []string
	e := &Engine{Subs: []Subsystem{
		{Name: "abilities", Tick: func() { got = append(got, "abilities") }},
		{Name: "pets", Tick: func() { got = append(got, "pets") }},
		{Name: "events", Tick: func() { got = append(got, "events") }},
	}}
	e.Tick()
	if len(got) != 3 || got[0] != "abilities" || got[1] != "pets" || got[2] != "events" {
		t.Fatalf("tick order = %v", got)
	}
	if n := e.Names(); len(n) != 3 || n[0] != "abilities" {
		t.Fatalf("names = %v", n)
	}
}
