package resets

import (
	"testing"
	"time"
)

func TestIsDueNeverReset(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) // Saturday
	if !IsDue(Daily, 0, now) {
		t.Fatal("never-reset daily should be due")
	}
	if !IsDue(Weekly, 0, now) {
		t.Fatal("never-reset weekly should be due")
	}
}

func TestIsDueDailyNotYetDue(t *testing.T) {
	// Last reset at 2026-09-27 01:00 UTC, now is 2026-09-27 12:00 UTC.
	// Next boundary after last reset is 2026-09-28 00:00 UTC. Not due.
	last := time.Date(2026, 9, 27, 1, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	if IsDue(Daily, last.UnixMilli(), now) {
		t.Fatal("daily should not be due within same day")
	}
}

func TestIsDueDailyCrossedMidnight(t *testing.T) {
	// Last reset at 2026-09-26 23:00 UTC, now is 2026-09-27 01:00 UTC.
	// Next boundary after 2026-09-26 23:00 is 2026-09-27 00:00. Due.
	last := time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 27, 1, 0, 0, 0, time.UTC)
	if !IsDue(Daily, last.UnixMilli(), now) {
		t.Fatal("daily should be due after midnight crossing")
	}
}

func TestIsDueWeeklyNotYetDue(t *testing.T) {
	// Last reset on Monday 2026-09-21 at 01:00 UTC, now is Wed 2026-09-23.
	// Next boundary is Monday 2026-09-28 00:00. Not due.
	last := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	if IsDue(Weekly, last.UnixMilli(), now) {
		t.Fatal("weekly should not be due within same week")
	}
}

func TestIsDueWeeklyCrossedMonday(t *testing.T) {
	// Last reset on Monday 2026-09-21 at 01:00 UTC, now is Mon 2026-09-28 01:00.
	// Next boundary after 2026-09-21 is Monday 2026-09-28 00:00. Due.
	last := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)
	if !IsDue(Weekly, last.UnixMilli(), now) {
		t.Fatal("weekly should be due after Monday midnight crossing")
	}
}

func TestMaybeResetFiresBoth(t *testing.T) {
	now := time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC) // Monday
	ts := Timestamps{} // never reset
	ts, fired := MaybeReset(ts, now)
	if len(fired) != 2 {
		t.Fatalf("expected 2 resets, got %d", len(fired))
	}
	if ts.LastDaily == 0 || ts.LastWeekly == 0 {
		t.Fatal("timestamps should be stamped")
	}
	// Second call should not fire again.
	ts, fired = MaybeReset(ts, now)
	if len(fired) != 0 {
		t.Fatalf("expected 0 resets on second call, got %d", len(fired))
	}
}

func TestMaybeResetDailyOnly(t *testing.T) {
	// Last weekly on Monday, last daily yesterday. Now is next day.
	last := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC) // Monday
	ts := Timestamps{LastDaily: last.UnixMilli(), LastWeekly: last.UnixMilli()}
	now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC) // Tuesday
	ts, fired := MaybeReset(ts, now)
	if len(fired) != 1 || fired[0] != Daily {
		t.Fatalf("expected daily only, got %v", fired)
	}
}
