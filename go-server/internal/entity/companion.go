// Pet companion orchestration, extracted behavior-frozen from the root
// pets_wire.go adapter (task D2b item 2: pets_wire.go -> internal/entity,
// the registry already lives here).
//
// TS sources (all read-only recon, no new opcodes):
//   - packages/server/src/game/entity/character/pet/pet.ts — Pet extends
//     Character, spawns at the owner's (x, y); serialize() is PetData
//     (types/pet.d.ts: EntityData + `owner` + `movementSpeed`).
//   - packages/server/src/controllers/entities.ts spawnPet/removePet —
//     spawnPet(owner, key) + addPet; removePet dispatches remove + deletes
//     the pets dict entry.
//   - packages/server/src/game/entity/character/player/player.ts — setPet
//     (spawnPet + follow, ALREADY_HAVE_PET guard), removePet (needs inventory
//     space, returns a "<mobkey>pet" item, NO_SPACE_PET otherwise), hasPet.
//   - packages/server/src/game/entity/character/player/handler.ts —
//     handleMovement: pet follows when Manhattan distance > 2 (follow packet);
//     distance > 10 -> removePet + spawnPet at the player + follow.
//     handleDrop: item.isPetItem() -> setPet(item.pet) instead of a world item.
//   - packages/server/src/game/entity/character/character.ts — follow()
//     sends Movement Follow [11,5,{instance,target}] to regions; teleport()
//     repositions + sends Teleport [12,{instance,x,y}].
//   - packages/server/src/game/entity/character/player/incoming.ts —
//     handlePet: C->S Pet [58,{opcode}] Pickup(0) -> removePet (pickingUpPet
//     spam guard).
//   - packages/server/data/items.json — pet items carry a "pet" mob key
//     (ratpet->rat, rathatpet->rathat, ratballoonpet->ratballoon).
//   - packages/server/data/quests/royalpet.json — the final stage grants a
//     "catpet" inventory ITEM (not a direct pet); the pet itself comes from
//     dropping/using that item (handleDrop parity below), so m11 needs no hook.
//
// Frames used (all pre-existing shapes, verified against the TS tree):
//
//	S->C Spawn [5, PetData] with type 7 (EntityType.Pet; protocol has no
//	  EntityPet constant so the literal 7 is used here). NOTE: there is no
//	  S->C Pet [58] spawn — packet 58 is C->S only (Pickup); pets ride the
//	  generic Spawn like every other entity (client entities.ts createPet
//	  reads info.owner + info.movementSpeed, so the payload carries `owner`,
//	  not just EntityData.ownerInstance).
//	S->C Movement Move [11,4,{instance,x,y}] per follow step (m9 moveToLocked
//	  precedent) + Movement Follow [11,5,{instance,target}] (character.follow).
//	S->C Despawn [13,{instance}] + Spawn [5, PetData] at the owner on
//	  teleport (handler.ts removePet+spawnPet parity).
//	S->C Teleport is NOT used for pets (handler.ts respawns instead).
//	C->S Pet [58,{opcode}] Pickup(0) -> inventory return + Despawn.
//	C->S Container Remove (drop) of a pet item -> setPet parity.
//	S->C Container Add on pickup return; S->C notify misc:ALREADY_HAVE_PET /
//	  misc:NO_SPACE_PET.
//
// Divergences from TS (documented, inherited from the root wire):
//   - Teleport threshold: TS respawns at distance > 10; internal/pets uses
//     > 10 (TS handler.ts parity).
//   - Follow stepping is server-side one-tile FollowStep toward the owner
//     (TS pets move via client pathing on the Follow packet; no server grid
//     step exists there). Pets never consult blocked() and never register in
//     resourceEntities, so they can never block movement (server isColliding
//     only checks tiles + resource occupants).
//   - Teleport reuses the SAME pet instance (TS spawnPet mints a fresh
//     "<type>-<n>" instance on every respawn); stable identity keeps Who/List
//     tracking trivial for the harness.
//   - Attack-mirror is new (TS pets never attack): each owner swing at a
//     killable mob deals a small fixed pet hit through the existing combat
//     pipeline (Animation + Combat Hit + m9PlayerHit / applyBossHitLocked),
//     credited to the owner (retaliate/loot/quest flow unchanged).
//   - Hunger/expiry (internal/pets IsHungry/IsExpired) have no TS source and
//     are NOT enforced here (documented-skip); born/fed timestamps are kept
//     so the state probe can report the predicates.
//
// Everything transport/world/state related stays with the root adapter and
// is reached only through CompanionConn (per-connection delivery) and
// CompanionDeps. Frames are built with internal/protocol (the same
// constructors the root pkt/pktOp shims wrap), so wire bytes are identical.
// All log strings and TESTMAP debug ops are kept verbatim.
package entity

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"rpg-world-server/internal/pets"
	"rpg-world-server/internal/protocol"
)

