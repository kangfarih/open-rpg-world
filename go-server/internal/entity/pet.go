// Package entity holds the stateful pet companion registry and the
// follow/teleport + attack-mirror orchestration, extracted behavior-frozen
// from the root pets_wire.go adapter.
//
// TS-mirror: pets live under game/entity (pet.ts extends Character; spawnPet/
// removePet in controllers/entities.ts; setPet/removePet/hasPet in
// player.ts; follow/teleport in player/handler.ts; follow()/teleport() in
// character.ts; handlePet Pickup in player/incoming.ts).
//
// Everything transport/world related stays with the root adapter and is
// reached only through the World seam below (root helpers in parentheses):
//
//	OwnerPos    -> entityPos(instance) (registry tile for the owner)
//	SpawnPet    -> setEntityPos + S->C Spawn [5, PetData] + Movement Follow [11,5]
//	MovePet     -> setEntityPos + S->C Movement Move [11,4] + Movement Follow [11,5]
//	TeleportPet -> setEntityPos + S->C Despawn [13] + Spawn [5] + Movement Follow [11,5]
//	DespawnPet  -> entities delete + S->C Despawn [13]
//	IsMob       -> m9MobFor(target) != nil (killable-mob gate)
//	HitMob      -> S->C Animation + Combat Hit broadcasts + m9PlayerHit
//	HitDummy    -> combatMu guard + applyBossHitLocked (combat pipeline)
//	DummyTarget -> combatDummyInstance
//
// Packet shapes and the 20Hz tick cadence are frozen: this package never
// builds frames (no pkt/pktOp) and never sends. Positions, tame outcomes and
// hunger/expiry predicates come from internal/pets (imported, not duplicated).
//
// Divergences from TS (inherited from the root wire, documented):
//   - Teleport threshold is pets.TeleportDistance (> 10, TS handler.ts parity).
//   - Follow stepping is a server-side one-tile FollowStep (TS pets move via
//     client pathing on the Follow packet).
//   - Teleport reuses the SAME pet instance (TS mints a fresh instance).
//   - Attack-mirror is new-in-Go (TS pets never attack): fixed MirrorDamage
//     credited to the owner.
//   - Hunger/expiry (pets.IsHungry/IsExpired) are report-only, not enforced.
package entity

import (
	"fmt"
	"sync"

	"rpg-world-server/internal/pets"
)

// MirrorDamage is the fixed pet swing damage (new-in-Go; TS pets never
// attack). Small enough to never skew combat-harness DPS (pets only exist
// when explicitly granted). Mirrors root petMirrorDamage (kept as an alias).
const MirrorDamage = 3

// itemMob maps items.json pet-item keys to their "pet" mob keys
// (verified: ratpet->rat, rathatpet->rathat, ratballoonpet->ratballoon;
// royalpet quest reward "catpet"->cat by the same "<mob>pet" convention).
var itemMob = map[string]string{
	"ratpet":        "rat",
	"rathatpet":     "rathat",
	"ratballoonpet": "ratballoon",
	"catpet":        "cat",
}

// LookupItem maps a pet-item key ("ratpet") to its mob key ("rat").
// Unknown keys report ok=false (handler.ts item.isPetItem() parity).
func LookupItem(itemKey string) (mob string, ok bool) {
	mob, ok = itemMob[itemKey]
	return mob, ok
}

// ResolveKey accepts a pet-item key ("ratpet") or a mob key ("rat", "cat")
// for the debug grant; unknown/empty defaults to rat/ratpet.
func ResolveKey(key string) (mob, item string) {
	if m, ok := itemMob[key]; ok {
		return m, key
	}
	for _, m := range itemMob {
		if key == m {
			return m, m + "pet"
		}
	}
	return "rat", "ratpet"
}

// Record is one live companion. Owner is the owner's player instance.
type Record struct {
	Instance string
	MobKey   string
	ItemKey  string
	Owner    string
	X, Y     int
	BornMs   int64
	FedMs    int64
}

