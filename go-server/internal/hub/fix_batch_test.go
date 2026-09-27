package hub

import (
	"testing"
	"time"
)

// MemoryMail per-recipient cap: oldest dropped when full.
func TestMemoryMailCap(t *testing.T) {
	m := NewMemoryMail()
	m.SetLimitsForTests(3, 0, nil)
	for i := 0; i < 5; i++ {
		if err := m.Store(Message{To: "bob", Kind: KindChat, Payload: i}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.Take("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Take after 5 Stores with cap 3 = %d, want 3 (oldest dropped)", len(got))
	}
}

// MemoryMail TTL: expired entries vanish on Take.
func TestMemoryMailTTL(t *testing.T) {
	base := time.Now()
	m := NewMemoryMail()
	m.SetLimitsForTests(50, time.Hour, func() time.Time { return base })
	if err := m.Store(Message{To: "bob", Kind: KindChat, Payload: "old"}); err != nil {
		t.Fatal(err)
	}
	m.SetLimitsForTests(50, time.Hour, func() time.Time { return base.Add(2 * time.Hour) })
	got, err := m.Take("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Take after TTL expiry = %v, want empty", got)
	}
}

// Token posture: router/shard require a token, all-in-one stays open.
func TestTokenRequiredForRole(t *testing.T) {
	if !TokenRequiredForRole("router") || !TokenRequiredForRole("shard") {
		t.Fatal("router/shard must require HUB_TOKEN (fail-closed)")
	}
	if !TokenRequiredForRole("ROUTER") || !TokenRequiredForRole(" Shard ") {
		t.Fatal("role match must be case-insensitive + trimmed")
	}
	if TokenRequiredForRole("all-in-one") || TokenRequiredForRole("") || TokenRequiredForRole("game") {
		t.Fatal("all-in-one/empty/unknown must stay open (dev default)")
	}
}

// firstSeen bumps on GVer change (same-name redeploy of a new wire version
// counts as new, like BuildID/Version).
func TestFirstSeenBumpsOnGVerChange(t *testing.T) {
	s := NewServer("", nil)
	base := time.Now()
	s.now = func() time.Time { return base }
	if err := s.Register(HubHandshake{Type: "hub", Name: "a", BuildID: "b1", GVer: "g1"}); err != nil {
		t.Fatal(err)
	}
	first := s.shards["a"].firstSeen
	s.now = func() time.Time { return base.Add(time.Minute) }
	if err := s.Register(HubHandshake{Type: "hub", Name: "a", BuildID: "b1", GVer: "g2"}); err != nil {
		t.Fatal(err)
	}
	if !s.shards["a"].firstSeen.After(first) {
		t.Fatal("GVer change must bump firstSeen (same-name redeploy)")
	}
}

// ListShards groups by version newness: a late DRAINING old-version shard
// (excluded from newness) must not sort above the newest RUNNING version.
func TestListShardsVersionGrouping(t *testing.T) {
	s := NewServer("", nil)
	base := time.Now()
	s.now = func() time.Time { return base }
	_ = s.Register(HubHandshake{Type: "hub", Name: "old1", Version: "v1"})
	s.now = func() time.Time { return base.Add(time.Minute) }
	_ = s.Register(HubHandshake{Type: "hub", Name: "new1", Version: "v2"})
	s.now = func() time.Time { return base.Add(2 * time.Minute) }
	_ = s.Register(HubHandshake{Type: "hub", Name: "old2", Version: "v1"})
	_ = s.HeartbeatEx("old2", nil, "DRAINING", 0)
	list := s.ListShards()
	if len(list) != 3 {
		t.Fatalf("shards = %d, want 3", len(list))
	}
	if list[0].Version != "v2" {
		t.Fatalf("first shard = %+v, want newest RUNNING version v2 grouped first", list[0])
	}
	if list[0].Name != "new1" || !list[0].Newest {
		t.Fatalf("first shard = %+v, want new1 + Newest", list[0])
	}
}

// SQLiteMail Take is atomic under concurrency (no duplicate delivery).
func TestSQLiteMailTakeAtomic(t *testing.T) {
	s, err := OpenSQLiteMail(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 10; i++ {
		if err := s.Store(Message{To: "bob", Kind: KindChat, Payload: i}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan []Message, 2)
	for i := 0; i < 2; i++ {
		go func() {
			out, _ := s.Take("bob")
			done <- out
		}()
	}
	a := <-done
	b := <-done
	seen := map[int]int{}
	for _, m := range append(a, b...) {
		var n int
		switch v := m.Payload.(type) {
		case float64:
			n = int(v)
		case int:
			n = v
		default:
			t.Fatalf("payload = %T", v)
		}
		seen[n]++
	}
	if len(seen) != 10 {
		t.Fatalf("concurrent Take delivered %d unique of 10 (dup/loss = race)", len(seen))
	}
	for n, c := range seen {
		if c != 1 {
			t.Fatalf("message %d delivered %d times, want once", n, c)
		}
	}
}
