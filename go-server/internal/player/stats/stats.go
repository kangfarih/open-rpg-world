// Package stats holds the PURE player-statistics domain: gather/kill/
// examine/drop counters plus the achievement-milestone derivation, ported
// from packages/server/src/game/entity/character/player/statistics.ts.
//
// TS contract mirrored here:
//   - milestones [10,50,100,500,1000,5000,10000]; reaching one for a
//     gather skill finishes the `<skill><N>` achievement (handleSkill).
//     Foraging is skipped (no foraging achievements exist).
//   - addMobKill is a counter only (handler.ts kill path): it feeds NO
//     achievement (verified: statistics.ts addMobKill has no finish call;
//     boss/miniboss achievements fire from handler.ts via mob.achievement,
//     a separate path Go does not model).
//   - addMobExamine dedupes by key and finishes examiner10/25/50 at
//     10/25/50 distinct examines.
//   - addDrop is a counter only.
//   - pvpKills/pvpDeaths are incremented on hero-vs-hero combat
//     (handler.ts handleKill/handleDeath when attacker/victim isPlayer).
//   - creationTime/totalTimePlayed/lastLogin/loginCount are tracked and
//     persisted. creationTime is set once on first login; totalTimePlayed
//     accumulates session durations on disconnect; lastLogin is updated
//     each login; loginCount increments each login. averageTimePlayed is
//     derived (totalTimePlayed / loginCount).
//
// Transport, achievement delivery and persistence stay with the callers:
// this package only computes state transitions and returns the achievement
// key to finish ("" = none). The server adapter (internal/server/stats.go)
// delivers finishes through the m11/quest path and persists snapshots
// through internal/persist (statistics table JSON blob).
package stats

import (
	"sync"
	"time"
)

// Milestones mirrors statistics.ts milestones.
var Milestones = []int{10, 50, 100, 500, 1000, 5000, 10000}

// ExaminerMilestones mirrors the addMobExamine switch (distinct-examine
// counts that finish examiner achievements).
var ExaminerMilestones = map[int]string{10: "examiner10", 25: "examiner25", 50: "examiner50"}

// State is one player's gameplay counters (statistics.ts parity). Zero
// value is usable; time fields use Unix seconds (time.Now().Unix()).
type State struct {
	MobKills    map[string]int
	MobExamines []string
	Resources   map[string]int
	Drops       map[string]int
	// PvP counters (handler.ts handleKill/handleDeath when isPlayer).
	PvPKills  int
	PvPDeaths int
	// Session/lifecycle tracking (statistics.ts construction + serialize).
	CreationTime   int64 // first login epoch seconds; set once.
	TotalTimePlayed int64 // accumulated session seconds across all logins.
	LastLogin      int64 // most recent login epoch seconds.
	LoginCount     int   // incremented each login.
	loginTime      time.Time // transient: when the current session started.
}

// Snapshot is a detached copy of State (persist handoff). Includes all
// persisted fields (everything except the transient loginTime).
type Snapshot struct {
	MobKills    map[string]int
	MobExamines []string
	Resources   map[string]int
	Drops       map[string]int
	PvPKills    int
	PvPDeaths   int
	CreationTime   int64
	TotalTimePlayed int64
	LastLogin      int64
	LoginCount     int
}

var (
	mu     sync.Mutex
	states = map[string]*State{}
)

// For returns the per-player counters (lazily created).
func For(username string) *State {
	mu.Lock()
	defer mu.Unlock()
	st, ok := states[username]
	if !ok {
		st = &State{}
		states[username] = st
	}
	return st
}

// Install replaces a player's counters from a persist snapshot (login load
// parity with statistics.load for the gameplay fields).
func Install(username string, snap Snapshot) {
	mu.Lock()
	defer mu.Unlock()
	states[username] = &State{
		MobKills:        cloneMap(snap.MobKills),
		MobExamines:     append([]string(nil), snap.MobExamines...),
		Resources:       cloneMap(snap.Resources),
		Drops:           cloneMap(snap.Drops),
		PvPKills:        snap.PvPKills,
		PvPDeaths:       snap.PvPDeaths,
		CreationTime:    snap.CreationTime,
		TotalTimePlayed: snap.TotalTimePlayed,
		LastLogin:       snap.LastLogin,
		LoginCount:      snap.LoginCount,
	}
}

// Forget drops in-memory counters (TESTMAP harness isolation).
func Forget(username string) {
	mu.Lock()
	delete(states, username)
	mu.Unlock()
}

