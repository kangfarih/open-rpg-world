// Package entity (effects) holds the Effect entity (type 9) slice —
// visual ground effects with duration-based auto-despawn.
//
// TS sources: game/entity/objects/effect.ts (Effect class: key+x+y ctor,
// duration from effectentities.json with 4000ms default, setTimeout ->
// despawnCallback), controllers/entities.ts (spawnEffect -> addEffect ->
// effect.onDespawn(removeEffect) -> add(entity) -> remove(entity) which
// broadcasts Spawn/Despawn through the standard entity pipeline).
//
// The effect entity is the simplest timed entity: spawn it, wait for its
// duration, then despawn. No interaction, no ownership, no blink. The
// only data-driven knob is the per-key duration in effectentities.json
// (currently just "lava": 5000ms). Unknown keys fall back to the TS
// default of 4000ms.
//
// Transport seam (EffectWorld) mirrors LootWorld:
//
//	SetEntityPos -> worldcore.SetEntityPos (position registry)
//	RemoveEntity -> worldcore.RemoveEntity (despawn teardown)
//	Broadcast    -> worldcore.Broadcast (Spawn/Despawn frames)
//
// Packet shapes are frozen: Spawn uses EntityData with Type=EntityEffect
// (9), Despawn uses the standard DespawnData{Instance}. The wire bytes
// match the TS add(entity)/remove(entity) pipeline.
package entity

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"rpg-world-server/internal/data"
	"rpg-world-server/internal/protocol"
)

// DefaultEffectDuration is the TS Effect.duration default (4000ms) used
// when effectentities.json has no entry for the key.
const DefaultEffectDuration = 4000 * time.Millisecond

// EffectWorld is the transport seam the adapter implements (same shape
// as LootWorld — the worldcore position index + broadcast fan-out).
type EffectWorld interface {
	SetEntityPos(inst string, x, y int)
	RemoveEntity(inst string)
	Broadcast(frames ...[]any)
}

// EffectDeps bundles the effect seams for one call. Configure once at
// boot (before the first spawn); zero value keeps every behavior dormant.
type EffectDeps struct {
	World EffectWorld
}

var effectDeps EffectDeps

// ConfigureEffects installs the effect seams (called once from the root
// boot, before the first spawn).
func ConfigureEffects(d EffectDeps) { effectDeps = d }

// ---------------------------------------------------------------------------
// Effect data (packages/server/data/effectentities.json).
// ---------------------------------------------------------------------------

type effectEntry struct {
	Duration *int `json:"duration"` // ms; nil -> DefaultEffectDuration
}

var (
	effectDataOnce sync.Once
	effectDurations = map[string]time.Duration{}
)

// loadEffectData parses effectentities.json once via the data package
// (env override -> filesystem -> embed). Unknown keys fall back to the
// 4000ms TS default at spawn time.
func loadEffectData() {
	effectDataOnce.Do(func() {
		raw, err := data.ReadFile("effectentities.json")
		if err != nil {
			log.Printf("effects: %v (using defaults)", err)
			return
		}
		var table map[string]effectEntry
		if err := json.Unmarshal(raw, &table); err != nil {
			log.Printf("effects: parse effectentities.json: %v", err)
			return
		}
		for k, v := range table {
			d := DefaultEffectDuration
			if v.Duration != nil && *v.Duration > 0 {
				d = time.Duration(*v.Duration) * time.Millisecond
			}
			effectDurations[k] = d
		}
		log.Printf("effects: loaded %d entries", len(effectDurations))
	})
}

// effectDuration returns the configured duration for key (loading the
// table on first call). Unknown keys get the TS default of 4000ms.
func effectDuration(key string) time.Duration {
	loadEffectData()
	if d, ok := effectDurations[key]; ok {
		return d
	}
	return DefaultEffectDuration
}

// ---------------------------------------------------------------------------
// Effect entity registry.
// ---------------------------------------------------------------------------

// Effect is one live effect entity in the world.
type Effect struct {
	Instance string
	Key      string
	X, Y     int
}

var (
	effectMu  sync.Mutex
	effectSeq int
	effects   = map[string]*Effect{}
)

// SpawnEffect creates a timed effect entity at (x, y), broadcasts its
// Spawn frame, and schedules auto-despawn after the key's duration
// (effectentities.json or 4000ms default). Returns the instance id.
//
// TS flow: new Effect(key, x, y) -> setTimeout(despawnCallback, duration)
// -> addEffect -> add(entity) which broadcasts Spawn. The despawnCallback
// calls removeEffect -> remove(entity) which broadcasts Despawn.
func SpawnEffect(key string, x, y int) string {
	effectMu.Lock()
	effectSeq++
	inst := fmt.Sprintf("effect-%d", effectSeq)
	e := &Effect{Instance: inst, Key: key, X: x, Y: y}
	effects[inst] = e
	effectMu.Unlock()

	dur := effectDuration(key)

	if effectDeps.World != nil {
		effectDeps.World.SetEntityPos(inst, x, y)
		payload := protocol.EntityData{
			Instance: inst,
			Type:     protocol.EntityEffect,
			Key:      key,
			Name:     key,
			X:        x,
			Y:        y,
		}
		effectDeps.World.Broadcast(protocol.Pkt(protocol.PacketSpawn, payload))
	}

	time.AfterFunc(dur, func() { DestroyEffect(inst, "expired") })

	log.Printf("effects: %s spawned (%s) at %d,%d duration=%v", inst, key, x, y, dur)
	return inst
}

// DestroyEffect tears an effect entity down (expiry or manual removal).
func DestroyEffect(inst, why string) {
	effectMu.Lock()
	e, ok := effects[inst]
	if ok {
		delete(effects, inst)
	}
	effectMu.Unlock()
	if !ok {
		return
	}
	if effectDeps.World != nil {
		effectDeps.World.RemoveEntity(inst)
		effectDeps.World.Broadcast(protocol.Pkt(protocol.PacketDespawn, protocol.DespawnData{Instance: inst}))
	}
	log.Printf("effects: %s destroyed (%s) (%s)", inst, e.Key, why)
}

// ActiveEffects snapshots the live effect count (test helper).
func ActiveEffects() int {
	effectMu.Lock()
	defer effectMu.Unlock()
	return len(effects)
}

// ResetEffects clears all effect state (test isolation helper).
func ResetEffects() {
	effectMu.Lock()
	defer effectMu.Unlock()
	effects = map[string]*Effect{}
	effectSeq = 0
}
