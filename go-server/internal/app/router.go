// Router driver (GO-PLAN §12 R1 / REWRITE-V2 V2-M1): ROLE=router serves the
// hub server-list + login routing without running the game sim.
//
// Endpoints (single listener, see RouterAddr):
//   - / (any non-GET path too): hub shard sockets (hub.Server: register +
//     heartbeat + relay + roster; 3-miss eviction).
//   - GET /servers: login/hub server-list directing NEW sessions to the
//     newest healthy RUNNING version: {"preferred":<addr>,
//     "shards":[{name,addr,buildId,gVer,state,load,newest,version,regions}]}.
//     preferred is "" when no RUNNING shard is registered (clients keep
//     their current session / retry). Old versions keep serving existing
//     sessions (no new logins).
//   - GET /healthz: instance state/load (+ stamps); 503 once DRAINING.
//
// Env contract:
//   - ROLE=router (or --role router) selects this driver.
//   - HUB_LISTEN is the listen addr; else HUB_ADDR when it is a bare
//     host:port; else DefaultRouterAddr. (Shards dial HUB_ADDR as a
//     ws(s):// URL, unchanged.)
//   - HUB_TOKEN authenticates shard registrations (unchanged).
//   - DRAIN_TIMEOUT bounds router shutdown after SIGTERM (the router holds
//     no sim state, so it observes DRAINING on /healthz and exits).
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"rpg-world-server/internal/account"
	"rpg-world-server/internal/api"
	"rpg-world-server/internal/console"
	"rpg-world-server/internal/data"
	"rpg-world-server/internal/hub"
	"rpg-world-server/internal/leaderboard"
	"rpg-world-server/internal/persist"
	"rpg-world-server/internal/version"
)

// DefaultRouterAddr is the router listen addr when neither HUB_LISTEN nor a
// bare host:port HUB_ADDR is set (new port: never the game default 9001).
const DefaultRouterAddr = "127.0.0.1:9101"

// EnvHubListen overrides the router listen addr.
const EnvHubListen = "HUB_LISTEN"

// RouterAddr resolves the router listen addr: HUB_LISTEN, else HUB_ADDR
// when it is a bare host:port (hub-side form; ws(s):// URLs belong to
// shards), else DefaultRouterAddr.
func RouterAddr() string { return RouterAddrFromEnv(os.Getenv) }

// RouterAddrFromEnv is RouterAddr over an injected env lookup (tests).
func RouterAddrFromEnv(getenv func(string) string) string {
	if getenv != nil {
		if v := strings.TrimSpace(getenv(EnvHubListen)); v != "" {
			return v
		}
		if v := strings.TrimSpace(getenv(hub.EnvHubAddr)); v != "" {
			if !strings.Contains(v, "://") {
				return v
			}
		}
	}
	return DefaultRouterAddr
}

// ServerEntry is one /servers row.
type ServerEntry struct {
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	BuildID string `json:"buildId,omitempty"`
	GVer    string `json:"gVer,omitempty"`
	State   string `json:"state"`
	Load    int    `json:"load"`
	Newest  bool   `json:"newest,omitempty"`
	// Version is the R2 world version (explicit VERSION tag or the
	// buildID+gVer pair; "" = unknown). New sessions go to the newest
	// healthy RUNNING version (Preferred); old versions keep serving
	// existing sessions only.
	Version string `json:"version,omitempty"`
	// Regions is the shard's reported scope for the region->shard lookup
	// (nil = unscoped).
	Regions []int `json:"regions,omitempty"`
}

// ServerList is the GET /servers shape.
type ServerList struct {
	Preferred string        `json:"preferred"`
	Shards    []ServerEntry `json:"shards"`
	// Previous is the previous healthy RUNNING version's addr (the canary
	// remainder target + rollback fallback; "" omits = single version, the
	// default boot renders byte-identical to before).
	Previous string `json:"previous,omitempty"`
	// CanaryPct is the active CANARY_PCT (omitted when 0 = today's
	// behavior: 100% of NEW logins to Preferred).
	CanaryPct int `json:"canaryPct,omitempty"`
	// WarmUntil is the RFC3339 time until which the previous version must
	// stay up (newest-observed + WARM_HOLD; "" omits = no previous).
	WarmUntil string `json:"warmUntil,omitempty"`
}

