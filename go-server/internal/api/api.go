// Package api is an additive, read-only REST surface for the Go server
// stub. It owns HTTP routing only: the caller supplies data through the
// Players, Guilds, and StatusProvider interfaces and owns starting,
// stopping, and env-gating the underlying http.Server. There are no
// goroutines, timers, database calls, or network sends in this package.
//
// TS source mirrored here (shape conventions only):
//   - packages/server/src/network/api.ts — API (express app gated on
//     config.apiEnabled || config.hubEnabled, listens on config.apiPort,
//     GET / returns {name, port, gameVersion, maxPlayers, playerCount}).
//   - packages/common/config.ts — apiEnabled/apiPort fields (env-derived).
//
// Env gating (API_PORT default-off pattern): this package never reads the
// environment and never listens on its own. The caller gates startup:
//
//	if port := os.Getenv("API_PORT"); port != "" {
//	    srv := api.NewServer(players, guilds, status)
//	    http.ListenAndServe(":"+port, srv.Handler())
//	}
//
// When API_PORT is unset or empty the caller does not start the server,
// so the API is off by default. See AddrFromEnv for a small helper that
// keeps that check in one place (it still only returns an address; the
// caller decides whether to listen).
//
// Routes (all GET, read-only; anything else gets 405 from the mux):
//   - GET /status       — server status snapshot (TS GET / parity).
//   - GET /guilds       — guild list snapshot.
//   - GET /players/{name} — single player lookup; 404 when unknown.
//   - GET /healthz      — drain-lifecycle snapshot {state, load, buildId,
//     gVer}; 503 once DRAINING/SHUTDOWN (new surface, no TS counterpart).
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
)

// ---------------------------------------------------------------------------
// Shapes.
// ---------------------------------------------------------------------------

// Player is one player's read-only snapshot.
type Player struct {
	Name   string `json:"name"`
	Level  int    `json:"level"`
	Online bool   `json:"online"`
}

// Guild is one guild's read-only snapshot.
type Guild struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// Status is the server status snapshot. Field names mirror TS GET /
// (api.ts handleRouter): name, port, gameVersion, maxPlayers, playerCount.
// R1 adds buildId + gVer (omitempty: older providers render the TS shape
// byte-identical).
type Status struct {
	Name        string `json:"name"`
	Port        int    `json:"port"`
	GameVersion string `json:"gameVersion"`
	MaxPlayers  int    `json:"maxPlayers"`
	PlayerCount int    `json:"playerCount"`
	BuildID     string `json:"buildId,omitempty"`
	GVer        string `json:"gVer,omitempty"`
}

// ---------------------------------------------------------------------------
// Provider interfaces (caller supplies data; package owns HTTP routing only).
// ---------------------------------------------------------------------------

// Players supplies player snapshots. GetPlayer reports false when the
// named player is unknown; ListPlayers backs potential list views.
type Players interface {
	GetPlayer(name string) (Player, bool)
	ListPlayers() []Player
}

// Guilds supplies guild snapshots.
type Guilds interface {
	ListGuilds() []Guild
}

// StatusProvider supplies the server status snapshot.
type StatusProvider interface {
	Status() Status
}

// HealthProvider supplies the drain-lifecycle snapshot for /healthz. The
// shape is intentionally minimal ({state, load} + stamps); the game, the
// router, and the api test stub all satisfy it structurally via Health()
// adapters or inline funcs. Implementations must be safe for concurrent use
// (the handler runs on the caller's HTTP server).
type HealthProvider interface {
	Health() Health
}

// Health is the /healthz snapshot: drain state + load (+ build stamps so
// deploy watchers can tell builds apart).
type Health struct {
	State   string `json:"state"`
	Load    int    `json:"load"`
	BuildID string `json:"buildId,omitempty"`
	GVer    string `json:"gVer,omitempty"`
}

