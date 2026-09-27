// Package resets tracks per-player daily and weekly reset timestamps.
//
// DESIGN (why this shape):
//
// The TS server had no reset system (only a weekend-event hourly check).
// Go adds a lightweight reset tracker: each player carries two epoch-ms
// timestamps (last daily, last weekly). A background ticker (60s) and the
// login path call MaybeReset, which compares against the next boundary
// (midnight UTC for daily, Monday 00:00 UTC for weekly) and bumps the
// timestamp when crossed. Other systems call IsDue to check whether a
// player's counters need refreshing, without this package owning the
// counter state itself.
//
// The package is pure: no goroutines, no DB, no globals. The server
// package owns the ticker, the DB table, and the callback fan-out.
package resets

import "time"

// Kind identifies a reset cadence.
type Kind int

const (
	Daily  Kind = iota // resets at 00:00 UTC each day
	Weekly             // resets at 00:00 UTC each Monday
)

// Timestamps holds one player's last-reset epoch-ms pair. Zero means
// "never reset" (first call to MaybeReset will stamp and return true).
type Timestamps struct {
	LastDaily  int64 // epoch ms
	LastWeekly int64 // epoch ms
}

// nextBoundary returns the next reset boundary (midnight UTC) for the
// given kind, relative to now. Daily = next 00:00 UTC; Weekly = next
// Monday 00:00 UTC.
func nextBoundary(k Kind, now time.Time) time.Time {
	utc := now.UTC()
	midnight := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	switch k {
	case Weekly:
		// Advance to next Monday.
		daysUntilMonday := (7 - int(midnight.Weekday())) % 7
		if daysUntilMonday == 0 {
			daysUntilMonday = 7 // today is Monday -> next Monday
		}
		return midnight.AddDate(0, 0, daysUntilMonday)
	default: // Daily
		return midnight.AddDate(0, 0, 1)
	}
}

// IsDue reports whether the reset of kind k is due for the player.
// lastMs is the epoch-ms timestamp of the last reset (0 = never).
// A reset is due when the last reset happened before the most recent
// boundary crossing (i.e. now >= nextBoundary(last)).
func IsDue(k Kind, lastMs int64, now time.Time) bool {
	if lastMs <= 0 {
		return true // never reset -> due now
	}
	last := time.UnixMilli(lastMs)
	boundary := nextBoundary(k, last)
	return !now.Before(boundary)
}

// MaybeReset checks each reset kind and bumps the timestamp when due.
// Returns the (possibly updated) Timestamps and a slice of kinds that
// were actually reset (so the caller can fire callbacks).
func MaybeReset(ts Timestamps, now time.Time) (Timestamps, []Kind) {
	nowMs := now.UnixMilli()
	var fired []Kind
	if IsDue(Daily, ts.LastDaily, now) {
		ts.LastDaily = nowMs
		fired = append(fired, Daily)
	}
	if IsDue(Weekly, ts.LastWeekly, now) {
		ts.LastWeekly = nowMs
		fired = append(fired, Weekly)
	}
	return ts, fired
}
