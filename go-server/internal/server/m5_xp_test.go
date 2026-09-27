package server

import (
	"testing"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/player"
)

// Negative /addexp awards subtract through the real m5 seam with XP clamped
// at 0 (TS addexp subtraction parity: setExperience(experience + x), floored
// so expToLevel never sees a negative). Uses a nil conn (no frames) with a
// unique key so no live state is touched; MarkDirty is a no-op without a DB.
func TestNegativeAddXPSubtractsAndClamps(t *testing.T) {
	key := "xpneg-hero"
	pstateMu.Lock()
	delete(pstates, key)
	pstateMu.Unlock()
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, key)
		pstateMu.Unlock()
	})

	d := xpDeps()
	if got := player.AddXP(d, nil, key, SkillFishing, 1000); got < 1 {
		t.Fatalf("AddXP(+1000) = %d, want a level", got)
	}
	st := playerStateFor(key)
	pstateMu.Lock()
	xp := st.Skills[SkillFishing].XP
	pstateMu.Unlock()
	if xp != 1000 {
		t.Fatalf("xp = %d, want 1000", xp)
	}

	player.AddXP(d, nil, key, SkillFishing, -300)
	pstateMu.Lock()
	xp = st.Skills[SkillFishing].XP
	pstateMu.Unlock()
	if xp != 700 {
		t.Fatalf("xp after -300 = %d, want 700", xp)
	}

	// Overshoot clamps at 0 with level floored at 1 (no negative store).
	player.AddXP(d, nil, key, SkillFishing, -99999)
	pstateMu.Lock()
	xp, level := st.Skills[SkillFishing].XP, st.Skills[SkillFishing].Level
	pstateMu.Unlock()
	if xp != 0 || level != 1 {
		t.Fatalf("xp/level after overshoot = %d/%d, want 0/1", xp, level)
	}

	// Zero stays a silent no-op returning 1.
	if got := player.AddXP(d, nil, key, SkillFishing, 0); got != 1 {
		t.Fatalf("AddXP(0) = %d, want 1", got)
	}
}

// awardCombatXP style/mana wiring (player.handleExperience parity,
// player.ts:990-1107): the equipped weapon's class routes first
// (heroIsArcher/heroIsMagic, exactly as the live swing sites pass),
// then the attack-style store (session switch, else the weapon's first
// style), with hasManaForAttack halving. Damage 10 -> xp 20.
func TestAwardCombatXPStyleAndManaWiring(t *testing.T) {
	for _, key := range []string{"goldsword", "woodenbow", "aquastaff"} {
		if itemInfoFor(key) == nil {
			t.Skip("items catalogue entry missing (needs items.json)")
		}
	}

	setup := func(user, inst, weapon string) *playerConn {
		c, _ := deathConn(t, inst, user)
		st := playerStateFor(user)
		pstateMu.Lock()
		if weapon != "" {
			st.Equip[EquipmentWeapon] = slotDef{Key: weapon, Count: 1}
		}
		pstateMu.Unlock()
		t.Cleanup(func() {
			pstateMu.Lock()
			delete(pstates, user)
			pstateMu.Unlock()
			abilities.ForgetPlayer(inst)
			controller.ForgetAttackStyle(user)
		})
		abilities.SetMana(inst, abilities.ManaMax(inst))
		return c
	}
	xpOf := func(user string, skill int) int {
		st := playerStateFor(user)
		pstateMu.Lock()
		defer pstateMu.Unlock()
		if s, ok := st.Skills[skill]; ok && s != nil {
			return s.XP
		}
		return 0
	}
	// Mirrors the live handlePlayerAttack call sites verbatim.
	swing := func(c *playerConn, user string, dmg int) {
		awardCombatXP(c, user, dmg, heroIsArcher(user), heroIsMagic(user))
	}

	// Sword defaults to its first style (Stab -> Accuracy). This is the
	// changed split: pre-parity melee always awarded Strength.
	c := setup("xpstyle-sword", "xpstyle-sword-inst", "goldsword")
	swing(c, "xpstyle-sword", 10)
	if got := xpOf("xpstyle-sword", SkillAccuracy); got != 15 {
		t.Fatalf("stab Accuracy xp = %d, want 15", got)
	}
	if got := xpOf("xpstyle-sword", SkillHealth); got != 5 {
		t.Fatalf("stab Health xp = %d, want 5", got)
	}
	if got := xpOf("xpstyle-sword", SkillStrength); got != 0 {
		t.Fatalf("stab Strength xp = %d, want 0", got)
	}

	// Explicit session Slash on the same sword -> Strength (unchanged).
	controller.UpdateAttackStyle(c, econDeps(), controller.AttackStyleSlash)
	swing(c, "xpstyle-sword", 10)
	if got := xpOf("xpstyle-sword", SkillStrength); got != 15 {
		t.Fatalf("slash Strength xp = %d, want 15", got)
	}

	// Bow routes by class ahead of any style -> Archery.
	bc := setup("xpstyle-bow", "xpstyle-bow-inst", "woodenbow")
	swing(bc, "xpstyle-bow", 10)
	if got := xpOf("xpstyle-bow", SkillArchery); got != 15 {
		t.Fatalf("bow Archery xp = %d, want 15", got)
	}
	if got := xpOf("xpstyle-bow", SkillHealth); got != 5 {
		t.Fatalf("bow Health xp = %d, want 5", got)
	}

	// Staff at full mana -> Magic, unhalved.
	mc := setup("xpstyle-staff", "xpstyle-staff-inst", "aquastaff")
	swing(mc, "xpstyle-staff", 10)
	if got := xpOf("xpstyle-staff", SkillMagic); got != 15 {
		t.Fatalf("staff Magic xp = %d, want 15", got)
	}

	// Staff at 1 mana (< aquastaff manaCost 2) -> floor(20/2)=10 pool:
	// Health ceil(10/4)=3, Magic ceil(10*0.75)=8.
	abilities.SetMana("xpstyle-staff-inst", 1)
	swing(mc, "xpstyle-staff", 10)
	if got := xpOf("xpstyle-staff", SkillMagic); got != 15+8 {
		t.Fatalf("low-mana staff Magic xp = %d, want %d", got, 15+8)
	}
	if got := xpOf("xpstyle-staff", SkillHealth); got != 5+3 {
		t.Fatalf("low-mana staff Health xp = %d, want %d", got, 5+3)
	}

	// Unarmed default (no weapon, no session style) -> Strength + Health
	// with the exact pre-parity amounts the combat e2e asserts.
	uc := setup("xpstyle-fists", "xpstyle-fists-inst", "")
	swing(uc, "xpstyle-fists", 10)
	if got := xpOf("xpstyle-fists", SkillStrength); got != 15 {
		t.Fatalf("unarmed Strength xp = %d, want 15", got)
	}
	if got := xpOf("xpstyle-fists", SkillHealth); got != 5 {
		t.Fatalf("unarmed Health xp = %d, want 5", got)
	}
}
