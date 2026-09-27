package app

import (
	"testing"
	"time"

	"rpg-world-server/internal/hub"
	"rpg-world-server/internal/version"
)

func TestListenAddrValidation(t *testing.T) {
	if got := ListenAddr(""); got != DefaultAddr {
		t.Fatalf("empty = %q, want %q", got, DefaultAddr)
	}
	if got := ListenAddr("9156"); got != "127.0.0.1:9156" {
		t.Fatalf("port = %q", got)
	}
	for _, bad := range []string{"abc", "0", "-1", "99999", "65536", "127.0.0.1:9001", ":8080", "  "} {
		if got := ListenAddr(bad); got != DefaultAddr {
			t.Fatalf("ListenAddr(%q) = %q, want fallback %q", bad, got, DefaultAddr)
		}
	}
	if got := ListenAddr(" 9156 "); got != "127.0.0.1:9156" {
		t.Fatalf("whitespace port = %q", got)
	}
}

func TestCheckHubTokenForRole(t *testing.T) {
	if err := CheckHubTokenForRole(RoleRouter, ""); err == nil {
		t.Fatal("router without HUB_TOKEN must fail closed")
	}
	if err := CheckHubTokenForRole(RoleShard, ""); err == nil {
		t.Fatal("shard without HUB_TOKEN must fail closed")
	}
	if err := CheckHubTokenForRole(RoleAllInOne, ""); err != nil {
		t.Fatalf("all-in-one without token must stay open (dev default): %v", err)
	}
	if err := CheckHubTokenForRole(RoleRouter, "secret"); err != nil {
		t.Fatalf("router with token = %v, want nil", err)
	}
}

func TestPreviousRunningFirstSeenTieBreak(t *testing.T) {
	h := hub.NewServer("", nil)
	base := time.Now()
	h.SetNowForTests(func() time.Time { return base })
	_ = h.Register(hub.HubHandshake{Type: "hub", Name: "oldA", Version: "v1", Addr: "127.0.0.1:9001", State: version.StateRunning, Load: 1})
	h.SetNowForTests(func() time.Time { return base.Add(time.Minute) })
	_ = h.Register(hub.HubHandshake{Type: "hub", Name: "oldB", Version: "v1", Addr: "127.0.0.1:9003", State: version.StateRunning, Load: 1})
	h.SetNowForTests(func() time.Time { return base.Add(2 * time.Minute) })
	_ = h.Register(hub.HubHandshake{Type: "hub", Name: "new", Version: "v2", Addr: "127.0.0.1:9002", State: version.StateRunning, Load: 1})
	prev, ok := PreviousRunning(h)
	if !ok {
		t.Fatal("PreviousRunning = false, want previous v1")
	}
	// Same load within v1: latest firstSeen (oldB) wins, mirroring
	// newestRunningLocked.
	if prev.Name != "oldB" {
		t.Fatalf("PreviousRunning = %+v, want oldB (latest firstSeen tie-break)", prev)
	}
}

func TestWarmTrackerRunningOnly(t *testing.T) {
	h := hub.NewServer("", nil)
	_ = h.Register(hub.HubHandshake{Type: "hub", Name: "old", Version: "v1", Addr: "127.0.0.1:9001"})
	_ = h.Register(hub.HubHandshake{Type: "hub", Name: "new", Version: "v2", Addr: "127.0.0.1:9002"})
	_ = h.HeartbeatEx("old", nil, version.StateDraining, 0)
	tr := NewWarmTracker()
	now := time.Now()
	tr.Observe(h, now)
	trMuSnapshot := func() map[string]time.Time {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		out := map[string]time.Time{}
		for k, v := range tr.seen {
			out[k] = v
		}
		return out
	}()
	if _, ok := trMuSnapshot["v1"]; ok {
		// v1 has no RUNNING shard (old is DRAINING) — must not stamp.
		// new v2 is RUNNING and must stamp.
		t.Fatalf("Observe stamped DRAINING-only v1: %v", trMuSnapshot)
	}
	if _, ok := trMuSnapshot["v2"]; !ok {
		t.Fatalf("Observe missed RUNNING v2: %v", trMuSnapshot)
	}
}

func TestBuildServerListCanaryPctEmit(t *testing.T) {
	h := hub.NewServer("", nil)
	_ = h.Register(hub.HubHandshake{Type: "hub", Name: "old", Version: "v1", Addr: "127.0.0.1:9001"})
	_ = h.Register(hub.HubHandshake{Type: "hub", Name: "new", Version: "v2", Addr: "127.0.0.1:9002"})
	tr := NewWarmTracker()
	now := time.Now()
	tr.Observe(h, now)
	// Anonymous list with pct>0 still reports the active percentage.
	got := BuildServerListForLogin(h, "", 5, tr, now)
	if got.CanaryPct != 5 {
		t.Fatalf("anonymous canary list CanaryPct = %d, want 5", got.CanaryPct)
	}
	if got.Preferred != "127.0.0.1:9002" {
		t.Fatalf("anonymous preferred = %q, want newest (no key = no personalization)", got.Preferred)
	}
}
