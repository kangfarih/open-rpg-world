// Social orchestration (friends + guilds + hub), extracted behavior-frozen
// from the root social_wire.go adapter (task D2b item 2: social_wire.go ->
// internal/social, the new package home for friends/guilds/hub
// orchestration; the friends/guilds/hub packages are re-exported below).
//
// TS sources (all read-only recon, no invented opcodes or packet shapes):
//   - packages/common/network/impl/friends.ts — FriendInfo {online, serverId}
//     per username; FriendsPacketData {list, username, status, serverId}.
//   - packages/common/network/impl/guild.ts — Member {username, rank?,
//     joinDate?, serverId?}, GuildData, GuildPacketData {identifier?, name?,
//     username?, usernames?, serverId?, member?, members?, total?, guilds?,
//     message?, owner?, decoration?, experience?, rank?}.
//   - packages/common/network/opcodes.ts — Opcodes.Friends List0 Add1
//     Remove2 Status3 Sync4; Opcodes.Guild Create0 Login1 Logout2 Join3
//     Leave4 Rank5 Update6 Experience7 Banner8 List9 Error10 Chat11
//     Promote12 Demote13 Kick14.
//   - packages/common/network/packets.ts — Packets.Guild 35, Packets.Friends 48.
//   - packages/server friends.ts — add guards (too-long/self/duplicate/
//     database.exists/world.isOnline), remove, sync of inactive friends,
//     setStatus; handler.ts S->C emits List {list}, Add {username, status,
//     serverId}, Remove {username}, Status {username, status, serverId}.
//   - packages/server controllers/guilds.ts — create/join/leave/kick/chat/
//     promote/demote/setRank/addExperience/get/list; synchronize() unicasts to
//     online members and relays to offline ones via the hub; connect() sends
//     Login {name, owner, members, decoration} + Update {online members};
//     join syncs Join {username, serverId}; leave/kick sync Leave {username};
//     setRank syncs Rank {username, rank}; guildNotify sends Error {message}.
//   - packages/server incoming.ts handleFriends (C->S Add/Remove {opcode,
//     username}) and handleGuild (C->S Create {name, colour, outline,
//     outlineColour, crest} / Join {identifier} / Leave / List {from, to} /
//     Chat {message} / Promote|Demote|Kick {username}).
//   - packages/server world.ts syncFriendsList/syncGuildMembers — login/logout
//     presence fanout (Status / Update {members:[{username, serverId}]},
//     serverId -1 on logout).
//   - packages/server controllers/commands.ts 'guild' — not-in-a-guild gate,
//     kick|rank subcommands with the exact notify strings kept below.
//   - packages/client menu.ts handleFriendConfirm/input.ts AddFriend (C->S
//     Friends {opcode, username}) and menu/guilds.ts (C->S Guild shapes above).
//
// Frames used (all pre-existing shapes, verified against the TS tree):
//
//	S->C Friends [48, opcode, {list|username,status,serverId}] List0 Add1
//	  Remove2 Status3 (Sync4 is hub-bound, never sent to the game client).
//	C->S Friends [48, {opcode, username}] Add1 Remove2 (incoming.ts parity;
//	  other opcodes ignored).
//	S->C Guild [35, opcode, data] Login1 Join3 Leave4 Rank5 Update6
//	  Experience7 List9 Error10 Chat11 (Logout2/Banner8 S->C-only or unused,
//	  mirroring the TS synchronize/connect/updateStatus emits).
//	C->S Guild [35, {opcode, ...}] Create0 Join3 Leave4 List9 Chat11
//	  Promote12 Demote13 Kick14 (incoming.ts handleGuild parity).
//	C->S Chat [19, [text]] carries `/guild invite <user>` (the invite path —
//	  TS has no invite RPC, so no new packet shape is invented for it).
//
// Divergences from TS (documented, inherited from the root wire):
//   - No economy/progression gates on create (30k gold, tutorial, guests):
//     the stub has no tutorial state and harness accounts carry no gold, so
//     Create checks membership + name uniqueness only (the registry-level
//     ErrAlreadyInGuild/ErrExists mapping the package documents as
//     caller-side).
//   - Guild join is invite-only: TS join() is open, but internal/guilds
//     requires a pending Invite (package-documented). C->S Join maps to
//     AcceptInvite; a join without an invite notifies 'guilds:NO_INVITE'
//     (new string, no TS counterpart). Invites flow over `/guild invite`.
//   - Decoration (banner/outline/crest) is now persisted to SQLite via the
//     guilds.decoration JSON column (v6 schema migration). The in-memory
//     socDeco map remains for fast Login frame assembly.
//   - Hub relay for offline members is a skip in all-in-one: TS
//     synchronize() relays to offline members over the hub socket; all-in-one
//     the Router resolves the online subset and offline members get nothing
//     (no cross-server socket exists). In hub mode (R2) guild/global fan out
//     to hub-known remote members via ForwardTo (one relay envelope each);
//     members online nowhere still get nothing. Guild chat therefore never
//     falls back to a global broadcast — that would leak guild chat to
//     non-members.
//   - Hub usernames are lowercase-normalized at the boundary (the Router
//     itself is exact-match per its docs; the stub's PM path lowercases names
//     before lookup, so registration normalizes the same way to keep
//     Route/lookup consistent).
//
// Everything transport/world/state related stays with the root adapter and
// is reached only through Conn (per-connection delivery) and Deps. Frames
// are built with internal/protocol (the same constructors the root pkt/pktOp
// shims wrap), so wire bytes are identical. All log strings and TESTMAP
// debug ops are kept verbatim.
package social

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	"rpg-world-server/internal/friends"
	"rpg-world-server/internal/guilds"
	"rpg-world-server/internal/hub"
	"rpg-world-server/internal/protocol"
)

// Re-exports from the orchestrated packages (the new home re-exports the
// existing homes so callers keep one import for social state).
type (
	// Guild is one guild (internal/guilds parity).
	Guild = guilds.Guild
	// FriendInfo mirrors FriendInfo (internal/friends parity).
	FriendInfo = friends.FriendInfo
	// Router is the all-in-one presence router (internal/hub parity).
	Router = hub.Router
	// HubMessage is one relay unit (internal/hub parity).
	HubMessage = hub.Message
)

// Re-exported friends limits (internal/friends parity).
const (
	// MaxUsernameLen caps friend names (friends.ts parity).
	MaxUsernameLen = friends.MaxUsernameLen
	// OfflineServerID marks offline presence (friends.ts parity).
	OfflineServerID = friends.OfflineServerID
)

