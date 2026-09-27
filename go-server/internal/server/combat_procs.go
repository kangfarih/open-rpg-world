// Combat procs — five audited-missing TS behaviors (behavior change
// INTENDED; everything else frozen).
//
// TS sources (read first; numbers/strings/gates are exact):
//  1. Passive regen: character.ts:40 (HEAL_RATE=7000 via modules.ts:627),
//     :100 (7s interval), :312 heal(amount=1) gates — dead/poisoned,
//     combat.started, attacker count, full, freezing/burning/terror.
//  2. Hero damage-type rolls: player.ts:2575 getDamageType,
//     formulas.ts:397 getEffectChance (randomInt(0,100)<5, i.e. 5%).
//  3. Bloodsucking proc: character.ts:292 (30% gate `randomInt(0,100)>30
//     return`, heal floor(damage*0.05*level), players via Heal packet).
//  4. Thorns reflect: player/handler.ts:192 (dead/no-attacker guard,
//     isThorns loop guard, chestplate Thorns level, 40% gate
//     `randomInt(0,100)>40 return`, reflect floor(damage*level*0.1) via
//     attacker.hit(thornsDamage, player) — WITHOUT isThorns=true).
//  5. Magic mana cost: player/handler.ts:219 (weapon manaCost,
//     hasManaForAttack gate, LOW_MANA notify once via
//     displayedManaWarning flag).
//
// No packet-shape inventions: Combat Hit / Points / Heal / Effect frames
// reuse the frozen builders. No balance changes beyond the mechanics.
// One ticker: regen rides the central 20Hz Engine (boot.go tickEngine)
// throttled to HEAL_RATE — no per-entity goroutines.
package server

import (
	"log"
	"math"
	"math/rand"
	"sync"
	"time"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/meta"
	worldcore "rpg-world-server/internal/world"
)

// Enchantment IDs (modules.ts Enchantment enum order: Bloodsucking0,
// Critical1, Evasion2, Thorns3, Explosive4, Stun5, AntiStun6, Splash7,
// DoubleEdged8 — controller trade.go:928-932 carries the same mapping).
const (
	enchBloodsucking = 0
	enchCritical     = 1
	enchThorns       = 3
	enchExplosive    = 4
	enchStun         = 5
)

// Effects IDs (modules.ts Effects enum) for the regen gates + stun apply.
const (
	fxTerror       = 2
	fxTerrorStatus = 3
	fxStun         = 4
	fxRunning      = 10
	fxHotSauce     = 11
	fxDualistsMark = 12
	fxThickSkin    = 13 // abilities EffectThickSkin (abilities.go EffectKind)
	fxSnowPotion   = 14
	fxFirePotion   = 15
	fxBurning      = 16
	fxFreezing     = 17
	fxInvincible   = 18
)

// Buff/debuff effect IDs (modules.ts Effects enum) for combat formula
// multipliers (formulas.ts getMaxDamage / getDamage accuracy adjustments).
const (
	fxAccuracyBuff     = controller.EffectAccuracyBuff     // 19
	fxStrengthBuff     = controller.EffectStrengthBuff     // 20
	fxDefenseBuff      = controller.EffectDefenseBuff      // 21
	fxAccuracySuperBuf = controller.EffectAccuracySuperBuf // 24
	fxStrengthSuperBuf = controller.EffectStrengthSuperBuf // 25
	fxDefenseSuperBuf  = controller.EffectDefenseSuperBuf  // 26
)

const (
	// healRateInterval is Modules.Constants.HEAL_RATE (modules.ts:627).
	healRateInterval = 7 * time.Second
	// stunDurationMs is Modules.Constants.STUN_DURATION (modules.ts:653).
	stunDurationMs = int64(10_000)
	// dotStatusDurationMs is FREEZING/BURNING_DURATION (modules.ts:649-650).
	dotStatusDurationMs = int64(60_000)
)

// combatRand is the proc-roll source (TS Utils.randomInt parity: inclusive
// bounds — randomInt(min,max) = min+floor(rand*(max-min+1))). Tests pass
// seeded sources; live rolls use this one.
//
// Shared across conn goroutines and combat tick goroutines: math/rand.Rand
// is not safe for concurrent use, so every live roll goes through
// combatRandMu (see lockedHeroDamageType/lockedBloodsuckRoll/lockedThornsRoll
// below). Pure helpers keep their *rand.Rand param for deterministic tests.
var (
	combatRand   = rand.New(rand.NewSource(time.Now().UnixNano()))
	combatRandMu sync.Mutex
)

