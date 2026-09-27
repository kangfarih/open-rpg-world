// Loitering skill + cheatScore forgiveness (handler.ts:126-136,
// player.ts:545-560).
//
// TS source: handler.startUpdateInterval runs every updateTime (600ms) and,
// on every isTickInterval(32) tick — i.e. every ~19.2s — calls
// player.loiter() and resets player.cheatScore to 0. loiter() skips unless
// the tutorial is finished AND the player has been in the same region for
// LOITERING_THRESHOLD (90_000ms, via lastRegionChange stamped on region
// enter), then awards loitering.level * 5 XP to the Loitering skill.
//
// Go port: one Engine subsystem on the central 20Hz tick loop (a time
// throttle, like combatRegenTick — NOT a goroutine per player). The same
// sweep resets every live conn's cheatScore (TS forgiveness semantics: the
// periodic reset keeps transient strikes from accumulating to the >15
// disconnect across a long session) and awards Loitering XP to eligible
// heroes via the existing addXP path (Experience Skill + Skill Update
// frames, level-up Sync fanout, dirty marking — unchanged).
//
// Region tracking reuses the UpdateRegion hook (Boot's worldcore.Configure
// onRegion): markLoiterRegion stamps only when the authoritative region id
// actually changes, mirroring regions.handle's `entity.region !== region`
// gate (TS does NOT restamp on intra-region moves). Packet shapes are
// untouched; the Loitering skill id rides the corrected consts
// (protocol/player SkillLoitering = 17, TS-exact).
package server

import (
	"log"
	"os"
	"strconv"
	"time"

	"rpg-world-server/internal/player"
	worldcore "rpg-world-server/internal/world"
)

// LoiterThresholdMs is Modules.Constants.LOITERING_THRESHOLD (modules.ts):
// 90 seconds in the same region before loitering activates.
const LoiterThresholdMs = 90_000

// loiterTickMsDefault is the handler.ts cadence: updateTime 600ms x
// isTickInterval(32) = 19_200ms.
const loiterTickMsDefault = 19_200

// LoiterIntervalMs resolves the sweep cadence: LOITER_MS when set to a
// positive integer (test hook mirroring the WORLD_EVENT_MS precedent),
// else the TS ~19.2s cadence.
func LoiterIntervalMs() int64 {
	if v := os.Getenv("LOITER_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return loiterTickMsDefault
}

// markLoiterRegion records an authoritative region sighting for c (called
// from the UpdateRegion hook with the conn's authoritative region id).
// The stamp moves only on an actual region change — or on the first
// sighting (entity.region -1 parity) — never on intra-region moves.
func markLoiterRegion(c *playerConn, rid int) {
	if c == nil {
		return
	}
	now := time.Now().UnixMilli()
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	if !c.loiterInit || rid != c.loiterRegion {
		c.loiterRegion, c.loiterSince, c.loiterInit = rid, now, true
	}
}

// loiterTutorialFinished is the quests.ts isTutorialFinished read the warp
// gate uses (world_wire.go): default true when the tutorial quest def is
// absent, otherwise the m11 quest-stage lookup.
func loiterTutorialFinished(username string) bool {
	if questDefs["tutorial"] == nil {
		return true
	}
	return questStateFor(username).isFinished("tutorial")
}

// loiterEligible ports the player.ts loiter() gate: tutorial finished AND
// same-region 90s+ via lastRegionChange. sinceMs == -1 (never stamped, the
// TS character.ts lastRegionChange = -1 init) always passes the threshold.
func loiterEligible(tutorialFinished bool, sinceMs, nowMs int64) bool {
	if !tutorialFinished {
		return false
	}
	return nowMs-sinceMs >= LoiterThresholdMs
}

// loiterLevel reads the hero's current Loitering level (TS skills.get
// always returns a level-1 skill, so a missing row reads as 1).
func loiterLevel(username string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if s := st.Skills[player.SkillLoitering]; s != nil && s.Level > 0 {
		return s.Level
	}
	return 1
}

// loiterLastSweep throttles the Engine-driven sweep to LoiterIntervalMs.
var loiterLastSweep time.Time

// loiterTick is the Engine subsystem entry (boot.go tickEngine): the ONE
// ticker for loitering + cheatScore forgiveness — no per-player goroutines.
func loiterTick() {
	now := time.Now()
	if !loiterLastSweep.IsZero() && now.Sub(loiterLastSweep) < time.Duration(LoiterIntervalMs())*time.Millisecond {
		return
	}
	loiterLastSweep = now
	runLoiterSweep(now.UnixMilli())
}

// runLoiterSweep ports handler.ts:130-134 over every live conn: reset
// cheatScore (TS forgiveness, unconditional — even when the award below is
// gated off), then award level*5 Loitering XP to tutorial-finished heroes
// past the same-region threshold via the existing addXP path.
func runLoiterSweep(nowMs int64) {
	for _, c := range worldcore.AllOf[*playerConn]() {
		if c == nil {
			continue
		}
		c.Sess.CheatScore = 0
		c.sessMu.RLock()
		init, region, since := c.loiterInit, c.loiterRegion, c.loiterSince
		c.sessMu.RUnlock()
		if init && !loiterEligible(true, since, nowMs) {
			continue
		}
		if !loiterTutorialFinished(c.Username) {
			continue
		}
		level := loiterLevel(c.Username)
		amount := level * 5 // player.ts:559 addExperience(loitering.level * 5).
		addXP(c, c.Username, player.SkillLoitering, amount)
		rid := region
		if !init {
			rid = -1 // never stamped (TS entity.region -1 parity).
		}
		log.Printf("loiter: %s +%d xp (loitering level %d, region %d)", c.Username, amount, level, rid)
	}
}
