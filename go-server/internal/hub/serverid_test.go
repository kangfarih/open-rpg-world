package hub

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWorldIDsAreSequential pins the TS hub's Server.id allocator: ids come from
// a counter in registration order, survive re-registration, and surface through
// ListShards/NewestRunning.
func TestWorldIDsAreSequential(t *testing.T) {
	s := NewServer("", nil)
	for _, name := range []string{"old", "mid", "new"} {
		if err := s.Register(HubHandshake{Type: "hub", Name: name, Addr: "127.0.0.1:9000"}); err != nil {
			t.Fatal(err)
		}
	}

	got := map[string]int{}
	for _, in := range s.ListShards() {
		got[in.Name] = in.ID
	}
	if got["old"] != 1 || got["mid"] != 2 || got["new"] != 3 {
		t.Fatalf("ids = %v, want old=1 mid=2 new=3", got)
	}

	// Re-registering the same shard (redeploy/reconnect) keeps its id: the
	// client's world list must not renumber underneath it.
	if err := s.Register(HubHandshake{
		Type: "hub", Name: "old", Addr: "127.0.0.1:9000", BuildID: "redeploy",
	}); err != nil {
		t.Fatal(err)
	}
	for _, in := range s.ListShards() {
		if in.Name == "old" && in.ID != 1 {
			t.Fatalf("old re-registered as id %d, want 1", in.ID)
		}
	}

	// NewestRunning carries the same id (the login target's world id).
	best, ok := s.NewestRunning()
	if !ok || best.ID == 0 {
		t.Fatalf("NewestRunning = %+v, %v; want a non-zero id", best, ok)
	}
}

// TestWorldIDPinnedAndCollisionFree pins the pinned-id path
// (HubHandshake.ServerID, SERVER_ID env): a shard may claim its own world id, and
// the counter still advances so later auto-ids never collide with it.
func TestWorldIDPinnedAndCollisionFree(t *testing.T) {
	s := NewServer("", nil)
	if err := s.Register(HubHandshake{Type: "hub", Name: "pinned", ServerID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(HubHandshake{Type: "hub", Name: "auto"}); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, in := range s.ListShards() {
		got[in.Name] = in.ID
	}
	if got["pinned"] != 7 {
		t.Fatalf("pinned id = %d, want 7", got["pinned"])
	}
	if got["auto"] != 8 {
		t.Fatalf("auto id = %d, want 8 (counter must skip the pinned id)", got["auto"])
	}
}

// TestShardPinsItsServerID pins the SERVER_ID round trip: the shard's hub
// handshake claims that world id, so the id it posts to /isOnline is the id the
// router knows it by. A shard whose live id disagrees with SERVER_ID would have
// its OWN players reported as online elsewhere (every login 'loggedin').
func TestShardPinsItsServerID(t *testing.T) {
	t.Setenv(EnvServerID, "7")
	if got := ServerIDFromEnv(); got != 7 {
		t.Fatalf("ServerIDFromEnv() = %d, want 7", got)
	}

	s := NewServer("", nil)
	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewClient("ws"+srv.URL[len("http"):], "", "shard-7", NewRouter(), NewMailer(nil), nil)
	c.SetHeartbeatInterval(20 * time.Millisecond)
	go c.Start(ctx)
	defer c.Stop()
	if !c.WaitConnected(5 * time.Second) {
		t.Fatal("client did not connect")
	}

	for _, in := range s.ListShards() {
		if in.Name == "shard-7" && in.ID != 7 {
			t.Fatalf("world id = %d, want the pinned SERVER_ID 7", in.ID)
		}
	}

	// Junk and non-positive values fall back to 1 instead of pinning id 0
	// (which the hub reads as "unreported").
	for _, bad := range []string{"", "abc", "0", "-3", "70000"} {
		t.Setenv(EnvServerID, bad)
		if got := ServerIDFromEnv(); got != 1 {
			t.Fatalf("ServerIDFromEnv(%q) = %d, want 1", bad, got)
		}
	}
}

// TestFindPlayerTracksRoster pins the presence source /isOnline reads: the
// heartbeat's player list is the authoritative roster.
func TestFindPlayerTracksRoster(t *testing.T) {
	s := NewServer("", nil)
	if err := s.Register(HubHandshake{
		Type: "hub", Name: "world-a", Players: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	if shard, ok := s.FindPlayer("alice"); !ok || shard != "world-a" {
		t.Fatalf("FindPlayer(alice) = %q, %v", shard, ok)
	}
	if _, ok := s.FindPlayer("ghost"); ok {
		t.Fatal("FindPlayer(ghost) must miss")
	}
	if err := s.HeartbeatEx("world-a", []string{"bob"}, "", 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.FindPlayer("alice"); ok {
		t.Fatal("alice must leave the roster when the heartbeat drops her")
	}
	if _, ok := s.FindPlayer("bob"); !ok {
		t.Fatal("bob must be on the roster after the heartbeat")
	}
}
