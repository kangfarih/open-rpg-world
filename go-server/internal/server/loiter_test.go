package server

import (
	"testing"
	"time"

	"rpg-world-server/internal/player"
	"rpg-world-server/internal/player/quest"
	"rpg-world-server/internal/protocol"
)

// loiterForget drops XP + quest state for test users (each test owns its
// usernames; registry conns self-clean via deathConn).
func loiterForget(t *testing.T, users ...string) {
	t.Helper()
	t.Cleanup(func() {
		pstateMu.Lock()
		for _, u := range users {
			delete(pstates, u)
		}
		pstateMu.Unlock()
		for _, u := range users {
			forgetQuestSession(u)
		}
	})
}

// loiterXP reads the hero's Loitering XP (0 when the skill row is absent).
func loiterXP(user string) int {
	st := playerStateFor(user)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if s := st.Skills[player.SkillLoitering]; s != nil {
		return s.XP
	}
	return 0
}

// loiterEnsureTutorialDef installs a tutorial quest def when the data dir
// is unavailable (unit-test CWDs never resolve it — quest.Load finds no
// files there, so the gate would default true and the negative leg could
// not run). Production loads the real tutorial.json at boot; the sweep
// only reads def presence + stage, both covered here.
func loiterEnsureTutorialDef(t *testing.T) {
	t.Helper()
	if _, ok := questDefs["tutorial"]; ok {
		return
	}
	questDefs["tutorial"] = &quest.Quest{Key: "tutorial", StageCount: 3}
	t.Cleanup(func() { delete(questDefs, "tutorial") })
}

// loiterFinishTutorial marks the tutorial quest finished for user (the same
// m11 quest-stage lookup the sweep and the warp gate read).
func loiterFinishTutorial(t *testing.T, user string) {
	t.Helper()
	loiterEnsureTutorialDef(t)
	questStateFor(user).Quest("tutorial").Stage = questDefs["tutorial"].StageCount
	if !loiterTutorialFinished(user) {
		t.Fatal("tutorial not finished after setting final stage")
	}
}

// loiterSetSince forces the conn's region stamp (simulates 90s+ idle
// without waiting, mirroring the LOITER_MS precedent of shortening time,
// not thresholds).
func loiterSetSince(c *playerConn, region int, sinceMs int64) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.loiterInit, c.loiterRegion, c.loiterSince = true, region, sinceMs
}

// TestLoiterCadencePinsTSExact pins the TS-exact constants: the 90s
// same-region threshold (Modules.Constants.LOITERING_THRESHOLD) and the
// ~19.2s sweep cadence (updateTime 600ms x isTickInterval(32)), plus the
// LOITER_MS test hook (WORLD_EVENT_MS precedent).
func TestLoiterCadencePinsTSExact(t *testing.T) {
	if LoiterThresholdMs != 90_000 {
		t.Fatalf("LoiterThresholdMs = %d, want 90000 (TS LOITERING_THRESHOLD)", LoiterThresholdMs)
	}
	if got := LoiterIntervalMs(); got != 19_200 {
		t.Fatalf("LoiterIntervalMs default = %d, want 19200 (600ms x 32)", got)
	}
	t.Setenv("LOITER_MS", "150")
	if got := LoiterIntervalMs(); got != 150 {
		t.Fatalf("LOITER_MS=150 -> %d, want 150", got)
	}
	t.Setenv("LOITER_MS", "bogus")
	if got := LoiterIntervalMs(); got != 19_200 {
		t.Fatalf("LOITER_MS=bogus -> %d, want default 19200", got)
	}
	t.Setenv("LOITER_MS", "0")
	if got := LoiterIntervalMs(); got != 19_200 {
		t.Fatalf("LOITER_MS=0 -> %d, want default 19200", got)
	}
}

// TestLoiterEligibleMatrix is the player.ts loiter() gate matrix:
// tutorial finished AND now-lastRegionChange >= 90000 (boundary-exact).
func TestLoiterEligibleMatrix(t *testing.T) {
	now := int64(1_700_000_000_000) // realistic epoch-ms (TS Date.now() scale)
	cases := []struct {
		name  string
		tut   bool
		since int64
		now   int64
		want  bool
	}{
		{"threshold boundary passes", true, now - 90_000, now, true},
		{"1ms under threshold fails", true, now - 89_999, now, false},
		{"over threshold passes", true, now - 90_001, now, true},
		{"long idle passes", true, now - 600_000, now, true},
		{"tutorial unfinished gates off", false, now - 600_000, now, false},
		{"tutorial unfinished at boundary", false, now - 90_000, now, false},
		{"never stamped (-1 init) passes at epoch scale", true, -1, now, true},
	}
	for _, tc := range cases {
		if got := loiterEligible(tc.tut, tc.since, tc.now); got != tc.want {
			t.Errorf("%s: loiterEligible(%v, %d, %d) = %v, want %v",
				tc.name, tc.tut, tc.since, tc.now, got, tc.want)
		}
	}
}