// CompanionEntityType is Modules.EntityType.Pet (7). internal/protocol
// defines no EntityPet constant and packet shapes are frozen, so the
// literal lives here.
const CompanionEntityType = 7

// CompanionPickup is Opcodes.Pet Pickup(0) (the only opcode).
const CompanionPickup = 0

// companionSpawnData mirrors PetData (common/types/pet.d.ts): EntityData
// plus the `owner` instance string the client createPet reads (EntityData
// only carries ownerInstance, used by projectiles). movementSpeed rides
// EntityData.
type companionSpawnData struct {
	protocol.EntityData
	Owner string `json:"owner"`
}

// CompanionConn is the minimal per-connection view for companion delivery.
type CompanionConn struct {
	Instance string
	Username string
	X, Y     int // authoritative tile snapshot (grant spawn parity)
	// Send delivers frames to this conn (gnet.Send parity).
	Send func(frames ...[]any)
	// Notify sends a client text notification (m6Notify parity).
	Notify func(message string)
}

// CompanionWorld is the position/fan-out seam (world Registry parity).
type CompanionWorld interface {
	OwnerPos(owner string) (x, y int, ok bool)
	SetEntityPos(inst string, x, y int)
	RemoveEntity(inst string)
	Broadcast(frames ...[]any)
}

// CompanionDeps bundles the companion seams (implemented by the root
// adapter; never by this package).
type CompanionDeps struct {
	World CompanionWorld
	// Test gates the TESTMAP debug dispatcher (testMode parity).
	Test bool
	// InventoryCount reports the inventory length (pickup space gate).
	InventoryCount func(username string) int
	// InventoryKey reads one inventory slot key (drop parity).
	InventoryKey func(username string, index int) (string, bool)
	// AddItem adds the pickup-return item, reporting its slot index
	// (m5AddItem parity).
	AddItem func(username, key string, count int) int
	// MarkDirty flags the player row for the 10s persist flush.
	MarkDirty func(key string)
	// IsMob reports m9MobFor(target) != nil (killable-mob gate).
	IsMob func(target string) bool
	// CreditMob runs the mob mirror leg's damage credit (m9PlayerHit with
	// the owner conn for retaliate/loot/quest flow, nil-safe).
	CreditMob func(ownerInstance, target string, dmg int)
	// StrikeDummy runs the combat-dummy mirror leg under combatMu via
	// applyBossHitLocked. False when the dummy is dead (no-op parity).
	StrikeDummy func(petInstance, ownerInstance string, dmg int) bool
	// DummyTarget is combatDummyInstance.
	DummyTarget string
	// NowMs is the millisecond clock (grant/probe timestamps).
	NowMs func() int64
}

var cdeps CompanionDeps

// ConfigureCompanions installs the companion seams (called once from the
// root boot, before grants or ticks run).
func ConfigureCompanions(d CompanionDeps) { cdeps = d }

var companions = NewRegistry()

func companionIntp(v int) *int { return &v }

