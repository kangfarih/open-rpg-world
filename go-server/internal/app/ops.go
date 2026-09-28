// Ops serving (API + console), extracted behavior-frozen from the root
// ops_wire.go adapter (task D2b item 2: ops_wire.go -> internal/app).
//
//   - API: a Server is started on API_PORT only when API_PORT is set
//     (default off). Providers read live state (players count, guild list,
//     status). A busy port never blocks game boot (log + continue).
//   - Console: a stdin bufio loop in a goroutine feeds console.Exec with a
//     Handler over the live world. Disabled when stdin is not a TTY or when
//     CONSOLE=0 (tests/harness must not hang on stdin).
//   - Limits/accept gate: owned by internal/net (Hub: per-IP cap at accept,
//     per-message budget in the read path, chat bucket in the m7 path, IP
//     bans, update-mode gate). The console commands below drive the Hub
//     through the Ban seams; per-conn state is forgotten on disconnect
//     by the Hub release path (D2a: no transport globals stay in root).
//
// With no env set the API never starts, the console never starts, and the
// limiter runs at the same budgets the server already enforced (16/IP,
// 300 msg/s, chat 3 burst @ 0.5/s), so existing behavior is unchanged.
//
// Everything live-world related stays with the root adapter and is reached
// only through OpsDeps. Output strings mirror the TS console log.info
// lines. All log strings are kept verbatim.
package app

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"rpg-world-server/internal/api"
	"rpg-world-server/internal/console"
	"rpg-world-server/internal/data"
	"rpg-world-server/internal/leaderboard"
	"rpg-world-server/internal/social"
	"rpg-world-server/internal/version"
)

// GameVersion / MaxPlayers feed the API status snapshot (the stub has
// no config file; the handshake gVer is client-supplied so there is no
// server constant to reuse).
const (
	GameVersion = "1.0.0"
	MaxPlayers  = 200
)

// OpsDeps bundles the ops seams (implemented by the root adapter; never by
// this package).
type OpsDeps struct {
	// Port is the resolved game port for the status snapshot.
	Port int
	// ConsoleOff disables the stdin console (CONSOLE=0/false/off/no).
	ConsoleOff bool
	// Usernames snapshots online usernames (m7PlayerUsernames parity).
	Usernames func() []string
	// LookupPlayer resolves a username (case-insensitive) to its level
	// (opsPlayerLevel parity: stored level when positive, else 1).
	LookupPlayer func(name string) (username string, level int, ok bool)
	// PlayerCount counts live connections (worldcore.PlayerCount parity).
	PlayerCount func() int
	// CountPlayers counts persisted player rows (console Total parity).
	// HaveDB=false renders "Total players: 0" without touching the DB.
	HaveDB       func() bool
	CountPlayers func() (int, error)
	// BeginUpdate disables accepts and drops every connection, reporting
	// the dropped count (console Update parity).
	BeginUpdate func() int
	// KillPlayer kills the player, reporting the display name
	// (console Kill parity: m9DamagePlayer full-HP kill).
	KillPlayer func(username string) (display string, ok bool)
	// DropPlayer disconnects the player, reporting the display name
	// (console Kick/Timeout parity).
	DropPlayer func(username string) (display string, ok bool)
	// SetPlayerRank sets the rank, reporting the display name
	// (console SetAdmin/SetMod parity).
	SetPlayerRank func(username string, rank int) (display string, ok bool)
	// StripPlayerRank strips the rank back to None, reporting the display
	// name (console RemoveAdmin/RemoveMod parity: TS removeadmin/removemod
	// do player.setRank() + target notify + sync; offline yields
	// 'Player is not logged in.').
	StripPlayerRank func(username string) (display string, ok bool)
	// AdminRank/ModeratorRank are the Modules.Ranks values for SetAdmin/SetMod.
	AdminRank     int
	ModeratorRank int
	// BannedIPs snapshots the IP ban set (console `ipban list` parity).
	BannedIPs func() []string
	// BanIP bans the IP and drops its connections, reporting the dropped
	// count (console IPBan parity).
	BanIP func(ip string) int
	// UnbanIP clears the IP ban without dropping connections (console
	// UnbanIP parity: TS unbanip is database.setIpBan(ip, false); same-IP
	// conns stay connected).
	UnbanIP func(ip string)
	// SaveWorld flushes dirty player rows (console Save parity).
	SaveWorld func()
	// GetDB returns the persist *sql.DB handle for leaderboard queries.
	// nil means no database is available (leaderboards return empty).
	GetDB func() *sql.DB
	// StartTime returns the server boot time for uptime display.
	StartTime func() time.Time
	// Accounts serves the password-reset endpoints (hub
	// /api/v1/requestReset + /api/v1/resetPassword parity). nil disables
	// them (the endpoints answer {error:"invalid"}).
	Accounts api.AccountProvider
	// ResetMailer delivers a reset link when one is minted. nil logs the
	// link instead (dev fallback; the token is still valid).
	ResetMailer func(email, link string)
}

