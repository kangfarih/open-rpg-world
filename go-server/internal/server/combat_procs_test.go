// Unit tests for the five ported TS combat behaviors (combat_procs.go).
//
// TS ground truth: character.ts heal gates (:312) + bloodsucking (:292),
// player.ts getDamageType (:2575) + getBloodsuckingLevel (:2633),
// formulas.ts getEffectChance (:397), player/handler.ts thorns (:192) +
// mana gate (:219), modules.ts HEAL_RATE (:627).
package server

import (
	"math/rand"
	"testing"
	"time"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/meta"
)

// ---------------------------------------------------------------------------
// 1. Passive regen gate matrix (character.ts:312, in gate order).
// ---------------------------------------------------------------------------

func TestRegenGateMatrix(t *testing.T) {
	cases := []struct {
		name                           string
		dead, poisoned, inCombat, full bool
		freezing, burning, terror      bool
		want                           bool
	}{
		{"idle hurt heals", false, false, false, false, false, false, false, true},
		{"dead never heals", true, false, false, false, false, false, false, false},
		{"poisoned never heals", false, true, false, false, false, false, false, false},
		{"in combat never heals", false, false, true, false, false, false, false, false},
		{"full never heals", false, false, false, true, false, false, false, false},
		{"freezing blocks", false, false, false, false, true, false, false, false},
		{"burning blocks", false, false, false, false, false, true, false, false},
		{"terror blocks", false, false, false, false, false, false, true, false},
		{"dead+full still false", true, false, false, true, false, false, false, false},
	}
	for _, tc := range cases {
		if got := regenEligible(tc.dead, tc.poisoned, tc.inCombat, tc.full, tc.freezing, tc.burning, tc.terror); got != tc.want {
			t.Errorf("%s: regenEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestHeroRegenSweepIdle: a hurt idle hero regens +1 HP with a Points sync;
// TestMobRegenSweepIdle: a hurt out-of-combat mob regens +1 HP.
func TestRegenSweepIdle(t *testing.T) {
	user, inst := "regen-hero", "regen-hero-inst"
	c, _ := deathConn(t, inst, user)
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
		gameWorld.ForgetHeroHP(inst)
		abilities.ForgetPlayer(inst)
	})
	gameWorld.SetHeroHP(inst, 60)
	drainOutbox(c)

	runRegenSweep()

	if got := gameWorld.GetHeroHP(inst); got != 61 {
		t.Fatalf("hurt idle hero HP = %d, want 61", got)
	}

	// Gated heroes do not regen: poison, combat target, full HP, freeze.
	abilities.ApplyPoison(inst)
	runRegenSweep()
	if got := gameWorld.GetHeroHP(inst); got != 61 {
		t.Fatalf("poisoned hero HP = %d, want 61", got)
	}
	abilities.RemovePoison(inst)

	abSetTarget(inst, "some-mob")
	runRegenSweep()
	if got := gameWorld.GetHeroHP(inst); got != 61 {
		t.Fatalf("in-combat hero HP = %d, want 61", got)
	}
	abSetTarget(inst, "")

	abilities.FreezeApply(inst)
	runRegenSweep()
	if got := gameWorld.GetHeroHP(inst); got != 61 {
		t.Fatalf("freezing hero HP = %d, want 61", got)
	}
	abilities.FreezeClear(inst)

	gameWorld.SetHeroHP(inst, meta.HeroMaxHPForLevel(1))
	runRegenSweep()
	if got := gameWorld.GetHeroHP(inst); got != meta.HeroMaxHPForLevel(1) {
		t.Fatalf("full hero HP = %d, want %d", got, meta.HeroMaxHPForLevel(1))
	}
}

func TestMobRegenSweepIdle(t *testing.T) {
	m := &mob{
		instance: "regen-mob", key: "rat",
		x: 100, y: 96, hp: 50, maxHP: 100,
		attackers: map[string]time.Time{},
	}
	mobMu.Lock()
	mobs[m.instance] = m
	mobMu.Unlock()
	t.Cleanup(func() {
		mobMu.Lock()
		delete(mobs, m.instance)
		mobMu.Unlock()
		abilities.ClearStatus(m.instance)
	})

	runRegenSweep()
	m.mu.Lock()
	hp := m.hp
	m.mu.Unlock()
	if hp != 51 {
		t.Fatalf("hurt idle mob HP = %d, want 51", hp)
	}

	// Fighting-idle mob (has attackers) does not regen.
	m.mu.Lock()
	m.attackers["hero-1"] = time.Now()
	m.mu.Unlock()
	runRegenSweep()
	m.mu.Lock()
	hp = m.hp
	m.mu.Unlock()
	if hp != 51 {
		t.Fatalf("attacked mob HP = %d, want 51", hp)
	}

	// Targeting mob (combat started) does not regen.
	m.mu.Lock()
	delete(m.attackers, "hero-1")
	m.target = "hero-1"
	m.mu.Unlock()
	runRegenSweep()
	m.mu.Lock()
	hp = m.hp
	m.mu.Unlock()
	if hp != 51 {
		t.Fatalf("targeting mob HP = %d, want 51", hp)
	}
	m.mu.Lock()
	m.target = ""
	m.mu.Unlock()

	// Full mob does not regen (and never exceeds max).
	m.mu.Lock()
	m.hp = m.maxHP
	m.mu.Unlock()
	runRegenSweep()
	m.mu.Lock()
	hp = m.hp
	m.mu.Unlock()
	if hp != m.maxHP {
		t.Fatalf("full mob HP = %d, want %d", hp, m.maxHP)
	}
}

// ---------------------------------------------------------------------------
// 2. Damage-type rolls (player.ts:2575, formulas.ts:397).
// ---------------------------------------------------------------------------

func TestEffectChanceDistribution(t *testing.T) {
	r := rand.New(rand.NewSource(1234))
	const n = 20000
	hits := 0
	for i := 0; i < n; i++ {
		if effectChance(r) {
			hits++
		}
	}
	rate := float64(hits) / float64(n)
	if rate < 0.03 || rate > 0.07 {
		t.Fatalf("effectChance rate = %.4f, want within [0.03, 0.07]", rate)
	}
}

func TestHeroDamageTypeCritical(t *testing.T) {
	user := "crit-hero"
	st := playerStateFor(user)
	pstateMu.Lock()
	st.Equip[EquipmentWeapon] = slotDef{Key: "plainsword", Count: 1, Ench: Enchantments{enchCritical: {Level: 1}}}
	pstateMu.Unlock()
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
	})

	r := rand.New(rand.NewSource(99))
	const n = 20000
	crits := 0
	for i := 0; i < n; i++ {
		typ, aoe := heroDamageType(user, r)
		if aoe != 0 {
			t.Fatalf("melee crit roll set aoe=%d, want 0", aoe)
		}
		switch typ {
		case HitsCritical:
			crits++
		case HitsNormal:
		default:
			t.Fatalf("melee roll type = %d, want Normal or Critical", typ)
		}
	}
	rate := float64(crits) / float64(n)
	if rate < 0.03 || rate > 0.07 {
		t.Fatalf("critical rate = %.4f, want within [0.03, 0.07]", rate)
	}
}