// companionPayload builds the Spawn payload for a live pet (owner's movement
// speed copied per pet.ts serialize; hero default 220).
func companionPayload(r *Record) companionSpawnData {
	return companionSpawnData{
		EntityData: protocol.EntityData{
			Instance:      r.Instance,
			Type:          CompanionEntityType,
			Key:           r.MobKey,
			Name:          r.MobKey,
			X:             r.X,
			Y:             r.Y,
			Orientation:   companionIntp(protocol.OrientationDown),
			Level:         companionIntp(1),
			HitPoints:     companionIntp(100),
			MaxHitPoints:  companionIntp(100),
			MovementSpeed: companionIntp(220),
			AttackRange:   companionIntp(1),
		},
		Owner: r.Owner,
	}
}

// companionFollowFrame builds the S->C Movement Follow frame
// (character.follow: [11,5,{instance,target}]).
func companionFollowFrame(instance, owner string) []any {
	return protocol.PktOp(protocol.PacketMovement, protocol.MovementFollow, map[string]any{
		"instance": instance, "target": owner,
	})
}

func companionMoveFrame(rec Record) []any {
	return protocol.PktOp(protocol.PacketMovement, protocol.MovementMove, protocol.ServerMovement{
		Instance: rec.Instance, X: companionIntp(rec.X), Y: companionIntp(rec.Y),
	})
}

// companionWorld adapts CompanionDeps.World to the entity.World seam so the
// Registry Tick/Mirror orchestration (pet.go) drives the package-owned
// frame builders below.
type companionWorld struct{}

func (companionWorld) OwnerPos(owner string) (int, int, bool) {
	if cdeps.World == nil || owner == "" {
		return 0, 0, false
	}
	return cdeps.World.OwnerPos(owner)
}

func (companionWorld) SpawnPet(rec Record) {
	if cdeps.World == nil {
		return
	}
	cdeps.World.SetEntityPos(rec.Instance, rec.X, rec.Y)
	cdeps.World.Broadcast(protocol.Pkt(protocol.PacketSpawn, companionPayload(&rec)))
	cdeps.World.Broadcast(companionFollowFrame(rec.Instance, rec.Owner))
}

func (companionWorld) MovePet(rec Record) {
	if cdeps.World == nil {
		return
	}
	cdeps.World.SetEntityPos(rec.Instance, rec.X, rec.Y)
	cdeps.World.Broadcast(companionMoveFrame(rec))
	cdeps.World.Broadcast(companionFollowFrame(rec.Instance, rec.Owner))
}

func (companionWorld) TeleportPet(rec Record) {
	if cdeps.World == nil {
		return
	}
	cdeps.World.SetEntityPos(rec.Instance, rec.X, rec.Y)
	cdeps.World.Broadcast(protocol.Pkt(protocol.PacketDespawn, protocol.DespawnData{Instance: rec.Instance}))
	cdeps.World.Broadcast(protocol.Pkt(protocol.PacketSpawn, companionPayload(&rec)))
	cdeps.World.Broadcast(companionFollowFrame(rec.Instance, rec.Owner))
	log.Printf("pets: %s teleported to owner %s (%d,%d)", rec.Instance, rec.Owner, rec.X, rec.Y)
}

func (companionWorld) DespawnPet(instance string) {
	if cdeps.World == nil {
		return
	}
	cdeps.World.RemoveEntity(instance)
	cdeps.World.Broadcast(protocol.Pkt(protocol.PacketDespawn, protocol.DespawnData{Instance: instance}))
}

func (companionWorld) IsMob(target string) bool {
	if cdeps.IsMob == nil {
		return false
	}
	return cdeps.IsMob(target)
}

func (companionWorld) HitMob(petInstance, ownerInstance, target string, dmg int) {
	if cdeps.World == nil {
		return
	}
	cdeps.World.Broadcast(protocol.Pkt(protocol.PacketAnimation, protocol.AnimationData{Instance: petInstance, Action: protocol.ActionAttack}))
	cdeps.World.Broadcast(protocol.PktOp(protocol.PacketCombat, protocol.CombatHit, protocol.CombatData{
		Instance: petInstance, Target: target,
		Hit: protocol.HitData{Type: protocol.HitsNormal, Damage: dmg},
	}))
	if cdeps.CreditMob != nil {
		cdeps.CreditMob(ownerInstance, target, dmg)
	}
	log.Printf("pets: %s mirrored %s -> %s dmg=%d", petInstance, ownerInstance, target, dmg)
}

