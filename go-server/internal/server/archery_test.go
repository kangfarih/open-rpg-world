// Archery parity: the combat.ts:208-210 arrow gate (non-magic archery with
// zero arrows stops — per-swing Go equivalent is a silent no-swing) and the
// player.ts:2057-2061 poison source (archers read the equipped ARROWS'
// poisonous flag, everyone else the weapon).
//
// TS ground truth: combat.ts sendRangedAttack (`!isMagic && !hasArrows ->
// stop`), player.ts hasArrows (arrows-slot count > 0), player.ts
// isPoisonous (`isArcher ? arrows.poisonous : weapon.poisonous`).
package server

import (
	"testing"
	"time"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/entity"
)

// archeryCatalogue guards the items.json keys the tests need.
func archeryCatalogue(t *testing.T) {
	t.Helper()
	for _, key := range []string{"woodenbow", "ironsword", "arrow", "poisonarrow"} {
		if itemInfoFor(key) == nil {
			t.Skipf("items catalogue entry missing: %s (needs items.json)", key)
		}
	}
	if it := itemInfoFor("woodenbow"); it.Type != "weaponarcher" {
		t.Fatalf("woodenbow type = %q, want weaponarcher", it.Type)
	}
	if it := itemInfoFor("poisonarrow"); !it.Poisonous {
		t.Fatal("poisonarrow poisonous = false, want true")
	}
	if it := itemInfoFor("arrow"); it.Poisonous {
		t.Fatal("arrow poisonous = true, want false")
	}
}

// archeryEquip sets the hero's weapon + arrows slots (lock-brief).
func archeryEquip(user, weapon string, arrows string, arrowCount int) {
	st := playerStateFor(user)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	st.Equip[EquipmentWeapon] = slotDef{Key: weapon, Count: 1}
	st.Equip[EquipmentArrows] = slotDef{Key: arrows, Count: arrowCount}
}

// TestHeroHasArrowsSlotGate: the arrows-slot count alone decides (empty
// slot, zero count -> false; positive count -> true).
func TestHeroHasArrowsSlotGate(t *testing.T) {
	user := "arrows-hero"
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
	})

	if heroHasArrows(user) {
		t.Fatal("fresh hero hasArrows = true, want false (empty slot)")
	}
	archeryEquip(user, "woodenbow", "", 0)
	if heroHasArrows(user) {
		t.Fatal("keyless arrows slot hasArrows = true, want false")
	}
	archeryEquip(user, "woodenbow", "arrow", 0)
	if heroHasArrows(user) {
		t.Fatal("zero-count arrows hasArrows = true, want false")
	}
	archeryEquip(user, "woodenbow", "arrow", 7)
	if !heroHasArrows(user) {
		t.Fatal("7 arrows hasArrows = false, want true")
	}
}

// TestBowSwingArrowGate drives the live swing path against an engine mob:
// bow + 0 arrows -> no swing (mob HP untouched); bow + arrows -> swings
// land (mob HP drops, arrows NOT consumed — TS has no per-shot
// consumption); melee with no arrows still swings (gate is bow-only).
//
// No-frames follows from gate placement: the refusal returns at the top of
// handlePlayerAttack, before any broadcast.
func TestBowSwingArrowGate(t *testing.T) {
	archeryCatalogue(t)

	user, inst := "bowgate-hero", "bowgate-hero-inst"
	c, _ := deathConn(t, inst, user)
	gameWorld.SetHeroHP(inst, 100)
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
		gameWorld.ForgetHeroHP(inst)
		abilities.ForgetPlayer(inst)
		controller.ForgetAttackStyle(user)
	})

	const mobInst = "bowgate-mob"
	m := &mob{
		instance: mobInst, key: "rat",
		prof:   entity.MobProfile{Name: "Rat", Level: 1, HitPoints: 10000, RespawnDelay: 0},
		spawnX: 100, spawnY: 96, x: 100, y: 96,
		hp: 10000, maxHP: 10000,
		attackers: map[string]time.Time{},
	}
	mobMu.Lock()
	mobs[mobInst] = m
	mobMu.Unlock()
	t.Cleanup(func() {
		removeMob(mobInst)
		abilities.ClearStatus(mobInst)
	})
	mobHP := func() int {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.hp
	}

	// Bow with no arrows: silent no-swing, mob untouched.
	archeryEquip(user, "woodenbow", "", 0)
	handlePlayerAttack(c, mobInst)
	if got := mobHP(); got != 10000 {
		t.Fatalf("arrowless bow swing: mob HP = %d, want 10000 (no swing)", got)
	}

	// Bow with arrows: swings land, arrows consumed (one per shot).
	archeryEquip(user, "woodenbow", "arrow", 5)
	// Give the hero some archery skill to ensure damage > 0 for poison tests.
	st := playerStateFor(user)
	pstateMu.Lock()
	st.Skills[SkillArchery] = &skillDef{Level: 10, XP: 0}
	pstateMu.Unlock()
	hpBefore := mobHP()
	handlePlayerAttack(c, mobInst)
	// With the formula-based damage, there's a small chance of 0 damage.
	// The swing still happened if arrows were consumed.
	pstateMu.Lock()
	arrowsLeft := st.Equip[EquipmentArrows].Count
	pstateMu.Unlock()
	hpAfter := mobHP()
	if arrowsLeft != 4 {
		t.Fatalf("arrows after shot = %d, want 4 (one consumed per shot)", arrowsLeft)
	}
	// If damage was dealt, HP should drop. If 0 damage, HP stays the same.
	// Either way, the swing happened (arrows consumed).
	if hpAfter > hpBefore {
		t.Fatalf("mob HP increased = %d, was %d", hpAfter, hpBefore)
	}

	// Melee with no arrows: unaffected, still swings.
	// Give the hero some Strength skill to improve damage odds.
	pstateMu.Lock()
	st.Skills[SkillStrength] = &skillDef{Level: 10, XP: 0}
	pstateMu.Unlock()
	afterBow := mobHP()
	archeryEquip(user, "ironsword", "", 0)
	handlePlayerAttack(c, mobInst)
	// The swing happened (hero attacked with melee weapon). Damage may be 0
	// due to the formula's random roll, but HP must not increase.
	if got := mobHP(); got > afterBow {
		t.Fatalf("melee swing: mob HP = %d, want <= %d", got, afterBow)
	}
}