func TestHeroDamageTypePlain(t *testing.T) {
	user := "plain-hero"
	st := playerStateFor(user)
	pstateMu.Lock()
	st.Equip[EquipmentWeapon] = slotDef{Key: "plainsword", Count: 1}
	pstateMu.Unlock()
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
	})

	r := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		if typ, aoe := heroDamageType(user, r); typ != HitsNormal || aoe != 0 {
			t.Fatalf("unenchanted roll = (%d,%d), want (Normal,0)", typ, aoe)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Bloodsucking math (character.ts:292-297).
// ---------------------------------------------------------------------------

func TestBloodsuckHealMath(t *testing.T) {
	cases := []struct{ damage, level, want int }{
		{100, 1, 5},  // 5% of damage at level 1
		{100, 3, 15}, // 5% per level
		{8, 1, 0},    // floor -> below 1, no heal (TS heal<1 return)
		{20, 2, 2},   // floor(20*0.1)
		{100, 0, 5},  // level defaults to 1 (character.ts:714)
		{100, -2, 5}, // same default
	}
	for _, tc := range cases {
		if got := bloodsuckHeal(tc.damage, tc.level); got != tc.want {
			t.Errorf("bloodsuckHeal(%d,%d) = %d, want %d", tc.damage, tc.level, got, tc.want)
		}
	}
}

func TestBloodsuckRollDistribution(t *testing.T) {
	r := rand.New(rand.NewSource(4242))
	const n = 20000
	procs := 0
	for i := 0; i < n; i++ {
		if bloodsuckRoll(r) {
			procs++
		}
	}
	rate := float64(procs) / float64(n)
	// TS gate `randomInt(0,100)>30 return` fires on <=30: 31/101 ≈ 0.307.
	if rate < 0.26 || rate > 0.36 {
		t.Fatalf("bloodsuck rate = %.4f, want within [0.26, 0.36]", rate)
	}
}

func TestHeroBloodsuckingLookup(t *testing.T) {
	user := "suck-hero"
	st := playerStateFor(user)
	pstateMu.Lock()
	st.Equip[EquipmentWeapon] = slotDef{Key: "fangedge", Count: 1, Ench: Enchantments{enchBloodsucking: {Level: 3}}}
	pstateMu.Unlock()
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
	})
	if ok, lv := heroBloodsucking(user); !ok || lv != 3 {
		t.Fatalf("heroBloodsucking = (%v,%d), want (true,3)", ok, lv)
	}
	if ok, _ := heroBloodsucking("nobody-here"); ok {
		t.Fatal("heroBloodsucking unknown user = true, want false")
	}
}