// BuildServerList renders the hub table for login routing: shards newest
// first, Preferred pointing at the newest healthy RUNNING version ("" when
// none — clients must not start new sessions anywhere).
func BuildServerList(h *hub.Server) ServerList {
	out := ServerList{}
	if h == nil {
		return out
	}
	infos := h.ListShards()
	out.Shards = make([]ServerEntry, 0, len(infos))
	for _, in := range infos {
		out.Shards = append(out.Shards, ServerEntry{
			Name: in.Name, Addr: in.Addr, BuildID: in.BuildID,
			GVer: in.GVer, State: in.State, Load: in.Load, Newest: in.Newest,
			Version: in.Version, Regions: in.Regions,
		})
	}
	if pref, ok := h.NewestRunning(); ok {
		out.Preferred = pref.Addr
		if out.Preferred == "" {
			out.Preferred = pref.Name
		}
	}
	return out
}

// BuildServerListForLogin renders the hub table for one NEW session key
// (instance/username, "" = anonymous): Preferred follows RouteLogin under
// pct (pct<=0 or "" key = BuildServerList, today's behavior), Previous
// always names the previous healthy version's addr when one is registered,
// and WarmUntil gates TERM-ing the old build (see WarmTracker). The
// canary decision is sticky per key: the same login key always resolves to
// the same build for a fixed table + pct.
func BuildServerListForLogin(h *hub.Server, key string, pct int, warm *WarmTracker, now time.Time) ServerList {
	out := BuildServerList(h)
	if h == nil {
		return out
	}
	// CanaryPct reports the active canary percentage whenever one is set
	// (even for anonymous lists without a login key); the personalized
	// Preferred pick additionally needs a key (pct<=0 keeps today's
	// newest-RUNNING routing via BuildServerList).
	if pct > 0 {
		out.CanaryPct = pct
		if key != "" {
			if pick, ok := RouteLogin(h, key, pct); ok {
				out.Preferred = pick.Addr
				if out.Preferred == "" {
					out.Preferred = pick.Name
				}
			}
		}
	}
	if prev, ok := PreviousRunning(h); ok {
		addr := prev.Addr
		if addr == "" {
			addr = prev.Name
		}
		out.Previous = addr
		if warm != nil {
			if ts, ok := warm.newestObserved(h); ok {
				hold := WarmHold()
				if hold <= 0 {
					hold = DefaultWarmHold
				}
				out.WarmUntil = ts.Add(hold).UTC().Format(time.RFC3339)
			}
		}
	}
	return out
}

// RouterHandler wires the hub socket + /servers + /healthz on one mux. The
// caller owns listening; h must be non-nil (RunRouter always supplies one).
//
// R3 canary: GET /servers accepts ?login=<instance-or-user> (also
// ?instance= / ?user=) for a personalized canary decision under the live
// CANARY_PCT env (router restart picks up env changes; the router is
// stateless with a 5s SIGTERM grace). Without the param the response is
// today's newest-RUNNING routing, unchanged.
func RouterHandler(h *hub.Server, lc *Lifecycle) http.Handler {
	return RouterHandlerWith(h, lc, RouterOptions{})
}

// RouterName is the human-readable hub name in the TS root response
// (config.name). It matches the shard status snapshot's name field.
const RouterName = "kaetram-stub"