// lockedHeroDamageType rolls the hero damage type under the shared-rand lock.
func lockedHeroDamageType(username string) (hitType, aoe int) {
	combatRandMu.Lock()
	defer combatRandMu.Unlock()
	return heroDamageType(username, combatRand)
}

// lockedBloodsuckRoll rolls the bloodsucking proc under the shared-rand lock.
func lockedBloodsuckRoll() bool {
	combatRandMu.Lock()
	defer combatRandMu.Unlock()
	return bloodsuckRoll(combatRand)
}

// lockedThornsRoll rolls the thorns proc under the shared-rand lock.
func lockedThornsRoll() bool {
	combatRandMu.Lock()
	defer combatRandMu.Unlock()
	return thornsRoll(combatRand)
}

// effectChance ports Formulas.getEffectChance (formulas.ts:397):
// randomInt(0,100)<5, i.e. a 5% roll.
func effectChance(r *rand.Rand) bool {
	return r.Intn(101) < 5
}

// hasEnch reports whether an equipment enchantment map carries id.
func hasEnch(ench Enchantments, id int) bool {
	if ench == nil {
		return false
	}
	_, ok := ench[id]
	return ok
}

// enchLevel reports the enchantment level (0 when absent).
func enchLevel(ench Enchantments, id int) int {
	if ench == nil {
		return 0
	}
	if e, ok := ench[id]; ok {
		return e.Level
	}
	return 0
}

// heroEquipSlot snapshots one equipment slot (lock-brief; returns zero on
// missing/short rows — playerStateFor normalizes the 12-slot array).
func heroEquipSlot(username string, slot int) slotDef {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if slot < 0 || slot >= len(st.Equip) {
		return slotDef{}
	}
	return st.Equip[slot]
}

// heroIsArcher ports player/character isArcher for the hero: the equipped
// weapon is archer-typed (item.ts isArcherWeapon — type `weaponarcher`).
// Unknown/absent weapons are melee.
func heroIsArcher(username string) bool {
	w := heroEquipSlot(username, EquipmentWeapon)
	if w.Key == "" {
		return false
	}
	it := itemInfoFor(w.Key)
	return it != nil && it.Type == "weaponarcher"
}

// heroIsMagic ports weapon.isMagic (item.ts isMagicWeapon — type
// `weaponmagic`) for the hero's equipped weapon.
func heroIsMagic(username string) bool {
	w := heroEquipSlot(username, EquipmentWeapon)
	if w.Key == "" {
		return false
	}
	it := itemInfoFor(w.Key)
	return it != nil && it.Type == "weaponmagic"
}

// heroHasArrows ports player.hasArrows (player.ts:1708-1711) for the hero:
// the equipped arrows-slot count is above zero. (TS additionally bypasses
// the check while the tutorial is unfinished; the hero models the slot
// only — an empty arrows slot never lets a bow swing.)
func heroHasArrows(username string) bool {
	a := heroEquipSlot(username, EquipmentArrows)
	return a.Count > 0
}

// heroManaCost ports equipment.getWeapon().manaCost (weapon.ts:54 from
// items.json `manaCost`) for the hero's equipped weapon (0 when none).
func heroManaCost(username string) int {
	w := heroEquipSlot(username, EquipmentWeapon)
	if w.Key == "" {
		return 0
	}
	if it := itemInfoFor(w.Key); it != nil {
		return it.ManaCost
	}
	return 0
}

// heroBloodsucking ports player.hasBloodsucking + getBloodsuckingLevel
// (player.ts:1718 + :2633): weapon Bloodsucking enchantment present, level
// `...?.level || 1` (default 1 per character.ts:714).
func heroBloodsucking(username string) (bool, int) {
	w := heroEquipSlot(username, EquipmentWeapon)
	if w.Key == "" || !hasEnch(w.Ench, enchBloodsucking) {
		return false, 0
	}
	if lv := enchLevel(w.Ench, enchBloodsucking); lv > 0 {
		return true, lv
	}
	return true, 1
}

