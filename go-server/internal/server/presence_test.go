package server

import (
	"net/http/httptest"
	"testing"

	"rpg-world-server/internal/app"
	"rpg-world-server/internal/hub"
	"rpg-world-server/internal/version"
)

// TestCrossShardOnlineAgainstRouter is the phase-6 contract: the shard's
// duplicate-login check reaches the router's POST /isOnline and scopes the
// answer to other shards, so one account cannot be logged in twice across a
// fleet.
func TestCrossShardOnlineAgainstRouter(t *testing.T) {
	h := hub.NewServer("", nil)
	if err := h.Register(hub.HubHandshake{
		Type: "hub", Name: "world-a", Addr: "127.0.0.1:9001",
		BuildID: "aaa", State: version.StateRunning, Load: 1,
		Players: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.RouterHandlerWith(h, &app.Lifecycle{}, app.RouterOptions{MaxPlayers: 200}))
	defer srv.Close()

	// This process is a DIFFERENT shard (SERVER_ID 2), so alice is a duplicate.
	t.Setenv("HUB_ADDR", srv.URL)
	t.Setenv("SERVER_ID", "2")
	t.Setenv("HUB_TOKEN", "")
	if !crossShardOnline("alice") {
		t.Fatal("alice is on world-a; a second shard must see her online")
	}
	if crossShardOnline("ghost") {
		t.Fatal("unknown player must not be reported online")
	}

	// SERVER_ID matching the host shard means "it's me": not a duplicate.
	t.Setenv("SERVER_ID", "1")
	if crossShardOnline("alice") {
		t.Fatal("the hosting shard must not see its own player as a duplicate")
	}

	// A configured token is enforced end to end.
	t.Setenv("HUB_TOKEN", "s3cret")
	if crossShardOnline("alice") {
		t.Fatal("a mismatched hub token must not be trusted")
	}
	t.Setenv("SERVER_ID", "2")
	if !crossShardOnline("alice") {
		t.Fatal("the matching hub token must be accepted")
	}
}

// TestCrossShardOnlineStandalone pins the all-in-one default: with no hub
// configured the check short-circuits, exactly like TS's hubEnabled=false.
func TestCrossShardOnlineStandalone(t *testing.T) {
	t.Setenv("HUB_ADDR", "")
	t.Setenv("HUB_LISTEN", "")
	t.Setenv("HUB_TOKEN", "")
	if crossShardBaseURL() != "" {
		t.Fatalf("standalone base URL = %q, want empty", crossShardBaseURL())
	}
	if crossShardOnline("alice") {
		t.Fatal("standalone processes must answer offline without a request")
	}
}

// TestCrossShardBaseURLForms pins the ws:// -> http:// translation: shards dial
// the hub with a socket URL but the presence check is plain HTTP.
func TestCrossShardBaseURLForms(t *testing.T) {
	cases := map[string]string{
		"ws://127.0.0.1:9101":     "http://127.0.0.1:9101",
		"ws://127.0.0.1:9101/":    "http://127.0.0.1:9101",
		"wss://hub.example:9101":  "https://hub.example:9101",
		"http://127.0.0.1:9101":   "http://127.0.0.1:9101",
		"127.0.0.1:9101":          "http://127.0.0.1:9101",
		"https://hub.example.com": "https://hub.example.com",
	}
	for raw, want := range cases {
		t.Setenv("HUB_ADDR", raw)
		t.Setenv("HUB_LISTEN", "")
		if got := crossShardBaseURL(); got != want {
			t.Fatalf("crossShardBaseURL(%q) = %q, want %q", raw, got, want)
		}
	}
	// HUB_LISTEN is the fallback when HUB_ADDR is unset (the router may have
	// been given a bare listen addr instead).
	t.Setenv("HUB_ADDR", "")
	t.Setenv("HUB_LISTEN", "127.0.0.1:9101")
	if got := crossShardBaseURL(); got != "http://127.0.0.1:9101" {
		t.Fatalf("HUB_LISTEN fallback = %q", got)
	}
}
