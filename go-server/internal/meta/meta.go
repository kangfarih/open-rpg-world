// Package meta is a self-contained, dependency-free copy of the root
// server's level/XP and combat-formula math for analysis and tooling.
//
// Sources (read-only mirrors, do NOT import the root package):
//   - m5.go: initLevelExp / expToLevel / nextExp (RuneScape LevelExp port
//     from formulas.ts LevelExp + loader.ts loadLevels).
//   - main.go: combatMaxDamageFloat / combatAccuracyWeight / combatAccuracy /
//     combatRollCrit + consts ModulesMaxAccuracy / ModulesMaxLevel.
//
// Divergence notes vs the root server:
//   - No globals: the root caches the XP table in a package-level slice
//     guarded by sync.Once (levelExpTbl/levelExpOnce) and reads bot stats
//     from the combatBotStats map plus live combatHP. Every helper here
//     takes explicit args (tables, stat scalars, RNG draws) and owns no
//     mutable state, so it is safe for concurrent use and unit tests.
//   - No randomness inside: combatRollCrit in main.go draws rand.Float64()
//     itself; here RollCrit/RollDamage take the caller's uniform draw r in
//     [0,1) so tests can inject deterministic values.
//   - Accuracy clamp [0.7, 2.0] is preserved verbatim. Per the M3 comment in
//     main.go this clamp is itself a demo-playability divergence from the
//     faithful high-level Kaetram accuracy (high-level hits would otherwise
//     skew to single digits); meta keeps the shipped behavior.
//   - RollDamage clamps to remainingHP like combatRollLocked clamps to
//     combatHP; pass remainingHP < 0 to skip the clamp in pure-math contexts.
package meta

import "math"

// Copies of the root Modules constants (main.go).
// Kept as consts for documentation; the helpers below still take them as
// explicit args where the root reads the globals, so callers are never
// coupled to package state.
const (
	// MaxLevel copies ModulesMaxLevel (modules.ts MAX_LEVEL).
	MaxLevel = 120
	// MaxAccuracy copies ModulesMaxAccuracy (formulas.ts MAX_ACCURACY).
	MaxAccuracy = 0.45
	// CritRate copies the flat 5% base crit chance in combatRollCrit.
	// Bots carry no critical-enchantment gear, so no gear bonus applies.
	CritRate = 0.05
)

// Accuracy bounds copied from combatAccuracy (demo-playability clamp, see
// package divergence note).
const (
	MinAccuracy   = 0.7
	ClampAccuracy = 2.0
)

// BuildLevelExp mirrors initLevelExp (m5.go): indices 0..maxLevel-1, table[0]
// is 0 and each later entry accumulates
// floor(0.25 * floor(i + 300 * 2^(i/7))).
// The root always calls it with ModulesMaxLevel (120); maxLevel is a param
// here so callers own the table with no global cache.
func BuildLevelExp(maxLevel int) []int {
	if maxLevel <= 0 {
		return nil
	}
	tbl := make([]int, maxLevel)
	tbl[0] = 0
	for i := 1; i < maxLevel; i++ {
		points := int(math.Floor(0.25 * math.Floor(float64(i)+300*math.Pow(2, float64(i)/7))))
		tbl[i] = points + tbl[i-1]
	}
	return tbl
}

// ExpToLevel mirrors expToLevel (m5.go): first index i with xp < table[i]
// wins; xp below 0 yields -1; xp at/past the top yields maxLevel.
func ExpToLevel(table []int, maxLevel int, xp int) int {
	if xp < 0 {
		return -1
	}
	for i := 1; i < len(table); i++ {
		if xp < table[i] {
			return i
		}
	}
	return maxLevel
}

// NextExp mirrors nextExp (m5.go): first table entry strictly above xp, or
// -1 when xp is negative or at/past the top.
func NextExp(table []int, xp int) int {
	if xp < 0 {
		return -1
	}
	for i := 1; i < len(table); i++ {
		if xp < table[i] {
			return table[i]
		}
	}
	return -1
}

// MaxDamageFloat mirrors combatMaxDamageFloat (main.go): maxDamage =
// (damageBonus + damageLevel) * 1.25 [+50% on crit] [+5 player bonus]
// [*attack-style multiplier], floored at 0.
// The root looks up damageBonus/damageLevel/attackStyle from
// combatBotStats[bot]; here they are explicit args.
func MaxDamageFloat(damageBonus, damageLevel int, attackStyle string, critical bool) float64 {
	dmg := float64(damageBonus+damageLevel) * 1.25
	if critical {
		dmg *= 1.5
	}
	dmg += 5 // player bonus
	switch attackStyle {
	case "slash":
		dmg *= 1.1
	case "crush":
		dmg *= 1.05
	case "shared":
		dmg *= 1.03
	}
	if dmg < 0 {
		dmg = 0
	}
	return dmg
}

// AccuracyWeight mirrors combatAccuracyWeight (main.go) for a zero-defense
// dummy target: archers/mages use their own school ((stat-0)/3, floored at
// 1), melee sums the positive schools. The root reads the bot's
// archer/magic flags and crush/slash/stab/magicStat/archery stats; here they
// are explicit args.
// Deprecated: use AccuracyModifier for triangle-advantage-aware weight.
func AccuracyWeight(archer, magic bool, crush, slash, stab, magicStat, archery int) float64 {
	if archer {
		if archery > 0 {
			return math.Max(float64(archery)/3, 1)
		}
		return 1
	}
	if magic {
		if magicStat > 0 {
			return math.Max(float64(magicStat)/3, 1)
		}
		return 1
	}
	total := 0.0
	for _, v := range []int{crush, slash, stab, magicStat, archery} {
		if v > 0 {
			total += float64(v) / 3
		}
	}
	if total < 1 {
		total = 1
	}
	return total
}