// heroThornsLevel ports chestplate.getThornsLevel (impl/chestplate.ts:25):
// 0 when the chestplate lacks the Thorns enchantment.
func heroThornsLevel(username string) int {
	c := heroEquipSlot(username, EquipmentChestplate)
	if c.Key == "" || !hasEnch(c.Ench, enchThorns) {
		return 0
	}
	return enchLevel(c.Ench, enchThorns)
}

// heroDamageType ports player.getDamageType (player.ts:2575) over the
// hero's equipped weapon + arrows. Returns the hit type and the AoE radius
// (explosive sets aoe=1 on the attacker, character.ts:61 + player.ts:2595).
// Damage numbers are untouched (the existing 8-12 roll shape stays); only
// the TYPE (+ AoE flag) changes. Rolls use effectChance (5%) each, in TS
// order. Lookups skip gracefully when items/equipment are absent.
func heroDamageType(username string, r *rand.Rand) (hitType, aoe int) {
	weapon := heroEquipSlot(username, EquipmentWeapon)
	if heroIsArcher(username) {
		arrows := heroEquipSlot(username, EquipmentArrows)
		if arrows.Key != "" {
			if it := itemInfoFor(arrows.Key); it != nil {
				if it.Freezing && effectChance(r) {
					return HitsFreezing, 0
				}
				if it.Burning && effectChance(r) {
					return HitsBurning, 0
				}
			}
		}
		if hasEnch(weapon.Ench, enchStun) && effectChance(r) {
			return HitsStun, 0
		}
		if hasEnch(weapon.Ench, enchExplosive) && effectChance(r) {
			return HitsExplosive, 1
		}
	} else if hasEnch(weapon.Ench, enchCritical) && effectChance(r) {
		return HitsCritical, 0
	}
	return HitsNormal, 0
}

// bloodsuckRoll ports the character.ts:294 gate (`randomInt(0,100) > 30
// return`): the proc fires on rolls <= 30.
func bloodsuckRoll(r *rand.Rand) bool {
	return r.Intn(101) <= 30
}

// bloodsuckHeal ports character.ts:297: 5% of damage per bloodsucking
// level, floored. Level defaults to 1 (character.ts:714).
func bloodsuckHeal(damage, level int) int {
	if level < 1 {
		level = 1
	}
	return int(math.Floor(float64(damage) * (0.05 * float64(level))))
}

// thornsRoll ports the player/handler.ts:206 gate (`randomInt(0,100) > 40
// return`): thorns fires on rolls <= 40.
func thornsRoll(r *rand.Rand) bool {
	return r.Intn(101) <= 40
}

// thornsReflect ports player/handler.ts:209: 10% of damage per thorns
// level, floored.
func thornsReflect(damage, level int) int {
	return int(math.Floor(float64(damage) * float64(level) * 0.1))
}

// heroMagicGate ports player/handler.ts:219 handleAttack for staff/magic
// swings: weapon manaCost via hasManaForAttack (player.ts:1700 — mana >=
// manaCost), `misc:LOW_MANA` notify once via the displayedManaWarning flag
// (warn + block the swing when short; clear the flag + consume on swing
// when mana suffices). Reports false when the swing must not happen.
func heroMagicGate(c *playerConn) bool {
	if c == nil || !heroIsMagic(c.Username) {
		return true
	}
	cost := heroManaCost(c.Username)
	if abilities.ManaFor(c.Instance) < cost {
		if !abilities.ManaWarningShown(c.Instance) {
			notifyPlayer(c, "misc:LOW_MANA")
			abilities.SetManaWarning(c.Instance, true)
		}
		log.Printf("m5: %s staff swing refused (low mana)", c.Instance)
		return false
	}
	abilities.SetManaWarning(c.Instance, false)
	abilities.SpendMana(c.Instance, cost)
	return true
}

// heroTotalBonuses sums the equipment bonuses across all 12 equipped slots
// (TS equipments.ts calculateStats totalBonuses aggregation parity).
func heroTotalBonuses(username string) controller.ItemBonuses {
	var b controller.ItemBonuses
	for slot := 0; slot < ModulesEquipmentCount; slot++ {
		eq := heroEquipSlot(username, slot)
		if eq.Key == "" {
			continue
		}
		if it := itemInfoFor(eq.Key); it != nil {
			b.Accuracy += it.Bonuses.Accuracy
			b.Strength += it.Bonuses.Strength
			b.Archery += it.Bonuses.Archery
			b.Magic += it.Bonuses.Magic
		}
	}
	return b
}