var opsDeps OpsDeps

// ConfigureOps installs the ops seams (called once from the root boot,
// before the API/console start).
func ConfigureOps(d OpsDeps) { opsDeps = d }

// StartAPI starts the read-only REST surface when API_PORT is set
// (default off). A busy port logs and the game boots without the API.
// Sentry is initialized when SENTRY_DSN is set (TS config.sentryDsn parity).
// Leaderboards are wired when the persist DB is available (GetDB != nil).
func StartAPI() {
	addr, ok := api.AddrFromEnv()
	if !ok {
		return
	}
	// Sentry init (TS api.ts lines 51-61 parity): opt-in via SENTRY_DSN env.
	// Empty DSN = disabled (tests, dev environments). InitSentry returns the
	// DSN on success; we pass it to the Server so the handler wraps with
	// Sentry middleware.
	dsn := api.InitSentry(os.Getenv("SENTRY_DSN"))
	srv := api.NewServer(opsPlayers{}, opsGuilds{}, opsStatus{})
	srv.Health = opsHealth{lc: Default}
	srv.SentryDSN = dsn

	// Leaderboards (TS hub GET /leaderboards parity): wire when DB available.
	if opsDeps.GetDB != nil {
		if db := opsDeps.GetDB(); db != nil {
			mobs := loadMobNames()
			skills := &leaderboard.DBSkillSource{DB: db}
			stats := &leaderboard.DBStatsSource{DB: db}
			srv.Leaderboards = opsLeaderboards{cache: leaderboard.New(skills, stats, mobs)}
			log.Printf("ops: leaderboards enabled (%d mobs)", len(mobs))
		}
	}

	// Uptime (admin dashboard).
	srv.Uptime = opsUptime{}

	// Password reset (hub /api/v1/* parity): available whenever the account
	// store is wired, which in the shard role means the same SQLite handle
	// the login path uses.
	srv.Accounts = opsDeps.Accounts
	srv.ResetMailer = opsDeps.ResetMailer

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("ops: api listen %s failed (%v), continuing without API", addr, err)
		return
	}
	go func() {
		log.Printf("ops: api listening on %s", ln.Addr())
		if err := http.Serve(ln, srv.Handler()); err != nil && err != http.ErrServerClosed {
			log.Printf("ops: api stopped: %v", err)
		}
	}()
}

// opsPlayers serves live player snapshots to the API.
type opsPlayers struct{}

