// Statistics adapter: wires the pure internal/player/stats counters into
// the live flows (gather exhaust, mob kill, pickup, examine) and finishes
// milestone achievements through the m11/quest path.
//
// TS sources: statistics.ts (handleSkill/addMobKill/addMobExamine/addDrop),
// resourceskill.ts:121 (handleSkill on exhaust), handler.ts:765 (addMobKill
// on mob death), player.ts:1268 (addDrop on owner pickup).
package server

import (
	"rpg-world-server/internal/persist"
	"rpg-world-server/internal/player/stats"
)

// statsCopyOf snapshots a player's counters for the persist write path
// (toPersist).
func statsCopyOf(username string) stats.Snapshot {
	return stats.CopyOf(username)
}

// statsInstall restores a player's counters from the persist load path
// (persistToM5), converting the storage blob to the domain snapshot.
func statsInstall(username string, blob persist.StatsBlob) {
	stats.Install(username, stats.Snapshot{
		MobKills:        blob.MobKills,
		MobExamines:     blob.MobExamines,
		Resources:       blob.Resources,
		Drops:           blob.Drops,
		PvPKills:        blob.PvPKills,
		PvPDeaths:       blob.PvPDeaths,
		CreationTime:    blob.CreationTime,
		TotalTimePlayed: blob.TotalTimePlayed,
		LastLogin:       blob.LastLogin,
		LoginCount:      blob.LoginCount,
	})
}

// statsHandleSkill records one successful gather for skill and finishes the
// milestone achievement when one is reached (statistics.handleSkill parity:
// foraging skipped, key `<skill><N>`). Unknown achievement keys are ignored
// (TS `?.finish()` nil parity). Nil-safe.
func statsHandleSkill(c *playerConn, skill string) {
	if c == nil || c.Username == "" {
		return
	}
	var ach string
	var fired bool
	stats.Update(c.Username, func(st *stats.State) {
		ach, fired = stats.HandleSkill(st, skill)
	})
	if !fired {
		// No milestone: still persist the advanced counter (foraging never
		// advances — HandleSkill early-returns — so this is a gather tick).
		if skill != "foraging" {
			markDirty(c.Username)
		}
		return
	}
	markDirty(c.Username)
	if achDefs[ach] == nil {
		return
	}
	finishAchievement(c, ach)
}

// statsRecordKill records a mob kill for the killer (statistics.addMobKill
// parity: counter only — mobKills feeds NO achievement, verified against
// statistics.ts). Nil-safe.
func statsRecordKill(c *playerConn, mobKey string) {
	if c == nil || c.Username == "" {
		return
	}
	stats.Update(c.Username, func(st *stats.State) {
		stats.AddMobKill(st, mobKey)
	})
	markDirty(c.Username)
}

// statsAddDrop records a pickup for the owner (player.ts:1268 parity: only
// when the loot owner matches the picker). Nil-safe.
func statsAddDrop(c *playerConn, key string, count int) {
	if c == nil || c.Username == "" {
		return
	}
	stats.Update(c.Username, func(st *stats.State) {
		stats.AddDrop(st, key, count)
	})
	markDirty(c.Username)
}

// statsAddMobExamine records examining key and finishes the examiner
// achievement at 10/25/50 distinct examines (statistics.addMobExamine
// parity). Nil-safe.
func statsAddMobExamine(c *playerConn, key string) {
	if c == nil || c.Username == "" {
		return
	}
	var ach string
	var fired bool
	stats.Update(c.Username, func(st *stats.State) {
		ach, fired = stats.AddMobExamine(st, key)
	})
	markDirty(c.Username)
	if !fired || achDefs[ach] == nil {
		return
	}
	finishAchievement(c, ach)
}

// statsRecordLogin stamps the login lifecycle fields (creationTime on first
// login, lastLogin, loginCount). Called once per loginWelcome, after
// statsInstall has restored the persisted counters.
func statsRecordLogin(username string) {
	if username == "" {
		return
	}
	stats.RecordLogin(username)
	markDirty(username)
}

// statsRecordDisconnect accumulates the session duration into
// TotalTimePlayed. Called once per disconnect, before the persist flush.
func statsRecordDisconnect(username string) {
	if username == "" {
		return
	}
	stats.AccumulateSession(username)
	markDirty(username)
}

// statsRecordPvPKill increments the killer's PvP kill counter (handler.ts
// handleKill when victim isPlayer). No achievement fires.
func statsRecordPvPKill(c *playerConn) {
	if c == nil || c.Username == "" {
		return
	}
	stats.Update(c.Username, func(st *stats.State) {
		stats.AddPvPKill(st)
	})
	markDirty(c.Username)
}

// statsRecordPvPDeath increments the victim's PvP death counter (handler.ts
// handleDeath when killer isPlayer). No achievement fires.
func statsRecordPvPDeath(c *playerConn) {
	if c == nil || c.Username == "" {
		return
	}
	stats.Update(c.Username, func(st *stats.State) {
		stats.AddPvPDeath(st)
	})
	markDirty(c.Username)
}