// Hub message kinds (internal/hub parity).
const (
	KindChat   = hub.KindChat
	KindGuild  = hub.KindGuild
	KindGlobal = hub.KindGlobal
)

// Friend opcodes (Opcodes.Friends): List0 Add1 Remove2 Status3 Sync4.
const (
	FriendList   = 0
	FriendAdd    = 1
	FriendRemove = 2
	FriendStatus = 3
	FriendSync   = 4
)

// Guild opcodes alias the internal/guilds Op* constants (Opcodes.Guild order:
// Create0 Login1 Logout2 Join3 Leave4 Rank5 Update6 Experience7 Banner8 List9
// Error10 Chat11 Promote12 Demote13 Kick14).
const (
	GuildCreate     = guilds.OpCreate
	GuildLogin      = guilds.OpLogin
	GuildLogout     = guilds.OpLogout
	GuildJoin       = guilds.OpJoin
	GuildLeave      = guilds.OpLeave
	GuildRank       = guilds.OpRank
	GuildUpdate     = guilds.OpUpdate
	GuildExperience = guilds.OpExperience
	GuildBanner     = guilds.OpBanner
	GuildList       = guilds.OpList
	GuildError      = guilds.OpError
	GuildChat       = guilds.OpChat
	GuildPromote    = guilds.OpPromote
	GuildDemote     = guilds.OpDemote
	GuildKick       = guilds.OpKick
)

// Conn is the minimal per-connection view for social delivery.
type Conn struct {
	Instance string
	Username string
	// Send delivers frames to this conn (gnet.Send parity).
	Send func(frames ...[]any)
	// Notify sends a client text notification (m6Notify parity).
	Notify func(message string)
}

// Deps bundles the social seams (implemented by the root adapter; never by
// this package).
type Deps struct {
	// DB is the guilds + friends table handle (nil = persistence disabled).
	DB *sql.DB
	// LockDB/UnlockDB guard every Exec/Query (dbMu parity; never nested
	// inside the social mutex).
	LockDB   func()
	UnlockDB func()
	// ServerID mirrors config.serverId on the wire (offline presence is -1
	// per friends.ts load/add/setStatus).
	ServerID int
	// Test gates the TESTMAP debug dispatcher (testMode parity).
	Test bool
	// Broadcast fans frames out (worldcore.Broadcast parity; the
	// RouteGlobal empty-router fallback).
	Broadcast func(frame []any)
	// PlayerConn resolves an online player to its conn view
	// (m7PlayerByName parity). ok=false = offline.
	PlayerConn func(name string) (*Conn, bool)
	// PlayerNames snapshots online usernames (m7PlayerUsernames parity).
	PlayerNames func() []string
	// ForwardTo relays one game frame to the shard owning username (R2
	// cross-shard fanout via the hub; nil = all-in-one, remote members are
	// skipped exactly as before).
	ForwardTo func(username string, frame []any)
	// RemotePlayers snapshots the hub-known online set minus local players
	// (R2 cross-shard audience; nil = all-in-one, no remote audience).
	RemotePlayers func() []string
	// Sanitize escapes chat text (m7Sanitize parity).
	Sanitize func(string) string
	// IsNonBlank reports displayable text (whitespaceRe parity).
	IsNonBlank func(string) bool
	// FormatName formats a display name (m7FormatName parity).
	FormatName func(string) string
	// GuildCreationGates checks the economy/progression gates for guild
	// creation (TS guilds.ts create() parity): has 30,000 gold, tutorial
	// finished. Returns (ok, errMsg); when ok=false the caller notifies
	// errMsg and returns. The remove-gold step is implicit: when ok=true
	// the gold has already been deducted.
	GuildCreationGates func(username string) (ok bool, errMsg string)
}

var sdeps Deps

// Configure installs the social seams (called once from the root boot,
// before tables, logins or ticks run).
func Configure(d Deps) { sdeps = d }

var (
	// socHub is the all-in-one Router: Register on login, Unregister on
	// logout/disconnect, Route every cross-player message through it.
	socHub = hub.NewRouter()

	socMu      sync.Mutex
	socFriends = map[string]*friends.List{} // username (exact login case) -> list
	socGuilds  = guilds.NewRegistry()

	// socGuildIDs tracks live guild identifiers (the Registry has no
	// enumerate API, so the wire layer keeps the set for persistence).
	socGuildIDs = map[string]bool{}

	// socDeco keeps per-guild banner choices in memory (no SchemaSQL column;
	// see divergences). Keyed by guild identifier.
	socDeco = map[string]socDecoration{}
)

// socDecoration mirrors Decoration (impl/guild.ts). BannerColour/Crest are
// strings ('grey', 'goldenyellow', 'none', ...); outline is numeric.
type socDecoration struct {
	Banner        string `json:"banner"`
	Outline       int    `json:"outline"`
	OutlineColour string `json:"outlineColour"`
	Crest         string `json:"crest"`
}

// socDefaultDeco mirrors the client handleConnect fallbacks
// (banner Grey, outline StyleOne, outlineColour GoldenYellow).
func socDefaultDeco() socDecoration {
	return socDecoration{Banner: "grey", Outline: 0, OutlineColour: "goldenyellow", Crest: "none"}
}

// socFriendInfo mirrors FriendInfo (impl/friends.ts).
type socFriendInfo struct {
	Online   bool `json:"online"`
	ServerID int  `json:"serverId"`
}

// socGuildMember mirrors Member (impl/guild.ts): optionals omitted when unset.
type socGuildMember struct {
	Username string `json:"username"`
	Rank     *int   `json:"rank,omitempty"`
	JoinDate *int64 `json:"joinDate,omitempty"`
	ServerID *int   `json:"serverId,omitempty"`
}

func socIntp(v int) *int { return &v }

func lockDB() {
	if sdeps.LockDB != nil {
		sdeps.LockDB()
	}
}

func unlockDB() {
	if sdeps.UnlockDB != nil {
		sdeps.UnlockDB()
	}
}

// ---------------------------------------------------------------------------
// Tables + persistence (m11EnsureTables/m13EnsureTables precedent; single
// writer: every Exec/Query runs under the DB lock, never nested inside socMu).
// ---------------------------------------------------------------------------

// EnsureTables executes guilds.SchemaSQL() plus the friends table (same
// TEXT/INT column style). Called at boot and on login like m11EnsureTables.
func EnsureTables() {
	if sdeps.DB == nil {
		return
	}
	lockDB()
	defer unlockDB()
	for _, ddl := range strings.Split(guilds.SchemaSQL(), "\n") {
		if strings.TrimSpace(ddl) == "" {
			continue
		}
		if _, err := sdeps.DB.Exec(ddl); err != nil {
			log.Printf("social: ddl: %v", err)
		}
	}
	if _, err := sdeps.DB.Exec(
		`CREATE TABLE IF NOT EXISTS friends(player TEXT, friend TEXT, PRIMARY KEY(player, friend))`); err != nil {
		log.Printf("social: ddl friends: %v", err)
	}
}