// RouterOptions are the optional, DB-backed surfaces the router serves for the
// stock client. Everything here is hub-only in TS (the hub owns MongoDB), so
// with a zero RouterOptions the router is the same roster-only process it has
// always been; the DB-backed routes then answer the TS 'unavailable' shapes
// instead of 404 so the client's fetch chain still parses.
type RouterOptions struct {
	// MaxPlayers is the per-world cap the client renders as
	// "(n/maxPlayers players)" (config.maxPlayers, MAX_PLAYERS).
	MaxPlayers int
	// Accounts enables POST /api/v1/requestReset + /api/v1/resetPassword
	// (hub api.ts parity). nil = the routes answer {error:"invalid"}.
	Accounts api.AccountProvider
	// ResetMailer delivers a reset link; nil logs the link (dev fallback).
	ResetMailer func(email, link string)
	// ResetURLBase prefixes the mailed link ("" = api.DefaultResetURLBase).
	ResetURLBase string
	// Leaderboards enables GET /leaderboards from a leaderboard.Cache over
	// the router's DB. nil = {status:"success", list:[]}.
	Leaderboards api.LeaderboardProvider
	// HubHTTPAddr is the router's own HTTP base for the /isOnline token
	// check. "" derives it from HUB_ADDR/HUB_LISTEN (the hub socket addr).
	HubHTTPAddr string
}

// RouterMaxPlayers resolves MAX_PLAYERS for the client's world list
// (Login's loginMaxPlayers reads the same env with the same default).
func RouterMaxPlayers() int {
	if v := strings.TrimSpace(os.Getenv("MAX_PLAYERS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return MaxPlayers
}

// SerializedServer is the stock client's world entry: the shape the client
// dials from (packages/common/types/network.d.ts SerializedServer, produced by
// hub/src/model/server.ts serialize). Host/Port are what the client opens its
// game socket to, so they come from the shard's login-redirect Addr.
type SerializedServer struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Players    int    `json:"players"`
	MaxPlayers int    `json:"maxPlayers"`
}

// SerializeShards renders every registered shard in ascending world-id order
// (hub models.serializeServers sorts by id). Shards whose Addr is not a
// host:port are skipped: the client dials Host/Port verbatim, so an
// unroutable entry would break the world list rather than degrade it.
func SerializeShards(h *hub.Server, maxPlayers int) []SerializedServer {
	out := []SerializedServer{}
	if h == nil {
		return out
	}
	for _, in := range h.ListShards() {
		host, port := splitHostPort(in.Addr)
		if host == "" || port == 0 {
			continue
		}
		out = append(out, SerializedServer{
			ID: in.ID, Name: in.Name, Host: host, Port: port,
			Players: in.Load, MaxPlayers: maxPlayers,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// splitHostPort splits a shard addr ("127.0.0.1:9001") into the client's
// dialable host/port pair.
func splitHostPort(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || host == "" {
		return "", 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0
	}
	return host, port
}

// findEmptyServer mirrors hub models.findEmptyServer: the first world (ascending
// id) with room for one more player, skipping a world that is within one slot
// of its cap. ok=false means no world has space (handleServer answers
// {status:"error"}).
func findEmptyServer(list []SerializedServer) (SerializedServer, bool) {
	for _, s := range list {
		if s.Players >= s.MaxPlayers-1 {
			continue
		}
		return s, true
	}
	return SerializedServer{}, false
}

// writeJSON answers one JSON body (the router's endpoints all answer 200, like
// the TS express handlers).
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// RouterHandlerWith is RouterHandler plus the client-facing hub HTTP surface
// the world-select / forgot-password flows need (GET /, /all, /server,
// /leaderboards, POST /isOnline, POST /api/v1/*). Every route is additive: the
// hub socket at "/", /servers and /healthz behave exactly as before.
func RouterHandlerWith(h *hub.Server, lc *Lifecycle, opts RouterOptions) http.Handler {
	if lc == nil {
		lc = Default
	}
	if opts.MaxPlayers <= 0 {
		opts.MaxPlayers = RouterMaxPlayers()
	}
	mux := http.NewServeMux()
	warm := NewWarmTracker()
	mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		key := q.Get("login")
		if key == "" {
			key = q.Get("instance")
		}
		if key == "" {
			key = q.Get("user")
		}
		now := time.Now()
		warm.Observe(h, now)
		writeJSON(w, BuildServerListForLogin(h, key, CanaryPct(), warm, now))
	})
	mux.HandleFunc("GET /healthz", lc.ServeHealth)

	// GET /all — the world list (`(n/max players)` per entry).
	mux.HandleFunc("GET /all", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, SerializeShards(h, opts.MaxPlayers))
	})

	// GET /server — the world the client should pick up (hub api.ts
	// handleServer: {status:"error"} when nothing has space).
	mux.HandleFunc("GET /server", func(w http.ResponseWriter, _ *http.Request) {
		pick, ok := findEmptyServer(SerializeShards(h, opts.MaxPlayers))
		if !ok {
			writeJSON(w, map[string]string{"status": "error"})
			return
		}
		writeJSON(w, pick)
	})

	// POST /isOnline — cross-shard duplicate-login check (hub api.ts
	// handleIsOnline). The shard posts {hubAccessToken, serverId, username} and
	// gets {status, online}: online means the name is on a DIFFERENT shard.
	mux.HandleFunc("POST /isOnline", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			HubAccessToken string `json:"hubAccessToken"`
			ServerID       int    `json:"serverId"`
			Username       string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" {
			writeJSON(w, map[string]any{"error": "invalid"})
			return
		}
		if !routerTokenOK(body.HubAccessToken) {
			writeJSON(w, map[string]any{"error": "invalid"})
			return
		}
		online := false
		if shard, ok := h.FindPlayer(body.Username); ok {
			// A shard that never reported an id (pre-R1) has id 0, which can
			// never equal a real reporter's serverId (SERVER_ID defaults to 1).
			online = shardID(h, shard) != body.ServerID
		}
		writeJSON(w, map[string]any{"status": "success", "online": online})
	})

	// GET /leaderboards — served by the shard's reader over the router's DB
	// when one is configured (hub api.ts handleLeaderboards).
	if opts.Leaderboards != nil {
		mux.HandleFunc("GET /leaderboards", leaderboardHandler(opts.Leaderboards))
	} else {
		mux.HandleFunc("GET /leaderboards", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"status": "success", "list": []any{}})
		})
	}

	// POST /api/v1/requestReset + /api/v1/resetPassword — password reset
	// (hub api.ts handleRequestReset/handleResetPassword).
	setup := &api.Server{Accounts: opts.Accounts, ResetMailer: opts.ResetMailer, ResetURLBase: opts.ResetURLBase}
	mux.HandleFunc("POST /api/v1/requestReset", setup.HandleRequestReset)
	mux.HandleFunc("POST /api/v1/resetPassword", setup.HandleResetPassword)

	// "/" carries two meanings, exactly as in TS (the hub's uWS app.ws('/')
	// takes upgrades; express answers plain GET / with the liveness object): a
	// shard registering with the hub, or the hub liveness body. It cannot be a
	// route pattern — Go 1.22's "GET /{$}" would win over the fallback for
	// upgrade requests too and answer JSON to a shard (`bad handshake`).
	mux.Handle("/", rootHandler(h))
	return mux
}

