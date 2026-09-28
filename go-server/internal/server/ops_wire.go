// Ops wiring (API + console) — thin root adapter over internal/app.
//
// Canonical owner: internal/app (ops.go: REST surface + stdin console over
// internal/api, internal/console, internal/net and internal/world). This
// file only wires the package seams to the root globals (players map,
// dbConn/dbMu, m5/m9 helpers, Hub accept-gate) and keeps the entry points
// main.go calls — with UNCHANGED signatures — delegating to the package.
// Output strings and limiter budgets are identical (owned by the package).
package server

import (
	"database/sql"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"rpg-world-server/internal/account"
	"rpg-world-server/internal/app"
	gnet "rpg-world-server/internal/net"
	worldcore "rpg-world-server/internal/world"
)

// opsGameVersion / opsMaxPlayers feed the API status snapshot (the stub has
// no config file; the handshake gVer is client-supplied so there is no
// server constant to reuse).
const (
	opsGameVersion = app.GameVersion
	opsMaxPlayers  = app.MaxPlayers
)

// serverStartTime is captured at opsConfigure (early in Boot) for the
// admin dashboard uptime display.
var serverStartTime = time.Now()

// Transport + accept-gate state moved to internal/net (D2a): the Hub owns
// the limiter, the update-mode gate, the admitted-conn table and the IP ban
// set. The accept/release/budget entry points live there as package funcs
// (gnet.Accept/Release/AllowMsg/AllowChat); console commands below drive
// the Hub via the app Ban seams (gnet.SetAccepting/gnet.BanIP/
// gnet.BannedIPs).

// Transport + accept-gate entry points moved to internal/net (D2a): use
// gnet.Accept/gnet.Release/gnet.AllowMsg/gnet.AllowChat at the former
// opsAccept/opsRelease/opsAllowMsg/opsAllowChat call sites (main.go
// handler, handleConn read path, m7 chat path).

// opsConfigure wires the ops seams (called once from initPlayerState, before the
// API/console start).
func opsConfigure() {
	port := 9001
	if p := os.Getenv("PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	app.ConfigureOps(app.OpsDeps{
		Port:       port,
		ConsoleOff: app.ConsoleDisabled(os.Getenv("CONSOLE")),
		Usernames:  playerUsernames,
		LookupPlayer: func(name string) (string, int, bool) {
			for _, n := range playerUsernames() {
				if strings.EqualFold(n, name) {
					return n, opsPlayerLevel(n), true
				}
			}
			return "", 0, false
		},
		PlayerCount: worldcore.PlayerCount,
		HaveDB:      func() bool { return dbConn != nil },
		CountPlayers: func() (int, error) {
			dbMu.Lock()
			defer dbMu.Unlock()
			var n int
			if err := dbConn.QueryRow(`SELECT COUNT(*) FROM players`).Scan(&n); err != nil {
				return 0, err
			}
			return n, nil
		},
		BeginUpdate: func() int {
			gnet.SetAccepting(false)
			conns := worldcore.AllWS()
			for _, k := range conns {
				worldcore.RemoveClient(k)
			}
			return len(conns)
		},
		KillPlayer: func(username string) (string, bool) {
			target := playerByName(username)
			if target == nil {
				return "", false
			}
			mobDamagePlayer(target, playerHP(target), nil)
			return target.Username, true
		},
		DropPlayer: func(username string) (string, bool) {
			target := playerByName(username)
			if target == nil {
				return "", false
			}
			name := target.Username
			worldcore.RemoveClient(target.Conn.WS)
			return name, true
		},
		SetPlayerRank: func(username string, rank int) (string, bool) {
			target := playerByName(username)
			if target == nil {
				return "", false
			}
			target.rank = rank
			chatStateFor(target).rank = rank
			st := playerStateFor(username)
			pstateMu.Lock()
			st.Rank = rank // durable across relogin via the persist rank column
			pstateMu.Unlock()
			markDirty(username)
			return target.Username, true
		},
		StripPlayerRank: func(username string) (string, bool) {
			// TS removeadmin/removemod: player.setRank() (rank None + [50])
			// + 'Your ranks have been stripped from you.' + sync.
			// Online path mirrors cmdRanks.SetRank (rank fields + [50] +
			// Sync) plus the strip notify.
			target := playerByName(username)
			if target == nil {
				return "", false
			}
			target.rank = 0 // Modules.Ranks.None
			chatStateFor(target).rank = 0
			_ = gnet.Send(target.Conn, pkt(PacketRank, 0)) // RankPacket(None)
			notifyPlayer(target, "Your ranks have been stripped from you.")
			st := playerStateFor(target.Username)
			pstateMu.Lock()
			x, y, level := st.X, st.Y, st.Level
			st.Rank = 0 // durable across relogin via the persist rank column
			pstateMu.Unlock()
			ph := welcomePlayer(target.Instance)
			ph.X, ph.Y = x, y
			ph.Level = intp(level)
			worldcore.Broadcast(pkt(PacketSync, ph))
			markDirty(target.Username)
			return target.Username, true
		},
		AdminRank:     RankAdmin,
		ModeratorRank: RankModerator,
		BannedIPs:     gnet.BannedIPs,
		BanIP: func(ip string) int {
			gnet.BanIP(ip)
			var conns []*websocket.Conn
			for _, k := range worldcore.AllWS() {
				host, _, err := net.SplitHostPort(gnet.AddrID(k))
				if err != nil {
					host = gnet.AddrID(k)
				}
				if host == ip {
					conns = append(conns, k)
				}
			}
			for _, k := range conns {
				worldcore.RemoveClient(k)
			}
			return len(conns)
		},
		UnbanIP: func(ip string) {
			// database.setIpBan(ip, false) parity: clear the ban only.
			// Same-IP conns stay connected (TS shares the kick loop with
			// ipban; the Go console deliberately does not re-kick on unban).
			gnet.UnbanIP(ip)
		},
		SaveWorld: flushDirty,
		GetDB:     func() *sql.DB { return dbConn },
		StartTime: func() time.Time { return serverStartTime },
		Accounts:  opsAccounts{},
	})
}

// opsAccounts adapts internal/account to the api.AccountProvider seam (the
// password-reset endpoints). It serializes on dbMu like every other
// direct-table path; with no DB wired every method answers the TS 'invalid'
// shape.
type opsAccounts struct{}

func (opsAccounts) CreateResetToken(email string) (id, token string, ok bool) {
	if dbConn == nil {
		return "", "", false
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	return account.CreateResetToken(dbConn, email)
}

func (opsAccounts) ResetPassword(id, token, password string) bool {
	if dbConn == nil {
		return false
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	return account.ResetPassword(dbConn, id, token, password)
}

func (opsAccounts) ValidEmail(email string) bool { return account.ValidEmail(email) }

func (opsAccounts) ValidPassword(password string) bool { return account.ValidPassword(password) }

func opsPlayerLevel(username string) int {
	if st := playerStateFor(username); st != nil && st.Level > 0 {
		return st.Level
	}
	return 1
}

// opsStartAPI starts the read-only REST surface when API_PORT is set
// (default off). A busy port logs and the game boots without the API.
func opsStartAPI() {
	app.StartAPI()
}

// opsStartConsole starts the stdin console loop unless CONSOLE=0 or stdin is
// not a TTY (pipes — tests, harnesses, CI — never block on stdin).
func opsStartConsole() {
	app.StartConsole()
}