// LoadGuilds rebuilds the registry from the guild tables (boot only, while
// single-threaded). Members rejoin via Invite+AcceptInvite+SetRank so the
// one-guild rule and ranks restore exactly.
func LoadGuilds() {
	if sdeps.DB == nil {
		return
	}
	type grow struct {
		id, name, owner string
		xp              int
		inviteOnly      bool
		creationDate    int64
		decoration      guilds.Decoration
	}
	lockDB()
	defer unlockDB()
	rows, err := sdeps.DB.Query(`SELECT id,name,owner FROM guilds`)
	if err != nil {
		return
	}
	var gs []grow
	for rows.Next() {
		var g grow
		if rows.Scan(&g.id, &g.name, &g.owner) == nil {
			gs = append(gs, g)
		}
	}
	rows.Close()
	// xp/invite_only/creation_date/decoration columns may be absent on older
	// DBs; tolerate their absence (zero values).
	for i := range gs {
		var xp int
		if err := sdeps.DB.QueryRow(`SELECT xp FROM guilds WHERE id=?`, gs[i].id).Scan(&xp); err == nil {
			gs[i].xp = xp
		}
		var inviteOnly int
		if err := sdeps.DB.QueryRow(`SELECT invite_only FROM guilds WHERE id=?`, gs[i].id).Scan(&inviteOnly); err == nil {
			gs[i].inviteOnly = inviteOnly != 0
		}
		var creationDate int64
		if err := sdeps.DB.QueryRow(`SELECT creation_date FROM guilds WHERE id=?`, gs[i].id).Scan(&creationDate); err == nil {
			gs[i].creationDate = creationDate
		}
		var decoJSON string
		if err := sdeps.DB.QueryRow(`SELECT decoration FROM guilds WHERE id=?`, gs[i].id).Scan(&decoJSON); err == nil && decoJSON != "" {
			var deco guilds.Decoration
			if json.Unmarshal([]byte(decoJSON), &deco) == nil {
				gs[i].decoration = deco
			}
		}
		if gs[i].decoration == (guilds.Decoration{}) {
			gs[i].decoration = guilds.DefaultDecoration()
		}
	}
	for _, g := range gs {
		created, err := socGuilds.Create(g.owner, g.name)
		if err != nil {
			continue
		}
		socGuildIDs[created.ID] = true
		// Overwrite the defaults Create() set with persisted values.
		created.InviteOnly = g.inviteOnly
		created.CreationDate = g.creationDate
		created.Decoration = g.decoration
		socMu.Lock()
		socDeco[created.ID] = socDecoration{
			Banner:        g.decoration.Banner,
			Outline:       g.decoration.Outline,
			OutlineColour: g.decoration.OutlineColour,
			Crest:         g.decoration.Crest,
		}
		socMu.Unlock()
		mrows, merr := sdeps.DB.Query(`SELECT player,rank,join_date FROM guild_members WHERE guild=?`, created.ID)
		if merr != nil {
			// Fallback: older schema without join_date column.
			mrows, merr = sdeps.DB.Query(`SELECT player,rank FROM guild_members WHERE guild=?`, created.ID)
			if merr != nil {
				continue
			}
		}
		var members []struct {
			name     string
			rank     int
			joinDate int64
		}
		for mrows.Next() {
			var m struct {
				name     string
				rank     int
				joinDate int64
			}
			if err := mrows.Scan(&m.name, &m.rank, &m.joinDate); err != nil {
				// Fallback scan without join_date.
				if err2 := mrows.Scan(&m.name, &m.rank); err2 != nil {
					continue
				}
			}
			members = append(members, m)
		}
		mrows.Close()
		sort.Slice(members, func(a, b int) bool { return members[a].name < members[b].name })
		for _, m := range members {
			if m.name == g.owner {
				// Restore owner's join date too.
				if m.joinDate != 0 {
					socGuilds.SetJoinDate(created.ID, g.owner, m.joinDate)
				}
				continue
			}
			if err := socGuilds.Invite(g.owner, m.name); err != nil {
				continue
			}
			if err := socGuilds.AcceptInvite(m.name, created.ID); err != nil {
				continue
			}
			if m.joinDate != 0 {
				socGuilds.SetJoinDate(created.ID, m.name, m.joinDate)
			}
			if m.rank != int(guilds.RankFledgling) {
				_ = socGuilds.SetRank(g.owner, m.name, guilds.Rank(m.rank))
			}
		}
		if g.xp != 0 {
			_ = socGuilds.AddXP(g.owner, g.xp)
		}
		// Persist the restored state (now with correct creationDate etc.)
		// so the DB reflects the restored guild.
		socGuilds.SetGuildMeta(created.ID, g.inviteOnly, g.creationDate, g.decoration)
	}
	if len(gs) > 0 {
		log.Printf("social: restored %d guilds", len(gs))
	}
}

// saveGuildRows persists one guild snapshot (caller holds no locks).
func saveGuildRows(g *guilds.Guild) {
	if sdeps.DB == nil || g == nil {
		return
	}
	lockDB()
	defer unlockDB()
	decoJSON, _ := json.Marshal(g.Decoration)
	inviteOnly := 0
	if g.InviteOnly {
		inviteOnly = 1
	}
	if _, err := sdeps.DB.Exec(`INSERT INTO guilds(id,name,owner,xp,invite_only,creation_date,decoration) VALUES(?,?,?,?,?,?,?) `+
		`ON CONFLICT(id) DO UPDATE SET name=?,owner=?,xp=?,invite_only=?,creation_date=?,decoration=?`,
		g.ID, g.Name, g.Owner, g.XP, inviteOnly, g.CreationDate, string(decoJSON),
		g.Name, g.Owner, g.XP, inviteOnly, g.CreationDate, string(decoJSON)); err != nil {
		log.Printf("social: save guild %s: %v", g.ID, err)
		return
	}
	if _, err := sdeps.DB.Exec(`DELETE FROM guild_members WHERE guild=?`, g.ID); err != nil {
		return
	}
	for user, rank := range g.Members {
		joinDate := g.JoinDates[user]
		if _, err := sdeps.DB.Exec(`INSERT INTO guild_members(guild,player,rank,join_date) VALUES(?,?,?,?)`,
			g.ID, user, int(rank), joinDate); err != nil {
			log.Printf("social: save member %s/%s: %v", g.ID, user, err)
		}
	}
}