func (opsPlayers) ListPlayers() []api.Player {
	names := opsDeps.Usernames()
	out := make([]api.Player, 0, len(names))
	for _, n := range names {
		_, level, _ := opsDeps.LookupPlayer(n)
		out = append(out, api.Player{Name: n, Level: level, Online: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (opsPlayers) GetPlayer(name string) (api.Player, bool) {
	username, level, ok := opsDeps.LookupPlayer(name)
	if !ok {
		return api.Player{}, false
	}
	return api.Player{Name: username, Level: level, Online: true}, true
}

// opsGuilds serves the live guild list to the API.
type opsGuilds struct{}

func (opsGuilds) ListGuilds() []api.Guild {
	ids := social.GuildIDs()
	out := make([]api.Guild, 0, len(ids))
	for _, id := range ids {
		g, err := social.GuildByID(id)
		if err != nil {
			continue
		}
		members := make([]string, 0, len(g.Members))
		for u := range g.Members {
			members = append(members, u)
		}
		sort.Strings(members)
		out = append(out, api.Guild{Name: g.Name, Members: members})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// opsStatus serves the server status snapshot to the API.
type opsStatus struct{}

func (opsStatus) Status() api.Status {
	return api.Status{
		Name:        "kaetram-stub",
		Port:        opsDeps.Port,
		GameVersion: GameVersion,
		MaxPlayers:  MaxPlayers,
		PlayerCount: opsDeps.PlayerCount(),
		BuildID:     version.BuildID,
		GVer:        version.GVer,
	}
}

// opsHealth adapts the process lifecycle to the api /healthz provider
// (state from the lifecycle, live load from the game when wired).
type opsHealth struct{ lc *Lifecycle }

func (h opsHealth) Health() api.Health {
	lc := h.lc
	if lc == nil {
		lc = Default
	}
	load := 0
	if opsDeps.PlayerCount != nil {
		load = opsDeps.PlayerCount()
	}
	s := lc.Health()
	s.Load = load
	return api.Health{State: s.State, Load: s.Load, BuildID: s.BuildID, GVer: s.GVer}
}

// opsLeaderboards adapts the leaderboard.Cache to the api.LeaderboardProvider
// interface (TS hub GET /leaderboards parity).
type opsLeaderboards struct{ cache *leaderboard.Cache }

func (o opsLeaderboards) TotalExperience() ([]api.LeaderboardEntry, error) {
	data, err := o.cache.GetTotalExperience()
	if err != nil {
		return nil, err
	}
	out := make([]api.LeaderboardEntry, len(data))
	for i, d := range data {
		out[i] = api.LeaderboardEntry{Username: d.Username, TotalXP: d.TotalExperience}
	}
	return out, nil
}

func (o opsLeaderboards) SkillExperience(id int) ([]api.LeaderboardEntry, error) {
	data, err := o.cache.GetSkillExperience(id)
	if err != nil {
		return nil, err
	}
	out := make([]api.LeaderboardEntry, len(data))
	for i, d := range data {
		out[i] = api.LeaderboardEntry{Username: d.Username, XP: d.Experience}
	}
	return out, nil
}

func (o opsLeaderboards) MobKills(key string) ([]api.LeaderboardEntry, error) {
	data, err := o.cache.GetMobKills(key)
	if err != nil {
		return nil, err
	}
	out := make([]api.LeaderboardEntry, len(data))
	for i, d := range data {
		out[i] = api.LeaderboardEntry{Username: d.Username, Kills: d.Kills}
	}
	return out, nil
}

func (o opsLeaderboards) PvPKills() ([]api.LeaderboardEntry, error) {
	data, err := o.cache.GetPvPData()
	if err != nil {
		return nil, err
	}
	out := make([]api.LeaderboardEntry, len(data))
	for i, d := range data {
		out[i] = api.LeaderboardEntry{Username: d.Username, PvPKills: d.PvPKills}
	}
	return out, nil
}

func (o opsLeaderboards) AvailableMobs() map[string]string {
	return o.cache.AvailableMobs()
}

func (o opsLeaderboards) ValidSkill(id int) bool {
	return id >= 0 && id <= 18 // SkillLumberjacking(0)..SkillAlchemy(18)
}

// opsUptime adapts the boot time to the api.UptimeProvider interface.
type opsUptime struct{}

func (opsUptime) StartTime() time.Time {
	if opsDeps.StartTime != nil {
		return opsDeps.StartTime()
	}
	return time.Now()
}

// loadMobNames loads the mob key -> display name dictionary from mobs.json
// (embedded or filesystem via the data package). Returns an empty map on
// error (leaderboards still work, just without mob name validation).
func loadMobNames() map[string]string {
	raw, err := data.ReadFile("mobs.json")
	if err != nil {
		log.Printf("ops: leaderboards: load mobs.json: %v", err)
		return map[string]string{}
	}
	var mobs map[string]struct{ Name string `json:"name"` }
	if err := json.Unmarshal(raw, &mobs); err != nil {
		log.Printf("ops: leaderboards: parse mobs.json: %v", err)
		return map[string]string{}
	}
	out := make(map[string]string, len(mobs))
	for k, v := range mobs {
		out[k] = v.Name
	}
	return out
}

// StartConsole starts the stdin console loop unless CONSOLE=0 or stdin is
// not a TTY (pipes — tests, harnesses, CI — never block on stdin).
func StartConsole() {
	if opsDeps.ConsoleOff {
		log.Printf("ops: console disabled (CONSOLE=%s)", os.Getenv("CONSOLE"))
		return
	}
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		log.Printf("ops: console disabled (stdin not a TTY)")
		return
	}
	h := &opsConsole{}
	go func() {
		log.Printf("ops: console ready (slash commands)")
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if out := console.Exec(h, sc.Text()); out != "" {
				log.Printf("console: %s", out)
			}
		}
		if err := sc.Err(); err != nil {
			log.Printf("ops: console ended: %v", err)
		}
	}()
}

// opsConsole applies console commands to the live world. Output strings
// mirror the TS console log.info lines; the caller prints them.
type opsConsole struct{}

func (opsConsole) Players() string {
	names := opsDeps.Usernames()
	if len(names) == 1 {
		return "There is currently 1 person online: " + names[0]
	}
	return fmt.Sprintf("There are currently %d people online: %s", len(names), strings.Join(names, ", "))
}

func (opsConsole) Total() string {
	if !opsDeps.HaveDB() {
		return "Total players: 0"
	}
	n, err := opsDeps.CountPlayers()
	if err != nil {
		return fmt.Sprintf("Total players: unknown (%v)", err)
	}
	return fmt.Sprintf("Total players: %d", n)
}

func (opsConsole) Update() string {
	dropped := opsDeps.BeginUpdate()
	return fmt.Sprintf("Server updating: rejected %d connection(s), new connections disabled.", dropped)
}

func (opsConsole) Kill(username string) string {
	name, ok := opsDeps.KillPlayer(username)
	if !ok {
		return fmt.Sprintf("Player %s not found.", username)
	}
	return fmt.Sprintf("%s has been killed.", name)
}

func consoleDrop(username, verb string) string {
	name, ok := opsDeps.DropPlayer(username)
	if !ok {
		return fmt.Sprintf("Player %s not found.", username)
	}
	return fmt.Sprintf("%s has been %s.", name, verb)
}

func (o opsConsole) Kick(username string) string    { return consoleDrop(username, "kicked") }
func (o opsConsole) Timeout(username string) string { return consoleDrop(username, "timed out") }

func consoleSetRank(username string, rank int, title string) string {
	name, ok := opsDeps.SetPlayerRank(username, rank)
	if !ok {
		return fmt.Sprintf("Player %s not found.", username)
	}
	return fmt.Sprintf("%s is now %s.", name, title)
}

func (o opsConsole) SetAdmin(username string) string {
	return consoleSetRank(username, opsDeps.AdminRank, "an admin")
}
func (o opsConsole) SetMod(username string) string {
	return consoleSetRank(username, opsDeps.ModeratorRank, "a moderator")
}

func consoleStripRank(username string) string {
	name, ok := opsDeps.StripPlayerRank(username)
	if !ok {
		return "Player is not logged in."
	}
	return fmt.Sprintf("%s's ranks have been stripped.", name)
}

func (o opsConsole) RemoveAdmin(username string) string {
	return consoleStripRank(username)
}
func (o opsConsole) RemoveMod(username string) string { return consoleStripRank(username) }

func (opsConsole) IPBan(ip string) string {
	if ip == "list" {
		out := opsDeps.BannedIPs()
		sort.Strings(out)
		if len(out) == 0 {
			return "No banned IPs."
		}
		return "Banned IPs: " + strings.Join(out, ", ")
	}
	dropped := opsDeps.BanIP(ip)
	return fmt.Sprintf("Banned %s (%d connection(s) dropped).", ip, dropped)
}

func (opsConsole) UnbanIP(ip string) string {
	opsDeps.UnbanIP(ip)
	// TS logs 'IP %s has been banned.' even for unban (console.ts shares the
	// log line across ipban/unbanip) — mirrored exactly.
	return fmt.Sprintf("IP %s has been banned.", ip)
}

func (opsConsole) Save() string {
	opsDeps.SaveWorld()
	return "World saved."
}
