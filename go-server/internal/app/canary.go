// R3 canary routing + warm-hold (GO-PLAN §12 R3), behavior-additive.
//
// Percentage routing: CANARY_PCT (default 0 = today's behavior: 100% of NEW
// logins route to the newest healthy RUNNING version). When >0, CANARY_PCT%
// of NEW logins route to the newest version and the rest route to the
// previous healthy RUNNING version. The decision is sticky per session: it
// is taken once at login from a stable hash of the login key
// (instance/username), so a session never flaps between builds on retries.
//
// Wiring: GET /servers keeps today's shape and default (Preferred = newest
// healthy RUNNING, "" when none). Callers pass ?login=<instance-or-user>
// for a personalized canary decision, and the list additionally reports the
// previous-version addr (Previous) and the active CANARY_PCT — both
// omitempty, so the default single-version boot renders byte-identical.
//
// Warm-hold: after a newer version registers, the router keeps the old
// version entry WARM (routable for existing sessions + the canary
// remainder) for WARM_HOLD (default 15m). WarmTracker records when each
// world version was first observed so the runbook can gate "do not TERM the
// old build until its hold expires". Rollback = router swap-back: SIGTERM
// the new-version shard(s) (or stop their units); once they go DRAINING or
// evict, NewestRunning falls back to the previous healthy version with no
// router restart and no data migration.
package app

import (
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"rpg-world-server/internal/hub"
	"rpg-world-server/internal/version"
)

// EnvCanaryPct selects the canary percentage (0-100, default 0).
const EnvCanaryPct = "CANARY_PCT"

// EnvWarmHold bounds the post-swap warm-hold (Go duration, e.g. "15m"; a
// bare number means seconds). Default DefaultWarmHold.
const EnvWarmHold = "WARM_HOLD"

// DefaultWarmHold keeps the old version entry WARM after a newer version
// registers when WARM_HOLD is unset/invalid.
const DefaultWarmHold = 15 * time.Minute

// CanaryPct returns the canary percentage (0-100; default 0 = all newest).
func CanaryPct() int { return CanaryPctFromEnv(os.Getenv) }

// CanaryPctFromEnv is CanaryPct over an injected env lookup (tests).
// Out-of-range or unparsable values clamp to 0 (today's behavior).
func CanaryPctFromEnv(getenv func(string) string) int {
	raw := ""
	if getenv != nil {
		raw = strings.TrimSpace(getenv(EnvCanaryPct))
	}
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// WarmHold returns the WARM_HOLD bound (default 15m; invalid falls back to
// the default; bare numbers mean seconds).
func WarmHold() time.Duration { return WarmHoldFromEnv(os.Getenv) }

// WarmHoldFromEnv is WarmHold over an injected env lookup (tests).
func WarmHoldFromEnv(getenv func(string) string) time.Duration {
	raw := ""
	if getenv != nil {
		raw = strings.TrimSpace(getenv(EnvWarmHold))
	}
	if raw == "" {
		return DefaultWarmHold
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return DefaultWarmHold
}

// CanaryBucket hashes a login key (instance/username) to a stable 0-99
// bucket. Same key always yields the same bucket (sticky per session);
// keys spread uniformly across buckets.
func CanaryBucket(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % 100)
}

// CanaryToNewest reports whether a login key routes to the newest version
// under pct (bucket < pct). pct<=0 routes nowhere-newest here; the caller
// (RouteLogin) treats pct<=0 as "all newest" to preserve today's default.
func CanaryToNewest(key string, pct int) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	return CanaryBucket(key) < pct
}

// PreviousRunning reports the previous healthy RUNNING world version: the
// second-newest version with at least one RUNNING shard. Within that
// version the lowest-load shard wins (ties: latest firstSeen, then name
// ascending), mirroring the newestRunningLocked tie-break. ok is false
// when fewer than two healthy versions are registered.
func PreviousRunning(h *hub.Server) (hub.ShardInfo, bool) {
	if h == nil {
		return hub.ShardInfo{}, false
	}
	prefVer, ok := h.PreferredVersion()
	if !ok {
		return hub.ShardInfo{}, false
	}
	var best *hub.ShardInfo
	for _, in := range h.ListShards() {
		if in.State != version.StateRunning || in.Version == prefVer {
			continue
		}
		// First distinct non-preferred version in version-grouped
		// newest-first order is the previous version; only compare
		// shards within it.
		if best != nil && in.Version != best.Version {
			continue
		}
		if best == nil {
			cp := in
			best = &cp
			continue
		}
		if in.Load < best.Load ||
			(in.Load == best.Load && (in.FirstSeen.After(best.FirstSeen) ||
				(in.FirstSeen.Equal(best.FirstSeen) && in.Name < best.Name))) {
			cp := in
			best = &cp
		}
	}
	if best == nil {
		return hub.ShardInfo{}, false
	}
	return *best, true
}