// AccuracyModifier computes the accuracy weight modifier with triangle
// advantage (formulas.ts getAccuracyWeight). Per-style weight is
// (attackStat - defenseStat) / 3. For melee, the primary attack style
// (highest attack stat) vs primary defense style (highest defense stat)
// determines the crush>slash>stab>crush triangle bonus/penalty. For
// archers/mages, the own-school weight is always a bonus.
func AccuracyModifier(archer, magic bool,
	atkCrush, atkSlash, atkStab, atkMagic, atkArchery int,
	defCrush, defSlash, defStab, defMagic, defArchery int) float64 {
	if archer {
		w := float64(atkArchery-defArchery) / 3
		if w < 0 {
			w = 0
		}
		return math.Max(w, 1)
	}
	if magic {
		w := float64(atkMagic-defMagic) / 3
		if w < 0 {
			w = 0
		}
		return math.Max(w, 1)
	}
	// Melee: per-school (attack - defense) / 3, positives summed.
	wCrush := float64(atkCrush-defCrush) / 3
	wSlash := float64(atkSlash-defSlash) / 3
	wStab := float64(atkStab-defStab) / 3
	total := 0.0
	for _, w := range []float64{wCrush, wSlash, wStab} {
		if w > 0 {
			total += w
		}
	}
	// Primary attack style (highest attack stat).
	primaryAtk, primaryAtkVal := 0, atkCrush // 0=crush
	if atkSlash > primaryAtkVal {
		primaryAtk, primaryAtkVal = 1, atkSlash
	}
	if atkStab > primaryAtkVal {
		primaryAtk = 2
	}
	// Primary defense style (highest defense stat).
	primaryDef, primaryDefVal := 0, defCrush // 0=crush
	if defSlash > primaryDefVal {
		primaryDef, primaryDefVal = 1, defSlash
	}
	if defStab > primaryDefVal {
		primaryDef = 2
	}
	// No triangle step when primary styles match or no attack stat.
	if primaryAtk == primaryDef || primaryAtkVal == 0 {
		if total < 1 {
			total = 1
		}
		return total
	}
	// Triangle: crush>slash>stab>crush.
	var wSchool float64
	switch primaryAtk {
	case 0:
		wSchool = wCrush
	case 1:
		wSchool = wSlash
	case 2:
		wSchool = wStab
	}
	advantage := (primaryAtk == 0 && primaryDef == 1) || // crush > slash
		(primaryAtk == 1 && primaryDef == 2) || // slash > stab
		(primaryAtk == 2 && primaryDef == 0) // stab > crush
	if advantage {
		total += wSchool
	} else {
		total -= wSchool / 2
	}
	if total < 1 {
		total = 1
	}
	return total
}

// Accuracy mirrors combatAccuracy (main.go): MAX_ACCURACY + bonus term +
// level term + target defense term + stat-weight term, -0.15 on crit,
// clamped to [0.7, 2.0]. Negative weight applies a flat +1.5 penalty
// (formulas.ts triangle disadvantage); positive weight uses the sqrt curve.
func Accuracy(maxAccuracy float64, maxLevel, accuracyBonus, accuracyLevel int, defenseLevel int, weight float64, critical bool) float64 {
	acc := maxAccuracy
	if bonus := float64(accuracyBonus); bonus <= 70 {
		acc += 1 - bonus/70
	}
	acc += float64(maxLevel-accuracyLevel+1) * 0.01
	acc += float64(defenseLevel) * 0.0175
	if weight < 0 {
		acc += 1.5
	} else {
		acc += -(math.Sqrt(weight) / 22.36) + 1
	}
	if critical {
		acc -= 0.15
	}
	if acc < MinAccuracy {
		acc = MinAccuracy
	}
	if acc > ClampAccuracy {
		acc = ClampAccuracy
	}
	return acc
}

// RollCrit mirrors combatRollCrit (main.go: rand.Float64() < 0.05) with the
// RNG draw as an explicit arg for determinism.
func RollCrit(r float64) bool { return r < CritRate }

// RollDamage mirrors combatRollLocked (main.go):
// floor(r^accuracy * (max+1)), clamped to remainingHP like the truth clamps
// to combatHP. Pass remainingHP < 0 to skip the HP clamp. r must be a
// uniform draw in [0,1).
func RollDamage(maxDamage, accuracy, r float64, remainingHP int) int {
	dmg := int(math.Floor(math.Pow(r, accuracy) * (maxDamage + 1)))
	if remainingHP >= 0 && dmg > remainingHP {
		dmg = remainingHP
	}
	if dmg < 0 {
		dmg = 0
	}
	return dmg
}

// HeroMaxHPForLevel mirrors formulas.ts getMaxHitPoints: 39 + level * 30.
// The TS server scales hero max HP by combat level; the Go stub previously
// used a hardcoded 100 (level-1 value).
func HeroMaxHPForLevel(level int) int {
	if level < 1 {
		level = 1
	}
	return 39 + level*30
}

// HeroMaxManaForLevel mirrors formulas.ts getMaxMana: 20 + level * 24.
// The TS server scales hero max mana by combat level; the Go stub previously
// used a hardcoded 50 (level-1 value).
func HeroMaxManaForLevel(level int) int {
	if level < 1 {
		level = 1
	}
	return 20 + level*24
}
