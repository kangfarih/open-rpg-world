package stats

import (
	"fmt"
	"testing"
)

// Milestone matrix: every gather skill (except foraging) fires exactly
// `<skill><N>` at each of the 7 TS milestones and nothing in between.
func TestHandleSkillMilestoneMatrix(t *testing.T) {
	for _, skill := range []string{"lumberjacking", "mining", "fishing"} {
		st := &State{}
		for n := 1; n <= 10_000; n++ {
			key, ok := HandleSkill(st, skill)
			wantMilestone := false
			for _, m := range Milestones {
				if n == m {
					wantMilestone = true
				}
			}
			if wantMilestone {
				if !ok || key != fmt.Sprintf("%s%d", skill, n) {
					t.Fatalf("%s swing %d: got (%q,%v), want (%q,true)",
						skill, n, key, ok, fmt.Sprintf("%s%d", skill, n))
				}
			} else if ok {
				t.Fatalf("%s swing %d: unexpected achievement %q", skill, n, key)
			}
		}
		if got := st.Resources[skill]; got != 10_000 {
			t.Fatalf("%s counter = %d, want 10000", skill, got)
		}
	}
}

// Foraging is skipped entirely (TS early return: no counter, no achievement).
func TestHandleSkillForagingSkipped(t *testing.T) {
	st := &State{}
	for i := 0; i < 20; i++ {
		if key, ok := HandleSkill(st, "foraging"); ok || key != "" {
			t.Fatalf("foraging swing %d fired %q", i, key)
		}
	}
	if len(st.Resources) != 0 {
		t.Fatalf("foraging must not advance counters, got %v", st.Resources)
	}
}

// Examiner path: dedupe, 10/25/50 milestones, no re-fire past 50.
func TestAddMobExamineMilestones(t *testing.T) {
	st := &State{}
	fired := map[string]int{}
	for i := 0; i < 60; i++ {
		if ach, ok := AddMobExamine(st, fmt.Sprintf("mob%d", i)); ok {
			fired[ach]++
		}
	}
	for _, want := range []string{"examiner10", "examiner25", "examiner50"} {
		if fired[want] != 1 {
			t.Fatalf("%s fired %d times, want 1 (fired=%v)", want, fired[want], fired)
		}
	}
	if len(fired) != 3 {
		t.Fatalf("unexpected examiner achievements: %v", fired)
	}
	// Re-examining known keys never advances or fires.
	before := len(st.MobExamines)
	for i := 0; i < 60; i++ {
		if ach, ok := AddMobExamine(st, fmt.Sprintf("mob%d", i)); ok || ach != "" {
			t.Fatalf("duplicate examine fired %q", ach)
		}
	}
	if len(st.MobExamines) != before {
		t.Fatal("duplicate examines must not grow the list")
	}
}

// Mob kills are counters only (TS addMobKill feeds no achievement).
func TestAddMobKillCounterOnly(t *testing.T) {
	st := &State{}
	for i := 0; i < 15; i++ {
		AddMobKill(st, "rat")
	}
	if st.MobKills["rat"] != 15 {
		t.Fatalf("mobKills[rat] = %d, want 15", st.MobKills["rat"])
	}
}

// Drops accumulate per key.
func TestAddDropAccumulates(t *testing.T) {
	st := &State{}
	AddDrop(st, "logs", 1)
	AddDrop(st, "logs", 3)
	AddDrop(st, "gold", 5)
	if st.Drops["logs"] != 4 || st.Drops["gold"] != 5 {
		t.Fatalf("drops = %v, want logs:4 gold:5", st.Drops)
	}
}

// Nil-state calls are safe (adapter may race a fresh username).
func TestNilStateSafe(t *testing.T) {
	if k, ok := HandleSkill(nil, "mining"); ok || k != "" {
		t.Fatal("nil HandleSkill must not fire")
	}
	AddMobKill(nil, "rat")
	if a, ok := AddMobExamine(nil, "rat"); ok || a != "" {
		t.Fatal("nil AddMobExamine must not fire")
	}
	AddDrop(nil, "gold", 1)
}

// PvP counters increment independently and are nil-safe.
func TestPvPCounters(t *testing.T) {
	st := &State{}
	AddPvPKill(st)
	AddPvPKill(st)
	AddPvPDeath(st)
	if st.PvPKills != 2 {
		t.Fatalf("PvPKills = %d, want 2", st.PvPKills)
	}
	if st.PvPDeaths != 1 {
		t.Fatalf("PvPDeaths = %d, want 1", st.PvPDeaths)
	}
	AddPvPKill(nil)
	AddPvPDeath(nil)
}

// RecordLogin sets creationTime on first login, bumps loginCount and
// lastLogin on subsequent logins.
func TestRecordLoginLifecycle(t *testing.T) {
	user := "__test_login_lifecycle__"
	defer Forget(user)

	RecordLogin(user)
	st := For(user)
	if st.CreationTime == 0 {
		t.Fatal("CreationTime not set on first login")
	}
	if st.LoginCount != 1 {
		t.Fatalf("LoginCount = %d, want 1", st.LoginCount)
	}
	firstCreation := st.CreationTime
	firstLast := st.LastLogin

	// Second login: creationTime unchanged, lastLogin advances, count bumps.
	RecordLogin(user)
	st = For(user)
	if st.CreationTime != firstCreation {
		t.Fatal("CreationTime changed on second login")
	}
	if st.LoginCount != 2 {
		t.Fatalf("LoginCount = %d, want 2", st.LoginCount)
	}
	if st.LastLogin < firstLast {
		t.Fatal("LastLogin did not advance")
	}
}

// AccumulateSession adds elapsed time to TotalTimePlayed.
func TestAccumulateSession(t *testing.T) {
	user := "__test_accumulate_session__"
	defer Forget(user)

	RecordLogin(user)
	// Simulate a session by backdating loginTime.
	Update(user, func(st *State) {
		st.loginTime = st.loginTime.Add(-10 * 60e9) // 10 minutes back (nanoseconds)
	})
	AccumulateSession(user)
	st := For(user)
	if st.TotalTimePlayed < 500 {
		t.Fatalf("TotalTimePlayed = %d, want >= 500 (10min backdate)", st.TotalTimePlayed)
	}
	// Second AccumulateSession without a new RecordLogin is a no-op.
	prev := st.TotalTimePlayed
	AccumulateSession(user)
	st = For(user)
	if st.TotalTimePlayed != prev {
		t.Fatalf("AccumulateSession without login should be no-op, got %d -> %d", prev, st.TotalTimePlayed)
	}
}

// Install/CopyOf round-trip the new fields.
func TestInstallCopyOfNewFields(t *testing.T) {
	user := "__test_install_copy__"
	defer Forget(user)

	snap := Snapshot{
		PvPKills: 3, PvPDeaths: 1,
		CreationTime: 1000, TotalTimePlayed: 500,
		LastLogin: 2000, LoginCount: 5,
	}
	Install(user, snap)
	got := CopyOf(user)
	if got.PvPKills != 3 || got.PvPDeaths != 1 {
		t.Fatalf("PvP mismatch: %+v", got)
	}
	if got.CreationTime != 1000 || got.TotalTimePlayed != 500 {
		t.Fatalf("Time mismatch: %+v", got)
	}
	if got.LastLogin != 2000 || got.LoginCount != 5 {
		t.Fatalf("Lifecycle mismatch: %+v", got)
	}
}