// Update runs fn on the player's counters under the registry lock, so
// concurrent conn goroutines (and the AI tick) never race the maps. The
// achievement key to finish (if any) is reported through the closure; the
// caller delivers it AFTER Update returns (quest/persist locks must never
// nest inside the stats lock).
func Update(username string, fn func(*State)) {
	if username == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	st, ok := states[username]
	if !ok {
		st = &State{}
		states[username] = st
	}
	fn(st)
}

// CopyOf returns a detached snapshot of a player's counters.
func CopyOf(username string) Snapshot {
	mu.Lock()
	defer mu.Unlock()
	st := states[username]
	if st == nil {
		return Snapshot{}
	}
	return Snapshot{
		MobKills:        cloneMap(st.MobKills),
		MobExamines:     append([]string(nil), st.MobExamines...),
		Resources:       cloneMap(st.Resources),
		Drops:           cloneMap(st.Drops),
		PvPKills:        st.PvPKills,
		PvPDeaths:       st.PvPDeaths,
		CreationTime:    st.CreationTime,
		TotalTimePlayed: st.TotalTimePlayed,
		LastLogin:       st.LastLogin,
		LoginCount:      st.LoginCount,
	}
}

func cloneMap(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// HandleSkill ports statistics.handleSkill: bump the skill's resource count
// and report the milestone achievement key when one is reached ("<skill><N>",
// e.g. "lumberjacking10"). Foraging is skipped (no achievements). ok=false
// means no achievement fires. The counter advances even when the achievement
// key is unknown to the registry (caller nil-guards the finish, mirroring
// TS `?.finish()`).
func HandleSkill(st *State, skill string) (key string, ok bool) {
	if st == nil || skill == "foraging" {
		return "", false
	}
	if st.Resources == nil {
		st.Resources = map[string]int{}
	}
	st.Resources[skill]++
	if isMilestone(st.Resources[skill]) {
		return skill + itoa(st.Resources[skill]), true
	}
	return "", false
}

// AddMobKill ports statistics.addMobKill: counter only, never an
// achievement (TS parity — mobKills feeds no achievement).
func AddMobKill(st *State, key string) {
	if st == nil {
		return
	}
	if st.MobKills == nil {
		st.MobKills = map[string]int{}
	}
	st.MobKills[key]++
}

// AddMobExamine ports statistics.addMobExamine: dedupe by key, then report
// the examiner achievement at 10/25/50 distinct examines.
func AddMobExamine(st *State, key string) (ach string, ok bool) {
	if st == nil {
		return "", false
	}
	for _, k := range st.MobExamines {
		if k == key {
			return "", false
		}
	}
	st.MobExamines = append(st.MobExamines, key)
	if ach, ok := ExaminerMilestones[len(st.MobExamines)]; ok {
		return ach, true
	}
	return "", false
}

// AddDrop ports statistics.addDrop: counter only (count defaults to 1 at
// the call sites, mirroring the TS default parameter).
func AddDrop(st *State, key string, count int) {
	if st == nil {
		return
	}
	if st.Drops == nil {
		st.Drops = map[string]int{}
	}
	st.Drops[key] += count
}

// RecordLogin ports the statistics.ts login-time bookkeeping: set
// creationTime on first login, stamp lastLogin, bump loginCount, and record
// the session start for AccumulateSession. Caller invokes once per login.
func RecordLogin(username string) {
	if username == "" {
		return
	}
	now := time.Now()
	Update(username, func(st *State) {
		if st.CreationTime == 0 {
			st.CreationTime = now.Unix()
		}
		st.LastLogin = now.Unix()
		st.LoginCount++
		st.loginTime = now
	})
}

// AccumulateSession adds the elapsed session duration to TotalTimePlayed.
// Caller invokes once per disconnect (before the persist flush).
func AccumulateSession(username string) {
	if username == "" {
		return
	}
	now := time.Now()
	Update(username, func(st *State) {
		if !st.loginTime.IsZero() {
			st.TotalTimePlayed += int64(now.Sub(st.loginTime).Seconds())
			st.loginTime = time.Time{}
		}
	})
}

// AddPvPKill increments the PvP kill counter (handler.ts handleKill when
// victim isPlayer). No achievement fires.
func AddPvPKill(st *State) {
	if st == nil {
		return
	}
	st.PvPKills++
}

// AddPvPDeath increments the PvP death counter (handler.ts handleDeath when
// killer isPlayer). No achievement fires.
func AddPvPDeath(st *State) {
	if st == nil {
		return
	}
	st.PvPDeaths++
}

func isMilestone(n int) bool {
	for _, m := range Milestones {
		if n == m {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	// VERIFY (itoa dup): strconv.Itoa is identical for the milestone path
	// (all inputs non-negative), but this local port is kept behavior-frozen
	// to avoid touching the hot HandleSkill string path. No change.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