// heroDamageBonus ports player.ts getDamageBonus: returns the equipment bonus
// matching the hero's attack type (magic→magic bonus, archer→archery bonus,
// melee→strength bonus).
func heroDamageBonus(username string) int {
	b := heroTotalBonuses(username)
	if heroIsMagic(username) {
		return b.Magic
	}
	if heroIsArcher(username) {
		return b.Archery
	}
	return b.Strength
}

// heroSkillDamageLevel ports player.ts getSkillDamageLevel: returns the
// hero's skill level for the matching attack school.
func heroSkillDamageLevel(username string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	var skillID int
	switch {
	case heroIsMagicU(username, st):
		skillID = SkillMagic
	case heroIsArcherU(username, st):
		skillID = SkillArchery
	default:
		skillID = SkillStrength
	}
	if s, ok := st.Skills[skillID]; ok {
		return s.Level
	}
	return 1
}

// heroIsArcherU / heroIsMagicU are unlocked variants that take a pre-loaded
// playerState to avoid double-locking when the caller already holds pstateMu.
func heroIsArcherU(username string, st *playerState) bool {
	if len(st.Equip) <= EquipmentWeapon {
		return false
	}
	w := st.Equip[EquipmentWeapon]
	if w.Key == "" {
		return false
	}
	it := itemInfoFor(w.Key)
	return it != nil && it.Type == "weaponarcher"
}

func heroIsMagicU(username string, st *playerState) bool {
	if len(st.Equip) <= EquipmentWeapon {
		return false
	}
	w := st.Equip[EquipmentWeapon]
	if w.Key == "" {
		return false
	}
	it := itemInfoFor(w.Key)
	return it != nil && it.Type == "weaponmagic"
}

// heroStrengthBuffMult returns the strength buff damage multiplier
// (formulas.ts:191-194): 1.1 for StrengthBuff, 1.15 for StrengthSuperBuff,
// multiplicative when both are present.
func heroStrengthBuffMult(instance string) float64 {
	mult := 1.0
	if abilities.HasStatusEffect(instance, fxStrengthBuff) {
		mult *= 1.1
	}
	if abilities.HasStatusEffect(instance, fxStrengthSuperBuf) {
		mult *= 1.15
	}
	return mult
}

// heroAccuracyBuffAdj returns the accuracy adjustment from active buffs and
// debuffs (formulas.ts:124-135). Negative = better accuracy (skews damage
// toward max), positive = worse.
func heroAccuracyBuffAdj(attackerInst, targetInst string) float64 {
	adj := 0.0
	// Attacker buffs (lower accuracy weight = better).
	if abilities.HasStatusEffect(attackerInst, fxAccuracyBuff) {
		adj -= 0.07
	}
	if abilities.HasStatusEffect(attackerInst, fxAccuracySuperBuf) {
		adj -= 0.12
	}
	// Attacker terror (worse accuracy).
	if abilities.HasStatusEffect(attackerInst, fxTerror) {
		adj += 1.0
	}
	// Target buffs/debuffs (higher accuracy weight = worse for attacker).
	if targetInst != "" {
		if abilities.HasStatusEffect(targetInst, fxDefenseBuff) {
			adj += 0.08
		}
		if abilities.HasStatusEffect(targetInst, fxDefenseSuperBuf) {
			adj += 0.12
		}
		if abilities.HasStatusEffect(targetInst, fxTerror) {
			adj -= 0.4
		}
	}
	return adj
}

// heroTotalAttackStats sums the per-item AttackStats across all 12 equipped
// slots (formulas.ts getAccuracyWeight attacker stats aggregation via
// character.ts equipment stats).
func heroTotalAttackStats(username string) controller.ItemAttackStats {
	var s controller.ItemAttackStats
	for slot := 0; slot < ModulesEquipmentCount; slot++ {
		eq := heroEquipSlot(username, slot)
		if eq.Key == "" {
			continue
		}
		if it := itemInfoFor(eq.Key); it != nil {
			s.Crush += it.AttackStats.Crush
			s.Slash += it.AttackStats.Slash
			s.Stab += it.AttackStats.Stab
			s.Archery += it.AttackStats.Archery
			s.Magic += it.AttackStats.Magic
		}
	}
	return s
}

