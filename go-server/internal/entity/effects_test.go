package entity

import (
	"sync"
	"testing"
	"time"
)

// fakeEffectWorld captures the transport calls SpawnEffect/DestroyEffect make.
type fakeEffectWorld struct {
	mu        sync.Mutex
	positions map[string][2]int
	removed   []string
	broadcasts int
}

func newFakeEffectWorld() *fakeEffectWorld {
	return &fakeEffectWorld{positions: map[string][2]int{}}
}

func (f *fakeEffectWorld) SetEntityPos(inst string, x, y int) {
	f.mu.Lock()
	f.positions[inst] = [2]int{x, y}
	f.mu.Unlock()
}

func (f *fakeEffectWorld) RemoveEntity(inst string) {
	f.mu.Lock()
	f.removed = append(f.removed, inst)
	f.mu.Unlock()
}

func (f *fakeEffectWorld) Broadcast(frames ...[]any) {
	f.mu.Lock()
	f.broadcasts += len(frames)
	f.mu.Unlock()
}

func TestSpawnEffectRegistersAndDespawns(t *testing.T) {
	ResetEffects()
	fw := newFakeEffectWorld()
	ConfigureEffects(EffectDeps{World: fw})
	defer ConfigureEffects(EffectDeps{})

	inst := SpawnEffect("lava", 10, 20)
	if inst == "" {
		t.Fatal("SpawnEffect returned empty instance")
	}
	if ActiveEffects() != 1 {
		t.Fatalf("expected 1 active effect, got %d", ActiveEffects())
	}

	fw.mu.Lock()
	pos, hasPos := fw.positions[inst]
	bcasts := fw.broadcasts
	fw.mu.Unlock()
	if !hasPos {
		t.Fatal("SetEntityPos not called")
	}
	if pos != [2]int{10, 20} {
		t.Fatalf("expected pos (10,20), got %v", pos)
	}
	if bcasts < 1 {
		t.Fatal("expected at least 1 broadcast (Spawn frame)")
	}

	// Wait for the lava duration (5000ms) plus a margin.
	time.Sleep(5500 * time.Millisecond)

	if ActiveEffects() != 0 {
		t.Fatalf("expected 0 active effects after expiry, got %d", ActiveEffects())
	}

	fw.mu.Lock()
	removed := len(fw.removed)
	fw.mu.Unlock()
	if removed == 0 {
		t.Fatal("RemoveEntity not called after expiry")
	}
}

func TestSpawnEffectUnknownKeyUsesDefault(t *testing.T) {
	ResetEffects()
	fw := newFakeEffectWorld()
	ConfigureEffects(EffectDeps{World: fw})
	defer ConfigureEffects(EffectDeps{})

	inst := SpawnEffect("unknown-effect", 5, 5)
	_ = inst

	// Default is 4000ms; wait 4500ms and check it's gone.
	time.Sleep(4500 * time.Millisecond)

	if ActiveEffects() != 0 {
		t.Fatalf("expected 0 active effects after default expiry, got %d", ActiveEffects())
	}
}

func TestDestroyEffectManual(t *testing.T) {
	ResetEffects()
	fw := newFakeEffectWorld()
	ConfigureEffects(EffectDeps{World: fw})
	defer ConfigureEffects(EffectDeps{})

	inst := SpawnEffect("lava", 1, 1)
	if ActiveEffects() != 1 {
		t.Fatal("expected 1 active effect")
	}

	DestroyEffect(inst, "manual")

	if ActiveEffects() != 0 {
		t.Fatalf("expected 0 active effects after manual destroy, got %d", ActiveEffects())
	}

	fw.mu.Lock()
	removed := len(fw.removed)
	fw.mu.Unlock()
	if removed == 0 {
		t.Fatal("RemoveEntity not called on manual destroy")
	}
}

func TestDestroyEffectIdempotent(t *testing.T) {
	ResetEffects()
	fw := newFakeEffectWorld()
	ConfigureEffects(EffectDeps{World: fw})
	defer ConfigureEffects(EffectDeps{})

	inst := SpawnEffect("lava", 1, 1)
	DestroyEffect(inst, "first")
	DestroyEffect(inst, "second") // should be a no-op

	fw.mu.Lock()
	removed := len(fw.removed)
	fw.mu.Unlock()
	if removed != 1 {
		t.Fatalf("expected exactly 1 RemoveEntity call, got %d", removed)
	}
}

func TestSpawnEffectNilWorld(t *testing.T) {
	ResetEffects()
	ConfigureEffects(EffectDeps{}) // no world — should not panic
	defer ConfigureEffects(EffectDeps{})

	inst := SpawnEffect("lava", 0, 0)
	if inst == "" {
		t.Fatal("SpawnEffect returned empty instance even without world")
	}
	if ActiveEffects() != 1 {
		t.Fatalf("expected 1 active effect, got %d", ActiveEffects())
	}

	// Clean up: wait for expiry.
	time.Sleep(5500 * time.Millisecond)
	if ActiveEffects() != 0 {
		t.Fatalf("expected 0 active effects after expiry, got %d", ActiveEffects())
	}
}