// ---------------------------------------------------------------------------
// 4. Thorns math + loop guard (player/handler.ts:192-213).
// ---------------------------------------------------------------------------

func TestThornsReflectMath(t *testing.T) {
	cases := []struct{ damage, level, want int }{
		{100, 1, 10}, // 10% per level
		{100, 3, 30},
		{8, 1, 0},   // floor, may be 0 (TS passes it to hit() regardless)
		{55, 2, 11}, // floor(55*0.2)
	}
	for _, tc := range cases {
		if got := thornsReflect(tc.damage, tc.level); got != tc.want {
			t.Errorf("thornsReflect(%d,%d) = %d, want %d", tc.damage, tc.level, got, tc.want)
		}
	}
}

func TestThornsRollDistribution(t *testing.T) {
	r := rand.New(rand.NewSource(777))
	const n = 20000
	procs := 0
	for i := 0; i < n; i++ {
		if thornsRoll(r) {
			procs++
		}
	}
	rate := float64(procs) / float64(n)
	// TS gate `randomInt(0,100)>40 return` fires on <=40: 41/101 ≈ 0.406.
	if rate < 0.35 || rate > 0.46 {
		t.Fatalf("thorns rate = %.4f, want within [0.35, 0.46]", rate)
	}
}

// TestThornsLoopGuard: isThorns receipt never reflects, even with a thorns
// chestplate equipped (the endless-loop guard).
func TestThornsLoopGuard(t *testing.T) {
	user, inst := "thorns-hero", "thorns-hero-inst"
	c, _ := deathConn(t, inst, user)
	st := playerStateFor(user)
	pstateMu.Lock()
	st.Equip[EquipmentChestplate] = slotDef{Key: "plate", Count: 1, Ench: Enchantments{enchThorns: {Level: 5}}}
	pstateMu.Unlock()
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
		gameWorld.ForgetHeroHP(inst)
		abilities.ForgetPlayer(inst)
		mobMu.Lock()
		delete(mobs, "thorns-mob")
		mobMu.Unlock()
	})
	m := &mob{
		instance: "thorns-mob", key: "rat",
		x: 100, y: 96, hp: 100, maxHP: 100,
		attackers: map[string]time.Time{},
	}
	mobMu.Lock()
	mobs[m.instance] = m
	mobMu.Unlock()
	gameWorld.SetHeroHP(inst, 100)

	mobDamagePlayerThorns(c, 20, m, true) // thorns-flagged receipt

	m.mu.Lock()
	hp := m.hp
	m.mu.Unlock()
	if hp != 100 {
		t.Fatalf("isThorns receipt reflected: mob HP = %d, want 100", hp)
	}
}

