// ---------------------------------------------------------------------------
// Areas — the area system: thin root adapter.
//
// The system lives in internal/entity (areas.go); this file keeps
// UNCHANGED public signatures so main.go, world_wire.go, mob_engine.go and
// commands.go call sites compile untouched. No area state stays here: the
// area/chest structs are entity.Area/entity.Chest aliases (no code outside
// this file touched their fields), the registries + per-player detection
// state live in entity, and packet shapes are frozen in the shared
// gameWorld adapter (mob_engine.go).
//
// The mob<->area call cycle (spawnMob->chestAreaAt/addChestMob;
// killMob->killHooks) is gone: both engines live in ONE package
// (internal/entity) behind the GameWorld seam.
//
// Only the TESTMAP dispatchers keep logic here (frame parsing + test
// wiring call the same public functions, so the debug frames are frozen).
// ---------------------------------------------------------------------------

package server

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"rpg-world-server/internal/entity"
)

// area/chest are entity-owned now (aliases — opaque uses in main.go
// keep compiling; no external code touches their fields).
type (
	area  = entity.Area
	chest = entity.Chest
)

// loadAreas parses world.json `areas` groups at boot (Node world.ts
// constructor builds one Areas subclass per group from map.areas).
func loadAreas() {
	loadWorld()
	var raw []byte
	if world != nil {
		var err error
		raw, err = os.ReadFile(worldPath())
		if err != nil {
			return
		}
	}
	entity.LoadAreas(raw)
	entity.SpawnStaticChests(gameWorld)
	// Marker population rides the same boot hook (entities.ts load parity):
	// all 4,226 world.json `entities` markers spawn here on the REAL path
	// only (TESTMAP/CLEAN/COMBAT return immediately, scenes untouched).
	// Static area-chests are NOT duplicated — SpawnStaticChests above owns
	// them (mimic batch).
	populateMarkers()
}

// chestAreaAt ports Mob.addToChestArea (mob.ts: chestAreas.inArea).
func chestAreaAt(x, y int) *area {
	return entity.ChestAreaAt(x, y)
}

// addChestMob ports Area.addEntity (mob.ts addToChestArea). Records the
// mob in the area and (first mob only) adopts its respawn delay as the
// chest spawn guard; a live unlooted chest is removed (chest.ts onSpawn ->
// removeChest).
func addChestMob(area *area, instance string, respawnDelay time.Duration) {
	entity.AddChestMob(area, instance, respawnDelay, gameWorld)
}

// killHooks fires the area chest-area death path for a killed mob
// (handler.ts: mob.area?.removeEntity(mob, attacker) -> onEmpty chest spawn
// + attacker achievement). killer is nil for killerless kills (no award).
func killHooks(m *mob, killer *playerConn) {
	killerInstance := ""
	if killer != nil {
		killerInstance = killer.Instance
	}
	entity.KillHookForMob(m.x, m.y, m.instance, killerInstance, gameWorld)
}

// chestFor finds a live chest entity by instance.
func chestFor(instance string) *chest {
	return entity.ChestFor(instance)
}

// chestItemsAt reports the chest occupying a tile (movement-block check).
func chestItemsAt(x, y int) bool {
	return entity.ChestAt(x, y)
}

// openChest ports entities.ts spawnChest onOpen in TS order: despawn
// the chest, spawn the mimic mob when flagged and opened by a player
// (non-respawnable, linked so its death re-spawns the chest), roll one
// entry and spawn it at the chest tile as a persistent M5 loot entity,
// then finish the chest's own achievement for the opener when set (static
// chests). Area achievements still fire at CLEAR time (RemoveChestMob,
// chest.ts onEmpty parity), never on open.
func openChest(c *playerConn, chest *chest) {
	if c == nil || c.Conn == nil || chest == nil {
		return
	}
	entity.OpenChest(chest, c.Instance, c.Username, gameWorld)
}

// areaPositionUpdate is the Area hook on the movement path (Node
// handleMovement -> detectAreas). Change-detection is per player per group.
func areaPositionUpdate(c *playerConn) {
	entity.OnPositionUpdate(c.Instance, c.Username, c.Sess.PlayerX, c.Sess.PlayerY, gameWorld)
}