// TestLoiterRegionTracking covers markLoiterRegion: first sighting stamps
// (entity.region -1 parity), intra-region moves never restamp (TS
// regions.handle gate), region changes restamp.
func TestLoiterRegionTracking(t *testing.T) {
	c, _ := deathConn(t, "loiter-track-inst", "loiter-track-user")
	loiterForget(t, "loiter-track-user")

	markLoiterRegion(nil, 50) // nil-safe (hook may see torn-down conns)

	markLoiterRegion(c, 50)
	c.sessMu.RLock()
	init, region, first := c.loiterInit, c.loiterRegion, c.loiterSince
	c.sessMu.RUnlock()
	if !init || region != 50 || first <= 0 {
		t.Fatalf("first sighting = init=%v region=%d since=%d, want true/50/>0", init, region, first)
	}

	// Same region must NOT restamp: backdate, re-mark, stamp sticks.
	loiterSetSince(c, 50, first-60_000)
	markLoiterRegion(c, 50)
	c.sessMu.RLock()
	since := c.loiterSince
	c.sessMu.RUnlock()
	if since != first-60_000 {
		t.Fatalf("same-region re-mark moved stamp to %d, want %d", since, first-60_000)
	}

	// Region change restamps to ~now.
	before := time.Now().UnixMilli()
	markLoiterRegion(c, 51)
	c.sessMu.RLock()
	region, since = c.loiterRegion, c.loiterSince
	c.sessMu.RUnlock()
	if region != 51 || since < before {
		t.Fatalf("region change = region=%d since=%d, want 51/>=%d", region, since, before)
	}
}

// loiterSkillFrames scans drained frames for the Skill Update pair the
// award must emit on the corrected Loitering id (17).
func loiterSkillFrames(t *testing.T, c *playerConn) (expSkill, skillUpdate bool) {
	t.Helper()
	for _, f := range drainOutbox(c) {
		if len(f) != 3 {
			continue
		}
		id, _ := f[0].(int)
		op, _ := f[1].(int)
		switch {
		case id == PacketExperience && op == ExperienceSkill:
			if d, ok := f[2].(protocol.ExperienceData); ok && d.Skill != nil && *d.Skill == 17 {
				expSkill = true
			}
		case id == PacketSkill && op == SkillUpdate:
			if d, ok := f[2].(protocol.SkillData); ok && d.Type == 17 {
				skillUpdate = true
			}
		}
	}
	return expSkill, skillUpdate
}

// TestLoiterAwardMathAndFrames: idle 90s+ post-tutorial -> the sweep awards
// level*5 Loitering XP (player.ts:559) through addXP, with the Experience
// Skill + Skill Update frames on skill 17 (the corrected const, not
// alchemy's 18). Covers level 1 and a pre-seeded higher level.
func TestLoiterAwardMathAndFrames(t *testing.T) {
	user := "loiter-award-user"
	c, _ := deathConn(t, "loiter-award-inst", user)
	loiterForget(t, user)
	loiterFinishTutorial(t, user)
	loiterSetSince(c, 50, time.Now().UnixMilli()-120_000)
	drainOutbox(c)

	before := loiterXP(user)
	level := loiterLevel(user)
	if level != 1 {
		t.Fatalf("fresh loitering level = %d, want 1", level)
	}
	runLoiterSweep(time.Now().UnixMilli())
	if got, want := loiterXP(user)-before, level*5; got != want {
		t.Fatalf("level-1 award delta = %d, want level*5 = %d", got, want)
	}
	if exp, upd := loiterSkillFrames(t, c); !exp || !upd {
		t.Fatalf("missing award frames on skill 17: experience=%v skillUpdate=%v", exp, upd)
	}

	// Pre-seed a higher level: the next award is the NEW level * 5.
	addXP(nil, user, player.SkillLoitering, 1000)
	drainOutbox(c)
	level2 := loiterLevel(user)
	if level2 <= 1 {
		t.Fatalf("seeded loitering level = %d, want > 1", level2)
	}
	before = loiterXP(user)
	runLoiterSweep(time.Now().UnixMilli())
	if got, want := loiterXP(user)-before, level2*5; got != want {
		t.Fatalf("level-%d award delta = %d, want level*5 = %d", level2, got, want)
	}
	if exp, upd := loiterSkillFrames(t, c); !exp || !upd {
		t.Fatalf("missing level-%d award frames on skill 17: experience=%v skillUpdate=%v",
			level2, exp, upd)
	}
}