// TestThornsReflectLive: a thorns chestplate reflects into the attacker
// through the damage pipeline (mob HP drops); dead heroes and missing
// attackers never reflect.
func TestThornsReflectLive(t *testing.T) {
	user, inst := "thorns-hero2", "thorns-hero2-inst"
	c, _ := deathConn(t, inst, user)
	st := playerStateFor(user)
	pstateMu.Lock()
	st.Equip[EquipmentChestplate] = slotDef{Key: "plate", Count: 1, Ench: Enchantments{enchThorns: {Level: 5}}}
	pstateMu.Unlock()
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
		gameWorld.ForgetHeroHP(inst)
		abilities.ForgetPlayer(inst)
		mobMu.Lock()
		delete(mobs, "thorns-mob2")
		mobMu.Unlock()
	})
	newMob := func() *mob {
		m := &mob{
			instance: "thorns-mob2", key: "rat",
			x: 100, y: 96, hp: 100, maxHP: 100,
			attackers: map[string]time.Time{},
		}
		mobMu.Lock()
		mobs[m.instance] = m
		mobMu.Unlock()
		return m
	}

	// Force the 40% roll deterministically: find a seed whose first roll
	// procs and one whose does not.
	var procSeed, missSeed int64 = -1, -1
	for s := int64(0); s < 200 && (procSeed < 0 || missSeed < 0); s++ {
		if rand.New(rand.NewSource(s)).Intn(101) <= 40 {
			if procSeed < 0 {
				procSeed = s
			}
		} else if missSeed < 0 {
			missSeed = s
		}
	}
	if procSeed < 0 || missSeed < 0 {
		t.Fatal("no deterministic thorns seeds found")
	}
	oldRand := combatRand
	defer func() { combatRand = oldRand }()

	gameWorld.SetHeroHP(inst, 100)
	m := newMob()
	combatRand = rand.New(rand.NewSource(procSeed))
	mobDamagePlayerThorns(c, 20, m, false)
	m.mu.Lock()
	hp := m.hp
	m.mu.Unlock()
	// reflect floor(20*5*0.1) = 10 through HitMob.
	if hp != 90 {
		t.Fatalf("thorns reflect mob HP = %d, want 90", hp)
	}

	// Dead hero: no reflect even on a proccing seed.
	gameWorld.SetHeroHP(inst, 100)
	m2 := newMob()
	m2.mu.Lock()
	m2.hp, m2.maxHP = 100, 100
	m2.mu.Unlock()
	combatRand = rand.New(rand.NewSource(procSeed))
	mobDamagePlayerThorns(c, 200, m2, false) // lethal to the hero
	m2.mu.Lock()
	hp = m2.hp
	m2.mu.Unlock()
	if hp != 100 {
		t.Fatalf("dead-hero reflect mob HP = %d, want 100", hp)
	}

	// No attacker: no reflect.
	gameWorld.SetHeroHP(inst, 100)
	combatRand = rand.New(rand.NewSource(procSeed))
	mobDamagePlayerThorns(c, 20, nil, false)
	if got := gameWorld.GetHeroHP(inst); got != 80 {
		t.Fatalf("attackerless hero HP = %d, want 80", got)
	}
}

// ---------------------------------------------------------------------------
// 5. Magic mana gate (player/handler.ts:219).
// ---------------------------------------------------------------------------

func TestHeroMagicGate(t *testing.T) {
	it := itemInfoFor("aquastaff")
	if it == nil || it.Type != "weaponmagic" {
		t.Skip("aquastaff catalogue entry missing (needs items.json)")
	}
	if it.ManaCost != 2 {
		t.Fatalf("aquastaff manaCost = %d, want 2", it.ManaCost)
	}
	user, inst := "mage-hero", "mage-hero-inst"
	c, _ := deathConn(t, inst, user)
	st := playerStateFor(user)
	pstateMu.Lock()
	st.Equip[EquipmentWeapon] = slotDef{Key: "aquastaff", Count: 1}
	pstateMu.Unlock()
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
		abilities.ForgetPlayer(inst)
	})
	if !heroIsMagic(user) {
		t.Fatal("aquastaff hero is not magic")
	}

	// Zero mana: no swing, exactly one LOW_MANA notify.
	abilities.SetMana(inst, 0)
	drainOutbox(c)
	if heroMagicGate(c) {
		t.Fatal("zero-mana staff swing allowed, want refused")
	}
	if heroMagicGate(c) {
		t.Fatal("zero-mana staff swing allowed on retry, want refused")
	}
	notifies := 0
	for _, f := range drainOutbox(c) {
		if len(f) == 3 {
			if id, ok := f[0].(int); ok && id == PacketNotification {
				notifies++
			}
		}
	}
	if notifies != 1 {
		t.Fatalf("LOW_MANA notifies = %d, want exactly 1", notifies)
	}

	// Restored mana: swing allowed, cost consumed, warning reset.
	abilities.SetMana(inst, abilities.ManaMax(inst))
	drainOutbox(c)
	if !heroMagicGate(c) {
		t.Fatal("full-mana staff swing refused, want allowed")
	}
	if mana, _ := abilities.ManaState(inst); mana != abilities.ManaMax(inst)-it.ManaCost {
		t.Fatalf("mana after swing = %d, want %d", mana, abilities.ManaMax(inst)-it.ManaCost)
	}
	if abilities.ManaWarningShown(inst) {
		t.Fatal("mana warning still shown after funded swing")
	}

	// Non-magic hero: gate always passes.
	plain, plainInst := "plain-mage", "plain-mage-inst"
	pc, _ := deathConn(t, plainInst, plain)
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, plain)
		pstateMu.Unlock()
		abilities.ForgetPlayer(plainInst)
	})
	abilities.SetMana(plainInst, 0)
	if !heroMagicGate(pc) {
		t.Fatal("non-magic swing gated, want allowed")
	}
}