// RouteLogin picks the login target for one NEW session key
// (instance/username): pct<=0 (default) returns the newest healthy RUNNING
// version (today's behavior); pct>0 returns the newest version for
// pct% of keys and the previous healthy version for the rest (newest when
// no previous version is registered). ok is false when no RUNNING shard
// is registered. The decision is a pure function of (key, pct): sticky per
// session, deterministic for tests.
func RouteLogin(h *hub.Server, key string, pct int) (hub.ShardInfo, bool) {
	if h == nil {
		return hub.ShardInfo{}, false
	}
	newest, ok := h.NewestRunning()
	if !ok {
		return hub.ShardInfo{}, false
	}
	if pct <= 0 {
		return newest, true
	}
	prev, hasPrev := PreviousRunning(h)
	if !hasPrev {
		return newest, true
	}
	if CanaryToNewest(key, pct) {
		return newest, true
	}
	return prev, true
}

// RouteLoginDefault is RouteLogin with the live CANARY_PCT env (the
// /servers handler path).
func RouteLoginDefault(h *hub.Server, key string) (hub.ShardInfo, bool) {
	return RouteLogin(h, key, CanaryPct())
}

// WarmTracker records when each world version was first observed so the
// router can answer "is the old version still within its warm-hold".
// Observe is called on every /servers render (and by tests with an
// explicit clock); WarmOld reports the previous version's pick while the
// hold measured from the newest version's first observation is active.
// Safe for concurrent use; the zero value is usable.
type WarmTracker struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewWarmTracker returns an empty WarmTracker.
func NewWarmTracker() *WarmTracker {
	return &WarmTracker{seen: make(map[string]time.Time)}
}

// Observe stamps the first-observation time of every RUNNING version
// currently in the hub table. DRAINING/shutdown shards never extend the
// hold: the warm-hold measures from the newest RUNNING version's first
// observation. Versions never observed before stamp now.
func (t *WarmTracker) Observe(h *hub.Server, now time.Time) {
	if t == nil || h == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = make(map[string]time.Time)
	}
	for _, in := range h.ListShards() {
		if in.State != version.StateRunning {
			continue
		}
		if _, ok := t.seen[in.Version]; !ok {
			t.seen[in.Version] = now
		}
	}
}

// newestObserved returns the newest version's first-observation time: the
// preferred version's stamp, or the latest stamp when no RUNNING shard
// exists (deploy mid-flap). ok is false when nothing was ever observed.
func (t *WarmTracker) newestObserved(h *hub.Server) (time.Time, bool) {
	if t == nil || h == nil {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.seen) == 0 {
		return time.Time{}, false
	}
	if prefVer, ok := h.PreferredVersion(); ok {
		if ts, ok := t.seen[prefVer]; ok {
			return ts, true
		}
	}
	var latest time.Time
	first := true
	for _, ts := range t.seen {
		if first || ts.After(latest) {
			latest, first = ts, false
		}
	}
	return latest, !first
}

// WarmOld reports the previous healthy version's pick while its warm-hold
// is active (now < newest-observed + hold). Within the hold the old build
// must stay up and stays routable for existing sessions + the canary
// remainder; after the hold the operator may TERM it (see docs/DEPLOY.md).
// ok is false when no previous healthy version exists or the hold expired.
func (t *WarmTracker) WarmOld(h *hub.Server, now time.Time, hold time.Duration) (hub.ShardInfo, bool) {
	if hold <= 0 {
		hold = DefaultWarmHold
	}
	prev, ok := PreviousRunning(h)
	if !ok {
		return hub.ShardInfo{}, false
	}
	ts, ok := t.newestObserved(h)
	if !ok {
		// Never observed the table: be conservative and keep old warm.
		return prev, true
	}
	if !now.Before(ts.Add(hold)) {
		return hub.ShardInfo{}, false
	}
	return prev, true
}