// TestLoiterTutorialGate: pre-tutorial heroes earn nothing from the sweep
// (player.ts:553 early return) while cheatScore forgiveness still applies
// (handler.ts:130-134 resets unconditionally on the same tick); finishing
// the tutorial opens the award on the next sweep.
func TestLoiterTutorialGate(t *testing.T) {
	user := "loiter-gate-user"
	c, _ := deathConn(t, "loiter-gate-inst", user)
	loiterForget(t, user)
	loiterEnsureTutorialDef(t)
	if loiterTutorialFinished(user) {
		t.Fatal("fresh user already tutorial-finished")
	}
	loiterSetSince(c, 50, time.Now().UnixMilli()-600_000)
	c.Sess.CheatScore = 7

	runLoiterSweep(time.Now().UnixMilli())
	if got := loiterXP(user); got != 0 {
		t.Fatalf("pre-tutorial loiter XP = %d, want 0", got)
	}
	if c.Sess.CheatScore != 0 {
		t.Fatalf("pre-tutorial cheatScore = %d, want 0 (forgiveness is unconditional)", c.Sess.CheatScore)
	}

	loiterFinishTutorial(t, user)
	c.Sess.CheatScore = 9
	before := loiterXP(user)
	runLoiterSweep(time.Now().UnixMilli())
	if got := loiterXP(user) - before; got != 1*5 {
		t.Fatalf("post-tutorial award delta = %d, want 5", got)
	}
	if c.Sess.CheatScore != 0 {
		t.Fatalf("post-tutorial cheatScore = %d, want 0", c.Sess.CheatScore)
	}
}

// TestCheatScoreResetOnLoiterTick: the sweep zeroes cheatScore on the shared
// ~19.2s cadence (handler.ts:130-134 TS forgiveness semantics — transient
// strikes decay instead of accumulating to the >15 disconnect), without
// changing the increment/disconnect path.
func TestCheatScoreResetOnLoiterTick(t *testing.T) {
	user := "loiter-cheat-user"
	c, _ := deathConn(t, "loiter-cheat-inst", user)
	loiterForget(t, user)
	loiterSetSince(c, 50, time.Now().UnixMilli()) // ineligible: no XP side effects

	c.Sess.CheatScore = 14
	runLoiterSweep(time.Now().UnixMilli())
	if c.Sess.CheatScore != 0 {
		t.Fatalf("cheatScore after sweep = %d, want 0", c.Sess.CheatScore)
	}

	// Forgiveness decays: the next strike starts from 1, not 15.
	if drop := rejectLocked(c, "test-strike"); drop {
		t.Fatal("first post-forgiveness strike dropped the conn (score must restart at 1)")
	}
	if c.Sess.CheatScore != 1 {
		t.Fatalf("cheatScore after one strike = %d, want 1", c.Sess.CheatScore)
	}

	// The >15 disconnect gate itself is unchanged: 15 more strikes drop.
	dropped := false
	for i := 0; i < 15; i++ {
		dropped = rejectLocked(c, "test-strike")
	}
	if !dropped {
		t.Fatal("16 strikes did not drop the conn (gate must stay >15)")
	}
}

// TestLoiterTickThrottle: loiterTick rides the central engine with a time
// throttle (no per-player goroutines) — a fresh sweep runs, an immediate
// second tick is skipped, and LOITER_MS shortens the cadence for tests.
func TestLoiterTickThrottle(t *testing.T) {
	old := loiterLastSweep
	t.Cleanup(func() { loiterLastSweep = old })

	user := "loiter-throttle-user"
	c, _ := deathConn(t, "loiter-throttle-inst", user)
	loiterForget(t, user)
	loiterFinishTutorial(t, user)
	loiterSetSince(c, 50, time.Now().UnixMilli()-600_000)

	loiterLastSweep = time.Time{} // force due
	before := loiterXP(user)
	loiterTick()
	if got := loiterXP(user) - before; got == 0 {
		t.Fatal("due loiterTick awarded nothing")
	}
	before = loiterXP(user)
	loiterTick() // throttled: no second award 50ms later
	if got := loiterXP(user) - before; got != 0 {
		t.Fatalf("throttled loiterTick awarded %d xp, want 0", got)
	}

	t.Setenv("LOITER_MS", "1")
	time.Sleep(2 * time.Millisecond)
	before = loiterXP(user)
	loiterTick() // shortened cadence: due again
	if got := loiterXP(user) - before; got == 0 {
		t.Fatal("LOITER_MS=1 loiterTick awarded nothing")
	}
}
