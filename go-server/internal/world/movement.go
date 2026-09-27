package world

import "time"

// Movement/anticheat verify core (E9b extraction of the main.go movement
// section). All functions are pure: the root session shape stays in package
// main (m13.go reads/writes sess.movementSpeed), so main.go adapts its
// session to SpeedState on each check.
type SpeedState struct {
	// MovementSpeed is ms per tile (Welcome default 220).
	MovementSpeed int
	LastStep      time.Time
}

// SpeedModifiers bundles the dynamic speed inputs (TS getMovementSpeed parity).
// The root adapter fills these from equipment/status/rank on each check.
type SpeedModifiers struct {
	BootsModifier float64 // 1.0 = no modifier, <1.0 = faster
	HasRunning    bool
	HasHotSauce   bool
	HasFreezing   bool
	HasSnowPotion bool
	IsCheater     bool
	Override      int // admin /ms override, -1 = use default
}

// CalculateMovementSpeed ports TS getMovementSpeed (player.ts:1419-1449):
// dynamic speed calculation with stacked modifiers. Returns ms per tile.
func CalculateMovementSpeed(mods SpeedModifiers) int {
	// Start with default or override.
	speed := 220
	if mods.Override >= 0 {
		speed = mods.Override
	}

	// Boots movement modifier (equipment.movementModifier).
	if mods.BootsModifier > 0 && mods.BootsModifier != 1.0 {
		speed = int(float64(speed) * mods.BootsModifier)
	}

	// Running effect: 10% speed boost (0.9x multiplier).
	if mods.HasRunning {
		speed = int(float64(speed) * 0.9)
	}

	// HotSauce effect: 20% speed boost (0.8x multiplier).
	if mods.HasHotSauce {
		speed = int(float64(speed) * 0.8)
	}

	// Freezing effect: 25% slower (1.25x multiplier), SnowPotion exempts.
	if mods.HasFreezing && !mods.HasSnowPotion {
		speed = int(float64(speed) * 1.25)
	}

	// Cheater rank: 50% slower (2x multiplier).
	if mods.IsCheater {
		speed = int(float64(speed) * 2.0)
	}

	return speed
}

// Abs mirrors main.go abs.
func Abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// JumpTooFar mirrors the far-jump (noclip) gate in handleMovement: a Request
// or Started report more than 2 tiles off the authoritative tile is
// rejected/resynced (player.ts handleMovementRequest diff>2).
func JumpTooFar(dx, dy int) bool {
	return dx > 2 || dy > 2
}

// CheckSpeed enforces max tiles/sec vs movementSpeed with the main.go
// semantics verbatim: 220ms/tile default, first step after idle (>2s)
// always passes, 5% margin per tile (verifyMovement), sliding lastStep
// window advanced even on reject. Returns true when the step is too fast
// (caller rejects + teleport-back; >15 disconnects).
//
// Enhanced with TS parity (player.ts:726-746):
//   - latency: subtract client latency from step timing (high-latency exemption)
//   - regionGrace: 1.5s grace after region change (ignore speed violations)
//   - isDoor: exempt door tiles from speed check
func CheckSpeed(st *SpeedState, tiles int, now time.Time, latencyMs int, regionGrace time.Duration, isDoor bool) bool {
	if st.MovementSpeed <= 0 {
		st.MovementSpeed = 220
	}
	if st.LastStep.IsZero() {
		st.LastStep = now
		return false
	}

	// Door exemption (player.ts:743).
	if isDoor {
		st.LastStep = now
		return false
	}

	// Region change exemption (player.ts:740): 1.5s grace.
	if regionGrace > 0 && now.Sub(st.LastStep) < regionGrace {
		st.LastStep = now
		return false
	}

	// Grace: first step after idle (>2s) always passes.
	if now.Sub(st.LastStep) > 2*time.Second {
		st.LastStep = now
		return false
	}

	if tiles < 1 {
		tiles = 1
	}

	// Latency compensation (player.ts:728): subtract latency from step diff.
	// High latency (>35ms) with very short step diff (<35ms) is exempt.
	stepDiff := now.Sub(st.LastStep)
	if latencyMs > 35 && stepDiff < 35*time.Millisecond {
		st.LastStep = now
		return false
	}
	adjustedDiff := stepDiff - time.Duration(latencyMs)*time.Millisecond
	if adjustedDiff < 0 {
		adjustedDiff = 0
	}

	minInterval := time.Duration(st.MovementSpeed) * time.Millisecond
	// 5% margin like verifyMovement, per-tile with no +2 padding.
	allowance := time.Duration(float64(minInterval) * 0.95 * float64(tiles))
	if adjustedDiff < allowance {
		// Sliding window: advance lastStep even on reject so legit
		// players paced at the legal rate never accumulate cheatScore.
		st.LastStep = now
		return true
	}
	st.LastStep = now
	return false
}

// TargetsOccupant mirrors main.go targetsResource without the tile lookup:
// reports whether any of the given target instances is the resource
// occupying the destination tile. An empty occupant never matches, so
// static collisions are always rejected even with no target set.
func TargetsOccupant(occupant string, targets ...string) bool {
	if occupant == "" {
		return false
	}
	for _, t := range targets {
		if t != "" && t == occupant {
			return true
		}
	}
	return false
}
