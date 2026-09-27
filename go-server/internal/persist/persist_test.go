package persist

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(nil) })
	return s
}

func testState() State {
	equip := make([]Slot, 5)
	equip[0] = Slot{Key: "ironhelmet", Count: 1}
	equip[4] = Slot{Key: "ironsword", Count: 1}
	return State{
		X: 100, Y: 96, Level: 5, HP: 80,
		Inv: []Slot{
			{Key: "gold", Count: 10, Ench: "{}"},
			{Key: "ironsword", Count: 1, Ench: `{"0":{"level":2}}`},
		},
		Bank:  []Slot{{Key: "logs", Count: 50}},
		Equip: equip,
		Skills: map[int]Skill{
			0: {Level: 2, XP: 100},
			3: {Level: 5, XP: 1000},
		},
	}
}

func TestRoundTripIdentical(t *testing.T) {
	s := openTestStore(t)
	want := testState()
	if err := s.WritePlayer("hero", want); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	got, ok := s.LoadPlayer("hero")
	if !ok {
		t.Fatalf("LoadPlayer(hero) = false, want true")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadPlayer mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestLoadMissingIsFalse(t *testing.T) {
	s := openTestStore(t)
	if _, ok := s.LoadPlayer("nobody"); ok {
		t.Fatalf("LoadPlayer(nobody) = true, want false")
	}
	if _, ok := s.LoadPlayer(""); ok {
		t.Fatalf("LoadPlayer(\"\") = true, want false")
	}
}

func TestDirtyFlushClears(t *testing.T) {
	s := openTestStore(t)
	s.MarkDirty("")
	s.MarkDirty("a")
	s.MarkDirty("b")
	dirty := s.DirtyList()
	sort.Strings(dirty)
	if !reflect.DeepEqual(dirty, []string{"a", "b"}) {
		t.Fatalf("DirtyList = %v, want [a b]", dirty)
	}
	snap := map[string]State{"a": testState()}
	s.FlushDirty(func(key string) (State, bool) {
		st, ok := snap[key]
		return st, ok
	})
	if dirty := s.DirtyList(); len(dirty) != 0 {
		t.Fatalf("DirtyList after flush = %v, want empty", dirty)
	}
	// "a" had a snapshot: persisted. "b" had none: flag cleared, no row.
	got, ok := s.LoadPlayer("a")
	if !ok {
		t.Fatalf("LoadPlayer(a) = false after flush, want true")
	}
	if !reflect.DeepEqual(got, testState()) {
		t.Fatalf("LoadPlayer(a) mismatch after flush: %+v", got)
	}
	if _, ok := s.LoadPlayer("b"); ok {
		t.Fatalf("LoadPlayer(b) = true, want false (no snapshot)")
	}
	// MarkClean clears without writing.
	s.MarkDirty("c")
	s.MarkClean("c")
	if dirty := s.DirtyList(); len(dirty) != 0 {
		t.Fatalf("DirtyList after MarkClean = %v, want empty", dirty)
	}
}

func TestSnapshotDeepCopy(t *testing.T) {
	s := openTestStore(t)
	orig := testState()
	orig.Stats = StatsBlob{
		MobKills:    map[string]int{"rat": 3},
		MobExamines: []string{"rat"},
		Resources:   map[string]int{"wood": 5},
		Drops:       map[string]int{"gold": 7},
	}
	cp := s.Snapshot(orig)
	cp.Inv[0].Count = 999
	cp.Skills[0] = Skill{Level: 9, XP: 9}
	cp.Equip[0].Key = "changed"
	cp.Stats.MobKills["rat"] = 999
	cp.Stats.MobExamines[0] = "changed"
	cp.Stats.Resources["wood"] = 999
	cp.Stats.Drops["gold"] = 999
	if orig.Inv[0].Count == 999 || orig.Skills[0].Level == 9 || orig.Equip[0].Key == "changed" {
		t.Fatalf("Snapshot shares memory with the original: %+v", orig)
	}
	if orig.Stats.MobKills["rat"] == 999 || orig.Stats.MobExamines[0] == "changed" ||
		orig.Stats.Resources["wood"] == 999 || orig.Stats.Drops["gold"] == 999 {
		t.Fatalf("Snapshot shares Stats memory with the original: %+v", orig.Stats)
	}
	if !reflect.DeepEqual(cp.X, orig.X) {
		t.Fatalf("Snapshot lost scalar fields")
	}
}

func TestWritePlayerAtomicWithStats(t *testing.T) {
	s := openTestStore(t)
	want := testState()
	want.Stats = StatsBlob{
		MobKills:    map[string]int{"rat": 3},
		MobExamines: []string{"rat"},
		Resources:   map[string]int{"wood": 5},
		Drops:       map[string]int{"gold": 7},
	}
	if err := s.WritePlayer("atomic", want); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	got, ok := s.LoadPlayer("atomic")
	if !ok {
		t.Fatal("LoadPlayer(atomic) = false, want true (single-transaction write)")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("atomic write mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestSubsystemTablesFoldedIn(t *testing.T) {
	s := openTestStore(t)
	for _, table := range []string{"abilities", "guilds", "guild_members", "friends", "quests", "achievements"} {
		var name string
		if err := s.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name); err != nil || name != table {
			t.Fatalf("subsystem table %q missing (v5 fold-in): %v", table, err)
		}
	}
}

func TestCloseReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.WritePlayer("hero", testState()); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	s.MarkDirty("hero")
	// Close with no new snapshot: hero was already written, flag clears.
	if err := s.Close(nil); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close(nil)
	got, ok := s2.LoadPlayer("hero")
	if !ok {
		t.Fatalf("LoadPlayer after reopen = false, want true")
	}
	if !reflect.DeepEqual(got, testState()) {
		t.Fatalf("state changed across reopen:\n got=%+v\nwant=%+v", got, testState())
	}
}
