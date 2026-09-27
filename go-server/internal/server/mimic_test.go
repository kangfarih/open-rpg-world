package server

import (
	"testing"
)

// The SpawnMimic seam (entities.ts onOpen spawnMob('mimic')) must spawn the
// real mobs.json mimic profile (1200 HP, Lv25) as a non-respawning mob at
// the chest tile through the full spawnMob path.
func TestSpawnMimicSeam(t *testing.T) {
	inst, ok := gameWorld.SpawnMimic(271, 731)
	if !ok || inst == "" {
		t.Fatal("SpawnMimic failed")
	}
	t.Cleanup(func() { removeMob(inst) })

	m := mobFor(inst)
	if m == nil {
		t.Fatal("mimic not registered")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key != "mimic" {
		t.Fatalf("mob key = %q, want mimic", m.key)
	}
	if m.hp != 1200 || m.maxHP != 1200 {
		t.Fatalf("mimic HP = %d/%d, want 1200/1200", m.hp, m.maxHP)
	}
	if m.prof.Level != 25 {
		t.Fatalf("mimic level = %d, want 25", m.prof.Level)
	}
	if m.x != 271 || m.y != 731 {
		t.Fatalf("mimic at %d,%d, want the chest tile 271,731", m.x, m.y)
	}
	if !m.over.NoRespawn {
		t.Fatal("mimic respawns, want TS mimic.respawnable = false")
	}
}

// RemoveMob (TS destroy for the dead mimic) drops the registry entry.
func TestRemoveMobSeam(t *testing.T) {
	inst, ok := gameWorld.SpawnMimic(10, 10)
	if !ok {
		t.Fatal("SpawnMimic failed")
	}
	gameWorld.RemoveMob(inst)
	if m := mobFor(inst); m != nil {
		t.Fatal("mimic still registered after RemoveMob")
		removeMob(inst)
	}
}