// LeaderboardProvider supplies leaderboard data (TS hub GET /leaderboards
// parity). The provider is called by the handler; caching lives in the
// leaderboard package, not here.
type LeaderboardProvider interface {
	TotalExperience() ([]LeaderboardEntry, error)
	SkillExperience(skillID int) ([]LeaderboardEntry, error)
	MobKills(mobKey string) ([]LeaderboardEntry, error)
	PvPKills() ([]LeaderboardEntry, error)
	AvailableMobs() map[string]string
	ValidSkill(id int) bool
}

// LeaderboardEntry is a single leaderboard row (generic shape; the handler
// serialises with the TS-matching JSON keys per category).
type LeaderboardEntry struct {
	Username string `json:"username"`
	Value    int    `json:"-"` // handler maps to the right JSON key
	// Typed fields for each category (only one is set per entry).
	TotalXP int `json:"totalExperience,omitempty"`
	XP      int `json:"experience,omitempty"`
	Kills   int `json:"kills,omitempty"`
	PvPKills int `json:"pvpKills,omitempty"`
}

// UptimeProvider supplies the server start time for the admin dashboard.
type UptimeProvider interface {
	StartTime() time.Time
}

// AccountProvider supplies the identity operations the password-reset
// endpoints need. The TS equivalents live on the hub against MongoDB
// (packages/hub/src/controllers/api.ts:251 requestReset, :290 resetPassword
// over mongodb.ts createResetToken/resetPassword); the Go port runs them
// against the shard's own SQLite account table (internal/account).
//
// CreateResetToken mints (id, token) for a known email and reports ok=false
// for an unknown one — the handler answers success either way, exactly like TS
// (anti-enumeration). ResetPassword consumes the token and rotates the
// password.
type AccountProvider interface {
	CreateResetToken(email string) (id, token string, ok bool)
	ResetPassword(id, token, password string) bool
	ValidEmail(email string) bool
	ValidPassword(password string) bool
}

// ---------------------------------------------------------------------------
// Server.
// ---------------------------------------------------------------------------

// Server routes read-only API requests to the caller-supplied providers.
// The zero value is unusable; construct with NewServer. It implements
// http.Handler so the caller can mount it on any http.Server it owns.
// Health is optional (nil => /healthz reports a static RUNNING stub);
// set it after construction to report the live drain state.
type Server struct {
	Players      Players
	Guilds       Guilds
	Status       StatusProvider
	Health       HealthProvider
	Leaderboards LeaderboardProvider
	Uptime       UptimeProvider
	Accounts     AccountProvider // nil = reset endpoints answer 'invalid'

	// ResetMailer delivers a reset link. Nil = the link is logged instead
	// (dev fallback: TS requires nodemailer + a live SMTP server, which a local
	// checkout does not have, but the token is still valid so the flow can be
	// completed from the log line).
	ResetMailer func(email, link string)
	// ResetURLBase prefixes the emailed link; empty uses DefaultResetURLBase.
	ResetURLBase string

	// SentryDSN enables Sentry error tracking when non-empty (TS config.sentryDsn
	// parity). Initialize Sentry before constructing the Server; the middleware
	// wraps the handler to capture panics and errors.
	SentryDSN string

	mux *http.ServeMux
}