// rootHandler splits the two meanings of "/": a WebSocket upgrade is a shard
// registering with the hub (hub.Server.ServeHTTP), anything else is the hub
// liveness response (hub api.ts handleRoot).
func rootHandler(h *hub.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			writeJSON(w, map[string]string{"status": RouterName + " hub is online and functional."})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// shardID resolves one shard's hub-assigned world id (0 = unknown).
func shardID(h *hub.Server, name string) int {
	for _, in := range h.ListShards() {
		if in.Name == name {
			return in.ID
		}
	}
	return 0
}

// routerTokenOK verifies the /isOnline access token (hub api.ts
// verifyRequest). hubAccessToken is required whenever the router has a token
// configured; the all-in-one dev default (HUB_TOKEN unset) stays open, matching
// the hub socket's checkAuth posture.
func routerTokenOK(supplied string) bool {
	want := hub.SharedToken()
	if want == "" {
		return true
	}
	return supplied == want
}

// leaderboardHandler serves the hub leaderboards shape from a
// leaderboard.Cache: ?skill=<id>, ?mob=<key>, ?pvp, else total experience
// (hub api.ts handleLeaderboards). Invalid params answer {error:"invalid"}.
func leaderboardHandler(p api.LeaderboardProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if raw := q.Get("skill"); raw != "" {
			id, err := strconv.Atoi(raw)
			if err != nil || !p.ValidSkill(id) {
				writeJSON(w, map[string]any{"error": "invalid"})
				return
			}
			list, err := p.SkillExperience(id)
			if err != nil {
				writeJSON(w, map[string]any{"error": "invalid"})
				return
			}
			writeJSON(w, map[string]any{"status": "success", "list": list})
			return
		}
		if key := q.Get("mob"); key != "" {
			if _, ok := p.AvailableMobs()[key]; !ok {
				writeJSON(w, map[string]any{"error": "invalid"})
				return
			}
			list, err := p.MobKills(key)
			if err != nil {
				writeJSON(w, map[string]any{"error": "invalid"})
				return
			}
			writeJSON(w, map[string]any{"status": "success", "list": list})
			return
		}
		if q.Get("pvp") != "" {
			list, err := p.PvPKills()
			if err != nil {
				writeJSON(w, map[string]any{"error": "invalid"})
				return
			}
			writeJSON(w, map[string]any{"status": "success", "list": list})
			return
		}
		list, err := p.TotalExperience()
		if err != nil {
			writeJSON(w, map[string]any{"error": "invalid"})
			return
		}
		writeJSON(w, map[string]any{
			"status": "success", "list": list, "availableMobs": p.AvailableMobs(),
		})
	}
}