// heroTotalDefenseStats sums the per-item DefenseStats across all 12 equipped
// slots (formulas.ts getAccuracyWeight target defense stats).
func heroTotalDefenseStats(username string) controller.ItemDefenseStats {
	var s controller.ItemDefenseStats
	for slot := 0; slot < ModulesEquipmentCount; slot++ {
		eq := heroEquipSlot(username, slot)
		if eq.Key == "" {
			continue
		}
		if it := itemInfoFor(eq.Key); it != nil {
			s.Crush += it.DefenseStats.Crush
			s.Slash += it.DefenseStats.Slash
			s.Stab += it.DefenseStats.Stab
			s.Archery += it.DefenseStats.Archery
			s.Magic += it.DefenseStats.Magic
		}
	}
	return s
}

// heroDefenseSkillLevel returns the hero's defense skill level (0 → 1,
// matching the Node default for unknown players).
func heroDefenseSkillLevel(username string) int {
	st := playerStateFor(username)
	if st == nil {
		return 1
	}
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if sk, ok := st.Skills[SkillDefense]; ok && sk.Level > 0 {
		return sk.Level
	}
	return 1
}

// heroDamageReduction computes the multiplicative damage reduction factor
// (formulas.ts getDamageReduction): ThickSkin ability × attack-style
// reduction. Returns a multiplier in (0, 1]; lower = more protection.
//   - ThickSkin: ×0.80 (ability Power -0.2)
//   - Defensive style: ×0.90
//   - Shared style: ×0.96
func heroDamageReduction(username, instance string) float64 {
	mult := 1.0
	if abilities.HasStatusEffect(instance, fxThickSkin) {
		mult *= 0.80
	}
	style := controller.AttackStyleFor(econDeps(), username)
	switch style {
	case controller.AttackStyleDefensive:
		mult *= 0.90
	case controller.AttackStyleShared:
		mult *= 0.96
	}
	return mult
}

// heroCombatMaxDamage ports formulas.ts getMaxDamage for the hero:
// (damageBonus + damageLevel) * 1.25, +5 player bonus, *attack-style mult,
// *strength-buff mult, floored at 0.
func heroCombatMaxDamage(username, instance string, critical bool) float64 {
	bonus := heroDamageBonus(username)
	level := heroSkillDamageLevel(username)
	styleInt := controller.AttackStyleFor(econDeps(), username)
	var styleName string
	switch styleInt {
	case controller.AttackStyleSlash:
		styleName = "slash"
	case controller.AttackStyleCrush:
		styleName = "crush"
	case controller.AttackStyleShared:
		styleName = "shared"
	}
	dmg := meta.MaxDamageFloat(bonus, level, styleName, critical)
	dmg *= heroStrengthBuffMult(instance)
	if dmg < 0 {
		dmg = 0
	}
	return dmg
}

// heroCombatAccuracy ports formulas.ts getDamage accuracy computation for
// the hero: MAX_ACCURACY + equipment bonus term + skill level term +
// target defense term + stat-weight term, +/- buff adjustments, crit adj,
// clamped to [MinAccuracy, ClampAccuracy].
// Note: hero→mob path uses zero defense stats for the target (mobs lack
// per-style defense stats in the player sense); triangle advantage for
// mob→hero attacks is applied in entity.RollMobDamage via PlayerView
// defense stats.
func heroCombatAccuracy(username, instance, targetInst string, critical bool) float64 {
	bonuses := heroTotalBonuses(username)
	level := heroSkillDamageLevel(username)
	archer := heroIsArcher(username)
	magic := heroIsMagic(username)
	weight := meta.AccuracyWeight(archer, magic, 0, 0, 0, 0, 0)
	acc := meta.Accuracy(meta.MaxAccuracy, meta.MaxLevel, bonuses.Accuracy, level, 1, weight, critical)
	// Apply buff/debuff adjustments.
	acc += heroAccuracyBuffAdj(instance, targetInst)
	if acc < meta.MinAccuracy {
		acc = meta.MinAccuracy
	}
	if acc > meta.ClampAccuracy {
		acc = meta.ClampAccuracy
	}
	return acc
}

