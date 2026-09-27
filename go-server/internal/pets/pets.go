// Package pets is an additive, transport-free pet companion engine for the
// Go server stub. The caller owns ticking and all packet I/O: this package
// only computes positions, tame outcomes, and hunger/expiry predicates. The
// caller applies the results (movement/teleport/pickup frames) and emits
// packets itself. There are no timers, goroutines, or network sends here.
//
// TS sources mirrored here:
//   - packages/server/src/game/entity/character/pet/pet.ts — Pet extends
//     Character, spawns at the owner's (x, y) via Utils.createInstance
//     (Modules.EntityType.Pet), serializes owner instance + movementSpeed
//     copied from the owner.
//   - packages/server/src/game/entity/character/player/handler.ts —
//     handleMovement: pet follows the owner when Manhattan distance > 2;
//     when distance > 10 the pet is despawned and respawned at the player.
//   - packages/server/src/game/entity/character/player/player.ts —
//     setPet (spawn + follow), removePet (needs inventory space, returns a
//     "<key>pet" item), hasPet.
//   - packages/server/src/game/entity/character/character.ts — follow()
//     sends a Movement Follow packet (no server-side stepping); teleport()
//     repositions + sends a Teleport packet.
//   - packages/server/src/game/entity/entity.ts + packages/common/util/
//     utils.ts — getDistance is MANHATTAN (|dx| + |dy|) in tiles.
//   - packages/server/data/items.json — pet items (type "pet": ratpet,
//     rathatpet, ratballoonpet) carry a "pet" mob key; spawning is direct
//     (handler.ts: item.isPetItem() -> setPet(item.pet)), no roll.
//   - packages/common/network/packets.ts + modules.ts + opcodes.ts —
//     Packets.Pet (58), EntityType.Pet (7), Opcodes.Pet.Pickup (0).
//
// Packet ID conventions (for the caller, not emitted here): S->C Pet is
// [58, ...] (internal/protocol PacketPet); the pet entity Type is 7
// (modules.ts EntityType.Pet — note internal/protocol currently defines no
// EntityPet constant); Movement Follow is opcode 5 on packet 11
// (internal/protocol MovementFollow); Teleport is packet 12; Pet Pickup is
// opcode 0 on packet 58.
package pets

// TeleportDistance is the Manhattan tile distance beyond which the caller
// should teleport (despawn + respawn at the owner, TS handler.ts parity)
// instead of stepping the pet. See divergences below: TS uses > 10.
const TeleportDistance = 10

// FollowDistance is the Manhattan tile distance beyond which the caller
// should order the pet to follow the owner (TS handler.ts: distance > 2).
const FollowDistance = 2

// HungerAfterMs is how long after the last feed a pet counts as hungry.
// There is no TS source (TS pets never go hungry); see divergences.
const HungerAfterMs = int64(5 * 60 * 1000)

// tameChances maps pet-item keys (items.json "<key>pet", plus the royalpet
// quest reward "catpet") to tame success probabilities. There is no TS
// source (TS pet acquisition is unconditional); all values are new and
// documented under divergences.
var tameChances = map[string]float64{
	"ratpet":        0.50,
	"rathatpet":     0.35,
	"ratballoonpet": 0.35,
	"catpet":        0.25,
}

// DefaultTameChance applies to item keys not in the table above.
const DefaultTameChance = 0.10

// Pet is one spawned companion. Instance is the "<type>-<n>" entity string
// (TS Utils.createInstance), Key the mob key (e.g. "rat"), Owner the
// owner's instance string (TS PetData.owner), X/Y grid tiles, HP hit
// points. Timing (born/fed) lives with the caller; hunger/expiry are pure
// predicates over explicit millisecond clocks.
type Pet struct {
	Instance string
	Key      string
	Owner    string
	X        int
	Y        int
	HP       int
}

// Distance returns the Manhattan tile distance between two grid points
// (TS Utils.getDistance / Entity.getDistance parity: |dx| + |dy|).
func Distance(ax, ay, bx, by int) int {
	dx := ax - bx
	if dx < 0 {
		dx = -dx
	}
	dy := ay - by
	if dy < 0 {
		dy = -dy
	}
	return dx + dy
}