// dropGuildRows deletes one guild's rows (disband path).
func dropGuildRows(id string) {
	if sdeps.DB == nil || id == "" {
		return
	}
	lockDB()
	defer unlockDB()
	if _, err := sdeps.DB.Exec(`DELETE FROM guild_members WHERE guild=?`, id); err != nil {
		log.Printf("social: drop members %s: %v", id, err)
	}
	if _, err := sdeps.DB.Exec(`DELETE FROM guilds WHERE id=?`, id); err != nil {
		log.Printf("social: drop guild %s: %v", id, err)
	}
}

// persistGuildID saves or drops the guild by identifier depending on
// whether it still exists (leave/disband/kick paths).
func persistGuildID(id string) {
	if id == "" {
		return
	}
	if g, err := socGuilds.Get(id); err == nil {
		saveGuildRows(g)
		return
	}
	socMu.Lock()
	delete(socGuildIDs, id)
	delete(socDeco, id)
	socMu.Unlock()
	dropGuildRows(id)
}

// friendsFor returns the in-memory list for username, creating it empty.
func friendsFor(username string) *friends.List {
	socMu.Lock()
	defer socMu.Unlock()
	if l := socFriends[username]; l != nil {
		return l
	}
	l := friends.New(username)
	socFriends[username] = l
	return l
}

// playerOnline reports whether name is online (PlayerConn parity).
func playerOnline(name string) bool {
	if sdeps.PlayerConn == nil {
		return false
	}
	_, ok := sdeps.PlayerConn(name)
	return ok
}

// playerSend unicasts a frame to an online player (nil-safe).
func playerSend(name string, frame []any) {
	if sdeps.PlayerConn == nil {
		return
	}
	if t, ok := sdeps.PlayerConn(name); ok && t != nil {
		t.Send(frame)
	}
}

// playerNotify notifies an online player (nil-safe).
func playerNotify(name, message string) {
	if sdeps.PlayerConn == nil {
		return
	}
	if t, ok := sdeps.PlayerConn(name); ok && t != nil {
		t.Notify(message)
	}
}

// LoadFriends restores username's rows into memory and resolves presence
// against the online map (friends.ts load() parity). Called on login.
func loadFriends(username string) {
	l := friendsFor(username)
	if sdeps.DB == nil || username == "" {
		return
	}
	lockDB()
	rows, err := sdeps.DB.Query(`SELECT friend FROM friends WHERE player=?`, username)
	if err != nil {
		unlockDB()
		return
	}
	var names []string
	for rows.Next() {
		var f string
		if rows.Scan(&f) == nil {
			names = append(names, f)
		}
	}
	rows.Close()
	unlockDB()
	presence := map[string]bool{}
	for _, n := range names {
		presence[n] = playerOnline(n)
	}
	socMu.Lock()
	defer socMu.Unlock()
	l.Load(names)
	for n, online := range presence {
		sid := friends.OfflineServerID
		if online {
			sid = sdeps.ServerID
		}
		l.SetStatus(n, online, sid)
	}
}

// persistFriends flushes username's list (load-on-login/flush-on-change/
// disconnect; caller holds no locks).
func persistFriends(username string) {
	if sdeps.DB == nil || username == "" {
		return
	}
	socMu.Lock()
	l := socFriends[username]
	var members []string
	if l != nil {
		members = l.Members()
	}
	socMu.Unlock()
	lockDB()
	defer unlockDB()
	if _, err := sdeps.DB.Exec(`DELETE FROM friends WHERE player=?`, username); err != nil {
		return
	}
	for _, f := range members {
		if _, err := sdeps.DB.Exec(`INSERT INTO friends(player,friend) VALUES(?,?)`, username, f); err != nil {
			log.Printf("social: save friend %s/%s: %v", username, f, err)
		}
	}
}

// FlushUser persists one player's friends rows outside the disconnect path
// (pre-shutdown drain barrier; the disconnect path reaches the same store
// via OnDisconnect).
func FlushUser(username string) { persistFriends(username) }

// PlayerExists mirrors the TS database.exists gate: an online conn or a
// persisted players row (instance or name column; m5 keys rows by username).
func PlayerExists(username string) bool {
	if playerOnline(username) {
		return true
	}
	if sdeps.DB == nil || username == "" {
		return false
	}
	lockDB()
	defer unlockDB()
	var one int
	if err := sdeps.DB.QueryRow(`SELECT 1 FROM players WHERE instance=? OR name=?`, username, username).Scan(&one); err != nil {
		return false
	}
	return true
}

// GuildOf reports the caller's guild (registry parity, for the /guild
// command gate in the root adapter).
func GuildOf(username string) (*guilds.Guild, error) { return socGuilds.GuildOf(username) }

// GuildByID fetches a guild snapshot by identifier (ops surface parity).
func GuildByID(id string) (*guilds.Guild, error) { return socGuilds.Get(id) }

// GuildIDs snapshots live guild identifiers (ops surface parity).
func GuildIDs() []string {
	socMu.Lock()
	defer socMu.Unlock()
	ids := make([]string, 0, len(socGuildIDs))
	for id := range socGuildIDs {
		ids = append(ids, id)
	}
	return ids
}

// ---------------------------------------------------------------------------
// S->C frame builders (TS handler.ts / guilds.ts emit parity).
// ---------------------------------------------------------------------------

func friendsListFrame(l *friends.List) []any {
	socMu.Lock()
	members := l.Members()
	info := make(map[string]socFriendInfo, len(members))
	for _, n := range members {
		fi, _ := l.Info(n)
		info[n] = socFriendInfo{Online: fi.Online, ServerID: fi.ServerID}
	}
	socMu.Unlock()
	if info == nil {
		info = map[string]socFriendInfo{}
	}
	return protocol.PktOp(protocol.PacketFriends, FriendList, map[string]any{"list": info})
}

func sendFriendsAdd(c *Conn, username string, online bool, serverID int) {
	c.Send(protocol.PktOp(protocol.PacketFriends, FriendAdd, map[string]any{
		"username": username, "status": online, "serverId": serverID,
	}))
}

func sendFriendsRemove(c *Conn, username string) {
	c.Send(protocol.PktOp(protocol.PacketFriends, FriendRemove, map[string]any{
		"username": username,
	}))
}

func sendFriendsStatus(c *Conn, username string, online bool, serverID int) {
	c.Send(protocol.PktOp(protocol.PacketFriends, FriendStatus, map[string]any{
		"username": username, "status": online, "serverId": serverID,
	}))
}

