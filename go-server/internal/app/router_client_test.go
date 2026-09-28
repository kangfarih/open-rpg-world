package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"rpg-world-server/internal/hub"
	"rpg-world-server/internal/version"
)

// routerClientServer builds the client-facing router over a two-world hub.
func routerClientServer(t *testing.T, opts RouterOptions) (*httptest.Server, *hub.Server) {
	t.Helper()
	h := hub.NewServer("", nil)
	if err := h.Register(hub.HubHandshake{
		Type: "hub", Name: "world-a", Addr: "127.0.0.1:9001",
		BuildID: "aaa", State: version.StateRunning, Load: 1,
		Players: []string{"alice"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Register(hub.HubHandshake{
		Type: "hub", Name: "world-b", Addr: "127.0.0.1:9002",
		BuildID: "bbb", State: version.StateRunning, Load: 3,
		Players: []string{"bob", "carol", "dave"},
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(RouterHandlerWith(h, &Lifecycle{}, opts))
	t.Cleanup(srv.Close)
	return srv, h
}

// TestRouterRootStillServesHubSocket pins the "/" dispatch: the liveness body
// is HTTP-only, so a shard's WebSocket upgrade must still reach the hub. A
// route pattern for GET / shadows the socket and every shard dial fails with
// "bad handshake" (multi-shard mode silently loses all routing).
func TestRouterRootStillServesHubSocket(t *testing.T) {
	srv, _ := routerClientServer(t, RouterOptions{MaxPlayers: 200})
	wsURL := "ws" + srv.URL[len("http"):]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := hub.NewClient(wsURL, "", "world-c", hub.NewRouter(), hub.NewMailer(nil), nil)
	c.SetHeartbeatInterval(50 * time.Millisecond)
	// A game addr: undialable shards are skipped by the world list.
	c.SetBuild("ccc", version.GVer, "127.0.0.1:9003")
	go c.Start(ctx)
	defer c.Stop()
	if !c.WaitConnected(5 * time.Second) {
		t.Fatal("shard socket never connected through the client-facing router")
	}

	// The registering shard shows up in the world list the client parses.
	var list []SerializedServer
	if code := getJSON(t, srv.URL+"/all", &list); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	found := false
	for _, w := range list {
		if w.Name == "world-c" {
			found = true
		}
	}
	if !found {
		t.Fatalf("/all = %+v, want the freshly registered world-c", list)
	}

	// Plain HTTP GET / is still the liveness object.
	var root struct {
		Status string `json:"status"`
	}
	getJSON(t, srv.URL+"/", &root)
	if !strings.Contains(root.Status, "online") {
		t.Fatalf("GET / = %+v, want the hub liveness status", root)
	}
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return res.StatusCode
}

func postJSON(t *testing.T, url, body string, out any) int {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return res.StatusCode
}

// TestRouterAllSerializedServers pins the world-select contract the stock
// client parses (GET /all -> SerializedServer[], ascending world id).
func TestRouterAllSerializedServers(t *testing.T) {
	srv, _ := routerClientServer(t, RouterOptions{MaxPlayers: 200})

	var list []SerializedServer
	if code := getJSON(t, srv.URL+"/all", &list); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(list) != 2 {
		t.Fatalf("worlds = %d, want 2", len(list))
	}
	if list[0].ID >= list[1].ID {
		t.Fatalf("worlds not in ascending id order: %+v", list)
	}
	first := list[0]
	if first.Name != "world-a" || first.Host != "127.0.0.1" || first.Port != 9001 {
		t.Fatalf("world-a serialized as %+v", first)
	}
	if first.Players != 1 || first.MaxPlayers != 200 {
		t.Fatalf("player counters = %d/%d, want 1/200", first.Players, first.MaxPlayers)
	}
	// The client needs a dialable host:port for every entry.
	for _, s := range list {
		if s.Host == "" || s.Port == 0 {
			t.Fatalf("world %q is not dialable: %+v", s.Name, s)
		}
	}
}

// TestRouterServerPickAndFull pins hub api.ts handleServer: the first world with
// room for one more player, and {status:"error"} once nothing has space.
func TestRouterServerPickAndFull(t *testing.T) {
	srv, h := routerClientServer(t, RouterOptions{MaxPlayers: 4})

	var pick SerializedServer
	if code := getJSON(t, srv.URL+"/server", &pick); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if pick.Name != "world-a" {
		t.Fatalf("pick = %+v, want world-a (lowest id with space)", pick)
	}

	// Fill both worlds to the cap: the pick must degrade, not error out.
	if err := h.HeartbeatEx("world-a", []string{"a1", "a2", "a3"}, "", 3); err != nil {
		t.Fatal(err)
	}
	if err := h.HeartbeatEx("world-b", []string{"b1", "b2", "b3"}, "", 3); err != nil {
		t.Fatal(err)
	}
	var full struct {
		Status string `json:"status"`
	}
	getJSON(t, srv.URL+"/server", &full)
	if full.Status != "error" {
		t.Fatalf("full worlds answered %+v, want status=error", full)
	}
}

// TestRouterIsOnlineScopesToOtherShards pins hub api.ts handleIsOnline: online
// means the name is on a shard OTHER than the asker's.
func TestRouterIsOnlineScopesToOtherShards(t *testing.T) {
	srv, h := routerClientServer(t, RouterOptions{MaxPlayers: 200})
	ids := map[string]int{}
	for _, in := range h.ListShards() {
		ids[in.Name] = in.ID
	}
	if ids["world-a"] == 0 || ids["world-a"] == ids["world-b"] {
		t.Fatalf("world ids not assigned distinctly: %v", ids)
	}

	// alice is on world-a: a different shard sees her online...
	var res struct {
		Status string `json:"status"`
		Online bool   `json:"online"`
	}
	body := `{"hubAccessToken":"","serverId":` + strconv.Itoa(ids["world-b"]) + `,"username":"alice"}`
	if code := postJSON(t, srv.URL+"/isOnline", body, &res); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if res.Status != "success" || !res.Online {
		t.Fatalf("alice from world-b = %+v, want online", res)
	}

	// ...but the shard hosting her reports no duplicate.
	body = `{"hubAccessToken":"","serverId":` + strconv.Itoa(ids["world-a"]) + `,"username":"alice"}`
	postJSON(t, srv.URL+"/isOnline", body, &res)
	if res.Online {
		t.Fatalf("alice from world-a = %+v, want offline (no duplicate)", res)
	}

	// An unknown player is never online, and a missing username is invalid.
	body = `{"hubAccessToken":"","serverId":` + strconv.Itoa(ids["world-a"]) + `,"username":"ghost"}`
	postJSON(t, srv.URL+"/isOnline", body, &res)
	if res.Online {
		t.Fatalf("ghost = %+v, want offline", res)
	}
	var bad struct {
		Error string `json:"error"`
	}
	postJSON(t, srv.URL+"/isOnline", `{"serverId":1}`, &bad)
	if bad.Error != "invalid" {
		t.Fatalf("missing username answered %+v, want error=invalid", bad)
	}
}

// TestRouterIsOnlineToken pins verifyRequest: a configured HUB_TOKEN is
// required, the all-in-one dev default (no token) stays open.
func TestRouterIsOnlineToken(t *testing.T) {
	t.Setenv("HUB_TOKEN", "s3cret")
	srv, h := routerClientServer(t, RouterOptions{MaxPlayers: 200})
	var id int
	for _, in := range h.ListShards() {
		id = in.ID
		break
	}

	var res struct {
		Error string `json:"error"`
	}
	body := `{"hubAccessToken":"wrong","serverId":` + strconv.Itoa(id) + `,"username":"alice"}`
	postJSON(t, srv.URL+"/isOnline", body, &res)
	if res.Error != "invalid" {
		t.Fatalf("bad token answered %+v, want error=invalid", res)
	}
	if routerTokenOK("") || routerTokenOK("nope") || !routerTokenOK("s3cret") {
		t.Fatal("routerTokenOK must require the configured token")
	}
}

// TestRouterDegradesWithoutDB pins the documented degradation: with no database
// the DB-backed surfaces answer the client-parsable shapes instead of failing.
func TestRouterDegradesWithoutDB(t *testing.T) {
	srv, _ := routerClientServer(t, RouterOptions{MaxPlayers: 200})

	var boards struct {
		Status string `json:"status"`
		List   []any  `json:"list"`
	}
	getJSON(t, srv.URL+"/leaderboards", &boards)
	if boards.Status != "success" || len(boards.List) != 0 {
		t.Fatalf("leaderboards without DB = %+v, want empty success", boards)
	}

	var reset struct {
		Error string `json:"error"`
	}
	postJSON(t, srv.URL+"/api/v1/requestReset", `{"email":"user@example.com"}`, &reset)
	if reset.Error != "invalid" {
		t.Fatalf("reset without accounts = %+v, want error=invalid", reset)
	}
	postJSON(t, srv.URL+"/api/v1/resetPassword", `{"id":"x","token":"y","password":"secret"}`, &reset)
	if reset.Error != "invalid" {
		t.Fatalf("resetPassword without accounts = %+v, want error=invalid", reset)
	}
}

// TestRouterRootAndHealthz pins the hub liveness shape (api.ts handleRoot) and
// that the drain lifecycle still owns /healthz.
func TestRouterRootAndHealthz(t *testing.T) {
	srv, _ := routerClientServer(t, RouterOptions{})

	var root struct {
		Status string `json:"status"`
	}
	getJSON(t, srv.URL+"/", &root)
	if !strings.Contains(root.Status, "online and functional") {
		t.Fatalf("root status = %q", root.Status)
	}
	if code := getJSON(t, srv.URL+"/healthz", nil); code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200 while RUNNING", code)
	}
}

// TestRouterDBPathResolution pins HUB_DB/DB_PATH precedence for the DB-backed
// router surfaces.
func TestRouterDBPathResolution(t *testing.T) {
	t.Setenv(EnvHubDB, "/tmp/hub.db")
	if got := RouterDBPath(Config{DBPath: "/tmp/shard.db"}); got != "/tmp/hub.db" {
		t.Fatalf("HUB_DB wins: got %q", got)
	}
	t.Setenv(EnvHubDB, "")
	if got := RouterDBPath(Config{DBPath: "/tmp/shard.db"}); got != "/tmp/shard.db" {
		t.Fatalf("DB_PATH fallback: got %q", got)
	}
	if got := RouterDBPath(Config{}); got != "data.db" {
		t.Fatalf("default: got %q", got)
	}
}