// heroDamageRoll computes the final hero damage using the TS-accurate formula:
// accuracy-weighted random roll on [0, maxDamage]. Returns the damage value.
func heroDamageRoll(username, instance, targetInst string, r *rand.Rand) int {
	critRoll := r.Float64()
	critical := meta.RollCrit(critRoll)
	maxDmg := heroCombatMaxDamage(username, instance, critical)
	acc := heroCombatAccuracy(username, instance, targetInst, critical)
	roll := r.Float64()
	return meta.RollDamage(maxDmg, acc, roll, -1) // -1 = skip HP clamp (caller handles)
}

// heroPoisonChance ports formulas.ts getPoisonChance + character.ts:279:
// the target's poison resistance is randomInt(0, 235-level), compared against
// the attacker's fixed POISON_CHANCE (15). Returns true if the target is
// poisoned (target roll < attacker chance).
func heroPoisonChance(targetLevel int) bool {
	combatRandMu.Lock()
	defer combatRandMu.Unlock()
	resistance := combatRand.Intn(235 - targetLevel + 1) // randomInt(0, 235-level) inclusive
	return resistance < 15                                // Modules.Defaults.POISON_CHANCE
}

// heroDecrementArrows ports equipments.ts decrementArrows: removes one arrow
// from the equipped arrow slot, clearing the slot when count reaches 0.
func heroDecrementArrows(username string) {
	pstateMu.Lock()
	defer pstateMu.Unlock()
	st, ok := pstates[username]
	if !ok || len(st.Equip) <= EquipmentArrows {
		return
	}
	a := &st.Equip[EquipmentArrows]
	if a.Count <= 0 {
		return
	}
	a.Count--
	if a.Count < 1 {
		a.Key = ""
		a.Ench = nil
	}
}

// applyHitStatus ports combat.ts:198 (sendAttack: hit, then
// target.addStatusEffect(hit)) for status-carrying hero hit types — Stun,
// Freezing, Burning via the existing status pipeline (Effect Add frame +
// tracker entry). Normal/Critical/Explosive carry no status (character.ts
// addStatusEffect returns early on Normal; Terror is mob-side only). Skips
// dead victims (TS death clears status).
//
// Potion immunity (TS character.ts addStatusEffect): Freezing is blocked by
// SnowPotion, Burning is blocked by FirePotion. The potion application
// (useSnowPotion/useFirePotion in controller/items.go) already removes the
// corresponding DoT; this guard prevents re-application while the potion
// is active.
func applyHitStatus(target string, hitType int) {
	if target == "" {
		return
	}
	var effect int
	var duration int64
	switch hitType {
	case HitsStun:
		effect, duration = fxStun, stunDurationMs
	case HitsFreezing:
		// SnowPotion blocks Freezing (TS addStatusEffect immunity check).
		if abilities.HasStatusEffect(target, fxSnowPotion) {
			return
		}
		// Permanent (area) Freezing guard (TS hasPermanentFreezing):
		// when the player is already standing in a freeze area, timed
		// Freezing from combat is suppressed (the area DoT covers it).
		if abilities.HasFreeze(target) {
			return
		}
		effect, duration = fxFreezing, dotStatusDurationMs
	case HitsBurning:
		// FirePotion blocks Burning (TS addStatusEffect immunity check).
		if abilities.HasStatusEffect(target, fxFirePotion) {
			return
		}
		effect, duration = fxBurning, dotStatusDurationMs
	default:
		return
	}
	abilities.ApplyStatusEffect(target, effect, duration)
}

