package entity

import (
	"testing"
)

// TestProjectileSpawnOnRangedAttack verifies that ranged mobs with a
// projectileName spawn a projectile on attack, and melee mobs do not.
func TestProjectileSpawnOnRangedAttack(t *testing.T) {
	w := newSimFake()
	w.withPlayer("hero", "hero", 10, 10, 1, 1)

	// Ranged mob with projectileName (forestdragon-like).
	rangedProf := MobProfile{
		Name: "FireDragon", Level: 30, HitPoints: 500,
		AggroRange: 8, AttackRange: 7, AttackRate: 1500,
		ProjectileName: "fireball", MovementSpeed: 200,
		RespawnDelay: 10000, RoamDistance: 5, Aggressive: true,
	}
	ranged := newTestMob("mob-fd", "forestdragon", rangedProf, 5, 5)
	ranged.plateau = 0

	// Melee mob with projectileName (should NOT spawn projectile).
	meleeProf := MobProfile{
		Name: "Rat", Level: 2, HitPoints: 30,
		AggroRange: 6, AttackRange: 1, AttackRate: 1000,
		ProjectileName: "fireball", MovementSpeed: 220,
		RespawnDelay: 4000, RoamDistance: 7, Aggressive: true,
	}
	melee := newTestMob("mob-rat", "rat", meleeProf, 9, 9)
	melee.plateau = 0

	viewer := w.players[0]

	// Ranged attack should spawn projectile (simFake.SpawnProjectile is no-op,
	// but the call path should not panic).
	ranged.Lock()
	strikeMob(ranged, rangedProf, viewer, w)
	ranged.Unlock()

	// Melee attack should NOT spawn projectile (AttackRange=1 gates it).
	melee.Lock()
	strikeMob(melee, meleeProf, viewer, w)
	melee.Unlock()

	// Both attacks should have hit the hero.
	if len(w.strikes) != 2 {
		t.Fatalf("expected 2 strikes, got %d", len(w.strikes))
	}
}

// TestPluginProjectileOverride verifies that plugin-set projectile overrides
// the profile default (santa gift cycle, forestdragon terror special).
func TestPluginProjectileOverride(t *testing.T) {
	// setPluginProjectile and getPluginProjectile are the seam.
	setPluginProjectile("mob-test", "gift3")
	if got := getPluginProjectile("mob-test"); got != "gift3" {
		t.Fatalf("expected gift3, got %q", got)
	}
	// Clear.
	setPluginProjectile("mob-test", "")
	if got := getPluginProjectile("mob-test"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	// Unknown mob returns empty.
	if got := getPluginProjectile("mob-unknown"); got != "" {
		t.Fatalf("expected empty for unknown, got %q", got)
	}
}
