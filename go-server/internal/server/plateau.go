// Plateau tracking + per-player dynamic-collision gating (map-physics port).
//
// Plateau (map.ts getPlateauLevel over `map.plateau`, handler.ts:333,
// mob.ts:148): every authoritative player position update refreshes the
// player's tracked plateauLevel (hooked into trackPos, which runs on
// Movement Started/Step, handoffs, warp landings and every server-side
// teleport: teleport, minigameTeleport, respawn, test tp, login seedPos).
// Mobs track their spawn plateau in spawnMob (mob.ts:148 parity).
//
// The mob roam-step refusal lives at its dispatch point, and each swing
// direction carries the TS-exact ranged-only plateau gate at its own
// dispatch point (see entity.RangedBlocked: character.ts isNearTarget —
// melee adjacency is ungated, only ranged shooting UP is refused).
//
// Dynamic collision (map.ts:234-244): blockedForPlayer resolves the tile for
// one player through the quest/achievement-gated dynamic remap before the
// static checks. Client tile SERVING is per-player too (dynmap.go
// buildMapFrameFor re-skins the same remapped tiles at Ready and on
// quest/achievement finish, regions.ts buildDynamicTile parity).
package server

import (
	"strings"
	"sync"

	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/player"
)

// ---------------------------------------------------------------------------
// Per-player plateauLevel store (handler.ts:333 player.plateauLevel).
// ---------------------------------------------------------------------------

var (
	plateauMu     sync.Mutex
	plateauLevels = map[string]int{}
)

// plateauLevelOf reads the plateau table for a tile (map.ts getPlateauLevel
// parity, default 0). loadWorld ensures the cached world is present.
func plateauLevelOf(x, y int) int {
	loadWorld()
	if world == nil {
		return 0
	}
	return world.PlateauLevel(x, y)
}

// plateauTrack refreshes the tracked plateauLevel from the connection's
// authoritative tile. Called from trackPos (movement/handoff/warp/teleport
// paths) and directly from teleport/m9 respawn (idempotent double refresh
// alongside the trackPos call there).
func plateauTrack(c *playerConn) {
	if c == nil {
		return
	}
	lvl := plateauLevelOf(c.Sess.PlayerX, c.Sess.PlayerY)
	plateauMu.Lock()
	plateauLevels[c.Instance] = lvl
	plateauMu.Unlock()
}

// plateauGet reports the tracked plateauLevel (0 when never tracked).
func plateauGet(instance string) int {
	plateauMu.Lock()
	defer plateauMu.Unlock()
	return plateauLevels[instance]
}

// plateauForget drops the tracked level on disconnect.
func plateauForget(instance string) {
	plateauMu.Lock()
	delete(plateauLevels, instance)
	plateauMu.Unlock()
}

// ---------------------------------------------------------------------------
// Per-player dynamic collision gate (map.ts:234-244 isColliding branch).
// ---------------------------------------------------------------------------

// questProg adapts one player's quest state to entity.Progression
// (area.ts fulfillsRequirement parity: quest finished, else achievement
// finished).
type questProg struct {
	username string
}

func (q questProg) QuestFinished(key string) bool {
	def := questDefs[key]
	if def == nil {
		return false
	}
	return questStateFor(q.username).PlayerState.IsFinished(key)
}

func (q questProg) AchFinished(key string) bool {
	def := achDefs[key]
	if def == nil {
		return false
	}
	st := questStateFor(q.username)
	return st.PlayerState.Achs[key] >= def.StageCount
}

// dynamicRemapFor resolves the mapped collision tile for a player tile
// (entity.DynamicRemap over that player's quest state). Nil-safe: a nil
// conn (mob/engine callers) never remaps.
func dynamicRemapFor(c *playerConn, x, y int) (int, int, bool) {
	if c == nil || c.Username == "" {
		return 0, 0, false
	}
	return entity.DynamicRemap(x, y, questProg{username: c.Username})
}

// blockedForPlayer is the per-player movement collision: the dynamic remap
// first (map.ts:234-244), then the static resource/chest checks at the
// resolved tile. Engine/mob callers keep using blocked (mobs never fulfill
// quest requirements — TS isColliding without a player).
func blockedForPlayer(c *playerConn, x, y int) bool {
	if mx, my, ok := dynamicRemapFor(c, x, y); ok {
		return tileBlocked(mx, my) || resourceAt(mx, my) != "" || chestItemsAt(mx, my)
	}
	return blocked(x, y)
}

// ---------------------------------------------------------------------------
// Skill-name gate helper (door skill doors carry e.g. "Mining").
// ---------------------------------------------------------------------------

// doorSkillID resolves a TS skill name (case-insensitive, Modules.Skills
// naming) to the Go skill id. ok=false for unknown names.
func doorSkillID(name string) (int, bool) {
	for _, id := range []int{
		player.SkillLumberjacking, player.SkillAccuracy, player.SkillArchery,
		player.SkillHealth, player.SkillMagic, player.SkillMining,
		player.SkillStrength, player.SkillDefense, player.SkillFishing,
		player.SkillForaging,
	} {
		if strings.EqualFold(player.SkillName(id), name) {
			return id, true
		}
	}
	return 0, false
}