// routerShardLine renders one server-list entry for the console: the
// shard name plus its login-redirect addr, drain state, load, and world
// version.
func routerShardLine(info hub.ShardInfo) string {
	addr := info.Addr
	if addr == "" {
		addr = info.Name
	}
	return fmt.Sprintf("Server %s (%s) state=%s load=%d version=%s",
		info.Name, addr, info.State, info.Load, info.Version)
}

// routerConsole applies hub console commands over the hub Server's
// server-list + presence roster (TS packages/hub/src/console.ts parity).
// `server` prints the emptiest/newest-target entry (the NewestRunning login
// target — the router's analogue of TS findEmptyServer's first server with
// space); `player <username>` prints the shard/presence entry hosting the
// name (TS findPlayer parity). "undefined" mirrors the TS
// console.log(undefined) when no shard or player matches. Read-only: it
// uses NewestRunning + FindPlayer and never mutates hub state.
type routerConsole struct{ h *hub.Server }

func (c *routerConsole) Server() string {
	if c == nil || c.h == nil {
		return "undefined"
	}
	info, ok := c.h.NewestRunning()
	if !ok {
		return "undefined"
	}
	return routerShardLine(info)
}

func (c *routerConsole) Player(username string) string {
	if c == nil || c.h == nil {
		return "undefined"
	}
	shard, ok := c.h.FindPlayer(username)
	if !ok {
		return "undefined"
	}
	return fmt.Sprintf("Player %s is on %s", username, shard)
}

// StartRouterConsole starts the router stdin console loop over h unless
// CONSOLE=0 or stdin is not a TTY — the same gate as the game StartConsole
// (pipes, tests, harnesses, CI never block on stdin). Lines feed
// console.HubExec with the routerConsole handler above.
func StartRouterConsole(h *hub.Server) {
	if ConsoleDisabled(os.Getenv("CONSOLE")) {
		log.Printf("router: console disabled (CONSOLE=%s)", os.Getenv("CONSOLE"))
		return
	}
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		log.Printf("router: console disabled (stdin not a TTY)")
		return
	}
	c := &routerConsole{h: h}
	go func() {
		log.Printf("router: console ready (slash commands)")
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if out := console.HubExec(c, sc.Text()); out != "" {
				log.Printf("console: %s", out)
			}
		}
		if err := sc.Err(); err != nil {
			log.Printf("router: console ended: %v", err)
		}
	}()
}

