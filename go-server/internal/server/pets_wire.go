// Pet companion wiring — thin root adapter over internal/entity.
//
// Canonical owner: internal/entity (companion.go: grant/pickup/drop/test
// orchestration + Spawn/Movement/Despawn frame builders; pet.go: registry,
// follow/teleport + attack-mirror orchestration). This file only wires the
// package seams to the root globals (world Registry positions, gnet sends,
// m5 inventory, m9/combat damage pipeline) and keeps the entry points
// main.go/m6.go call — with UNCHANGED signatures — delegating to the
// package. Packet shapes, log strings and debug ops are identical (owned by
// the package).
package server

import (
	"rpg-world-server/internal/entity"
	gnet "rpg-world-server/internal/net"
	worldcore "rpg-world-server/internal/world"
)

// petEntityType is Modules.EntityType.Pet (7). internal/protocol defines no
// EntityPet constant and packet shapes are frozen, so the literal lives here.
const petEntityType = entity.CompanionEntityType

// Pet opcodes (Opcodes.Pet in opcodes.ts): Pickup 0 (the only opcode).
const petPickup = entity.CompanionPickup

// petMirrorDamage is the fixed pet swing damage (new-in-Go; TS pets never
// attack). Alias of entity.MirrorDamage; the orchestration passes the value
// through the World seam so this file never hard-codes a second copy.
const petMirrorDamage = entity.MirrorDamage

// petRecord is one live companion (alias of the entity registry record).
type petRecord = entity.Record

// petConn converts a root conn to the package view (nil-safe). The tile is
// snapshotted at call time (grant spawn parity); delivery stays direct.
func petConn(c *playerConn) *entity.CompanionConn {
	if c == nil {
		return nil
	}
	return &entity.CompanionConn{
		Instance: c.Instance,
		Username: c.Username,
		X:        c.Sess.PlayerX,
		Y:        c.Sess.PlayerY,
		Send: func(frames ...[]any) {
			_ = gnet.Send(c.Conn, frames...)
		},
		Notify: func(message string) {
			notifyPlayer(c, message)
		},
	}
}

// petConfigure wires the companion seams (called once from initPlayerState, before
// grants or ticks run).
func petConfigure() {
	entity.ConfigureCompanions(entity.CompanionDeps{
		World: petWorld{},
		Test:  testMode,
		InventoryCount: func(username string) int {
			return len(playerStateFor(username).Inv)
		},
		InventoryKey: func(username string, index int) (string, bool) {
			st := playerStateFor(username)
			pstateMu.Lock()
			defer pstateMu.Unlock()
			if index < 0 || index >= len(st.Inv) {
				return "", false
			}
			return st.Inv[index].Key, true
		},
		AddItem: func(username, key string, count int) int {
			return addItem(username, key, count)
		},
		MarkDirty: markDirty,
		IsMob: func(target string) bool {
			return mobFor(target) != nil
		},
		CreditMob: func(ownerInstance, target string, dmg int) {
			m := mobFor(target)
			if m == nil {
				return
			}
			mobPlayerHit(m, petConnByInstance(ownerInstance), dmg)
		},
		StrikeDummy: func(petInstance, ownerInstance string, dmg int) bool {
			combatMu.Lock()
			defer combatMu.Unlock()
			if combatDead {
				return false
			}
			applyBossHitLocked(petInstance, dmg, HitsNormal, nil, false, -1, true)
			return true
		},
		DummyTarget: combatDummyInstance,
	})
}

// petWorld implements entity.CompanionWorld over the world Registry
// (positions + fan-out). Frame construction lives in the package.
type petWorld struct{}

func (petWorld) OwnerPos(owner string) (int, int, bool) {
	return worldcore.EntityPos(owner)
}
func (petWorld) SetEntityPos(inst string, x, y int) { worldcore.SetEntityPos(inst, x, y) }
func (petWorld) RemoveEntity(inst string)           { worldcore.RemoveEntity(inst) }
func (petWorld) Broadcast(frames ...[]any)          { worldcore.Broadcast(frames...) }

// petConnByInstance resolves a live playerConn by instance for combat credit
// (retaliate/loot/quest flow). Nil when the owner is gone; mobPlayerHit is
// nil-safe (skips credit, still applies broadcast-side damage already sent).
func petConnByInstance(instance string) *playerConn {
	c, _ := worldcore.Find[*playerConn](instance)
	return c
}

func petGrant(c *playerConn, mobKey, itemKey string) *petRecord {
	return entity.GrantCompanion(petConn(c), mobKey, itemKey)
}

func petHasOwner(owner string) bool { return entity.HasCompanionOwner(owner) }

func petPayloadByInstance(instance string) (any, bool) {
	return entity.CompanionPayloadByInstance(instance)
}

func petTick() { entity.CompanionTick() }

func petMirrorSwing(c *playerConn, target string) {
	if c == nil || target == "" {
		return
	}
	entity.MirrorCompanionSwing(c.Instance, target)
}

func petForgetPlayer(c *playerConn) {
	if c == nil {
		return
	}
	entity.ForgetCompanion(c.Instance)
}

func petHandlePacket(c *playerConn, frame clientFrame) {
	entity.HandleCompanionPacket(petConn(c), frame)
}

func petDropKey(c *playerConn, index int) (mob, item string, ok bool) {
	if c == nil {
		return "", "", false
	}
	return entity.CompanionDropKey(c.Username, index)
}

func petResolveKey(key string) (mob, item string) {
	return entity.CompanionResolveKey(key)
}

func petTestHandler(c *playerConn, data []byte) {
	// Admin-rank gate (see handleMinigameTest: TESTMAP default stays ON, the gate
	// closes the any-client pet-grant hole).
	if !isAdmin(c) {
		return
	}
	entity.CompanionTestHandler(petConn(c), data)
}