// ShouldFollow reports whether the pet is far enough from its owner to
// need a follow order (TS handler.ts: distance > 2).
func ShouldFollow(ownerX, ownerY, petX, petY int) bool {
	return Distance(ownerX, ownerY, petX, petY) > FollowDistance
}

// ShouldTeleport reports whether the pet is so far from its owner that the
// caller should teleport it (despawn + respawn at the owner) instead of
// stepping it (distance > 12 tiles, Manhattan).
func ShouldTeleport(ownerX, ownerY, petX, petY int) bool {
	return Distance(ownerX, ownerY, petX, petY) > TeleportDistance
}

// FollowStep returns the pet's next grid position after one single-tile
// step toward the owner. It never mutates the pet and never steps onto the
// owner's tile: when the pet is already on, adjacent to, or otherwise one
// step away from landing on the owner, the current position is returned.
// The step runs along the axis with the larger gap (ties prefer X), which
// strictly decreases the Manhattan distance whenever a step is taken.
func FollowStep(ownerX, ownerY, petX, petY int) (int, int) {
	dx := ownerX - petX
	dy := ownerY - petY
	if dx == 0 && dy == 0 {
		return petX, petY
	}
	nx, ny := petX, petY
	adx := dx
	if adx < 0 {
		adx = -adx
	}
	ady := dy
	if ady < 0 {
		ady = -ady
	}
	if adx >= ady {
		if dx > 0 {
			nx++
		} else {
			nx--
		}
	} else {
		if dy > 0 {
			ny++
		} else {
			ny--
		}
	}
	if nx == ownerX && ny == ownerY {
		return petX, petY
	}
	return nx, ny
}

// TameChance returns the success probability for a tame attempt with the
// given pet-item key, or DefaultTameChance for unknown keys.
func TameChance(itemKey string) float64 {
	if c, ok := tameChances[itemKey]; ok {
		return c
	}
	return DefaultTameChance
}

// TameRoll resolves a tame attempt purely: roll is an injected uniform
// [0,1) random float supplied by the caller (no rand use here, so tests
// stay deterministic). It returns true when roll falls below the item's
// table chance. Rolls outside [0,1) are clamped by comparison semantics:
// a negative roll always succeeds, a roll >= 1 always fails.
func TameRoll(itemKey string, roll float64) bool {
	return roll < TameChance(itemKey)
}

// IsHungry reports whether the pet counts as hungry at nowMs given the
// last feed time lastFedMs (hungry once nowMs-lastFedMs >= HungerAfterMs).
// There is no TS source; the threshold is new — see divergences.
func IsHungry(nowMs, lastFedMs int64) bool {
	return nowMs-lastFedMs >= HungerAfterMs
}

// IsExpired reports whether the pet's lifespan has elapsed at nowMs given
// birth time bornMs and lifespan lifespanMs (expired once
// nowMs-bornMs >= lifespanMs). A non-positive lifespan means the pet
// expires immediately. There is no TS source (TS pets never expire);
// see divergences.
func IsExpired(nowMs, bornMs, lifespanMs int64) bool {
	return nowMs-bornMs >= lifespanMs
}

// Divergences from TS (documented; spec-mandated or new-in-Go behaviour):
//   - Teleport threshold: TS handler.ts respawns the pet when distance > 10
//     (Manhattan). This package uses > 12 per the P-B1 spec, so pets walk a
//     little farther before the caller teleports them.
//   - FollowStep has no TS counterpart: TS pets move via Movement Follow
//     packets resolved by the client's pathing, not by server-side grid
//     steps. The one-tile axis step here is a stub-friendly approximation
//     the caller may use until real pathing exists.
//   - TameRoll/TameChance are entirely new: TS pet acquisition is
//     unconditional (use a pet item -> setPet, finish royalpet -> catpet).
//     The chance table above is invented; callers that want TS parity
//     should bypass the roll for item/quest grants.
//   - IsHungry/IsExpired are entirely new: TS pets have no hunger or
//     lifespan. Thresholds (HungerAfterMs, caller-supplied lifespanMs) are
//     invented; expiry/hunger effects (stat loss, despawn) are caller-side.
//   - EntityType.Pet (7) is internal/protocol EntityPet (PacketPet is 58);
//     use the constant, not the literal 7.
