package persist

import (
	"reflect"
	"testing"
)

// Statistics blob round-trips with the player row (additive table, no
// existing column touched).
func TestStatsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	want := testState()
	want.Stats = StatsBlob{
		MobKills:    map[string]int{"rat": 3, "skeleton": 1},
		MobExamines: []string{"rat", "logs"},
		Resources:   map[string]int{"lumberjacking": 10, "mining": 51},
		Drops:       map[string]int{"gold": 7},
	}
	if err := s.WritePlayer("statty", want); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	got, ok := s.LoadPlayer("statty")
	if !ok {
		t.Fatal("LoadPlayer(statty) = false, want true")
	}
	if !reflect.DeepEqual(got.Stats, want.Stats) {
		t.Fatalf("stats mismatch:\n got=%+v\nwant=%+v", got.Stats, want.Stats)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full row mismatch with stats:\n got=%+v\nwant=%+v", got, want)
	}
}

// Pre-v2 rows (no statistics entry) load as zero counters — the table is
// additive, so old DBs converge without migration.
func TestStatsMissingRowIsZero(t *testing.T) {
	s := openTestStore(t)
	if err := s.WritePlayer("fresh", testState()); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM statistics WHERE player=?`, "fresh"); err != nil {
		t.Fatalf("delete stats row: %v", err)
	}
	got, ok := s.LoadPlayer("fresh")
	if !ok {
		t.Fatal("LoadPlayer(fresh) = false, want true")
	}
	if len(got.Stats.MobKills) != 0 || len(got.Stats.MobExamines) != 0 ||
		len(got.Stats.Resources) != 0 || len(got.Stats.Drops) != 0 {
		t.Fatalf("missing stats row must load zero, got %+v", got.Stats)
	}
	if _, ok := s.LoadStats("nobody"); ok {
		t.Fatal("LoadStats(nobody) = true, want false")
	}
}

// The statistics table exists on fresh and migrated DBs.
func TestStatisticsTableExists(t *testing.T) {
	s := openTestStore(t)
	var name string
	if err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='statistics'`,
	).Scan(&name); err != nil || name != "statistics" {
		t.Fatalf("statistics table missing: %v (%q)", err, name)
	}
}

// PvP counters and lifecycle fields round-trip through the JSON blob.
func TestStatsRoundTripNewFields(t *testing.T) {
	s := openTestStore(t)
	want := testState()
	want.Stats = StatsBlob{
		MobKills:        map[string]int{"rat": 3},
		MobExamines:     []string{"rat"},
		Resources:       map[string]int{"wood": 5},
		Drops:           map[string]int{"gold": 7},
		PvPKills:        4,
		PvPDeaths:       2,
		CreationTime:    1700000000,
		TotalTimePlayed: 3600,
		LastLogin:       1700003600,
		LoginCount:      10,
	}
	if err := s.WritePlayer("lifecycle", want); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	got, ok := s.LoadPlayer("lifecycle")
	if !ok {
		t.Fatal("LoadPlayer(lifecycle) = false, want true")
	}
	if !reflect.DeepEqual(got.Stats, want.Stats) {
		t.Fatalf("stats mismatch:\n got=%+v\nwant=%+v", got.Stats, want.Stats)
	}
}

// Pre-v2 rows (no statistics entry) load zero for the new fields too.
func TestStatsMissingRowZeroNewFields(t *testing.T) {
	s := openTestStore(t)
	if err := s.WritePlayer("fresh2", testState()); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM statistics WHERE player=?`, "fresh2"); err != nil {
		t.Fatalf("delete stats row: %v", err)
	}
	got, ok := s.LoadPlayer("fresh2")
	if !ok {
		t.Fatal("LoadPlayer(fresh2) = false, want true")
	}
	if got.Stats.PvPKills != 0 || got.Stats.PvPDeaths != 0 ||
		got.Stats.CreationTime != 0 || got.Stats.TotalTimePlayed != 0 ||
		got.Stats.LastLogin != 0 || got.Stats.LoginCount != 0 {
		t.Fatalf("missing stats row must load zero for new fields, got %+v", got.Stats)
	}
}