// guildLoginFrame builds the connect/Login frame (guilds.ts create/connect
// parity: {name, owner, members, decoration, creationDate, inviteOnly}).
func guildLoginFrame(g *guilds.Guild) []any {
	names := make([]string, 0, len(g.Members))
	for u := range g.Members {
		names = append(names, u)
	}
	sort.Strings(names)
	members := make([]socGuildMember, 0, len(names))
	for _, u := range names {
		r := int(g.Members[u])
		m := socGuildMember{Username: u, Rank: &r}
		if jd, ok := g.JoinDates[u]; ok && jd != 0 {
			m.JoinDate = &jd
		}
		members = append(members, m)
	}
	socMu.Lock()
	deco, ok := socDeco[g.ID]
	socMu.Unlock()
	if !ok {
		deco = socDefaultDeco()
	}
	return protocol.PktOp(protocol.PacketGuild, GuildLogin, map[string]any{
		"name": g.Name, "owner": g.Owner, "members": members, "decoration": deco,
		"creationDate": g.CreationDate, "inviteOnly": g.InviteOnly,
	})
}

func sendGuildError(c *Conn, message string) {
	c.Send(protocol.PktOp(protocol.PacketGuild, GuildError, map[string]any{"message": message}))
}

// onlineGuildmates snapshots the online members of g excluding skip.
func onlineGuildmates(g *guilds.Guild, skip string) []string {
	var out []string
	for u := range g.Members {
		if u == skip {
			continue
		}
		if playerOnline(u) {
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

// deliverGuildChat fans a Chat frame out to the Router-resolved online
// subset (all-in-one: Route filters the candidate roster; offline members are
// skipped — no hub socket exists to relay further, and a global broadcast
// fallback would leak guild chat to non-members). In hub mode (R2) members
// owned by remote shards are relayed one [53, ...] envelope each via
// ForwardTo (same per-username scope as direct chat, D4 parity).
func deliverGuildChat(from, message string, members []string) {
	recips, _ := socHub.Route(hub.Message{From: from, Kind: hub.KindGuild, Payload: members})
	frame := protocol.PktOp(protocol.PacketGuild, GuildChat, map[string]any{
		"username": from, "serverId": sdeps.ServerID, "message": message,
	})
	local := make(map[string]struct{}, len(recips))
	for _, name := range recips {
		local[name] = struct{}{}
		if t, ok := sdeps.PlayerConn(name); ok && t != nil {
			t.Send(frame)
		}
	}
	if sdeps.ForwardTo == nil {
		return
	}
	remote := remoteSet()
	for _, name := range members {
		if _, ok := local[name]; ok {
			continue
		}
		if _, ok := remote[name]; ok {
			sdeps.ForwardTo(name, frame)
		}
	}
}

// remoteSet snapshots the hub-known remote audience (nil-safe).
func remoteSet() map[string]struct{} {
	out := map[string]struct{}{}
	if sdeps.RemotePlayers == nil {
		return out
	}
	for _, u := range sdeps.RemotePlayers() {
		if u != "" {
			out[u] = struct{}{}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// C->S dispatch.
// ---------------------------------------------------------------------------

// HandleFriends routes C->S Friends frames [48,{opcode,username}]
// (incoming.ts handleFriends parity: Add/Remove only, the rest ignored).
func HandleFriends(c *Conn, data []byte) {
	if c == nil {
		return
	}
	var d struct {
		Opcode   *int   `json:"opcode"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Opcode == nil {
		return
	}
	switch *d.Opcode {
	case FriendAdd:
		friendAdd(c, d.Username)
	case FriendRemove:
		friendRemove(c, d.Username)
	}
}

// friendAdd ports friends.ts add() with the misc:FRIENDS_* notify mapping
// (caller-owned per the friends package docs).
func friendAdd(c *Conn, username string) {
	key := strings.ToLower(strings.TrimSpace(username))
	if len(key) > friends.MaxUsernameLen {
		c.Notify("misc:FRIENDS_USERNAME_TOO_LONG")
		return
	}
	if key == strings.ToLower(c.Username) {
		c.Notify("misc:FRIENDS_ADD_SELF")
		return
	}
	l := friendsFor(c.Username)
	socMu.Lock()
	dup := l.IsFriend(key)
	socMu.Unlock()
	if dup {
		c.Notify("misc:FRIENDS_ALREADY_ADDED")
		return
	}
	if !PlayerExists(key) && !PlayerExists(username) {
		c.Notify("misc:FRIENDS_USER_DOES_NOT_EXIST")
		return
	}
	socMu.Lock()
	added := l.Add(key)
	socMu.Unlock()
	if !added {
		c.Notify("misc:FRIENDS_USER_DOES_NOT_EXIST")
		return
	}
	online := playerOnline(key)
	sid := friends.OfflineServerID
	if online {
		sid = sdeps.ServerID
	}
	socMu.Lock()
	l.SetStatus(key, online, sid)
	socMu.Unlock()
	persistFriends(c.Username)
	sendFriendsAdd(c, key, online, sid)
	log.Printf("social: %s added friend %s online=%v", c.Username, key, online)
}

// friendRemove ports friends.ts remove().
func friendRemove(c *Conn, username string) {
	key := strings.ToLower(strings.TrimSpace(username))
	l := friendsFor(c.Username)
	socMu.Lock()
	removed := l.Remove(key)
	socMu.Unlock()
	if !removed {
		c.Notify("misc:FRIENDS_NOT_IN_LIST")
		return
	}
	persistFriends(c.Username)
	sendFriendsRemove(c, key)
	log.Printf("social: %s removed friend %s", c.Username, key)
}

// HandleGuild routes C->S Guild frames [35,{opcode,...}]
// (incoming.ts handleGuild parity).
func HandleGuild(c *Conn, data []byte) {
	if c == nil {
		return
	}
	var d struct {
		Opcode        *int   `json:"opcode"`
		Name          string `json:"name"`
		Colour        string `json:"colour"`
		Outline       *int   `json:"outline"`
		OutlineColour string `json:"outlineColour"`
		Crest         string `json:"crest"`
		Identifier    string `json:"identifier"`
		From          *int   `json:"from"`
		To            *int   `json:"to"`
		Message       string `json:"message"`
		Username      string `json:"username"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Opcode == nil {
		return
	}
	switch *d.Opcode {
	case GuildCreate:
		CreateGuild(c, d.Name, d.Colour, d.Outline, d.OutlineColour, d.Crest)
	case GuildJoin:
		JoinGuild(c, d.Identifier)
	case GuildLeave:
		LeaveGuild(c)
	case GuildList:
		from, to := 0, 50
		if d.From != nil {
			from = *d.From
		}
		if d.To != nil {
			to = *d.To
		}
		guildList(c, from, to)
	case GuildChat:
		ChatGuild(c, d.Message)
	case GuildPromote:
		guildSetRankDelta(c, d.Username, +1)
	case GuildDemote:
		guildSetRankDelta(c, d.Username, -1)
	case GuildKick:
		KickGuild(c, d.Username, false)
	}
}

// guildCreate ports guilds.ts create(): economy/progression gates (30k gold,
// tutorial finished) + membership + name-uniqueness checks.
func CreateGuild(c *Conn, name, colour string, outline *int, outlineColour, crest string) {
	// TS parity: economy/progression gates (guilds.ts create() lines 44-60).
	if sdeps.GuildCreationGates != nil {
		if ok, errMsg := sdeps.GuildCreationGates(c.Username); !ok {
			if errMsg != "" {
				c.Notify(errMsg)
			}
			return
		}
	}
	if _, err := socGuilds.GuildOf(c.Username); err == nil {
		c.Notify("guilds:ALREADY_IN_GUILD")
		return
	}
	g, err := socGuilds.Create(c.Username, name)
	if err != nil {
		if err == guilds.ErrAlreadyInGuild {
			c.Notify("guilds:ALREADY_IN_GUILD")
		} else if err == guilds.ErrExists {
			sendGuildError(c, "A guild with that name already exists.")
		}
		return
	}
	socMu.Lock()
	socGuildIDs[g.ID] = true
	deco := socDefaultDeco()
	if colour != "" {
		deco.Banner = colour
	}
	if outline != nil {
		deco.Outline = *outline
	}
	if outlineColour != "" {
		deco.OutlineColour = outlineColour
	}
	if crest != "" {
		deco.Crest = crest
	}
	socDeco[g.ID] = deco
	socMu.Unlock()
	// Set decoration on the guild struct so saveGuildRows persists it.
	g.Decoration = guilds.Decoration{
		Banner:        deco.Banner,
		Outline:       deco.Outline,
		OutlineColour: deco.OutlineColour,
		Crest:         deco.Crest,
	}
	socGuilds.SetGuildMeta(g.ID, g.InviteOnly, g.CreationDate, g.Decoration)
	saveGuildRows(g)
	c.Send(guildLoginFrame(g))
	log.Printf("social: %s created guild %s (%s)", c.Username, g.Name, g.ID)
}

// guildJoin ports the join flow over a pending invite (invite-only
// divergence): AcceptInvite, then connect parity (Login + Update of online
// members to the joiner) plus a Join sync to the other online members.
func JoinGuild(c *Conn, identifier string) {
	if _, err := socGuilds.GuildOf(c.Username); err == nil {
		c.Notify("guilds:ALREADY_IN_GUILD")
		return
	}
	id := strings.ToLower(identifier)
	if err := socGuilds.AcceptInvite(c.Username, id); err != nil {
		switch err {
		case guilds.ErrAlreadyInGuild:
			c.Notify("guilds:ALREADY_IN_GUILD")
		case guilds.ErrFull:
			c.Notify("guilds:GUILD_FULL")
		case guilds.ErrNotFound:
			guildList(c, 0, 10) // TS join-miss resends the guild list
		default: // ErrNoInvite / ErrInvalid
			c.Notify("guilds:NO_INVITE")
		}
		return
	}
	g, err := socGuilds.GuildOf(c.Username)
	if err != nil {
		return
	}
	persistGuildID(g.ID)
	c.Send(guildLoginFrame(g))
	sendGuildUpdateOnline(c)
	for _, name := range onlineGuildmates(g, c.Username) {
		playerSend(name, protocol.PktOp(protocol.PacketGuild, GuildJoin, map[string]any{
			"username": c.Username, "serverId": sdeps.ServerID,
		}))
	}
	log.Printf("social: %s joined guild %s", c.Username, g.ID)
}

// sendGuildUpdateOnline sends the joiner the online roster
// (guilds.ts updateStatus parity: Update {members:[{username, serverId}]}).
func sendGuildUpdateOnline(c *Conn) {
	g, err := socGuilds.GuildOf(c.Username)
	if err != nil {
		return
	}
	var online []socGuildMember
	for u := range g.Members {
		if !playerOnline(u) {
			continue
		}
		sid := sdeps.ServerID
		online = append(online, socGuildMember{Username: u, ServerID: &sid})
	}
	sort.Slice(online, func(a, b int) bool { return online[a].Username < online[b].Username })
	if online == nil {
		online = []socGuildMember{}
	}
	c.Send(protocol.PktOp(protocol.PacketGuild, GuildUpdate, map[string]any{"members": online}))
}

// GuildLeave ports guilds.ts leave() (owner leaving disbands).
func LeaveGuild(c *Conn) {
	if c == nil {
		return
	}
	before, err := socGuilds.GuildOf(c.Username)
	if err != nil {
		return
	}
	members := make([]string, 0, len(before.Members))
	for u := range before.Members {
		members = append(members, u)
	}
	ownerLeaving := before.Owner == c.Username
	if err := socGuilds.Leave(c.Username); err != nil {
		return
	}
	persistGuildID(before.ID)
	c.Send(protocol.PktOp(protocol.PacketGuild, GuildLeave, nil))
	for _, name := range members {
		if name == c.Username {
			continue
		}
		playerSend(name, protocol.PktOp(protocol.PacketGuild, GuildLeave, map[string]any{
			"username": c.Username,
		}))
	}
	log.Printf("social: %s left guild %s disband=%v", c.Username, before.ID, ownerLeaving)
}

// guildList ports guilds.ts get() from the persisted rows (the registry
// has no enumerate API; the DB is the list source like the TS loader).
func guildList(c *Conn, from, to int) {
	if sdeps.DB == nil {
		return
	}
	if from < 0 {
		from = 0
	}
	if to <= from {
		to = from + 50
	}
	lockDB()
	rows, err := sdeps.DB.Query(`SELECT id,name,owner FROM guilds`)
	if err != nil {
		unlockDB()
		return
	}
	var ids []string
	names := map[string]string{}
	inviteOnlyMap := map[string]bool{}
	for rows.Next() {
		var id, name, owner string
		if rows.Scan(&id, &name, &owner) == nil {
			ids = append(ids, id)
			names[id] = name
		}
	}
	rows.Close()
	// Read invite_only (tolerate missing column on older DBs).
	for _, id := range ids {
		var inv int
		if err := sdeps.DB.QueryRow(`SELECT invite_only FROM guilds WHERE id=?`, id).Scan(&inv); err == nil {
			inviteOnlyMap[id] = inv != 0
		}
	}
	sort.Strings(ids)
	var list []map[string]any
	total := 0
	for _, id := range ids {
		var n int
		if err := sdeps.DB.QueryRow(`SELECT COUNT(*) FROM guild_members WHERE guild=?`, id).Scan(&n); err != nil {
			continue
		}
		if n >= guilds.MaxMembers {
			continue
		}
		total++
	}
	unlockDB()
	for i, id := range ids {
		if i < from || i >= to {
			continue
		}
		var n int
		lockDB()
		_ = sdeps.DB.QueryRow(`SELECT COUNT(*) FROM guild_members WHERE guild=?`, id).Scan(&n)
		unlockDB()
		if n >= guilds.MaxMembers {
			continue
		}
		socMu.Lock()
		deco, ok := socDeco[id]
		socMu.Unlock()
		if !ok {
			deco = socDefaultDeco()
		}
		list = append(list, map[string]any{
			"name": names[id], "members": n, "decoration": deco,
			"inviteOnly": inviteOnlyMap[id],
		})
	}
	if list == nil {
		list = []map[string]any{}
	}
	c.Send(protocol.PktOp(protocol.PacketGuild, GuildList, map[string]any{
		"guilds": list, "total": total,
	}))
}

// guildChat ports guilds.ts chat() through the Router fanout.
func ChatGuild(c *Conn, message string) {
	g, err := socGuilds.GuildOf(c.Username)
	if err != nil {
		return
	}
	text := sdeps.Sanitize(message)
	if !sdeps.IsNonBlank(text) {
		return
	}
	members := make([]string, 0, len(g.Members))
	for u := range g.Members {
		members = append(members, u)
	}
	sort.Strings(members)
	deliverGuildChat(c.Username, text, members)
	log.Printf("social: guild %s chat from %s", g.ID, c.Username)
}

// guildSetRankDelta ports promote/demote via SetRank(rank +/- 1).
func guildSetRankDelta(c *Conn, username string, delta int) {
	g, err := socGuilds.GuildOf(c.Username)
	if err != nil {
		return
	}
	cur, ok := g.Members[username]
	if !ok {
		return
	}
	if err := socGuilds.SetRank(c.Username, username, cur+guilds.Rank(delta)); err != nil {
		if err == guilds.ErrNoPermission {
			c.Notify("guilds:NO_PERMISSION_RANK")
		}
		return
	}
	persistGuildID(g.ID)
	rank := int(cur + guilds.Rank(delta))
	for _, name := range onlineGuildmates(g, "") {
		playerSend(name, protocol.PktOp(protocol.PacketGuild, GuildRank, map[string]any{
			"username": username, "rank": rank,
		}))
	}
	log.Printf("social: %s set %s rank to %d in %s", c.Username, username, rank, g.ID)
}

// guildKick ports guilds.ts kick(). Packet path is silent like TS
// incoming (the /guild command path notifies separately).
func KickGuild(c *Conn, username string, viaCommand bool) {
	if c == nil {
		if viaCommand {
			return
		}
		return
	}
	before, err := socGuilds.GuildOf(c.Username)
	if err != nil {
		if viaCommand {
			c.Notify("You are not in a guild.")
		}
		return
	}
	if err := socGuilds.Kick(c.Username, username); err != nil {
		if viaCommand {
			if err == guilds.ErrNoPermission {
				c.Notify("guilds:NO_PERMISSION")
			} else {
				c.Notify("guilds:NO_PERMISSION")
			}
		}
		return
	}
	persistGuildID(before.ID)
	if t, ok := sdeps.PlayerConn(username); ok && t != nil {
		t.Send(protocol.PktOp(protocol.PacketGuild, GuildLeave, nil))
	}
	for _, name := range onlineGuildmates(before, username) {
		if name == c.Username {
			continue
		}
		playerSend(name, protocol.PktOp(protocol.PacketGuild, GuildLeave, map[string]any{
			"username": username,
		}))
	}
	if viaCommand {
		c.Notify("You have kicked " + username + " from your guild.")
	}
	log.Printf("social: %s kicked %s from %s", c.Username, username, before.ID)
}

// broadcastGuildRank fans a Rank frame to every online member of g.
func broadcastGuildRank(g *guilds.Guild, username string, rank int) {
	for _, name := range onlineGuildmates(g, "") {
		playerSend(name, protocol.PktOp(protocol.PacketGuild, GuildRank, map[string]any{
			"username": username, "rank": rank,
		}))
	}
}

// GuildRankCommand ports the /guild rank subcommand strings
// (commands.ts:129-158): landlord guard, member lookup, rank set + ack.
func GuildRankCommand(c *Conn, rankStr, username string) {
	if c == nil {
		return
	}
	if rankStr == "7" || rankStr == "landlord" {
		c.Notify("You cannot set a rank to landlord.")
		return
	}
	var n int
	if _, err := fmt.Sscanf(rankStr, "%d", &n); err != nil {
		c.Notify("Malformed command, expected /guild rank [rank 0-6] [username]")
		return
	}
	g, err := socGuilds.GuildOf(c.Username)
	if err != nil {
		c.Notify("You are not in a guild.")
		return
	}
	if _, ok := g.Members[username]; !ok {
		c.Notify("Could not find a member with the name: " + username + ".")
		return
	}
	if serr := socGuilds.SetRank(c.Username, username, guilds.Rank(n)); serr != nil {
		if serr == guilds.ErrNoPermission {
			c.Notify("guilds:NO_PERMISSION_RANK")
		}
		return
	}
	persistGuildID(g.ID)
	broadcastGuildRank(g, username, n)
	c.Notify("You have set " + username + "'s rank to " + rankStr + ".")
}

// GuildInvite records a pending invite (owner-only gate mirrors kick's).
// There is no TS invite RPC, so invites ride `/guild invite` (no new packet
// shape) with plain-text notifies on both ends.
func GuildInvite(c *Conn, target string) {
	if c == nil {
		return
	}
	name := strings.TrimSpace(target)
	if name == "" {
		c.Notify("Malformed command, expected /guild invite [username]")
		return
	}
	g, err := socGuilds.GuildOf(c.Username)
	if err != nil {
		c.Notify("You are not in a guild.")
		return
	}
	if err := socGuilds.Invite(c.Username, name); err != nil {
		switch err {
		case guilds.ErrNoPermission:
			c.Notify("guilds:NO_PERMISSION")
		case guilds.ErrAlreadyInGuild:
			c.Notify("guilds:ALREADY_IN_GUILD")
		default:
			c.Notify("guilds:NO_PERMISSION")
		}
		return
	}
	c.Notify("You have invited " + name + " to your guild.")
	playerNotify(name, "You have been invited to guild "+g.Name+".")
	log.Printf("social: %s invited %s to %s", c.Username, name, g.ID)
}

// GuildKickCommand ports the /guild kick subcommand (m13 command path).
func GuildKickCommand(c *Conn, username string) { KickGuild(c, username, true) }

// ---------------------------------------------------------------------------
// Login / logout (world.ts syncFriendsList/syncGuildMembers parity) + hub
// presence + disconnect cleanup for all three subsystems.
// ---------------------------------------------------------------------------

// OnLogin registers hub presence, restores friends, and builds the login
// frames (Friends List + guild Login/Update when guilded). Presence fanout to
// other conns goes out directly; the returned frames join the Welcome bulk.
func OnLogin(c *Conn) [][]any {
	if c == nil || c.Username == "" {
		return nil
	}
	socHub.Register(strings.ToLower(c.Username))
	loadFriends(c.Username)
	l := friendsFor(c.Username)
	var frames [][]any
	frames = append(frames, friendsListFrame(l))
	if g, err := socGuilds.GuildOf(c.Username); err == nil {
		frames = append(frames, guildLoginFrame(g))
		sendGuildUpdateOnline(c)
		for _, name := range onlineGuildmates(g, c.Username) {
			playerSend(name, protocol.PktOp(protocol.PacketGuild, GuildUpdate, map[string]any{
				"members": []socGuildMember{{Username: c.Username, ServerID: socIntp(sdeps.ServerID)}},
			}))
		}
	}
	// Friends online fanout: every online owner listing this user gets a
	// Status update (world.ts syncFriendsList parity).
	for _, name := range sdeps.PlayerNames() {
		if name == c.Username {
			continue
		}
		o, ok := sdeps.PlayerConn(name)
		if !ok || o == nil {
			continue
		}
		ol := friendsFor(name)
		socMu.Lock()
		has := ol.IsFriend(c.Username)
		if has {
			ol.SetStatus(c.Username, true, sdeps.ServerID)
		}
		socMu.Unlock()
		if has {
			sendFriendsStatus(o, c.Username, true, sdeps.ServerID)
		}
	}
	return frames
}

// OnDisconnect unregisters hub presence, flushes friends, and fans logout
// presence out (Status offline + guild Update serverId -1). Called from
// removeClient after the conn leaves the players map.
func OnDisconnect(c *Conn) {
	if c == nil || c.Username == "" {
		return
	}
	socHub.Unregister(strings.ToLower(c.Username))
	persistFriends(c.Username)
	for _, name := range sdeps.PlayerNames() {
		o, ok := sdeps.PlayerConn(name)
		if !ok || o == nil {
			continue
		}
		ol := friendsFor(name)
		socMu.Lock()
		has := ol.IsFriend(c.Username)
		if has {
			ol.SetStatus(c.Username, false, friends.OfflineServerID)
		}
		socMu.Unlock()
		if has {
			sendFriendsStatus(o, c.Username, false, friends.OfflineServerID)
		}
	}
	if g, err := socGuilds.GuildOf(c.Username); err == nil {
		for _, name := range onlineGuildmates(g, c.Username) {
			playerSend(name, protocol.PktOp(protocol.PacketGuild, GuildUpdate, map[string]any{
				"members": []socGuildMember{{Username: c.Username, ServerID: socIntp(friends.OfflineServerID)}},
			}))
		}
	}
	log.Printf("social: %s disconnected (presence flushed)", c.Username)
}

// RouteChat resolves a PM target through the Router (all-in-one: the
// single process owns every player, so ErrOffline falls back to the existing
// local lookup + misc:NOT_ONLINE notify — behavior unchanged). It reports
// whether the hub routed; the caller always falls back to the local lookup
// (the Route has no side effects).
func RouteChat(target string) bool {
	recips, err := socHub.Route(hub.Message{Kind: hub.KindChat, To: strings.ToLower(target)})
	return err == nil && len(recips) > 0
}

// RouteGlobal delivers a global chat line to the Router-resolved online
// set, falling back to the existing broadcast when the target set is offline
// (empty router — same recipients either way in all-in-one mode). In hub
// mode (R2) the hub-known remote audience gets one relayed copy each via
// ForwardTo.
func RouteGlobal(frame []any) {
	recips, _ := socHub.Route(hub.Message{Kind: hub.KindGlobal})
	if len(recips) == 0 {
		sdeps.Broadcast(frame)
		return
	}
	anySent := false
	for _, name := range recips {
		if t, ok := sdeps.PlayerConn(name); ok && t != nil {
			t.Send(frame)
			anySent = true
		}
	}
	if !anySent {
		sdeps.Broadcast(frame)
	}
	if sdeps.ForwardTo != nil {
		for _, name := range sdeps.RemotePlayers() {
			sdeps.ForwardTo(name, frame)
		}
	}
}

// ---------------------------------------------------------------------------
// TESTMAP debug dispatcher ([46 {socialtest:...}], abtest/pettest precedent).
// ---------------------------------------------------------------------------

// TestHandler echoes friends/guild/hub state through notifies (the e2e
// greps these): ops "friends", "guild", "hub".
func TestHandler(c *Conn, data []byte) {
	if !sdeps.Test || c == nil {
		return
	}
	var d struct {
		SocialTest string `json:"socialtest"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.SocialTest == "" {
		return
	}
	switch d.SocialTest {
	case "friends":
		l := friendsFor(c.Username)
		socMu.Lock()
		members := l.Members()
		parts := make([]string, 0, len(members))
		for _, n := range members {
			fi, _ := l.Info(n)
			parts = append(parts, n)
			_ = fi
		}
		socMu.Unlock()
		sort.Strings(parts)
		c.Notify("social:friends " + c.Username + "=[" + strings.Join(parts, ",") + "]")
	case "guild":
		g, err := socGuilds.GuildOf(c.Username)
		if err != nil {
			c.Notify("social:guild " + c.Username + "=none")
			return
		}
		names := make([]string, 0, len(g.Members))
		for u, r := range g.Members {
			names = append(names, u+"="+Itoa(int64(int(r))))
		}
		sort.Strings(names)
		c.Notify("social:guild " + g.ID + " [" + strings.Join(names, ",") + "]")
	case "hub":
		recips, _ := socHub.Route(hub.Message{Kind: hub.KindGlobal})
		c.Notify("social:hub [" + strings.Join(recips, ",") + "]")
	}
}

// Itoa formats an int64 (notify echo parity).
func Itoa(v int64) string { return strconv.FormatInt(v, 10) }