// RouterDrainGrace is the router's SIGTERM grace: it holds no sim state,
// so DRAINING only needs to stay observable on /healthz (503) long enough
// for a balancer scrape before the process exits. Game/shard instances use
// DRAIN_TIMEOUT instead (empty-or-timeout with the sim running).
const RouterDrainGrace = 5 * time.Second

// awaitRouterDrain observes SIGTERM/SIGINT for the router: DRAINING (health
// flips to 503, shards keep heartbeating) for RouterDrainGrace, then exit.
// A second signal exits immediately.
func awaitRouterDrain(lc *Lifecycle) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
	lc.BeginDraining()
	log.Printf("router: shutdown signal -> DRAINING (no new routing)")
	select {
	case <-ch:
	case <-time.After(RouterDrainGrace):
	}
	lc.MarkShutdown()
	log.Printf("router: SHUTDOWN")
	os.Exit(0)
}

// CheckHubTokenForRole enforces the fail-closed posture for multi-server
// roles: ROLE=router/shard require HUB_TOKEN (see hub.TokenRequiredForRole).
// The all-in-one dev default stays open (empty token allowed, documented).
// Callers surface the returned error instead of serving unauthenticated.
func CheckHubTokenForRole(role string, token string) error {
	if hub.TokenRequiredForRole(role) && strings.TrimSpace(token) == "" {
		return fmt.Errorf("hub: HUB_TOKEN required for ROLE=%s (fail-closed; all-in-one dev default stays open)", role)
	}
	return nil
}

// EnvHubDB selects the SQLite file the router reads for the DB-backed client
// endpoints (GET /leaderboards and the password-reset pair). Unset falls back to
// DB_PATH, then "data.db" — the shard's own file, which is what a single-host
// deployment wants.
const EnvHubDB = "HUB_DB"

// RouterDBPath resolves EnvHubDB, then cfg.DBPath, then the shard default.
func RouterDBPath(cfg Config) string {
	if v := strings.TrimSpace(os.Getenv(EnvHubDB)); v != "" {
		return v
	}
	if strings.TrimSpace(cfg.DBPath) != "" {
		return cfg.DBPath
	}
	return "data.db"
}

// routerOptionsFor opens the account/leaderboard surfaces for the router.
//
// Divergence from TS, documented: the TS hub owns MongoDB, so its
// /leaderboards and reset endpoints cover every world. Here SQLite is per-shard
// (docs/DEPLOY.md: "one single-writer owner per file"), so the router answers
// for exactly the file it is pointed at — set HUB_DB to the shard you want
// covered. Without any reachable DB the two surfaces degrade to the TS
// 'unavailable' shapes ({error:"invalid"} on reset, an empty leaderboard) and
// the router logs why, once, at boot.
func routerOptionsFor(cfg Config) RouterOptions {
	opts := RouterOptions{MaxPlayers: RouterMaxPlayers()}
	path := RouterDBPath(cfg)
	store, err := persist.Open(path)
	if err != nil {
		log.Printf("router: %s=%s unavailable (%v) — /leaderboards and password reset disabled", EnvHubDB, path, err)
		return opts
	}
	db := store.DB()
	if db == nil {
		log.Printf("router: %s=%s has no handle — /leaderboards and password reset disabled", EnvHubDB, path)
		return opts
	}
	// The router shares the file with the shard that owns it (WAL, one writer at
	// a time). The store holds a single pooled connection, so this pragma sticks
	// and a concurrent account write waits instead of failing with SQLITE_BUSY.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		log.Printf("router: %s=%s busy_timeout failed (%v)", EnvHubDB, path, err)
	}
	account.EnsureTables(db)
	opts.Accounts = account.Provider{DB: db}
	opts.Leaderboards = routerLeaderboards{
		cache: leaderboard.New(
			&leaderboard.DBSkillSource{DB: db},
			&leaderboard.DBStatsSource{DB: db},
			routerMobNames(),
		),
	}
	log.Printf("router: %s=%s wired (leaderboards + password reset)", EnvHubDB, path)
	return opts
}