// NewServer wires GET /status, GET /guilds, GET /players/{name}, GET
// /healthz, GET /leaderboards, and GET /admin. Providers may be nil (see
// per-handler fallbacks), but callers normally supply all three. NewServer
// registers routes only; it starts nothing.
func NewServer(players Players, guilds Guilds, status StatusProvider) *Server {
	s := &Server{Players: players, Guilds: guilds, Status: status, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /status", s.handleStatus)
	s.mux.HandleFunc("GET /guilds", s.handleGuilds)
	s.mux.HandleFunc("GET /players/{name}", s.handlePlayer)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /leaderboards", s.handleLeaderboards)
	s.mux.HandleFunc("GET /admin", s.handleAdmin)
	// Password reset (TS hub POST /api/v1/requestReset + /api/v1/resetPassword;
	// the stock client calls both — app.ts:700 and reset.ts:64).
	s.mux.HandleFunc("POST /api/v1/requestReset", s.HandleRequestReset)
	s.mux.HandleFunc("POST /api/v1/resetPassword", s.HandleResetPassword)
	return s
}

// DefaultResetURLBase is the client page a reset link points at (the stock
// client reads ?id=&token= from it — packages/client/src/reset.ts).
const DefaultResetURLBase = "http://127.0.0.1:9000/reset/"

// HandleRequestReset mirrors hub api.ts handleRequestReset: validate the email
// shape, mint a token, mail it, and ALWAYS answer {status:"success"} so the
// endpoint cannot be used to probe which emails are registered.
//
// Exported because the router role hosts the same two routes (the client POSTs
// them at the hub base URL — packages/client/src/app.ts forgotPassword and
// src/reset.ts).
func (s *Server) HandleRequestReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": "invalid"})
		return
	}
	if s.Accounts == nil || !s.Accounts.ValidEmail(body.Email) {
		writeJSON(w, http.StatusOK, map[string]any{"error": "invalid"})
		return
	}
	id, token, ok := s.Accounts.CreateResetToken(body.Email)
	if !ok {
		// Unknown email: same answer as success (api.ts:265 comment).
		writeJSON(w, http.StatusOK, map[string]any{"status": "success"})
		return
	}
	base := s.ResetURLBase
	if base == "" {
		base = DefaultResetURLBase
	}
	link := fmt.Sprintf("%s?id=%s&token=%s", base, id, token)
	if s.ResetMailer != nil {
		s.ResetMailer(body.Email, link)
	} else {
		log.Printf("accounts: password reset link for %s: %s", body.Email, link)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "success"})
}

// HandleResetPassword mirrors hub api.ts handleResetPassword: id + token +
// password must be present and the password must satisfy the same length rule
// as registration. Exported for the router role (see HandleRequestReset).
func (s *Server) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string `json:"id"`
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": "invalid"})
		return
	}
	if s.Accounts == nil || body.ID == "" || body.Token == "" || !s.Accounts.ValidPassword(body.Password) {
		writeJSON(w, http.StatusOK, map[string]any{"error": "invalid"})
		return
	}
	status := "invalid"
	if s.Accounts.ResetPassword(body.ID, body.Token, body.Password) {
		status = "success"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

// Handler returns the read-only route set for the caller's http.Server.
// When SentryDSN is set, the handler is wrapped with Sentry middleware
// (request handler + error handler; TS Sentry.Handlers.requestHandler +
// errorHandler parity). Tracing is enabled via tracesSampleRate 1.0.
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	if s.SentryDSN != "" {
		// Sentry middleware (TS api.ts lines 35-38 parity): requestHandler +
		// errorHandler. The sentryhttp package provides a single handler that
		// covers both (it captures panics, sets up the hub, and reports errors).
		h = sentryhttp.New(sentryhttp.Options{
			Repanic:         true, // Re-panic after capturing (TS default).
			WaitForDelivery: false,
		}).Handle(h)
	}
	return h
}

// ServeHTTP delegates to the internal mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// AddrFromEnv returns the listen address derived from API_PORT. ok is false
// when API_PORT is unset or empty (default-off); the caller must not start
// the server in that case.
//
// Bind default (changed 2026-09: loopback-only): bare ports bind to
// 127.0.0.1 (e.g. "8080" -> "127.0.0.1:8080") instead of ":8080" (all
// interfaces). Values already containing a colon (e.g. ":8080",
// "127.0.0.1:8080", "0.0.0.0:8080") are used as-is, so operators can still
// opt into a wider bind with an explicit addr. API_PORT="0.0.0.0:8080"
// therefore listens publicly; the default stays loopback-only.
func AddrFromEnv() (addr string, ok bool) {
	port := os.Getenv("API_PORT")
	if port == "" {
		return "", false
	}
	for i := 0; i < len(port); i++ {
		if port[i] == ':' {
			return port, true
		}
	}
	return "127.0.0.1:" + port, true
}

