package pets

import "testing"

func TestShouldTeleportThreshold(t *testing.T) {
	// Owner at origin; Manhattan distance decides (TS handler.ts parity: > 10).
	if ShouldTeleport(0, 0, 10, 0) {
		t.Error("distance 10 must NOT teleport (rule is > 10)")
	}
	if ShouldTeleport(0, 0, 5, 5) {
		t.Error("distance 10 (5+5) must NOT teleport (rule is > 10)")
	}
	if !ShouldTeleport(0, 0, 11, 0) {
		t.Error("distance 11 must teleport")
	}
	if !ShouldTeleport(0, 0, 6, 5) {
		t.Error("distance 11 (6+5) must teleport")
	}
	if ShouldTeleport(5, 5, 5, 5) {
		t.Error("distance 0 must NOT teleport")
	}
}

func TestShouldFollowThreshold(t *testing.T) {
	if ShouldFollow(0, 0, 1, 1) {
		t.Error("distance 2 must NOT follow (rule is > 2, TS handler.ts parity)")
	}
	if !ShouldFollow(0, 0, 2, 1) {
		t.Error("distance 3 must follow")
	}
}

func TestFollowStepApproachesOwner(t *testing.T) {
	// Far pet: one step must strictly shrink the Manhattan distance
	// and move exactly one tile.
	nx, ny := FollowStep(5, 5, 1, 1)
	if Distance(5, 5, nx, ny) != Distance(5, 5, 1, 1)-1 {
		t.Errorf("step must shrink distance by 1, got (%d,%d)", nx, ny)
	}
	if Distance(nx, ny, 1, 1) != 1 {
		t.Errorf("step must move exactly one tile, got (%d,%d)", nx, ny)
	}

	// Straight-line approach stays on the line.
	nx, ny = FollowStep(5, 1, 1, 1)
	if nx != 2 || ny != 1 {
		t.Errorf("expected (2,1), got (%d,%d)", nx, ny)
	}

	// Adjacent pet must NOT step onto the owner's tile.
	nx, ny = FollowStep(5, 5, 5, 4)
	if nx != 5 || ny != 4 {
		t.Errorf("adjacent pet must hold position, got (%d,%d)", nx, ny)
	}

	// Pet on the owner's tile stays put.
	nx, ny = FollowStep(5, 5, 5, 5)
	if nx != 5 || ny != 5 {
		t.Errorf("stacked pet must hold position, got (%d,%d)", nx, ny)
	}

	// Diagonal neighbour: the X-preferring step lands beside the owner,
	// shrinking distance 2 -> 1 without stepping onto the owner's tile.
	nx, ny = FollowStep(5, 5, 4, 4)
	if nx != 5 || ny != 4 {
		t.Errorf("diagonal neighbour must step to (5,4), got (%d,%d)", nx, ny)
	}
}

func TestTameRollBounds(t *testing.T) {
	// ratpet chance is 0.50: below succeeds, at/above fails.
	if !TameRoll("ratpet", 0.0) {
		t.Error("roll 0.0 must always succeed")
	}
	if !TameRoll("ratpet", 0.4999) {
		t.Error("roll below chance must succeed")
	}
	if TameRoll("ratpet", 0.5) {
		t.Error("roll equal to chance must fail (strict <)")
	}
	if TameRoll("ratpet", 0.9999) {
		t.Error("roll above chance must fail")
	}

	// Unknown keys fall back to DefaultTameChance.
	if TameChance("nope") != DefaultTameChance {
		t.Errorf("unknown key must use default %v", DefaultTameChance)
	}
	if !TameRoll("nope", 0.0) {
		t.Error("roll 0.0 must succeed even for unknown keys")
	}
	if TameRoll("nope", 1.0) {
		t.Error("roll 1.0 must fail even for unknown keys")
	}
	if !TameRoll("nope", -0.5) {
		t.Error("negative roll must succeed (below any chance)")
	}
}

func TestHungerPredicate(t *testing.T) {
	if IsHungry(1000, 1000) {
		t.Error("just-fed pet must NOT be hungry")
	}
	if IsHungry(1000+HungerAfterMs-1, 1000) {
		t.Error("pet fed HungerAfterMs-1 ms ago must NOT be hungry")
	}
	if !IsHungry(1000+HungerAfterMs, 1000) {
		t.Error("pet fed HungerAfterMs ms ago MUST be hungry (boundary inclusive)")
	}
	if !IsHungry(1000+HungerAfterMs+1, 1000) {
		t.Error("long-unfed pet MUST be hungry")
	}
}

func TestExpirePredicate(t *testing.T) {
	const born, life = int64(5000), int64(60_000)
	if IsExpired(born, born, life) {
		t.Error("newborn pet must NOT be expired")
	}
	if IsExpired(born+life-1, born, life) {
		t.Error("pet 1ms short of lifespan must NOT be expired")
	}
	if !IsExpired(born+life, born, life) {
		t.Error("pet at exactly lifespan MUST be expired (boundary inclusive)")
	}
	if !IsExpired(born+life+1, born, life) {
		t.Error("pet past lifespan MUST be expired")
	}
	if !IsExpired(9999, 9999, 0) {
		t.Error("zero lifespan must expire immediately")
	}
}
