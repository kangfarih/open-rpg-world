package meta

import (
	"math"
	"testing"
)

func testTable(t *testing.T) []int {
	t.Helper()
	tbl := BuildLevelExp(MaxLevel)
	if len(tbl) != MaxLevel {
		t.Fatalf("BuildLevelExp len = %d, want %d", len(tbl), MaxLevel)
	}
	return tbl
}

func TestLevelExpMonotonic(t *testing.T) {
	tbl := testTable(t)
	if tbl[0] != 0 {
		t.Fatalf("table[0] = %d, want 0", tbl[0])
	}
	for i := 1; i < len(tbl); i++ {
		if tbl[i] <= tbl[i-1] {
			t.Fatalf("table not strictly increasing at %d: %d <= %d", i, tbl[i], tbl[i-1])
		}
	}
}

func TestLevelExpKnownThresholds(t *testing.T) {
	tbl := testTable(t)
	// RuneScape curve as ported in m5.go: level 2 starts at 83 XP.
	if tbl[1] != 83 {
		t.Fatalf("table[1] = %d, want 83 (RuneScape curve)", tbl[1])
	}
	if got := ExpToLevel(tbl, MaxLevel, 0); got != 1 {
		t.Fatalf("ExpToLevel(0) = %d, want 1", got)
	}
	if got := ExpToLevel(tbl, MaxLevel, 82); got != 1 {
		t.Fatalf("ExpToLevel(82) = %d, want 1", got)
	}
	if got := ExpToLevel(tbl, MaxLevel, 83); got != 2 {
		t.Fatalf("ExpToLevel(83) = %d, want 2", got)
	}
	if got := NextExp(tbl, 0); got != 83 {
		t.Fatalf("NextExp(0) = %d, want 83", got)
	}
	if got := ExpToLevel(tbl, MaxLevel, -1); got != -1 {
		t.Fatalf("ExpToLevel(-1) = %d, want -1", got)
	}
	if got := NextExp(tbl, -1); got != -1 {
		t.Fatalf("NextExp(-1) = %d, want -1", got)
	}
}

func TestMaxDamageCeilings(t *testing.T) {
	// Bot inputs copied from combatBotStats in main.go.
	war := MaxDamageFloat(3, 20, "slash", false)
	warCrit := MaxDamageFloat(3, 20, "slash", true)
	archer := MaxDamageFloat(9, 20, "", false)
	archerCrit := MaxDamageFloat(9, 20, "", true)
	mage := MaxDamageFloat(26, 20, "", false)
	mageCrit := MaxDamageFloat(26, 20, "", true)

	for name, v := range map[string]float64{
		"war": war, "warCrit": warCrit,
		"archer": archer, "archerCrit": archerCrit,
		"mage": mage, "mageCrit": mageCrit,
	} {
		if !(v > 0) {
			t.Fatalf("%s MaxDamage = %v, want > 0", name, v)
		}
	}
	if !(mage > war) {
		t.Fatalf("mage (%v) should exceed war (%v)", mage, war)
	}
	if !(mage > archer && archer > war) {
		t.Fatalf("ordering want mage > archer > war, got %v > %v > %v", mage, archer, war)
	}
	if !(warCrit > war && archerCrit > archer && mageCrit > mage) {
		t.Fatalf("crit should exceed non-crit: war %v/%v archer %v/%v mage %v/%v",
			warCrit, war, archerCrit, archer, mageCrit, mage)
	}
	// Documented ceilings in combatAccuracyWeight comment: war 37 (crit 52),
	// archer 41 (crit 59), mage 62 (crit 91) — floors of the float maxima.
	cases := []struct {
		name string
		got  float64
		want int
	}{
		{"war", war, 37},
		{"warCrit", warCrit, 52},
		{"archer", archer, 41},
		{"archerCrit", archerCrit, 59},
		{"mage", mage, 62},
		{"mageCrit", mageCrit, 91},
	}
	for _, c := range cases {
		if int(math.Floor(c.got)) != c.want {
			t.Fatalf("%s floor = %d, want %d (raw %v)", c.name, int(math.Floor(c.got)), c.want, c.got)
		}
	}
}

func TestAccuracySanity(t *testing.T) {
	warW := AccuracyWeight(false, false, 6, 10, 7, 0, 0)
	archerW := AccuracyWeight(true, false, 1, 2, 1, 0, 2)
	mageW := AccuracyWeight(false, true, 2, 4, 4, 36, 0)
	for name, w := range map[string]float64{"war": warW, "archer": archerW, "mage": mageW} {
		if !(w >= 1) {
			t.Fatalf("%s weight = %v, want >= 1", name, w)
		}
	}
	warA := Accuracy(MaxAccuracy, MaxLevel, 6, 20, 1, warW, false)
	if warA < MinAccuracy || warA > ClampAccuracy {
		t.Fatalf("war accuracy = %v, want within [%v,%v]", warA, MinAccuracy, ClampAccuracy)
	}
	// Crit costs 0.15 accuracy before clamping. War-bot stats clamp at 2.0
	// (both plain and crit), so probe the delta with unclamped inputs.
	plain := Accuracy(MaxAccuracy, MaxLevel, 70, 120, 1, warW, false)
	crit := Accuracy(MaxAccuracy, MaxLevel, 70, 120, 1, warW, true)
	if plain-crit < 0.149 || plain-crit > 0.151 {
		t.Fatalf("crit delta = %v, want 0.15 (plain %v crit %v)", plain-crit, plain, crit)
	}
}