// updatePVP ports player.updatePVP: notify + PVP packet on state flip.
func updatePVP(c *playerConn, inPVP bool) {
	entity.UpdatePVP(c.Instance, c.Username, inPVP, gameWorld)
}

// setFreezing applies/removes the Freezing status effect
// (Area.addPlayer/removePlayer -> player.status Effects.Freezing).
func setFreezing(c *playerConn, on bool) {
	entity.SetFreezing(c.Instance, on, gameWorld)
}

// pvpState reports the player's current pvp flag (Spawn PlayerData.pvp).
func pvpState(instance string) bool {
	return entity.PVPState(instance)
}

// forgetAreaPlayer drops per-player area state on disconnect.
func forgetAreaPlayer(instance string) {
	entity.ForgetPlayer(instance, gameWorld)
}

// injectTestAreas ensures the TESTMAP synthetic area bands (TESTMAP
// mode only; the per-group "don't shadow the real world" rule lives in
// entity.InjectTestAreas).
func injectTestAreas() {
	if !testMode || cleanMode || combatMode {
		return
	}
	entity.InjectTestAreas()
}

// ---------------------------------------------------------------------------
// AREA TEST debug frame (TESTMAP-only): chest-mob adoption + area echo for the
// e2e. Shape: C->S [46, {"m10test":"..."}] (rides the Minigame dispatcher).
// ---------------------------------------------------------------------------

func handleAreaTest(c *playerConn, frame clientFrame) {
	if !testMode || cleanMode || combatMode || len(frame) < 2 {
		return
	}
	// Admin-rank gate (see handleMinigameTest: TESTMAP default stays ON, the gate
	// closes the any-client warp/spawn hole).
	if !isAdmin(c) {
		return
	}
	var data struct {
		M10Test  string `json:"m10test"`
		Instance string `json:"instance"`
		Key      string `json:"key"`
		Delay    int    `json:"delay"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
	}
	if err := json.Unmarshal(frame[1], &data); err != nil {
		return
	}
	// Default key preserved for the M9-era rat spawn; the area chest harness
	// picks a passive low-HP mob (crab) so the kill lands inside the area
	// before the engine's roam pass can move it out.
	mobKey := data.Key
	if mobKey == "" {
		mobKey = "rat"
	}
	switch data.M10Test {
	case "chestmob":
		// Spawn a mob inside the chest area and adopt it (Mob.addToChestArea
		// parity — the harness spawns at coordinates inside the area). The
		// Respawn override rides the spawn call (no post-spawn mutation).
		// Area membership is looked up at the spawn tile (not the first
		// area): coords outside every chest area adopt nothing.
		area := entity.ChestAreaAt(data.X, data.Y)
		if area == nil {
			return
		}
		over := mobOverrides{}
		if data.Delay > 0 {
			over.Respawn = time.Duration(data.Delay) * time.Millisecond
		}
		if !spawnMob(data.Instance, mobKey, data.X, data.Y, over) {
			return
		}
		if m := mobFor(data.Instance); m != nil {
			addChestMob(area, data.Instance, m.respawnDelay())
			notifyPlayer(c, fmt.Sprintf("m10:chestmob=%s", data.Instance))
		}
	case "mobhp":
		// Echo mob state (mirrors m9's mobhp for the kill leg).
		m := mobFor(data.Instance)
		if m == nil || c == nil {
			return
		}
		m.mu.Lock()
		echo := fmt.Sprintf("m10:mob=%s hp=%d/%d", data.Instance, m.hp, m.maxHP)
		m.mu.Unlock()
		notifyPlayer(c, echo)
	case "chest":
		// Echo live chest state for the chest leg (introspection).
		var echo string
		for _, a := range entity.ChestAreas() {
			if ch := a.LiveChest(); ch != nil {
				echo = fmt.Sprintf("m10:chest=%s x=%d y=%d", ch.Instance, ch.X, ch.Y)
			}
		}
		if echo == "" {
			echo = "m10:chest=none"
		}
		notifyPlayer(c, echo)
	}
}