// Registry is the stateful pet store (petByOwner/petByInstance + mutex +
// sequence from the root wire). The zero value is not usable; use
// NewRegistry. All methods are safe for concurrent use.
type Registry struct {
	mu         sync.Mutex
	byOwner    map[string]*Record
	byInstance map[string]*Record
	seq        int64
}

// NewRegistry returns an empty pet registry.
func NewRegistry() *Registry {
	return &Registry{
		byOwner:    map[string]*Record{},
		byInstance: map[string]*Record{},
	}
}

// Grant inserts a companion for the owner at (ox, oy) (player.ts setPet:
// ALREADY_HAVE_PET guard, spawn at the owner's tile). itemKey defaults to
// "<mob>pet" when empty. It returns (nil, true) when the owner already has
// a pet, (nil, false) for invalid args, and a detached copy of the live
// record on success. Side effects (Spawn + Follow frames) are the caller's
// via World.SpawnPet.
func (r *Registry) Grant(owner string, ox, oy int, mobKey, itemKey string, nowMs int64) (*Record, bool) {
	if r == nil || owner == "" || mobKey == "" {
		return nil, false
	}
	if itemKey == "" {
		itemKey = mobKey + "pet"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, has := r.byOwner[owner]; has {
		return nil, true
	}
	r.seq++
	rec := &Record{
		Instance: fmt.Sprintf("pet-%d", r.seq),
		MobKey:   mobKey, ItemKey: itemKey, Owner: owner,
		X: ox, Y: oy,
		BornMs: nowMs, FedMs: nowMs,
	}
	r.byOwner[owner] = rec
	r.byInstance[rec.Instance] = rec
	out := *rec
	return &out, false
}

// Has reports whether the player instance currently owns a pet.
func (r *Registry) Has(owner string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byOwner[owner] != nil
}

// ByOwner returns a detached copy of the owner's pet (ok=false when none).
func (r *Registry) ByOwner(owner string) (Record, bool) {
	if r == nil {
		return Record{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.byOwner[owner]
	if rec == nil {
		return Record{}, false
	}
	return *rec, true
}

// ByInstance returns a detached copy of the live pet instance (Who lookup).
func (r *Registry) ByInstance(instance string) (Record, bool) {
	if r == nil {
		return Record{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.byInstance[instance]
	if rec == nil {
		return Record{}, false
	}
	return *rec, true
}

// RemoveByOwner drops the owner's pet from both indexes (despawn/drop
// parity: silent registry removal; Despawn frames are the caller's via
// World.DespawnPet). It returns a detached copy of the removed record.
func (r *Registry) RemoveByOwner(owner string) (Record, bool) {
	if r == nil {
		return Record{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.byOwner[owner]
	if rec == nil {
		return Record{}, false
	}
	delete(r.byOwner, owner)
	delete(r.byInstance, rec.Instance)
	return *rec, true
}

// World is the transport/world seam implemented by the root adapter.
// Each method maps to existing root helpers (see the package doc); the
// entity package never touches globals, frames, m5 state or the combat
// pipeline directly.
type World interface {
	// OwnerPos is entityPos(owner): the owner's registry tile.
	OwnerPos(owner string) (x, y int, ok bool)
	// SpawnPet is setEntityPos + Spawn [5] + Follow [11,5] for a grant.
	SpawnPet(rec Record)
	// MovePet is setEntityPos + Move [11,4] + Follow [11,5] for one step.
	// rec is the post-move snapshot (X/Y already advanced).
	MovePet(rec Record)
	// TeleportPet is setEntityPos + Despawn [13] + Spawn [5] + Follow
	// [11,5] for a long-range catch-up. rec is the post-teleport
	// snapshot (X/Y at the owner tile).
	TeleportPet(rec Record)
	// DespawnPet deletes the entity registry entry + Despawn [13].
	DespawnPet(instance string)
	// IsMob reports m9MobFor(target) != nil (killable-mob gate).
	IsMob(target string) bool
	// HitMob runs the mob mirror leg: Animation + Combat Hit + m9PlayerHit
	// credited to the owner. dmg is MirrorDamage.
	HitMob(petInstance, ownerInstance, target string, dmg int)
	// HitDummy runs the combat-dummy mirror leg under combatMu via
	// applyBossHitLocked. It reports false when the dummy is dead
	// (no-op, combatDead parity).
	HitDummy(petInstance, ownerInstance string, dmg int) bool
	// DummyTarget is combatDummyInstance.
	DummyTarget() string
}

// Tick steps every owned pet toward its owner (called from the central 20Hz
// tick loop; empty registry = no World calls). Beyond
// pets.ShouldTeleport the pet teleports to the owner; otherwise one
// pets.FollowStep + move. Pets never block movement: no blocked() consult,
// no resource registration (unchanged from the root wire). Pets removed
// concurrently are skipped (ghost re-validate under lock before each
// mutation).
func (r *Registry) Tick(w World) {
	if r == nil || w == nil {
		return
	}
	r.mu.Lock()
	recs := make([]*Record, 0, len(r.byOwner))
	for _, rec := range r.byOwner {
		recs = append(recs, rec)
	}
	r.mu.Unlock()
	for _, rec := range recs {
		ox, oy, ok := w.OwnerPos(rec.Owner)
		if !ok {
			continue // owner gone (disconnect cleanup removes the pet)
		}
		r.mu.Lock()
		live, stillThere := r.byOwner[rec.Owner]
		if !stillThere || live != rec {
			r.mu.Unlock()
			continue // removed concurrently: ghost, skip
		}
		px, py := rec.X, rec.Y
		r.mu.Unlock()
		switch {
		case pets.ShouldTeleport(ox, oy, px, py):
			r.mu.Lock()
			live, stillThere := r.byOwner[rec.Owner]
			if !stillThere || live != rec {
				r.mu.Unlock()
				continue
			}
			rec.X, rec.Y = ox, oy
			updated := *rec
			r.mu.Unlock()
			w.TeleportPet(updated)
		case pets.ShouldFollow(ox, oy, px, py):
			nx, ny := pets.FollowStep(ox, oy, px, py)
			if nx == px && ny == py {
				continue
			}
			r.mu.Lock()
			live, stillThere := r.byOwner[rec.Owner]
			if !stillThere || live != rec {
				r.mu.Unlock()
				continue
			}
			rec.X, rec.Y = nx, ny
			updated := *rec
			r.mu.Unlock()
			w.MovePet(updated)
		}
	}
}

// Mirror ports the attack-mirror (new-in-Go; TS pets never attack): after an
// owner swing at a killable target the pet issues a same-target swing
// through the combat pipeline, credited to the owner so retaliate/loot/quest
// flow is unchanged. Unknown targets are a silent no-op. The pet is
// re-validated after the target gate so a concurrently removed pet never
// swings (ghost).
func (r *Registry) Mirror(w World, owner, target string) {
	if r == nil || w == nil || owner == "" || target == "" {
		return
	}
	r.mu.Lock()
	rec := r.byOwner[owner]
	var petInstance string
	if rec != nil {
		petInstance = rec.Instance
	}
	r.mu.Unlock()
	if petInstance == "" {
		return
	}
	isMob := w.IsMob(target)
	isDummy := false
	if !isMob {
		isDummy = target == w.DummyTarget()
		if !isDummy {
			return
		}
	}
	r.mu.Lock()
	live := r.byOwner[owner]
	if live == nil || live.Instance != petInstance {
		r.mu.Unlock()
		return // removed concurrently: ghost, skip
	}
	r.mu.Unlock()
	if isMob {
		w.HitMob(petInstance, owner, target, MirrorDamage)
		return
	}
	w.HitDummy(petInstance, owner, MirrorDamage)
}