// explosiveSplash ports character.ts handleAoE for a hero's explosive
// swing: every OTHER engine mob within Chebyshev 1 of the victim takes
// floor(damage/(manhattan+1)) (distance = getDistance+1, hit.ts aoe range)
// through the full damage pipeline (Combat Hit frame + HitMob Points/death/
// loot). The primary victim is excluded (already struck). PvP filtering has
// no stub counterpart (hero swings never target heroes), so only mobs splash.
func explosiveSplash(victim *mob, attacker *playerConn, dmg int) {
	if victim == nil || dmg < 1 {
		return
	}
	victim.mu.Lock()
	vx, vy := victim.x, victim.y
	victim.mu.Unlock()
	var av *entity.PlayerView
	if attacker != nil {
		av = &entity.PlayerView{Instance: attacker.Instance, Username: attacker.Username}
	}
	mobMu.Lock()
	type splash struct {
		m   *mob
		dmg int
	}
	var hits []splash
	for _, m := range mobs {
		if m == victim {
			continue
		}
		m.mu.Lock()
		if m.dead {
			m.mu.Unlock()
			continue
		}
		mx, my := m.x, m.y
		m.mu.Unlock()
		if entity.Cheb(mx, my, vx, vy) > 1 {
			continue
		}
		sec := int(math.Floor(float64(dmg) / float64(entity.Manhattan(mx, my, vx, vy)+1)))
		if sec < 1 {
			continue
		}
		hits = append(hits, splash{m: m, dmg: sec})
	}
	mobMu.Unlock()
	attackerInst := ""
	if attacker != nil {
		attackerInst = attacker.Instance
	}
	for _, h := range hits {
		h.m.mu.Lock()
		target := h.m.instance
		h.m.mu.Unlock()
		worldcore.Broadcast(pktOp(PacketCombat, CombatHit, combatData{
			Instance: attackerInst, Target: target,
			Hit: HitData{Type: HitsNormal, Damage: h.dmg},
		}))
		entity.HitMob(h.m, av, h.dmg, gameWorld, time.Now(), func() bool {
			return mobAlive(h.m)
		})
	}
}

// regenEligible ports the character.ts:312 heal() gates in order: dead or
// poisoned → no; in combat (combat.started / attacker count — heroes reuse
// the live-target + mob-targeting seam, mobs their target/attackers map) →
// no; full → no; freezing/burning/terror → no.
func regenEligible(dead, poisoned, inCombat, full, freezing, burning, terror bool) bool {
	if dead || poisoned {
		return false
	}
	if inCombat {
		return false
	}
	if full {
		return false
	}
	if freezing || burning || terror {
		return false
	}
	return true
}

// heroRegenEligible evaluates the regen gates for one live hero instance.
func heroRegenEligible(instance string, hp int) bool {
	maxHP := gameWorld.HeroMaxHP(instance)
	return regenEligible(
		hp <= 0,
		abilities.HasPoison(instance),
		econVitals{}.InCombat(instance),
		hp >= maxHP,
		abilities.HasStatusEffect(instance, fxFreezing) || abilities.HasFreeze(instance),
		abilities.HasStatusEffect(instance, fxBurning),
		abilities.HasStatusEffect(instance, fxTerror),
	)
}

// runRegenSweep heals every eligible hero and engine mob by +1 HP
// (character.ts:312 heal(amount=1) → hitPoints.increment(1)) reusing the
// existing Points sync paths (hero HeroPoints seam, mob MobPoints path).
// Lock order mobMu -> m.mu (mobTick parity); tracker/registry calls are leaf
// (their own mutexes, never reversed).
func runRegenSweep() {
	for _, c := range worldcore.AllOf[*playerConn]() {
		hp := gameWorld.GetHeroHP(c.Instance)
		if !heroRegenEligible(c.Instance, hp) {
			continue
		}
		gameWorld.SetHeroHP(c.Instance, hp+1)
		gameWorld.HeroPoints(c.Instance, hp+1, gameWorld.HeroMaxHP(c.Instance))
	}
	mobMu.Lock()
	defer mobMu.Unlock()
	for _, m := range mobs {
		m.mu.Lock()
		if m.dead {
			m.mu.Unlock()
			continue
		}
		eligible := regenEligible(
			false,
			abilities.HasPoison(m.instance),
			m.target != "" || len(m.attackers) > 0,
			m.hp >= m.maxHP,
			abilities.HasStatusEffect(m.instance, fxFreezing),
			abilities.HasStatusEffect(m.instance, fxBurning),
			abilities.HasStatusEffect(m.instance, fxTerror),
		)
		if !eligible {
			m.mu.Unlock()
			continue
		}
		m.hp++
		hp, max, inst := m.hp, m.maxHP, m.instance
		m.mu.Unlock()
		gameWorld.MobPoints(inst, hp, max)
	}
}

// regenLastSweep throttles the Engine-driven sweep to HEAL_RATE.
var regenLastSweep time.Time

// combatRegenTick is the Engine subsystem entry (boot.go tickEngine): the
// ONE ticker for passive regen — no per-entity goroutines.
func combatRegenTick() {
	now := time.Now()
	if !regenLastSweep.IsZero() && now.Sub(regenLastSweep) < healRateInterval {
		return
	}
	regenLastSweep = now
	runRegenSweep()
}