// routerMobNames loads the mob key -> display name dictionary from mobs.json
// (embedded or filesystem) for /leaderboards ?mob=<key> validation. An empty
// map only narrows which mob keys are accepted, exactly like app.loadMobNames.
func routerMobNames() map[string]string {
	raw, err := data.ReadFile("mobs.json")
	if err != nil {
		log.Printf("router: leaderboards: load mobs.json: %v", err)
		return map[string]string{}
	}
	var mobs map[string]struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &mobs); err != nil {
		log.Printf("router: leaderboards: parse mobs.json: %v", err)
		return map[string]string{}
	}
	out := make(map[string]string, len(mobs))
	for k, v := range mobs {
		out[k] = v.Name
	}
	return out
}

// routerLeaderboards adapts leaderboard.Cache to api.LeaderboardProvider
// (same adapter shape as app.opsLeaderboards, which the router cannot import
// from here without a cycle).
type routerLeaderboards struct{ cache *leaderboard.Cache }

func (o routerLeaderboards) TotalExperience() ([]api.LeaderboardEntry, error) {
	rows, err := o.cache.GetTotalExperience()
	if err != nil {
		return nil, err
	}
	out := make([]api.LeaderboardEntry, len(rows))
	for i, d := range rows {
		out[i] = api.LeaderboardEntry{Username: d.Username, TotalXP: d.TotalExperience}
	}
	return out, nil
}

func (o routerLeaderboards) SkillExperience(id int) ([]api.LeaderboardEntry, error) {
	rows, err := o.cache.GetSkillExperience(id)
	if err != nil {
		return nil, err
	}
	out := make([]api.LeaderboardEntry, len(rows))
	for i, d := range rows {
		out[i] = api.LeaderboardEntry{Username: d.Username, XP: d.Experience}
	}
	return out, nil
}

func (o routerLeaderboards) MobKills(key string) ([]api.LeaderboardEntry, error) {
	rows, err := o.cache.GetMobKills(key)
	if err != nil {
		return nil, err
	}
	out := make([]api.LeaderboardEntry, len(rows))
	for i, d := range rows {
		out[i] = api.LeaderboardEntry{Username: d.Username, Kills: d.Kills}
	}
	return out, nil
}

func (o routerLeaderboards) PvPKills() ([]api.LeaderboardEntry, error) {
	rows, err := o.cache.GetPvPData()
	if err != nil {
		return nil, err
	}
	out := make([]api.LeaderboardEntry, len(rows))
	for i, d := range rows {
		out[i] = api.LeaderboardEntry{Username: d.Username, PvPKills: d.PvPKills}
	}
	return out, nil
}

func (o routerLeaderboards) AvailableMobs() map[string]string { return o.cache.AvailableMobs() }

// ValidSkill mirrors the TS bound (0..SkillAlchemy(18)).
func (o routerLeaderboards) ValidSkill(id int) bool { return id >= 0 && id <= 18 }

// RunRouter serves the hub server-list until the listener fails or SIGTERM
// completes the drain. It runs no sim: SIGTERM observes DRAINING on
// /healthz, then the process exits (see awaitRouterDrain above).
// Fail-closed: without HUB_TOKEN the router refuses to serve (see
// CheckHubTokenForRole); the all-in-one dev default stays open by design.
func RunRouter(cfg Config) error {
	if err := CheckHubTokenForRole(RoleRouter, hub.SharedToken()); err != nil {
		return err
	}
	addr := RouterAddr()
	h := hub.NewServer(hub.SharedToken(), nil)
	h.StartSweeper(context.Background())
	StartRouterConsole(h)
	lc := Default
	lc.SetLoad(0)
	go awaitRouterDrain(lc)
	opts := routerOptionsFor(cfg)
	log.Printf("router: hub listening on %s (buildID=%s gVer=%s)", addr, version.BuildID, version.GVer)
	return http.ListenAndServe(addr, RouterHandlerWith(h, lc, opts))
}