func (companionWorld) HitDummy(petInstance, ownerInstance string, dmg int) bool {
	if cdeps.StrikeDummy == nil {
		return false
	}
	ok := cdeps.StrikeDummy(petInstance, ownerInstance, dmg)
	if ok {
		log.Printf("pets: %s mirrored %s -> dummy dmg=%d", petInstance, ownerInstance, dmg)
	}
	return ok
}

func (companionWorld) DummyTarget() string { return cdeps.DummyTarget }

func companionNow() int64 {
	if cdeps.NowMs != nil {
		return cdeps.NowMs()
	}
	return time.Now().UnixMilli()
}

// GrantCompanion spawns a companion for the owner's connection (player.ts
// setPet: ALREADY_HAVE_PET guard, spawn at the owner's tile, immediate
// follow). Returns nil when the owner already has a pet.
func GrantCompanion(c *CompanionConn, mobKey, itemKey string) *Record {
	if c == nil || c.Instance == "" || mobKey == "" {
		return nil
	}
	rec, already := companions.Grant(c.Instance, c.X, c.Y, mobKey, itemKey, companionNow())
	if already {
		if c.Notify != nil {
			c.Notify("misc:ALREADY_HAVE_PET")
		}
		return nil
	}
	if rec == nil {
		return nil
	}
	companionWorld{}.SpawnPet(*rec)
	log.Printf("pets: %s granted %s (%s) at %d,%d", c.Instance, rec.Instance, mobKey, rec.X, rec.Y)
	return rec
}

// HasCompanionOwner reports whether the player instance owns a pet.
func HasCompanionOwner(owner string) bool { return companions.Has(owner) }

// CompanionPayloadByInstance resolves a Who lookup for a live pet instance.
func CompanionPayloadByInstance(instance string) (any, bool) {
	r, ok := companions.ByInstance(instance)
	if !ok {
		return nil, false
	}
	return companionPayload(&r), true
}

// CompanionTick steps every owned pet toward its owner (called from the
// central 20Hz tick loop; empty registry = no frames).
func CompanionTick() { companions.Tick(companionWorld{}) }

// MirrorCompanionSwing ports the attack-mirror (new-in-Go; TS pets never
// attack): after an owner swing at a killable target the pet issues a
// same-target swing through the existing combat pipeline (Animation +
// Combat Hit + damage), credited to the owner so retaliate/loot/quest flow
// is unchanged.
func MirrorCompanionSwing(ownerInstance, target string) {
	if ownerInstance == "" || target == "" {
		return
	}
	companions.Mirror(companionWorld{}, ownerInstance, target)
}

// ForgetCompanion despawns + drops pet state on disconnect
// (abForgetPlayer / handler.ts disconnect removePet precedent: silent, no
// inventory return).
func ForgetCompanion(ownerInstance string) {
	if ownerInstance == "" {
		return
	}
	r, ok := companions.RemoveByOwner(ownerInstance)
	if !ok {
		return
	}
	companionWorld{}.DespawnPet(r.Instance)
	log.Printf("pets: %s forgotten on disconnect of %s", r.Instance, ownerInstance)
}