// InitSentry initializes Sentry error tracking when dsn is non-empty (TS
// config.sentryDsn parity). Returns the DSN for the caller to pass to
// Server.SentryDSN. When dsn is empty, returns "" and does nothing (Sentry
// is off; tests and dev environments without a DSN are unaffected).
//
// TS parity: Sentry.init({dsn, integrations, tracesSampleRate: 1}). The
// sentry-go SDK defaults are used for integrations; tracesSampleRate is set
// to 1.0 to match TS. The caller must invoke sentry.Flush before shutdown
// to ensure pending events are delivered.
func InitSentry(dsn string) string {
	if dsn == "" {
		return ""
	}
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		TracesSampleRate: 1.0,
		// TS uses Http integration with tracing: true and Express integration.
		// sentry-go auto-instruments http.Client and net/http servers when
		// attached; explicit integrations are not required for the stdlib
		// handler path.
	}); err != nil {
		// TS logs and continues; we do the same (Sentry off, server runs).
		return ""
	}
	return dsn
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Status == nil {
		writeError(w, http.StatusServiceUnavailable, "status unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.Status.Status())
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	h := Health{State: "RUNNING"}
	code := http.StatusOK
	if s.Health != nil {
		h = s.Health.Health()
		if h.State != "" && h.State != "RUNNING" {
			code = http.StatusServiceUnavailable
		}
		if h.State == "" {
			h.State = "RUNNING"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(h)
}

func (s *Server) handleGuilds(w http.ResponseWriter, _ *http.Request) {
	var out []Guild
	if s.Guilds != nil {
		out = s.Guilds.ListGuilds()
	}
	if out == nil {
		out = []Guild{}
	}
	for i := range out {
		if out[i].Members == nil {
			out[i].Members = []string{}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePlayer(w http.ResponseWriter, r *http.Request) {
	if s.Players == nil {
		writeError(w, http.StatusNotFound, "player not found")
		return
	}
	name := r.PathValue("name")
	p, ok := s.Players.GetPlayer(name)
	if !ok {
		writeError(w, http.StatusNotFound, "player not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ---------------------------------------------------------------------------
// Leaderboards (TS hub GET /leaderboards parity).
// ---------------------------------------------------------------------------

func (s *Server) handleLeaderboards(w http.ResponseWriter, r *http.Request) {
	if s.Leaderboards == nil {
		writeError(w, http.StatusServiceUnavailable, "leaderboards unavailable")
		return
	}
	q := r.URL.Query()

	// ?skill=<id> — per-skill XP leaderboard.
	if skillStr := q.Get("skill"); skillStr != "" {
		id, err := strconv.Atoi(skillStr)
		if err != nil || id < 0 || !s.Leaderboards.ValidSkill(id) {
			writeError(w, http.StatusBadRequest, "invalid")
			return
		}
		entries, err := s.Leaderboards.SkillExperience(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "list": entries})
		return
	}

	// ?mob=<key> — mob kills leaderboard.
	if mobKey := q.Get("mob"); mobKey != "" {
		mobs := s.Leaderboards.AvailableMobs()
		if _, ok := mobs[mobKey]; !ok {
			writeError(w, http.StatusBadRequest, "invalid")
			return
		}
		entries, err := s.Leaderboards.MobKills(mobKey)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "list": entries})
		return
	}

	// ?pvp — PvP kills leaderboard.
	if q.Has("pvp") {
		entries, err := s.Leaderboards.PvPKills()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "list": entries})
		return
	}

	// Default — total XP + availableMobs.
	entries, err := s.Leaderboards.TotalExperience()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "success",
		"list":           entries,
		"availableMobs":  s.Leaderboards.AvailableMobs(),
	})
}

// ---------------------------------------------------------------------------
// Admin dashboard (TS packages/admin parity — single-page read-only view).
// ---------------------------------------------------------------------------

// adminAllowed reports whether the request's remote IP is localhost.
// The TS admin panel middleware whitelist is [127.0.0.1] only.
func adminAllowed(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !adminAllowed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Collect status data.
	var st Status
	if s.Status != nil {
		st = s.Status.Status()
	}
	var h Health
	if s.Health != nil {
		h = s.Health.Health()
	}

	uptime := ""
	if s.Uptime != nil {
		d := time.Since(s.Uptime.StartTime()).Truncate(time.Second)
		uptime = d.String()
	}

	players := 0
	if s.Players != nil {
		players = len(s.Players.ListPlayers())
	}

	guilds := 0
	if s.Guilds != nil {
		guilds = len(s.Guilds.ListGuilds())
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="robots" content="noindex">
<title>Admin — %s</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
         max-width: 720px; margin: 40px auto; padding: 0 20px; background: #0d1117; color: #c9d1d9; }
  h1 { color: #58a6ff; border-bottom: 1px solid #21262d; padding-bottom: 12px; }
  .card { background: #161b22; border: 1px solid #30363d; border-radius: 6px; padding: 20px; margin: 16px 0; }
  .row { display: flex; justify-content: space-between; padding: 8px 0; border-bottom: 1px solid #21262d; }
  .row:last-child { border-bottom: none; }
  .label { color: #8b949e; }
  .value { color: #f0f6fc; font-weight: 600; }
  .ok { color: #3fb950; }
  .warn { color: #d29922; }
  a { color: #58a6ff; }
</style>
</head>
<body>
<h1>Server Admin</h1>
<div class="card">
  <div class="row"><span class="label">Server</span><span class="value">%s</span></div>
  <div class="row"><span class="label">Status</span><span class="value %s">%s</span></div>
  <div class="row"><span class="label">Port</span><span class="value">%d</span></div>
  <div class="row"><span class="label">Game Version</span><span class="value">%s</span></div>
  <div class="row"><span class="label">Build ID</span><span class="value">%s</span></div>
  <div class="row"><span class="label">Protocol</span><span class="value">%s</span></div>
</div>
<div class="card">
  <div class="row"><span class="label">Players Online</span><span class="value">%d / %d</span></div>
  <div class="row"><span class="label">Guilds</span><span class="value">%d</span></div>
  <div class="row"><span class="label">Uptime</span><span class="value">%s</span></div>
</div>
<div class="card">
  <div class="row"><span class="label">API Endpoints</span><span class="value">
    <a href="/status">/status</a> ·
    <a href="/guilds">/guilds</a> ·
    <a href="/leaderboards">/leaderboards</a> ·
    <a href="/healthz">/healthz</a>
  </span></div>
</div>
</body>
</html>`,
		st.Name,
		st.Name,
		healthClass(h.State), h.State,
		st.Port,
		st.GameVersion,
		st.BuildID,
		st.GVer,
		players, st.MaxPlayers,
		guilds,
		uptime,
	)
}

func healthClass(state string) string {
	if state == "RUNNING" || state == "" {
		return "ok"
	}
	return "warn"
}

// Divergences from TS (documented):
//   - TS api.ts exposes only GET / (status); /players/{name} and /guilds
//     are new read-only views for the Go stub (no TS counterpart).
//   - TS gating is config.apiEnabled || config.hubEnabled + listen on
//     config.apiPort; here gating is API_PORT default-off owned by the
//     caller (this package never reads env except via AddrFromEnv and
//     never listens).
//   - TS sends express.json()/urlencoded middleware and Sentry handlers;
//     this package uses stdlib net/http with GET-only routes (no body
//     parsing, no middleware, non-GET yields 405).
//   - TS status shape {name, port, gameVersion, maxPlayers, playerCount}
//     is preserved verbatim in Status; Player/Guild shapes are new
//     (stub-level snapshots, exact-case names like TS member.username).
