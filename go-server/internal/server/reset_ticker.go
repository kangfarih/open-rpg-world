// Daily/weekly reset ticker — background goroutine that checks online
// players' reset timestamps every 60s and fires callbacks when a boundary
// is crossed. Login-time check lives in loginWelcome (player_state.go).
//
// The pure logic is in internal/resets (Kind, Timestamps, IsDue, MaybeReset).
// This file owns the server-side wiring: the ticker goroutine, the DB
// round-trip (read timestamps from playerState, write back after bump),
// and the notification fan-out to connected clients.
package server

import (
	"log"
	"time"

	"rpg-world-server/internal/resets"
	worldcore "rpg-world-server/internal/world"
)

// startResetTicker launches the 60s background goroutine that checks every
// online player for daily/weekly reset boundaries. Called once from
// initPlayerState (same boot step as the dirty-flush ticker).
func startResetTicker() {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for now := range t.C {
			tickResets(now)
		}
	}()
}

// tickResets walks all online player connections, runs MaybeReset on each,
// persists bumped timestamps and sends a chat notification for every fired
// kind. Lock discipline: playerStateFor takes pstateMu internally; we
// snapshot the timestamps under it, run MaybeReset (pure), then take it
// again to write back — same pattern as the rest of the pstate API.
func tickResets(now time.Time) {
	for _, c := range worldcore.AllOf[*playerConn]() {
		if c == nil || c.Username == "" {
			continue
		}
		st := playerStateFor(c.Username)
		pstateMu.Lock()
		ts := resets.Timestamps{LastDaily: st.LastDailyReset, LastWeekly: st.LastWeeklyReset}
		pstateMu.Unlock()

		ts, fired := resets.MaybeReset(ts, now)
		if len(fired) == 0 {
			continue
		}

		pstateMu.Lock()
		st.LastDailyReset = ts.LastDaily
		st.LastWeeklyReset = ts.LastWeekly
		pstateMu.Unlock()
		markDirty(c.Username)

		for _, k := range fired {
			label := "daily"
			if k == resets.Weekly {
				label = "weekly"
			}
			log.Printf("reset: %s %s reset fired for %s", c.Username, label, c.Instance)
			notifyPlayer(c, "reset:"+label)
		}
	}
}

// checkLoginResets runs MaybeReset once for a player at login time (so a
// player who logs in after a boundary crossing sees the reset immediately,
// without waiting for the next 60s tick). Returns the kinds that fired.
func checkLoginResets(username string, now time.Time) []resets.Kind {
	st := playerStateFor(username)
	pstateMu.Lock()
	ts := resets.Timestamps{LastDaily: st.LastDailyReset, LastWeekly: st.LastWeeklyReset}
	pstateMu.Unlock()

	ts, fired := resets.MaybeReset(ts, now)
	if len(fired) == 0 {
		return nil
	}

	pstateMu.Lock()
	st.LastDailyReset = ts.LastDaily
	st.LastWeeklyReset = ts.LastWeekly
	pstateMu.Unlock()
	markDirty(username)
	return fired
}