func TestRollDamageBounds(t *testing.T) {
	// r=0 always misses; r near 1 approaches the ceiling; HP clamp holds.
	if got := RollDamage(37.125, 1.0, 0, 100); got != 0 {
		t.Fatalf("RollDamage(r=0) = %d, want 0", got)
	}
	if got := RollDamage(37.125, 1.0, 0.999999, 100000); got <= 0 {
		t.Fatalf("RollDamage(r~1) = %d, want > 0", got)
	}
	if got := RollDamage(91.25, 1.0, 0.999999, 10); got != 10 {
		t.Fatalf("RollDamage clamped = %d, want 10", got)
	}
	if !RollCrit(0.049) || RollCrit(0.051) {
		t.Fatalf("RollCrit boundary failed")
	}
}

func TestAccuracyModifierTriangle(t *testing.T) {
	// Archer/mage: own-school weight, always >= 1.
	archerW := AccuracyModifier(true, false, 0, 0, 0, 0, 12, 0, 0, 0, 0, 0)
	if archerW < 1 {
		t.Fatalf("archer weight = %v, want >= 1", archerW)
	}
	mageW := AccuracyModifier(false, true, 0, 0, 0, 36, 0, 0, 0, 0, 0, 0)
	if mageW < 1 {
		t.Fatalf("mage weight = %v, want >= 1", mageW)
	}

	// Triangle advantage: crush attack vs slash defense → bonus (higher weight
	// = better accuracy via -(sqrt(w)/22.36)+1 producing a lower value).
	advW := AccuracyModifier(false, false, 10, 3, 3, 0, 0, 3, 10, 3, 0, 0)
	// Triangle disadvantage: slash attack vs crush defense → penalty (lower
	// weight = worse accuracy).
	disW := AccuracyModifier(false, false, 3, 10, 3, 0, 0, 10, 3, 3, 0, 0)
	if advW <= disW {
		t.Fatalf("advantage weight (%v) should exceed disadvantage weight (%v) — higher weight = better accuracy", advW, disW)
	}

	// Equal primary styles (crush atk vs crush def): no triangle step.
	// Use high enough attack stats that the no-triangle weight exceeds the
	// floor (1). With atk (20,3,3) vs def (10,3,3): crush diff = 3.33,
	// no triangle → weight = 3.33.
	eqW := AccuracyModifier(false, false, 20, 3, 3, 0, 0, 10, 3, 3, 0, 0)
	// Verify it's below the advantage case (same atk, matching def gives no bonus).
	if eqW >= advW {
		t.Fatalf("equal-primary weight %v should be below advantage %v", eqW, advW)
	}

	// Stab > crush advantage.
	stabAdv := AccuracyModifier(false, false, 3, 3, 10, 0, 0, 10, 3, 3, 0, 0)
	stabDis := AccuracyModifier(false, false, 3, 3, 10, 0, 0, 3, 10, 3, 0, 0)
	if stabAdv <= stabDis {
		t.Fatalf("stab>crush advantage weight (%v) should exceed disadvantage (%v)", stabAdv, stabDis)
	}

	// Slash > stab advantage.
	slashAdv := AccuracyModifier(false, false, 3, 10, 3, 0, 0, 3, 3, 10, 0, 0)
	slashDis := AccuracyModifier(false, false, 3, 10, 3, 0, 0, 3, 10, 3, 0, 0)
	if slashAdv <= slashDis {
		t.Fatalf("slash>stab advantage weight (%v) should exceed disadvantage (%v)", slashAdv, slashDis)
	}

	// All defense stats zero: minimum weight is 1 (floor).
	zeroDef := AccuracyModifier(false, false, 6, 10, 7, 0, 0, 0, 0, 0, 0, 0)
	if zeroDef < 1 {
		t.Fatalf("zero-defense weight = %v, want >= 1", zeroDef)
	}
}

func TestAccuracyWithDefenseLevel(t *testing.T) {
	// Higher defense level → higher accuracy value (worse for attacker).
	// Use inputs that don't clamp so the delta is visible.
	low := Accuracy(MaxAccuracy, MaxLevel, 70, 120, 1, 5.0, false)
	high := Accuracy(MaxAccuracy, MaxLevel, 70, 120, 30, 5.0, false)
	if high <= low {
		t.Fatalf("defenseLevel 30 (%v) should produce higher accuracy value (worse) than level 1 (%v)", high, low)
	}
	// Both must stay within clamp bounds.
	if low < MinAccuracy || low > ClampAccuracy {
		t.Fatalf("low-def accuracy %v out of [%v, %v]", low, MinAccuracy, ClampAccuracy)
	}
	if high < MinAccuracy || high > ClampAccuracy {
		t.Fatalf("high-def accuracy %v out of [%v, %v]", high, MinAccuracy, ClampAccuracy)
	}
}