// HandleCompanionPacket routes C->S Pet frames [58,{opcode}] (incoming.ts
// handlePet): Pickup(0) returns the pet to the inventory (removePet:
// NO_SPACE_PET when full, else a "<mobkey>pet" Container Add + Despawn).
// The registry removal is the CAS: the space gate runs first (no loss on
// full), then RemoveByOwner claims the pet — a concurrent double-pickup
// loses the CAS and returns without a second AddItem.
func HandleCompanionPacket(c *CompanionConn, frame []json.RawMessage) {
	if len(frame) < 2 || c == nil || c.Instance == "" {
		return
	}
	var d struct {
		Opcode *int `json:"opcode"`
	}
	if err := json.Unmarshal(frame[1], &d); err != nil || d.Opcode == nil || *d.Opcode != CompanionPickup {
		return
	}
	if _, ok := companions.ByOwner(c.Instance); !ok {
		return
	}
	if cdeps.InventoryCount != nil && cdeps.InventoryCount(c.Username) >= protocol.ModulesInventorySize {
		if c.Notify != nil {
			c.Notify("misc:NO_SPACE_PET")
		}
		return
	}
	if cdeps.AddItem == nil {
		return
	}
	r, ok := companions.RemoveByOwner(c.Instance)
	if !ok {
		return // lost the CAS: concurrent pickup already claimed it
	}
	idx := cdeps.AddItem(c.Username, r.ItemKey, 1)
	if c.Send != nil {
		c.Send(protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: idx, Key: r.ItemKey, Count: 1, Enchantments: map[string]any{}},
		}))
	}
	if cdeps.MarkDirty != nil {
		cdeps.MarkDirty(c.Username)
	}
	companionWorld{}.DespawnPet(r.Instance)
	log.Printf("pets: %s picked up by %s (+%s)", r.Instance, c.Instance, r.ItemKey)
}

// CompanionDropKey peeks the inventory slot for a pet item (handler.ts
// item.isPetItem() parity via the items.json table in this package).
func CompanionDropKey(username string, index int) (mob, item string, ok bool) {
	if cdeps.InventoryKey == nil {
		return "", "", false
	}
	key, ok := cdeps.InventoryKey(username, index)
	if !ok {
		return "", "", false
	}
	mob, ok = LookupItem(key)
	if !ok {
		return "", "", false
	}
	return mob, key, true
}

// CompanionResolveKey accepts a pet-item key ("ratpet") or a mob key ("rat",
// "cat") for the debug grant; unknown/empty defaults to rat/ratpet.
func CompanionResolveKey(key string) (mob, item string) { return ResolveKey(key) }

// CompanionTestHandler is the TESTMAP-only debug dispatcher (m9test/m11test/
// abtest precedent, rides [46 {pettest}]): grant spawns a companion, state
// echoes the live record + hunger/expiry predicates, remove despawns it.
func CompanionTestHandler(c *CompanionConn, data []byte) {
	if !cdeps.Test || c == nil {
		return
	}
	var d struct {
		PetTest string `json:"pettest"`
		Key     string `json:"key"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.PetTest == "" {
		return
	}
	switch d.PetTest {
	case "grant":
		mob, item := CompanionResolveKey(d.Key)
		if r := GrantCompanion(c, mob, item); r != nil {
			if c.Notify != nil {
				c.Notify(fmt.Sprintf("pet:grant %s mob=%s at=%d,%d", r.Instance, mob, r.X, r.Y))
			}
		}
	case "state":
		r, ok := companions.ByOwner(c.Instance)
		if !ok {
			if c.Notify != nil {
				c.Notify("pet:state none")
			}
			return
		}
		now := companionNow()
		ox, oy, ok := companionWorld{}.OwnerPos(r.Owner)
		dist := -1
		if ok {
			dist = pets.Distance(ox, oy, r.X, r.Y)
		}
		if c.Notify != nil {
			c.Notify(fmt.Sprintf("pet:state %s mob=%s x=%d y=%d dist=%d hungry=%v expired=%v",
				r.Instance, r.MobKey, r.X, r.Y, dist,
				// Hunger/expiry are report-only (no TS source, not enforced):
				// hunger uses the package threshold, expiry a nominal
				// never-elapsing lifespan.
				pets.IsHungry(now, r.FedMs), pets.IsExpired(now, r.BornMs, 1<<62)))
		}
	case "remove":
		r, ok := companions.RemoveByOwner(c.Instance)
		if !ok {
			if c.Notify != nil {
				c.Notify("pet:state none")
			}
			return
		}
		companionWorld{}.DespawnPet(r.Instance)
		if c.Notify != nil {
			c.Notify("pet:removed " + r.Instance)
		}
	}
}
