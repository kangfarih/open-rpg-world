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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"rpg-world-server/internal/console"
	"rpg-world-server/internal/hub"
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
	if lc == nil {
		lc = Default
	}
	mux := http.NewServeMux()
	warm := NewWarmTracker()
	mux.HandleFunc("GET /servers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
		_ = json.NewEncoder(w).Encode(BuildServerListForLogin(h, key, CanaryPct(), warm, now))
	})
	mux.HandleFunc("GET /healthz", lc.ServeHealth)
	mux.Handle("/", h)
	return mux
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
	log.Printf("router: hub listening on %s (buildID=%s gVer=%s)", addr, version.BuildID, version.GVer)
	return http.ListenAndServe(addr, RouterHandler(h, lc))
}