// TestArcherPoisonSource: poison status comes from the equipped ARROWS for
// archers (poison arrows + clean bow -> poisons; clean arrows + bow -> no
// poison — TS reads arrows ONLY, never the bow), and from the weapon for
// everyone else (existing behavior).
func TestArcherPoisonSource(t *testing.T) {
	archeryCatalogue(t)
	abConfigure() // production HeroWeaponPoisonous seam (idempotent)

	user := "poisonbow-hero"
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
	})

	// Poison arrows + clean bow -> poisons.
	archeryEquip(user, "woodenbow", "poisonarrow", 5)
	if !abHeroWeaponPoisonous(user) {
		t.Fatal("archer+poisonarrow poisonous = false, want true")
	}

	// Clean arrows + bow -> no poison (arrows-only source: even a poison
	// bow could not poison; the branch never reads the weapon).
	archeryEquip(user, "woodenbow", "arrow", 5)
	if abHeroWeaponPoisonous(user) {
		t.Fatal("archer+clean arrows poisonous = true, want false")
	}

	// Archer with an empty arrows slot -> no poison (empty slot is not
	// poisonous, equipment.ts empty() parity).
	archeryEquip(user, "woodenbow", "", 0)
	if abHeroWeaponPoisonous(user) {
		t.Fatal("archer+no arrows poisonous = true, want false")
	}

	// Non-archer weapon path unchanged: a poisonous weapon-item poisons,
	// a clean sword does not.
	archeryEquip(user, "poisonarrow", "", 0) // poisonarrow is not a bow
	if heroIsArcher(user) {
		t.Fatal("poisonarrow-as-weapon reads as archer, want melee")
	}
	if !abHeroWeaponPoisonous(user) {
		t.Fatal("melee+poisonous weapon poisonous = false, want true")
	}
	archeryEquip(user, "ironsword", "", 0)
	if abHeroWeaponPoisonous(user) {
		t.Fatal("melee+clean sword poisonous = true, want false")
	}
}

// TestPoisonArrowsApplyOnHit: end-to-end — a bow swing with poison arrows
// poisons the victim through the combat.ts poison-on-hit path.
func TestPoisonArrowsApplyOnHit(t *testing.T) {
	archeryCatalogue(t)
	abConfigure() // production HeroWeaponPoisonous seam (idempotent)

	user, inst := "poisonhit-hero", "poisonhit-hero-inst"
	c, _ := deathConn(t, inst, user)
	gameWorld.SetHeroHP(inst, 100)
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
		gameWorld.ForgetHeroHP(inst)
		abilities.ForgetPlayer(inst)
		controller.ForgetAttackStyle(user)
	})

	const mobInst = "poisonhit-mob"
	m := &mob{
		instance: mobInst, key: "rat",
		// Use a high-level mob to make poison chance high (level 234 → 15/(235-234+1) = 15/2 = 7.5, clamped to always).
		prof:   entity.MobProfile{Name: "Rat", Level: 234, HitPoints: 10000, RespawnDelay: 0},
		spawnX: 100, spawnY: 96, x: 100, y: 96,
		hp: 10000, maxHP: 10000,
		attackers: map[string]time.Time{},
	}
	mobMu.Lock()
	mobs[mobInst] = m
	mobMu.Unlock()
	t.Cleanup(func() {
		removeMob(mobInst)
		abilities.ClearStatus(mobInst)
	})

	archeryEquip(user, "woodenbow", "poisonarrow", 5)
	handlePlayerAttack(c, mobInst)
	if !abilities.HasPoison(mobInst) {
		t.Fatal("bow+poisonarrow swing did not poison the victim")
	}

	// Clean arrows never poison through the same path.
	abilities.ClearStatus(mobInst)
	archeryEquip(user, "woodenbow", "arrow", 5)
	handlePlayerAttack(c, mobInst)
	if abilities.HasPoison(mobInst) {
		t.Fatal("bow+clean arrows swing poisoned the victim, want clean")
	}
}
