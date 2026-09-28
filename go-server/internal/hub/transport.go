// Multi-server hub transport (ADDITIVE-ONLY).
//
// This file adds the socket transport for hub mode WITHOUT changing the
// all-in-one path: internal/hub/hub.go (Router/Register/Unregister/Route)
// is untouched, and nothing here runs unless HUB_ADDR is set (see Enabled).
// Importing this package starts no goroutines and opens no sockets; the
// caller drives Server/Client explicitly.
//
// Architecture (who owns what):
//
//	all-in-one (default, no HUB_ADDR): one Router is the local online map.
//	Register on login, Unregister on logout, Route every cross-player
//	message. On ErrOffline the caller falls back to local broadcast.
//	Unchanged behavior — this file is not even constructed.
//
//	multi-server (HUB_ADDR set): one hub process owns a Server (the
//	server-list: shard registrations + last-heartbeat per shard, relay
//	routing between shards, central offline mail for unreachable targets
//	drained on the owning shard's next register/heartbeat). Each world
//	process (shard) owns a Router for its local players plus a Client that
//	dials the hub, registers with a handshake frame, heartbeats every 5s,
//	and auto-reconnects with backoff.
//	On a local Route miss the shard's RouteOrForward consults the roster
//	pushed by the hub: target on a remote shard -> forward the relay frame
//	over the socket; target nowhere -> Mailer.Store for delivery on the
//	target's next Register/login.
//
//	Offline mail is shard-side: Mailer fronts a MailStore (MemoryMail by
//	default; SQLiteMail for file persistence via a hub-owned additive
//	table). Deliver is called from the login path right after
//	Router.Register (Client.Register does both).
//
// Env contract:
//
//	HUB_ADDR   hub socket address as ws(s)://host:port[/path] (shard side)
//	           or listen addr host:port (hub side, used by the hub main).
//	           Unset/empty => all-in-one mode: everything here stays idle.
//	HUB_TOKEN  shared secret. When set, the hub rejects mismatches (log +
//	           close/401); shards send it as handshake accessToken and as
//	           an Authorization: Bearer header. When empty, all shards are
//	           accepted (dev default).
//	SHARD_NAME shard identity in register/heartbeat frames (default "shard").
//	VERSION    explicit world version tag. When set, the shard reports it as
//	           its version instead of the derived buildID+gVer pair, and the
//	           router prefers the newest healthy RUNNING version. Distinct
//	           VERSION values (e.g. rolling deploys) coexist: old versions
//	           keep serving existing sessions (no new logins).
//	SHARD_REGIONS comma-separated region ids this shard simulates
//	           (e.g. "25,26,27"; empty = unscoped). Reported in register/
//	           heartbeat frames; the hub serves the region->shard lookup
//	           (LookupRegion) that handoff senders use to pick a target.
//
// R2 architecture (GO-PLAN §12 R2 / REWRITE-V2 V2-M2):
//
//	version rows: the hub tracks version (= explicit VERSION tag, else the
//	  buildID+gVer pair via VersionOf) per shard; multiple versions coexist.
//	  Router preferred = the newest healthy RUNNING version (NewestRunning:
//	  newest version wins; within a version the lowest-load shard wins, ties
//	  by latest registration then name). Old versions keep heartbeating and
//	  serving existing sessions but take no new logins.
//	handoff RPC: same-build shard-to-shard player transfer as JSON control
//	  objects on the existing hub sockets (no new packet opcode, relay
//	  envelope reuse for chat only): sender -> hub {"t":"handoff",...} ->
//	  target; target -> hub {"t":"handoff-ack",...} -> sender. The hub gates
//	  on version equality (mismatch => immediate reject ack; the transfer
//	  must go disconnect+reconnect via login/hub, never a bare Teleport
//	  across builds) and forwards to the target's live socket. The payload
//	  is opaque to the hub (player persist snapshot + quests + region/pos).
//	  Same-build move = seamless socket re-point: the target pre-loads the
//	  transferred rows, the sender notifies its client (existing
//	  Notification-25) with the target addr, despawns and disconnects; the
//	  client re-opens its game WS at the target (no page reload, no asset
//	  refetch — same build) and logs in normally, landing on the
//	  transferred state; the only Teleport emitted lives inside the target
//	  shard's own socket flow.
//	refresh banner: the hub pushes the preferred version inside every roster
//	  push; shards compare it with their own version and emit the existing
//	  Chat-19 frame ("new version available — refresh when ready", no new
//	  opcode), rate-limited to once per session per version change.
//	fanout: guild/global resolve across shards via hub presence: the sender
//	  relays one [53, ...] envelope per remote recipient (direct-chat scope
//	  parity, D4) and each owning shard delivers locally.
//
// Wire conventions (mirrors the TS reference, read-only):
//
//   - register = handshake frame: [1, null, {type:"hub",name,serverId,
//     accessToken,players}] — client.ts handleOpen sends HandshakePacket,
//     HandshakePacketData.type "hub" (packages/common/network/impl/
//     handshake.ts); Packet.Handshake == 1 (packages/common/network/
//     packets.ts, go protocol.PacketHandshake).
//   - relay envelope = [53, null, [username, innerFrame]] — RelayPacket
//     (packages/common/network/impl/relay.ts:
//     super(Packets.Relay, undefined, [username, [...packet.serialize()]]));
//     Packets.Relay == 53; hub->world delivery is verbatim
//     (controllers/incoming.ts handleRelay). Here innerFrame is the JSON
//     encoding of Message.Payload: game frames ([]any{packet,opcode,data})
//     pass through byte-identical.
//   - heartbeat cadence 5s mirrors client.ts handleError/handleClose retry
//     ("attempting again in 5 seconds"); the TS hub has no heartbeat packet,
//     so heartbeats (and roster pushes) are small JSON objects
//     {"t":"heartbeat"|"roster",...} on the same socket — new surface only,
//     no existing packet shape changes.
//   - eviction: shard idle > 3 missed beats (3 * 5s = 15s) is dropped from
//     the server-list and the roster is re-pushed.
package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"

	"rpg-world-server/internal/version"
)

// Env keys for the hub transport.
const (
	// EnvHubAddr gates everything in this file. Empty => all-in-one mode.
	EnvHubAddr = "HUB_ADDR"
	// EnvHubToken is the shared secret (bearer).
	EnvHubToken = "HUB_TOKEN"
	// EnvShardName is the shard identity (default "shard").
	EnvShardName = "SHARD_NAME"
	// EnvVersion is the explicit world version tag (default: derived
	// buildID+gVer pair via VersionOf).
	EnvVersion = "VERSION"
	// EnvShardRegions is the comma-separated region-id list the shard
	// simulates (default: unscoped).
	EnvShardRegions = "SHARD_REGIONS"
	// EnvServerID is the world id this shard claims on the hub
	// (config.serverId parity, default 1). The SAME value is posted to
	// /isOnline, so the hub must know the shard by it: an auto-allocated id
	// that disagrees with SERVER_ID makes the router report the shard's own
	// players as online elsewhere (every login rejected 'loggedin').
	EnvServerID = "SERVER_ID"
)

// ServerIDFromEnv resolves the shard's claimed world id (SERVER_ID, default 1;
// junk and non-positive values fall back to 1).
func ServerIDFromEnv() int {
	n := 0
	for _, r := range strings.TrimSpace(os.Getenv(EnvServerID)) {
		if r < '0' || r > '9' {
			return 1
		}
		n = n*10 + int(r-'0')
		if n > 1<<16 {
			return 1
		}
	}
	if n <= 0 {
		return 1
	}
	return n
}

// Transport cadence. HeartbeatInterval mirrors the TS 5s hub retry cadence;
// shards idle longer than EvictAfter (3 missed beats) are evicted.
const (
	HeartbeatInterval  = 5 * time.Second
	MissedBeatsToEvict = 3
	EvictAfter         = MissedBeatsToEvict * HeartbeatInterval

	// ReconnectBase/Max bound the shard dial backoff (1s doubling to 30s).
	ReconnectBase = 1 * time.Second
	ReconnectMax  = 30 * time.Second
)

// Frame packet ids (parity with packages/common/network/packets.ts and
// go-server/internal/protocol; kept local so hub stays dependency-free).
const (
	frameHandshake = 1
	frameRelay     = 53
)

// Enabled reports whether multi-server hub mode is on. Everything new in
// this file is gated behind it; the default (no HUB_ADDR) path never
// constructs a Server/Client and behaves exactly as before.
func Enabled() bool { return os.Getenv(EnvHubAddr) != "" }

// SharedToken returns the HUB_TOKEN secret ("" allows all — dev default).
//
// Auth posture (fail-closed for multi-server roles, open for all-in-one):
//   - all-in-one (default, no HUB_ADDR): the hub transport never runs and
//     an empty token is the documented dev default (everything local).
//   - router/shard roles: an empty HUB_TOKEN is a misconfiguration —
//     see TokenRequiredForRole. The hub Server itself stays permissive
//     when constructed with "" (so unit tests and the all-in-one path
//     keep working); the app driver (ROLE=router) refuses to serve
//     without a token, and shard operators must set one (documented;
//     the shard dial carries it as bearer + handshake accessToken).
func SharedToken() string { return os.Getenv(EnvHubToken) }

// TokenRequiredForRole reports whether role must authenticate to the hub.
// Router and shard roles are fail-closed (require HUB_TOKEN); every other
// role — including the all-in-one dev default — stays open. Case-insensitive;
// unknown/empty roles are treated as all-in-one (open).
func TokenRequiredForRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "router", "shard":
		return true
	default:
		return false
	}
}

// ShardName returns the SHARD_NAME identity (default "shard").
func ShardName() string {
	if n := os.Getenv(EnvShardName); n != "" {
		return n
	}
	return "shard"
}

// VersionTag returns the explicit VERSION tag ("" = derive from the
// buildID+gVer pair via VersionOf).
func VersionTag() string { return strings.TrimSpace(os.Getenv(EnvVersion)) }

// VersionOf resolves the world version for one shard: the explicit tag when
// set, else the buildID+gVer pair (either half alone when only one is
// stamped, "" when neither is — pre-R1 shards group as unknown).
func VersionOf(buildID, gVer, tag string) string {
	if tag != "" {
		return tag
	}
	switch {
	case buildID != "" && gVer != "":
		return buildID + "+" + gVer
	case buildID != "":
		return buildID
	default:
		return gVer
	}
}

// ShardRegions parses the SHARD_REGIONS region-id list (comma/space
// separated; empty = unscoped). Malformed entries are skipped.
func ShardRegions() []int { return ParseRegions(os.Getenv(EnvShardRegions)) }

// ParseRegions parses a comma/space-separated region-id list (testable core
// of ShardRegions).
func ParseRegions(raw string) []int {
	var out []int
	for _, f := range strings.Fields(strings.ReplaceAll(raw, ",", " ")) {
		var n int
		if _, err := fmt.Sscanf(f, "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ErrOfflineAuth is returned when a shard registration carries a bad token.
var ErrOfflineAuth = errors.New("hub: hub token mismatch")

// ErrForwarded is returned by RouteOrForward when a direct message missed
// locally and was handed to a remote shard over the hub socket. The caller
// must NOT fall back to local broadcast on this error (unlike ErrOffline).
var ErrForwarded = errors.New("hub: forwarded to remote shard")

// ErrNoRecipient is returned by mail Store when the message has no To.
var ErrNoRecipient = errors.New("hub: mail message has no recipient")

// ---------------------------------------------------------------------------
// Wire frames
// ---------------------------------------------------------------------------

// HubHandshake mirrors TS HubHandshakePacketData (hub variant): sent as the
// data of a [1, null, {...}] handshake frame on connect (client.ts
// handleOpen parity; remoteHost/port/maxPlayers omitted — the Go hub
// does not serve game clients or a server browser). R1 adds the additive
// routing stamps BuildID/GVer/State/Load/Addr (omitempty: pre-R1 shards
// register without them and are treated as RUNNING with load 0).
type HubHandshake struct {
	Type        string   `json:"type"` // always "hub"
	Name        string   `json:"name"`
	ServerID    int      `json:"serverId,omitempty"`
	AccessToken string   `json:"accessToken,omitempty"`
	Players     []string `json:"players,omitempty"`
	// BuildID is the shard binary stamp (version.BuildID via ldflags).
	BuildID string `json:"buildId,omitempty"`
	// GVer is the shard wire version (must equal version.GVer to take
	// matching clients; the router prefers newest RUNNING regardless).
	GVer string `json:"gVer,omitempty"`
	// State is the drain state (RUNNING/DRAINING; "" means RUNNING).
	State string `json:"state,omitempty"`
	// Load is the shard's live-conn count (router info only).
	Load int `json:"load,omitempty"`
	// Addr is the shard's game address for login redirect
	// (e.g. "127.0.0.1:9001"; "" falls back to the shard name).
	Addr string `json:"addr,omitempty"`
	// Version is the explicit VERSION tag (R2). When empty the hub derives
	// the version via VersionOf(BuildID, GVer, "").
	Version string `json:"version,omitempty"`
	// Regions is the SHARD_REGIONS scope (R2 region->shard lookup). Nil =
	// unscoped (no constraint); non-nil replaces the stored scope.
	Regions []int `json:"regions,omitempty"`
}

// encodeFrame serializes a TS-style [packet, opcode, data] array frame.
func encodeFrame(packet int, opcode any, data any) ([]byte, error) {
	return json.Marshal([]any{packet, opcode, data})
}

// decodeFrame parses a [packet, opcode, data] array frame.
func decodeFrame(raw []byte) (packet int, opcode, data json.RawMessage, err error) {
	var parts []json.RawMessage
	if err = json.Unmarshal(raw, &parts); err != nil {
		return 0, nil, nil, err
	}
	if len(parts) != 3 {
		return 0, nil, nil, errors.New("hub: bad frame length")
	}
	if err = json.Unmarshal(parts[0], &packet); err != nil {
		return 0, nil, nil, err
	}
	return packet, parts[1], parts[2], nil
}

// encodeRelay wraps inner (already JSON) in the [53, null, [user, inner]]
// relay envelope (RelayPacket parity).
func encodeRelay(username string, inner json.RawMessage) ([]byte, error) {
	if inner == nil {
		inner = json.RawMessage("null")
	}
	data := []any{username, json.RawMessage(inner)}
	return encodeFrame(frameRelay, nil, data)
}

// decodeRelay splits a [53, null, [user, inner]] envelope.
func decodeRelay(raw []byte) (username string, inner json.RawMessage, err error) {
	packet, _, data, err := decodeFrame(raw)
	if err != nil {
		return "", nil, err
	}
	if packet != frameRelay {
		return "", nil, errors.New("hub: not a relay frame")
	}
	var parts []json.RawMessage
	if err = json.Unmarshal(data, &parts); err != nil || len(parts) != 2 {
		return "", nil, errors.New("hub: bad relay envelope")
	}
	if err = json.Unmarshal(parts[0], &username); err != nil {
		return "", nil, err
	}
	return username, parts[1], nil
}

// isJSONObject reports whether raw is a JSON object (hub control message:
// heartbeat/roster) as opposed to an array frame.
func isJSONObject(raw []byte) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

// heartbeatMsg is the shard->hub heartbeat (full local player list doubles
// as presence reconciliation, so no separate online/offline frames exist).
// State/Load are the R1 routing stamps (State "" = no change, Load always
// set: the shard reports its live-conn count every beat).
type heartbeatMsg struct {
	Type    string   `json:"t"` // "heartbeat"
	Name    string   `json:"name"`
	Players []string `json:"players,omitempty"`
	State   string   `json:"state,omitempty"`
	Load    int      `json:"load,omitempty"`
	// Version is the R2 world version ("" = no change, like State).
	Version string `json:"version,omitempty"`
	// Regions is the R2 scope (nil = no change; non-nil replaces).
	Regions []int `json:"regions,omitempty"`
}

// rosterMsg is the hub->shard roster push: every known online player and the
// shard hosting them (drives RouteOrForward remote decisions), plus the
// preferred world version (drives the refresh banner).
type rosterMsg struct {
	Type      string            `json:"t"` // "roster"
	Players   map[string]string `json:"players"`
	Preferred string            `json:"preferred,omitempty"`
}

// ---------------------------------------------------------------------------
// Cross-shard handoff (R2)
// ---------------------------------------------------------------------------

// Handoff control object types (JSON objects on the hub socket, heartbeat/
// roster parity — no packet-shape changes).
const (
	handoffType    = "handoff"
	handoffAckType = "handoff-ack"
)

// HandoffRequest is one same-build player transfer: the sender flushes its
// persist rows (players/inventory/quests), packs the snapshot opaquely, and
// the receiver pre-loads it so the re-pointed client lands on live state.
// State/Quests are opaque to the hub (game-owned JSON).
type HandoffRequest struct {
	ID      string          `json:"id"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	Player  string          `json:"player"`
	BuildID string          `json:"buildId,omitempty"`
	GVer    string          `json:"gVer,omitempty"`
	Version string          `json:"version,omitempty"`
	State   json.RawMessage `json:"state,omitempty"`
	Quests  json.RawMessage `json:"quests,omitempty"`
	Region  int             `json:"region,omitempty"`
	X       int             `json:"x,omitempty"`
	Y       int             `json:"y,omitempty"`
}

// HandoffAck answers a HandoffRequest (To = the original sender shard).
// Ok=false (e.g. build mismatch, unknown/unavailable target) means the
// transfer never started: the sender keeps its session (cross-build moves
// must go disconnect+reconnect via login/hub, never a bare Teleport).
// Addr is the receiver's game address for the client socket re-point.
type HandoffAck struct {
	ID     string `json:"id"`
	To     string `json:"to"`
	Ok     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Addr   string `json:"addr,omitempty"`
}

// handoffMsg is the on-socket form of HandoffRequest (t="handoff").
type handoffMsg struct {
	Type string `json:"t"`
	HandoffRequest
}

// handoffAckMsg is the on-socket form of HandoffAck (t="handoff-ack").
type handoffAckMsg struct {
	Type string `json:"t"`
	HandoffAck
}

// encodeHandoff wraps req in its control object.
func encodeHandoff(req HandoffRequest) ([]byte, error) {
	return json.Marshal(handoffMsg{Type: handoffType, HandoffRequest: req})
}

// decodeHandoff parses a control object into its request.
func decodeHandoff(raw []byte) (HandoffRequest, error) {
	var m handoffMsg
	if err := json.Unmarshal(raw, &m); err != nil || m.Type != handoffType {
		return HandoffRequest{}, errors.New("hub: not a handoff frame")
	}
	return m.HandoffRequest, nil
}

// encodeHandoffAck wraps ack in its control object.
func encodeHandoffAck(ack HandoffAck) ([]byte, error) {
	return json.Marshal(handoffAckMsg{Type: handoffAckType, HandoffAck: ack})
}

// decodeHandoffAck parses a control object into its ack.
func decodeHandoffAck(raw []byte) (HandoffAck, error) {
	var m handoffAckMsg
	if err := json.Unmarshal(raw, &m); err != nil || m.Type != handoffAckType {
		return HandoffAck{}, errors.New("hub: not a handoff-ack frame")
	}
	return m.HandoffAck, nil
}

// HandoffCompatible reports whether a transfer may proceed on version
// grounds: equal versions pass; either side unknown ("") passes at the hub
// (the receiver still enforces its exact build/gVer gate on the payload).
func HandoffCompatible(fromVersion, toVersion string) bool {
	return fromVersion == "" || toVersion == "" || fromVersion == toVersion
}

// ---------------------------------------------------------------------------
// Offline mailer
// ---------------------------------------------------------------------------

// MailStore persists offline mail. Implementations must be safe for
// concurrent use. Payloads round-trip through JSON (documented lossy for Go
// values without a JSON mapping, e.g. KindGuild []string candidates decode
// as []any — the mailer preserves bytes, not Go types).
type MailStore interface {
	Store(m Message) error
	Take(username string) ([]Message, error)
}

// MemoryMail is the default in-memory MailStore (exact-match usernames,
// like Router; the caller normalizes case at the boundary).
//
// Per-recipient cap + TTL (data-loss and unbounded-growth guard): each
// recipient holds at most MaxMailPerRecipient entries (Store drops the
// oldest when full) and entries older than MailTTL are expired on Take
// (and opportunistically on Store). Both bounds are documented tunables;
// use SetLimitsForTests to inject a clock/cap in unit tests.
type MemoryMail struct {
	mu  sync.Mutex
	box map[string][]mailEntry
	// maxPerRecipient caps one recipient's queue (<=0 = DefaultMaxMailPerRecipient).
	maxPerRecipient int
	// ttl bounds entry age (<=0 = DefaultMailTTL).
	ttl time.Duration
	// now supplies the clock (nil = time.Now; tests inject a fake).
	now func() time.Time
}

// Default bounds for MemoryMail.
const (
	// DefaultMaxMailPerRecipient caps one recipient's offline queue; the
	// oldest entry is dropped when a Store would exceed it.
	DefaultMaxMailPerRecipient = 50
	// DefaultMailTTL bounds offline-mail age; Take expires older entries.
	DefaultMailTTL = 24 * time.Hour
)

// mailEntry is one queued message plus its arrival time (TTL basis).
type mailEntry struct {
	msg Message
	at  time.Time
}

// NewMemoryMail returns an empty MemoryMail with default bounds.
func NewMemoryMail() *MemoryMail {
	return &MemoryMail{box: make(map[string][]mailEntry)}
}

// mailNow returns the clock (time.Now when unset).
func (m *MemoryMail) mailNow() time.Time {
	if m != nil && m.now != nil {
		return m.now()
	}
	return time.Now()
}

// mailCap returns the effective per-recipient cap.
func (m *MemoryMail) mailCap() int {
	if m != nil && m.maxPerRecipient > 0 {
		return m.maxPerRecipient
	}
	return DefaultMaxMailPerRecipient
}

// mailTTL returns the effective TTL.
func (m *MemoryMail) mailTTL() time.Duration {
	if m != nil && m.ttl > 0 {
		return m.ttl
	}
	return DefaultMailTTL
}

// SetLimitsForTests injects cap/ttl/clock for unit tests (nil clock =
// time.Now). Not for production use.
func (m *MemoryMail) SetLimitsForTests(cap int, ttl time.Duration, now func() time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxPerRecipient = cap
	m.ttl = ttl
	m.now = now
}

// Store queues m for m.To, expiring TTL-old entries for the recipient
// first, then dropping the oldest when the queue would exceed the
// per-recipient cap.
func (m *MemoryMail) Store(msg Message) error {
	if msg.To == "" {
		return ErrNoRecipient
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.mailNow()
	ttl := m.mailTTL()
	cap := m.mailCap()
	q := m.box[msg.To]
	if ttl > 0 {
		kept := q[:0]
		for _, e := range q {
			if now.Sub(e.at) < ttl {
				kept = append(kept, e)
			}
		}
		q = kept
	}
	q = append(q, mailEntry{msg: msg, at: now})
	for len(q) > cap && cap > 0 {
		q = q[len(q)-cap:]
	}
	m.box[msg.To] = q
	return nil
}

// Take pops all queued, non-expired mail for username (nil when empty).
func (m *MemoryMail) Take(username string) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.box[username]
	delete(m.box, username)
	if len(q) == 0 {
		return nil, nil
	}
	ttl := m.mailTTL()
	now := m.mailNow()
	out := make([]Message, 0, len(q))
	for _, e := range q {
		if ttl > 0 && now.Sub(e.at) >= ttl {
			continue
		}
		out = append(out, e.msg)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// sqliteMailSchema is owned by the hub package and purely additive: one
// table, no changes to any existing schema.
const sqliteMailSchema = `CREATE TABLE IF NOT EXISTS hub_mail(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	recipient TEXT NOT NULL,
	sender TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	payload TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hub_mail_recipient ON hub_mail(recipient, id);`

// SQLiteMail is a file-backed MailStore (same exact-match semantics as
// MemoryMail). Open with OpenSQLiteMail; Close releases the handle. All
// methods hold an internal mutex and Take runs in a single transaction so
// concurrent Store/Take callers cannot interleave a SELECT with a DELETE
// (the pre-fix Take raced: two takers could both SELECT the same rows
// before either DELETEd them).
type SQLiteMail struct {
	mu sync.Mutex
	db *sql.DB
}

// OpenSQLiteMail opens path (e.g. "hub_mail.db" or ":memory:" for tests)
// and ensures the additive hub_mail table exists.
func OpenSQLiteMail(path string) (*SQLiteMail, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(sqliteMailSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteMail{db: db}, nil
}

// Close releases the database handle.
func (s *SQLiteMail) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// Store queues m for m.To with its payload JSON-encoded.
func (s *SQLiteMail) Store(m Message) error {
	if m.To == "" {
		return ErrNoRecipient
	}
	if s == nil || s.db == nil {
		return errors.New("hub: nil mail db")
	}
	raw, err := json.Marshal(m.Payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(
		`INSERT INTO hub_mail(recipient, sender, kind, payload, created_at) VALUES(?,?,?,?,?)`,
		m.To, m.From, string(m.Kind), string(raw), time.Now().Unix(),
	)
	return err
}

// Take pops all queued mail for username in FIFO order atomically: one
// BEGIN IMMEDIATE transaction covers the SELECT and the DELETE, so two
// concurrent Takers cannot both observe the same rows.
func (s *SQLiteMail) Take(username string) ([]Message, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("hub: nil mail db")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.Query(
		`SELECT id, sender, kind, payload FROM hub_mail WHERE recipient=? ORDER BY id`, username)
	if err != nil {
		return nil, err
	}
	var out []Message
	var ids []int64
	for rows.Next() {
		var id int64
		var from, kind, payload string
		if err := rows.Scan(&id, &from, &kind, &payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var p any
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, Message{From: from, To: username, Kind: Kind(kind), Payload: p})
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM hub_mail WHERE id=?`, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return out, nil
}

// Mailer fronts a MailStore with the login-path delivery helper. The zero
// value is not usable; build with NewMailer.
type Mailer struct {
	store MailStore
}

// NewMailer returns a Mailer over store, or an in-memory store when nil.
func NewMailer(store MailStore) *Mailer {
	if store == nil {
		store = NewMemoryMail()
	}
	return &Mailer{store: store}
}

// Store queues a direct message for an offline player. Messages without To
// (guild fanout, global) are caller-routed and rejected here.
func (m *Mailer) Store(msg Message) error { return m.store.Store(msg) }

// Deliver pops all pending mail for username. Call on the login path right
// after Router.Register; the caller delivers each message locally. Returns
// nil when nothing is pending.
func (m *Mailer) Deliver(username string) []Message {
	if m == nil {
		return nil
	}
	out, err := m.store.Take(username)
	if err != nil {
		log.Printf("hub: mail deliver for %q: %v", username, err)
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Hub-side Server
// ---------------------------------------------------------------------------

// shardEntry is one registered world process.
type shardEntry struct {
	conn *websocket.Conn // nil in unit tests (transport-free)
	wmu  sync.Mutex      // guards conn writes
	name string
	// id is the hub-assigned world id the client sees on
	// SerializedServer.id (TS hub: Server.id assigned at registration).
	id       int
	players  map[string]struct{}
	lastBeat time.Time
	// R1 routing stamps (from HubHandshake/heartbeat; see Register).
	buildID   string
	gVer      string
	state     string // "" means RUNNING (pre-R1 shards)
	load      int
	addr      string
	firstSeen time.Time // registration order = build newness
	// R2 version + region scope. version is the explicit VERSION tag or the
	// derived buildID+gVer pair ("" = unknown, pre-R1); regions is nil when
	// the shard never reported a scope (unscoped).
	version string
	regions []int
}

// ShardInfo is the transport-free snapshot of one shardEntry for the
// router server-list (see Server.ListShards / NewestRunning).
type ShardInfo struct {
	// ID is the hub-assigned world id (SerializedServer.id). 0 only on the
	// zero ShardInfo returned with ok=false.
	ID      int
	Name    string
	Addr    string
	BuildID string
	GVer    string
	State   string
	Load    int
	// Newest is true on the preferred-version pick (login target for NEW
	// sessions) when rendered via ListShards.
	Newest bool
	// Version is the R2 world version (explicit VERSION tag or the
	// buildID+gVer pair; "" = unknown).
	Version string
	// Regions is the shard's reported scope (nil = unscoped).
	Regions []int
	// FirstSeen is the registration order (build newness basis for
	// newestRunningLocked and PreviousRunning tie-breaks).
	FirstSeen time.Time
}

// effectiveState normalizes "" (pre-R1 shard) to RUNNING.
func effectiveState(state string) string {
	if state == "" {
		return version.StateRunning
	}
	return state
}

// Server is the hub-side server-list: it accepts shard registrations,
// tracks last-heartbeat per shard, evicts shards idle longer than
// EvictAfter, routes relay frames to the owning shard, and stores mail for
// players online nowhere. Safe for concurrent use.
//
// The zero value is not usable; build with NewServer. now is a test hook
// (defaults to time.Now).
type Server struct {
	mu    sync.Mutex
	token string
	mail  *Mailer
	// nextID is the sequential world-id allocator (TS hub Server.id counter).
	nextID int
	shards map[string]*shardEntry
	// playerShard maps username -> hosting shard name.
	playerShard map[string]string
	now         func() time.Time

	// Upgrader handles shard sockets (CheckOrigin allow-all mirrors the
	// game stub transport; the socket is token-authenticated instead).
	Upgrader websocket.Upgrader
}

// NewServer returns a Server enforcing token ("" allows all) with mail for
// nowhere-online recipients (nil => in-memory default).
func NewServer(token string, mail *Mailer) *Server {
	if mail == nil {
		mail = NewMailer(nil)
	}
	return &Server{
		token:       token,
		mail:        mail,
		shards:      make(map[string]*shardEntry),
		playerShard: make(map[string]string),
		now:         time.Now,
		Upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
	}
}

// SetNowForTests injects the clock used for firstSeen/lastBeat (tests
// outside this package). A nil fn restores time.Now.
func (s *Server) SetNowForTests(fn func() time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if fn == nil {
		fn = time.Now
	}
	s.now = fn
}

// checkAuth verifies a registration: when the server token is set, either
// the Authorization bearer header (checked at upgrade) or the handshake
// accessToken must match. Mismatches are rejected and logged by the caller.
func (s *Server) checkAuth(headerToken, handshakeToken string) bool {
	if s.token == "" {
		return true
	}
	return headerToken == s.token || handshakeToken == s.token
}

// bearerToken parses an "Authorization: Bearer <token>" header value.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// Register records (or refreshes) a shard from its handshake: verifies
// auth, upserts the entry, adopts the handshake player list, records the R1
// routing stamps (buildID/gVer/state/load/addr) plus the R2 version
// (explicit tag or derived pair) and region scope, and stamps last-heartbeat.
// firstSeen tracks registration order (build newness for NewestRunning): a
// brand-new name sets it; a re-register keeps it unless the buildID or the
// version changed (same-name redeploy of a new build counts as new).
// Transport-free (unit-testable); the socket is attached separately by
// ServeHTTP via Attach.
func (s *Server) Register(hs HubHandshake) error {
	if hs.Name == "" || hs.Type != "hub" {
		return errors.New("hub: bad handshake")
	}
	if !s.checkAuth("", hs.AccessToken) {
		log.Printf("hub: shard %q rejected (token mismatch)", hs.Name)
		return ErrOfflineAuth
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	ver := hs.Version
	if ver == "" {
		ver = VersionOf(hs.BuildID, hs.GVer, "")
	}
	e := s.shards[hs.Name]
	if e == nil {
		s.nextID++
		id := s.nextID
		// A shard may pin its own id (HubHandshake.ServerID, SERVER_ID env);
		// otherwise the hub allocates the next sequential world id, exactly
		// like the TS hub's serverId counter. Pinned ids still advance the
		// counter so later auto-ids never collide.
		if hs.ServerID > 0 {
			id = hs.ServerID
			if id > s.nextID {
				s.nextID = id
			}
		}
		e = &shardEntry{
			name:      hs.Name,
			id:        id,
			players:   make(map[string]struct{}),
			firstSeen: now,
		}
		s.shards[hs.Name] = e
	} else if (hs.BuildID != "" && hs.BuildID != e.buildID) ||
		(hs.GVer != "" && hs.GVer != e.gVer) ||
		(hs.Version != "" && hs.Version != e.version) {
		// Same-name redeploy of a new build: it is newer than everything
		// registered before it (buildID, wire GVer, or explicit version
		// change all count as new).
		e.firstSeen = now
	}
	e.players = make(map[string]struct{}, len(hs.Players))
	for _, p := range hs.Players {
		if p != "" {
			e.players[p] = struct{}{}
		}
	}
	if hs.BuildID != "" {
		e.buildID = hs.BuildID
	}
	if hs.GVer != "" {
		e.gVer = hs.GVer
	}
	if hs.State != "" {
		e.state = hs.State
	}
	e.load = hs.Load
	if hs.Addr != "" {
		e.addr = hs.Addr
	}
	if hs.Version != "" || hs.BuildID != "" || hs.GVer != "" {
		e.version = ver
	}
	if hs.Regions != nil {
		e.regions = append([]int(nil), hs.Regions...)
	}
	e.lastBeat = now
	s.rebuildLocked()
	return nil
}

// Heartbeat refreshes a shard's last-heartbeat and reconciles its player
// list (full-list replace). Unknown shards are rejected so a restarted hub
// forces a clean re-register. Routing stamps are preserved (use
// HeartbeatEx to move them).
func (s *Server) Heartbeat(name string, players []string) error {
	return s.HeartbeatEx(name, players, "", -1)
}

// HeartbeatEx is Heartbeat plus the R1 routing stamps: a non-"" state moves
// the drain state (RUNNING/DRAINING), and a non-negative load replaces it.
// Heartbeats carrying neither leave the stamps untouched (pre-R1 shape).
// The R2 version/scope are preserved (use HeartbeatFull to move them).
func (s *Server) HeartbeatEx(name string, players []string, state string, load int) error {
	return s.HeartbeatFull(name, players, state, load, "", nil)
}

// HeartbeatFull is HeartbeatEx plus the R2 version and region scope: a
// non-"" version replaces the world version, and a non-nil regions list
// replaces the scope (nil preserves both, so plain heartbeats never clobber
// the register stamps).
func (s *Server) HeartbeatFull(name string, players []string, state string, load int, version string, regions []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.shards[name]
	if e == nil {
		return errors.New("hub: unknown shard")
	}
	e.players = make(map[string]struct{}, len(players))
	for _, p := range players {
		if p != "" {
			e.players[p] = struct{}{}
		}
	}
	if state != "" {
		e.state = state
	}
	if load >= 0 {
		e.load = load
	}
	if version != "" {
		e.version = version
	}
	if regions != nil {
		e.regions = append([]int(nil), regions...)
	}
	e.lastBeat = s.now()
	s.rebuildLocked()
	return nil
}

// Attach binds a live socket to a registered shard (called by ServeHTTP
// after Register). Detach unbinds on disconnect (presence is kept until
// eviction so a flapping socket does not drop the server-list entry).
func (s *Server) Attach(name string, conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.shards[name]; e != nil {
		e.conn = conn
	}
}

// Detach unbinds a socket from a shard (conn identity-checked).
func (s *Server) Detach(name string, conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.shards[name]; e != nil && e.conn == conn {
		e.conn = nil
	}
}

// FindPlayer reports the shard hosting username, if any.
func (s *Server) FindPlayer(username string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	shard, ok := s.playerShard[username]
	return shard, ok
}

// ShardCount reports the number of registered shards.
func (s *Server) ShardCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.shards)
}

// NewestRunning reports the login target for NEW sessions: a shard of the
// newest healthy RUNNING world version (R2 — not just the newest shard).
// Version newness is the latest firstSeen among the version's RUNNING
// shards; within the winning version the lowest-load shard wins (ties: latest
// firstSeen, then name ascending). DRAINING shards are skipped (they keep
// existing players, take no new sessions); "" state counts as RUNNING for
// pre-R1 shards. Evicted (3-miss) shards are gone from the table, so they
// never win. ok is false when no RUNNING shard is registered — the caller
// must not start new sessions anywhere (clients keep current sessions).
// Transport-free.
func (s *Server) NewestRunning() (ShardInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best, ok := s.newestRunningLocked()
	if !ok {
		return ShardInfo{}, false
	}
	return s.infoLocked(best, true), true
}

// newestRunningLocked is NewestRunning under the lock (shared with
// ListShards/PreferredVersion/pushRoster).
func (s *Server) newestRunningLocked() (*shardEntry, bool) {
	// Newness per version: latest firstSeen among its RUNNING shards.
	newness := map[string]time.Time{}
	for _, e := range s.shards {
		if effectiveState(e.state) != version.StateRunning {
			continue
		}
		if t, ok := newness[e.version]; !ok || e.firstSeen.After(t) {
			newness[e.version] = e.firstSeen
		}
	}
	if len(newness) == 0 {
		return nil, false
	}
	bestVer := ""
	var bestNew time.Time
	first := true
	for ver, t := range newness {
		if first || t.After(bestNew) || (t.Equal(bestNew) && ver > bestVer) {
			bestVer, bestNew, first = ver, t, false
		}
	}
	var best *shardEntry
	for _, e := range s.shards {
		if effectiveState(e.state) != version.StateRunning || e.version != bestVer {
			continue
		}
		if best == nil || e.load < best.load ||
			(e.load == best.load && (e.firstSeen.After(best.firstSeen) ||
				(e.firstSeen.Equal(best.firstSeen) && e.name < best.name))) {
			best = e
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// PreferredVersion reports the newest healthy RUNNING world version (the
// version NewestRunning serves; "" with ok=false when no RUNNING shard is
// registered). Transport-free; pushed to shards inside every roster push
// (drives the refresh banner).
func (s *Server) PreferredVersion() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best, ok := s.newestRunningLocked()
	if !ok {
		return "", false
	}
	return best.version, true
}

// RegionsOf snapshots one shard's reported scope (nil = unscoped or
// unknown shard). Transport-free.
func (s *Server) RegionsOf(name string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.shards[name]; e != nil {
		return append([]int(nil), e.regions...)
	}
	return nil
}

// LookupRegion resolves a region id to its owning shard for handoff target
// selection (R2 region->shard table): the RUNNING scoped shard claiming the
// region (lowest load wins, ties by latest firstSeen then name). Unscoped
// shards (nil regions) match nothing here — handoff senders fall back to an
// explicit target or the preferred version. Transport-free.
func (s *Server) LookupRegion(region int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *shardEntry
	for _, e := range s.shards {
		if effectiveState(e.state) != version.StateRunning || e.regions == nil {
			continue
		}
		claims := false
		for _, r := range e.regions {
			if r == region {
				claims = true
				break
			}
		}
		if !claims {
			continue
		}
		if best == nil || e.load < best.load ||
			(e.load == best.load && (e.firstSeen.After(best.firstSeen) ||
				(e.firstSeen.Equal(best.firstSeen) && e.name < best.name))) {
			best = e
		}
	}
	if best == nil {
		return "", false
	}
	return best.name, true
}

// CheckHandoff gates one transfer on transport-free grounds (used by the
// live relay and unit-testable): both shards registered, distinct, versions
// compatible (see HandoffCompatible — mismatch must go
// disconnect+reconnect, never a bare Teleport across builds).
func (s *Server) CheckHandoff(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if from == "" || to == "" || from == to {
		return errors.New("hub: bad handoff target")
	}
	src := s.shards[from]
	dst := s.shards[to]
	if src == nil || dst == nil {
		return errors.New("hub: unknown handoff shard")
	}
	if !HandoffCompatible(src.version, dst.version) {
		return fmt.Errorf("hub: handoff %s (%q) -> %s (%q): build mismatch",
			from, src.version, to, dst.version)
	}
	return nil
}

// ListShards snapshots every registered shard grouped by version newness
// (newest version first, same order as NewestRunning), flagging the login
// target: every RUNNING shard of the preferred version. Within a version
// shards sort newest-first by (firstSeen desc, name asc). Transport-free;
// backs the router GET /servers server-list.
func (s *Server) ListShards() []ShardInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	best, _ := s.newestRunningLocked()
	var prefVer string
	hasPref := best != nil
	if hasPref {
		prefVer = best.version
	}
	// Version newness: latest firstSeen among each version's RUNNING
	// shards (same basis as newestRunningLocked), so shards group by
	// version even when an old-version shard registers last.
	newness := map[string]time.Time{}
	for _, e := range s.shards {
		if effectiveState(e.state) != version.StateRunning {
			continue
		}
		if t, ok := newness[e.version]; !ok || e.firstSeen.After(t) {
			newness[e.version] = e.firstSeen
		}
	}
	out := make([]ShardInfo, 0, len(s.shards))
	for _, e := range s.shards {
		out = append(out, s.infoLocked(e, hasPref &&
			effectiveState(e.state) == version.StateRunning && e.version == prefVer))
	}
	sort.Slice(out, func(i, j int) bool {
		ni, nj := newness[out[i].Version], newness[out[j].Version]
		hasI, hasJ := true, true
		if _, ok := newness[out[i].Version]; !ok {
			hasI = false
		}
		if _, ok := newness[out[j].Version]; !ok {
			hasJ = false
		}
		// RUNNING versions (in newness) sort before non-RUNNING ones.
		if hasI != hasJ {
			return hasI
		}
		if hasI && hasJ && !ni.Equal(nj) {
			return ni.After(nj)
		}
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.After(out[j].FirstSeen)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// infoLocked snapshots e. Caller holds mu.
func (s *Server) infoLocked(e *shardEntry, newest bool) ShardInfo {
	return ShardInfo{
		ID:   e.id,
		Name: e.name, Addr: e.addr, BuildID: e.buildID, GVer: e.gVer,
		State: effectiveState(e.state), Load: e.load, Newest: newest,
		Version: e.version, Regions: append([]int(nil), e.regions...),
		FirstSeen: e.firstSeen,
	}
}

// Roster snapshots username -> shard for every known online player,
// sorted keys available via SortedRoster.
func (s *Server) Roster() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.playerShard))
	for u, sh := range s.playerShard {
		out[u] = sh
	}
	return out
}

// EvictIdle drops shards idle longer than EvictAfter (3 missed 5s beats)
// and returns their names, sorted. Call it from StartSweeper or on any
// cadence the embedder owns; Register/Heartbeat also reconcile the roster
// but never evict (eviction is time-based only).
func (s *Server) EvictIdle() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evictLocked(s.now())
}

// StartSweeper evicts idle shards plus pushes the roster on EvictAfter
// cadence until ctx ends. Optional: without it, call EvictIdle manually.
func (s *Server) StartSweeper(ctx context.Context) {
	go func() {
		t := time.NewTicker(EvictAfter)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if evicted := s.EvictIdle(); len(evicted) > 0 {
					s.pushRoster()
				}
			}
		}
	}()
}

// evictLocked removes shards idle past EvictAfter. Caller holds mu.
func (s *Server) evictLocked(now time.Time) []string {
	var evicted []string
	for name, e := range s.shards {
		if now.Sub(e.lastBeat) > EvictAfter {
			if e.conn != nil {
				_ = e.conn.Close()
				e.conn = nil
			}
			delete(s.shards, name)
			evicted = append(evicted, name)
			log.Printf("hub: shard %q evicted (3 missed heartbeats)", name)
		}
	}
	if len(evicted) > 0 {
		sort.Strings(evicted)
		s.rebuildLocked()
	}
	return evicted
}

// rebuildLocked recomputes playerShard from all entries. Caller holds mu.
func (s *Server) rebuildLocked() {
	s.playerShard = make(map[string]string)
	for name, e := range s.shards {
		for p := range e.players {
			s.playerShard[p] = name
		}
	}
}

// Relay routes one [53, ...] envelope to the shard hosting its target
// player, or stores central offline mail when the player is online nowhere
// (or the owning socket is down or the write fails). raw is forwarded
// verbatim (handleRelay parity). Queued hub mail drains on the shard's next
// register/heartbeat via deliverPending. Transport-free except the
// live-socket write.
func (s *Server) Relay(raw []byte) {
	username, _, err := decodeRelay(raw)
	if err != nil {
		log.Printf("hub: bad relay frame: %v", err)
		return
	}
	s.mu.Lock()
	shardName, ok := s.playerShard[username]
	var target *shardEntry
	if ok {
		target = s.shards[shardName]
	}
	s.mu.Unlock()
	if !ok || target == nil {
		s.storeOffline(username, raw)
		return
	}
	target.wmu.Lock()
	connDown := target.conn == nil
	writeErr := false
	if !connDown {
		_ = target.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := target.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
			log.Printf("hub: relay to shard %q: %v", shardName, err)
			writeErr = true
		}
	}
	target.wmu.Unlock()
	if connDown || writeErr {
		// Owning socket down (reconnect race) or write failed: keep as
		// offline mail instead of dropping; presence stays until eviction.
		s.storeOffline(username, raw)
	}
}

// storeOffline queues a relay envelope's content as central offline mail.
func (s *Server) storeOffline(username string, raw []byte) {
	var inner any
	if _, in, derr := decodeRelay(raw); derr == nil {
		_ = json.Unmarshal(in, &inner)
	}
	if err := s.mail.Store(Message{To: username, Kind: KindChat, Payload: inner}); err != nil {
		log.Printf("hub: offline store for %q: %v", username, err)
	}
}

// deliverPending pushes queued hub mail for the shard's players as relay
// envelopes. Called after attach and on every heartbeat so reconnects
// drain the queue. Shards without a live socket keep their mail queued.
// Mail is deleted only after a successful socket write: marshal/encode or
// write failures re-queue the message so it is not lost.
func (s *Server) deliverPending(name string) {
	s.mu.Lock()
	e := s.shards[name]
	if e == nil || e.conn == nil {
		s.mu.Unlock()
		return
	}
	players := make([]string, 0, len(e.players))
	for p := range e.players {
		players = append(players, p)
	}
	s.mu.Unlock()
	for _, p := range players {
		for _, m := range s.mail.Deliver(p) {
			inner, err := json.Marshal(m.Payload)
			if err != nil {
				log.Printf("hub: mail marshal for %q: %v", p, err)
				_ = s.mail.Store(m)
				continue
			}
			raw, err := encodeRelay(p, inner)
			if err != nil {
				_ = s.mail.Store(m)
				continue
			}
			e.wmu.Lock()
			sent := false
			if e.conn != nil {
				_ = e.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if werr := e.conn.WriteMessage(websocket.TextMessage, raw); werr != nil {
					log.Printf("hub: mail deliver for %q: %v", p, werr)
				} else {
					sent = true
				}
			}
			e.wmu.Unlock()
			if !sent {
				_ = s.mail.Store(m)
			}
		}
	}
}

// pushRoster broadcasts the current roster plus the preferred world version
// to every attached shard.
func (s *Server) pushRoster() {
	s.mu.Lock()
	msg := rosterMsg{Type: "roster", Players: s.playerShardCopyLocked()}
	// Preferred version rides the roster (refresh-banner + login routing).
	if best, ok := s.newestRunningLocked(); ok {
		msg.Preferred = best.version
	}
	raw, err := json.Marshal(msg)
	entries := make([]*shardEntry, 0, len(s.shards))
	for _, e := range s.shards {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	if err != nil {
		return
	}
	for _, e := range entries {
		e.wmu.Lock()
		if e.conn != nil {
			_ = e.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = e.conn.WriteMessage(websocket.TextMessage, raw)
		}
		e.wmu.Unlock()
	}
}

// playerShardCopyLocked snapshots the roster. Caller holds mu.
func (s *Server) playerShardCopyLocked() map[string]string {
	out := make(map[string]string, len(s.playerShard))
	for u, sh := range s.playerShard {
		out[u] = sh
	}
	return out
}

// sendToShard writes raw to one shard's live socket. Reports false when the
// shard is unknown or its socket is down (caller decides: drop, reject, or
// store as offline mail).
func (s *Server) sendToShard(name string, raw []byte) bool {
	s.mu.Lock()
	e := s.shards[name]
	s.mu.Unlock()
	if e == nil {
		return false
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	if e.conn == nil {
		return false
	}
	_ = e.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := e.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		log.Printf("hub: send to shard %q: %v", name, err)
		return false
	}
	return true
}

// rejectHandoff answers the sender with a failed ack (the transfer never
// started; the sender keeps its session).
func (s *Server) rejectHandoff(sender string, id, reason string) {
	raw, err := encodeHandoffAck(HandoffAck{ID: id, To: sender, Reason: reason})
	if err != nil {
		return
	}
	if !s.sendToShard(sender, raw) {
		log.Printf("hub: handoff reject for %q dropped (sender down): %s", sender, reason)
	}
}

// handleHandoff relays one sender->hub handoff request: version-gate first
// (mismatch => immediate reject ack, never forwarded — cross-build moves
// must go disconnect+reconnect via login/hub), then forward verbatim to the
// target's live socket. sender is the socket owner's shard name (the From
// field must match it).
func (s *Server) handleHandoff(sender string, raw []byte) {
	req, err := decodeHandoff(raw)
	if err != nil {
		log.Printf("hub: bad handoff frame from %q", sender)
		return
	}
	if req.From == "" {
		req.From = sender
	}
	if req.From != sender {
		log.Printf("hub: handoff From mismatch from %q", sender)
		return
	}
	if req.To == "" || req.To == sender {
		s.rejectHandoff(sender, req.ID, "bad handoff target")
		return
	}
	if err := s.CheckHandoff(sender, req.To); err != nil {
		s.rejectHandoff(sender, req.ID, err.Error())
		return
	}
	fwd, err := encodeHandoff(req)
	if err != nil {
		s.rejectHandoff(sender, req.ID, "handoff encode failed")
		return
	}
	if !s.sendToShard(req.To, fwd) {
		s.rejectHandoff(sender, req.ID, "target shard unavailable")
		return
	}
	log.Printf("hub: handoff %s (%q) -> %s", req.Player, req.Version, req.To)
}

// forwardAck relays one target->hub handoff ack to the original sender
// shard named in its To field.
func (s *Server) forwardAck(raw []byte) {
	ack, err := decodeHandoffAck(raw)
	if err != nil {
		log.Printf("hub: bad handoff-ack frame")
		return
	}
	if ack.To == "" {
		return
	}
	if !s.sendToShard(ack.To, raw) {
		log.Printf("hub: handoff-ack %q for %q dropped (sender down)", ack.ID, ack.To)
	}
}

// ServeHTTP upgrades shard sockets: bearer-header check first (401 when a
// header is present and mismatches; an absent header defers to the
// handshake token below), then the first frame must be a [1, ...]
// handshake whose accessToken (or the header) must match when the server
// token is set (mismatch: log + return with the socket closed via defer).
// Afterwards it serves the heartbeat objects and relay frames until
// disconnect, pushing the roster on every membership change.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	headerToken := bearerToken(r)
	if s.token != "" && headerToken != "" && headerToken != s.token {
		log.Printf("hub: connection from %q rejected (bearer mismatch)", r.RemoteAddr)
		http.Error(w, "hub: token mismatch", http.StatusUnauthorized)
		return
	}
	conn, err := s.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("hub: upgrade from %q: %v", r.RemoteAddr, err)
		return
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		log.Printf("hub: handshake read from %q: %v", r.RemoteAddr, err)
		return
	}
	packet, _, data, err := decodeFrame(raw)
	if err != nil || packet != frameHandshake {
		log.Printf("hub: bad handshake from %q", r.RemoteAddr)
		return
	}
	var hs HubHandshake
	if err := json.Unmarshal(data, &hs); err != nil {
		log.Printf("hub: bad handshake payload from %q", r.RemoteAddr)
		return
	}
	if !s.checkAuth(headerToken, hs.AccessToken) {
		log.Printf("hub: shard %q rejected (token mismatch)", hs.Name)
		return
	}
	if err := s.Register(hs); err != nil {
		log.Printf("hub: shard %q register: %v", hs.Name, err)
		return
	}
	log.Printf("hub: shard %q registered (%d players)", hs.Name, len(hs.Players))
	s.Attach(hs.Name, conn)
	defer s.Detach(hs.Name, conn)
	s.deliverPending(hs.Name)
	s.pushRoster()
	defer s.pushRoster()

	_ = conn.SetReadDeadline(time.Time{})
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if isJSONObject(raw) {
			var hb heartbeatMsg
			if err := json.Unmarshal(raw, &hb); err != nil || hb.Type != "heartbeat" {
				// R2 control objects (no packet-shape changes): handoff
				// requests route to the target shard (version-gated),
				// handoff acks route back to the sender shard.
				var probe struct {
					Type string `json:"t"`
				}
				if perr := json.Unmarshal(raw, &probe); perr != nil {
					continue
				}
				switch probe.Type {
				case handoffType:
					s.handleHandoff(hs.Name, raw)
				case handoffAckType:
					s.forwardAck(raw)
				}
				continue
			}
			if hb.Name == "" {
				hb.Name = hs.Name
			}
			if err := s.HeartbeatFull(hb.Name, hb.Players, hb.State, hb.Load, hb.Version, hb.Regions); err != nil {
				log.Printf("hub: heartbeat: %v", err)
				continue
			}
			s.deliverPending(hb.Name)
			s.pushRoster()
			continue
		}
		packet, _, _, err := decodeFrame(raw)
		if err != nil || packet != frameRelay {
			continue
		}
		s.Relay(raw)
	}
}

// ---------------------------------------------------------------------------
// Shard-side Client
// ---------------------------------------------------------------------------

// RelayHandler receives hub->world relay deliveries: to is the local
// player, inner is the verbatim inner frame (handleRelay parity — the
// caller passes it to the player conn untouched).
type RelayHandler func(to string, inner json.RawMessage)

// Client is the shard side of the hub transport: dials the hub, registers
// with a handshake frame, heartbeats, auto-reconnects with backoff, and
// forwards local Route misses to remote shards. A nil *Client is valid and
// means all-in-one (every method is a documented no-op returning the
// all-in-one answer), so callers can hold one unconditionally and stay
// byte-identical when HUB_ADDR is unset.
type Client struct {
	addr  string
	token string
	name  string

	router  *Router
	mail    *Mailer
	onRelay RelayHandler

	hbInterval time.Duration

	mu      sync.Mutex
	conn    *websocket.Conn
	wmu     sync.Mutex
	remote  map[string]string // username -> shard (hub roster)
	local   map[string]struct{}
	stop    chan struct{}
	stopped bool
	wg      sync.WaitGroup

	// R1 shard stamps, reported in the register frame and every
	// heartbeat (router server-list + login routing). players, when set,
	// supplies the presence list instead of the Register/Unregister-tracked
	// set (shard role: the game owns login, the client only reports).
	buildID string
	gVer    string
	game    string // shard game addr for login redirect
	state   string // drain state override ("" = RUNNING)
	players func() []string

	// R2 world version + scope. version, when set, overrides both the
	// VERSION env tag and the derived buildID+gVer pair in register/
	// heartbeat frames; regions scopes the region->shard lookup.
	version string
	regions []int
	// preferred is the hub-known preferred world version (latest roster
	// push; "" = unknown). onPreferred fires on change (refresh banner).
	preferred   string
	onPreferred func(string)
	// localCheck overrides the relay-delivery local test (shard role: the
	// game owns login, so the client's own Router stays empty and the
	// server supplies m7PlayerByName parity instead).
	localCheck func(string) bool
	// onHandoff answers inbound handoff requests (shard role: validate,
	// pre-load rows, ack). Nil = reject ("no handler").
	onHandoff func(HandoffRequest) HandoffAck
	// pending tracks outbound handoff requests by id until their ack
	// arrives (or the waiter gives up).
	pending map[string]chan HandoffAck
}

// NewClient builds a shard client for addr (ws://host:port/path). router is
// the shard's local Router (must be non-nil when the client runs); mail
// defaults to in-memory; onRelay may be nil (deliveries are then dropped
// with a debug log).
func NewClient(addr, token, name string, router *Router, mail *Mailer, onRelay RelayHandler) *Client {
	if mail == nil {
		mail = NewMailer(nil)
	}
	return &Client{
		addr: addr, token: token, name: name,
		router: router, mail: mail, onRelay: onRelay,
		hbInterval: HeartbeatInterval,
		remote:     make(map[string]string),
		local:      make(map[string]struct{}),
		stop:       make(chan struct{}),
	}
}

// ClientFromEnv builds a Client from HUB_ADDR/HUB_TOKEN/SHARD_NAME, or
// returns nil in all-in-one mode (no HUB_ADDR) so the default path stays
// exactly as today.
func ClientFromEnv(router *Router, mail *Mailer, onRelay RelayHandler) *Client {
	addr := os.Getenv(EnvHubAddr)
	if addr == "" {
		return nil
	}
	return NewClient(addr, SharedToken(), ShardName(), router, mail, onRelay)
}

// SetHeartbeatInterval overrides the 5s heartbeat cadence (tests).
func (c *Client) SetHeartbeatInterval(d time.Duration) {
	if c == nil || d <= 0 {
		return
	}
	c.hbInterval = d
}

// SetBuild records the shard routing stamps reported in the register frame
// and every heartbeat (buildID via version.BuildID, gVer via version.GVer,
// game = shard game addr for login redirect). Nil-safe.
func (c *Client) SetBuild(buildID, gVer, game string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buildID, c.gVer, c.game = buildID, gVer, game
}

// SetState overrides the drain state reported in heartbeats ("" = RUNNING).
// The shard role flips this to DRAINING on SIGTERM so the router stops
// sending NEW sessions while existing ones play on. Nil-safe.
func (c *Client) SetState(state string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
}

// SetPlayersProvider supplies the presence list for register/heartbeat
// frames. When set, the game owns login (shard role) and the client only
// reports; otherwise presence comes from Register/Unregister. Nil-safe.
func (c *Client) SetPlayersProvider(players func() []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.players = players
}

// SetVersion overrides the reported world version (register + heartbeat).
// When unset, the VERSION env tag wins, else the buildID+gVer pair derived
// in SetBuild. Nil-safe.
func (c *Client) SetVersion(tag string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version = tag
}

// SetRegions reports the region scope for the region->shard lookup
// (SHARD_REGIONS parity, programmatic form). Nil-safe.
func (c *Client) SetRegions(regions []int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regions = append([]int(nil), regions...)
}

// SetLocalCheck overrides the relay-delivery local test (shard role: the
// game owns login, so the caller supplies live presence instead of the
// client's own Router). Nil-safe (nil = Router parity).
func (c *Client) SetLocalCheck(check func(string) bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.localCheck = check
}

// SetOnPreferred installs the preferred-version change callback (refresh
// banner). It fires on the read loop when a roster push moves the preferred
// version (including the first known value). Nil-safe (nil disables).
func (c *Client) SetOnPreferred(fn func(string)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onPreferred = fn
}

// PreferredVersion reports the hub-known preferred world version ("" =
// unknown — all-in-one or no roster yet). The game compares it with its own
// version for the refresh banner.
func (c *Client) PreferredVersion() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.preferred
}

// SetHandoffHandler installs the inbound handoff receiver (shard role:
// validate the payload, pre-load rows, ack). Nil-safe (nil = reject every
// request with "no handler").
func (c *Client) SetHandoffHandler(fn func(HandoffRequest) HandoffAck) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onHandoff = fn
}

// Register marks username online locally and returns its pending offline
// mail for the caller to deliver. Login-path hook (Router.Register +
// Mailer.Deliver in one step).
func (c *Client) Register(username string) []Message {
	if c == nil || c.router == nil {
		return nil
	}
	c.router.Register(username)
	c.mu.Lock()
	if c.local == nil {
		c.local = make(map[string]struct{})
	}
	c.local[username] = struct{}{}
	c.mu.Unlock()
	return c.mail.Deliver(username)
}

// Unregister marks username offline locally.
func (c *Client) Unregister(username string) {
	if c == nil || c.router == nil {
		return
	}
	c.router.Unregister(username)
	c.mu.Lock()
	delete(c.local, username)
	c.mu.Unlock()
}

// Connected reports whether the hub socket is up. Nil-safe (false).
func (c *Client) Connected() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// WaitConnected polls for a live socket until timeout. Nil-safe (false).
func (c *Client) WaitConnected(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Connected() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c.Connected()
}

// RemoteOf reports the hub-known shard hosting username, per the last
// roster push. Nil-safe (false). Lets the caller distinguish
// remote-vs-nowhere before routing (the future social_wire branch).
func (c *Client) RemoteOf(username string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	shard, ok := c.remote[username]
	return shard, ok
}

// RemotePlayers snapshots the hub-known online set minus the local players
// (the cross-shard audience for guild/global fanout), sorted. Nil-safe.
func (c *Client) RemotePlayers() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	local := c.localSetLocked()
	var out []string
	for u := range c.remote {
		if _, ok := local[u]; !ok {
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

// localSetLocked snapshots the local online set. Caller holds mu (it may
// invoke the players provider, matching localPlayers).
func (c *Client) localSetLocked() map[string]struct{} {
	if c.players != nil {
		out := make(map[string]struct{})
		for _, u := range c.players() {
			if u != "" {
				out[u] = struct{}{}
			}
		}
		return out
	}
	out := make(map[string]struct{}, len(c.local))
	for u := range c.local {
		out[u] = struct{}{}
	}
	return out
}

// ForwardTo relays one game frame to the shard owning username (one [53,
// ...] envelope, direct-chat parity). inner is the already-marshalled game
// frame. Reports false when the target is local, unknown, or the socket is
// down (caller keeps its local/all-in-one answer). Nil-safe.
func (c *Client) ForwardTo(username string, inner json.RawMessage) bool {
	if c == nil || username == "" {
		return false
	}
	c.mu.Lock()
	if _, ok := c.localSetLocked()[username]; ok {
		c.mu.Unlock()
		return false
	}
	_, remote := c.remote[username]
	conn := c.conn
	c.mu.Unlock()
	if !remote || conn == nil {
		return false
	}
	raw, err := encodeRelay(username, inner)
	if err != nil {
		return false
	}
	c.wmu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, raw)
	c.wmu.Unlock()
	if err != nil {
		log.Printf("hub: fanout forward for %q: %v", username, err)
		return false
	}
	return true
}

// FanoutGuild relays frame to the remote owners among members (per-username
// [53, ...] envelopes): local members are skipped (the caller delivers them
// directly) and unknown members are skipped (offline nowhere — no hub socket
// exists to relay further, and a global broadcast fallback would leak guild
// chat to non-members). Returns the sorted remote recipients handed off.
// Nil-safe.
func (c *Client) FanoutGuild(members []string, frame []any) []string {
	if c == nil || len(members) == 0 {
		return nil
	}
	inner, err := json.Marshal(frame)
	if err != nil {
		log.Printf("hub: fanout guild marshal: %v", err)
		return nil
	}
	seen := make(map[string]struct{}, len(members))
	var out []string
	for _, m := range members {
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		if c.ForwardTo(m, inner) {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// FanoutGlobal relays frame to every remote-known player (one [53, ...]
// envelope each — hub-routed per username, owning shards deliver locally).
// Returns the sorted recipients handed off. Nil-safe.
func (c *Client) FanoutGlobal(frame []any) []string {
	if c == nil {
		return nil
	}
	inner, err := json.Marshal(frame)
	if err != nil {
		log.Printf("hub: fanout global marshal: %v", err)
		return nil
	}
	var out []string
	for _, u := range c.RemotePlayers() {
		if c.ForwardTo(u, inner) {
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

// RequestHandoff sends one handoff request to target and waits for its ack
// (or ctx expiry). A rejected ack (ok=false) is returned as the ack, not an
// error — the transfer never started and the sender keeps its session.
// Errors report transport failures only (nil client, down socket, encode or
// write failure, ctx expiry).
func (c *Client) RequestHandoff(ctx context.Context, target string, req HandoffRequest) (HandoffAck, error) {
	if c == nil {
		return HandoffAck{}, errors.New("hub: no hub client")
	}
	if target == "" {
		return HandoffAck{}, errors.New("hub: bad handoff target")
	}
	if req.ID == "" {
		req.ID = fmt.Sprintf("%s-%d", c.name, time.Now().UnixNano())
	}
	req.From = c.name
	req.To = target
	ch := make(chan HandoffAck, 1)
	c.mu.Lock()
	if c.pending == nil {
		c.pending = make(map[string]chan HandoffAck)
	}
	c.pending[req.ID] = ch
	conn := c.conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if cur, ok := c.pending[req.ID]; ok && cur == ch {
			delete(c.pending, req.ID)
		}
		c.mu.Unlock()
	}()
	if conn == nil {
		return HandoffAck{}, errors.New("hub: hub socket down")
	}
	raw, err := encodeHandoff(req)
	if err != nil {
		return HandoffAck{}, err
	}
	c.wmu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, raw)
	c.wmu.Unlock()
	if err != nil {
		return HandoffAck{}, err
	}
	select {
	case <-ctx.Done():
		return HandoffAck{}, ctx.Err()
	case ack := <-ch:
		return ack, nil
	}
}

// RouteOrForward resolves m locally first (Route parity). On a local miss
// of a direct message it forwards the [53, ...] relay envelope over the
// socket when the hub roster places the target on a remote shard
// (returning ErrForwarded — no local fallback), and otherwise stores
// offline mail and returns ErrOffline (caller falls back exactly as in
// all-in-one). Guild/global kinds resolve locally only here (cross-shard
// guild/global fanout goes through FanoutGuild/FanoutGlobal). Nil-client (or
// offline-socket) behavior is exactly the all-in-one answer: local Route,
// else store + ErrOffline.
func (c *Client) RouteOrForward(m Message) ([]string, error) {
	if c == nil || c.router == nil {
		return nil, ErrOffline
	}
	if recips, err := c.router.Route(m); err == nil {
		return recips, nil
	} else if !errors.Is(err, ErrOffline) {
		return nil, err
	}
	if m.To == "" || m.Kind == KindGuild || m.Kind == KindGlobal {
		return nil, ErrOffline
	}
	if c.forwardRemote(m) {
		return nil, ErrForwarded
	}
	if err := c.mail.Store(m); err != nil {
		return nil, err
	}
	return nil, ErrOffline
}

// forwardRemote sends m to the shard hosting m.To per the hub roster.
// Reports whether the target is remote AND the socket is up (only then is
// the message handed off).
func (c *Client) forwardRemote(m Message) bool {
	c.mu.Lock()
	_, remote := c.remote[m.To]
	conn := c.conn
	c.mu.Unlock()
	if !remote || conn == nil {
		return false
	}
	inner, err := json.Marshal(m.Payload)
	if err != nil {
		log.Printf("hub: relay marshal for %q: %v", m.To, err)
		return false
	}
	raw, err := encodeRelay(m.To, inner)
	if err != nil {
		return false
	}
	c.wmu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, raw)
	c.wmu.Unlock()
	if err != nil {
		log.Printf("hub: relay forward for %q: %v", m.To, err)
		return false
	}
	return true
}

// localPlayers snapshots the shard's online set: the provider list when
// SetPlayersProvider is used (shard role), else the set observed through
// Client.Register/Unregister.
func (c *Client) localPlayers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.players != nil {
		out := append([]string(nil), c.players()...)
		sort.Strings(out)
		return out
	}
	out := make([]string, 0, len(c.local))
	for u := range c.local {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// clientStamps snapshots the R1 routing stamps under lock.
func (c *Client) clientStamps() (buildID, gVer, game, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buildID, c.gVer, c.game, c.state
}

// reportedVersion snapshots the R2 world version under lock: the explicit
// SetVersion override, else the VERSION env tag, else the buildID+gVer pair.
func (c *Client) reportedVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != "" {
		return c.version
	}
	if tag := VersionTag(); tag != "" {
		return tag
	}
	return VersionOf(c.buildID, c.gVer, "")
}

// reportedRegions snapshots the region scope under lock.
func (c *Client) reportedRegions() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.regions...)
}

// handshakeFrame builds the [1, null, {...}] register frame.
func (c *Client) handshakeFrame() ([]byte, error) {
	players := c.localPlayers()
	buildID, gVer, game, state := c.clientStamps()
	hs := HubHandshake{
		Type: "hub", Name: c.name, AccessToken: c.token, Players: players,
		BuildID: buildID, GVer: gVer, State: state, Load: len(players), Addr: game,
		Version: c.reportedVersion(), Regions: c.reportedRegions(),
		// Pin the world id the shard reports to /isOnline (SERVER_ID), so the
		// hub roster and the shard's duplicate-login check agree.
		ServerID: ServerIDFromEnv(),
	}
	return encodeFrame(frameHandshake, nil, hs)
}

// heartbeatFrame builds the {"t":"heartbeat",...} frame.
func (c *Client) heartbeatFrame() ([]byte, error) {
	players := c.localPlayers()
	_, _, _, state := c.clientStamps()
	return json.Marshal(heartbeatMsg{
		Type: "heartbeat", Name: c.name, Players: players,
		State: state, Load: len(players),
		Version: c.reportedVersion(), Regions: c.reportedRegions(),
	})
}

// Start dials the hub and serves register/heartbeat/read until ctx ends or
// Stop is called. Nil-safe (no-op). It blocks; run it in a goroutine.
func (c *Client) Start(ctx context.Context) {
	if c == nil {
		return
	}
	backoff := ReconnectBase
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		default:
		}
		if c.dialOnce(ctx) {
			backoff = ReconnectBase
		} else {
			select {
			case <-ctx.Done():
				return
			case <-c.stop:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > ReconnectMax {
				backoff = ReconnectMax
			}
		}
	}
}

// Stop ends a running Start loop and closes the socket. Nil-safe.
func (c *Client) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.stopped {
		c.stopped = true
		close(c.stop)
	}
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	c.wg.Wait()
}

// dialOnce connects, registers, and serves until the socket drops.
// Reports false when the caller should back off before retrying.
func (c *Client) dialOnce(ctx context.Context) bool {
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, c.addr, header)
	if err != nil {
		log.Printf("hub: dial %q: %v (retrying)", c.addr, err)
		return false
	}
	hs, err := c.handshakeFrame()
	if err != nil {
		_ = conn.Close()
		return false
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, hs); err != nil {
		log.Printf("hub: register: %v (retrying)", err)
		_ = conn.Close()
		return false
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	done := make(chan struct{})
	c.wg.Add(1)
	go c.readLoop(conn, done)
	defer func() {
		<-done
		c.wg.Done()
	}()

	t := time.NewTicker(c.hbInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.detach(conn)
			_ = conn.Close()
			return true
		case <-c.stop:
			c.detach(conn)
			_ = conn.Close()
			return true
		case <-t.C:
			hb, err := c.heartbeatFrame()
			if err != nil {
				continue
			}
			c.wmu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err = conn.WriteMessage(websocket.TextMessage, hb)
			c.wmu.Unlock()
			if err != nil {
				log.Printf("hub: heartbeat: %v (reconnecting)", err)
				c.detach(conn)
				_ = conn.Close()
				return true
			}
		}
	}
}

// detach clears the socket if it is still conn.
func (c *Client) detach(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn = nil
	}
}

// readLoop serves inbound hub frames until the socket drops, then closes
// done. Relay envelopes addressed to local players go to onRelay verbatim
// (handleRelay parity); roster pushes refresh the remote map and the
// preferred version (onPreferred fires on change); handoff requests go to
// the handoff handler and handoff acks resolve outbound RequestHandoff
// waiters.
func (c *Client) readLoop(conn *websocket.Conn, done chan struct{}) {
	defer close(done)
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			c.detach(conn)
			_ = conn.Close()
			return
		}
		if isJSONObject(raw) {
			c.readObject(conn, raw)
			continue
		}
		packet, _, _, err := decodeFrame(raw)
		if err != nil || packet != frameRelay {
			continue
		}
		to, inner, err := decodeRelay(raw)
		if err != nil {
			continue
		}
		if !c.isLocal(to) {
			// Target left between hub routing and delivery: keep it as
			// offline mail instead of dropping it.
			var payload any
			_ = json.Unmarshal(inner, &payload)
			if serr := c.mail.Store(Message{To: to, Kind: KindChat, Payload: payload}); serr != nil {
				log.Printf("hub: relay for offline %q dropped: %v", to, serr)
			}
			continue
		}
		if c.onRelay != nil {
			c.onRelay(to, inner)
		} else {
			log.Printf("hub: relay for %q dropped (no handler)", to)
		}
	}
}

// isLocal reports whether username is served locally: the shard-role hook
// when set, else the client's own Router (Register/Unregister parity).
func (c *Client) isLocal(username string) bool {
	c.mu.Lock()
	check := c.localCheck
	c.mu.Unlock()
	if check != nil {
		return check(username)
	}
	if _, rerr := c.router.Route(Message{Kind: KindChat, To: username}); rerr != nil {
		return false
	}
	return true
}

// readObject serves one inbound hub control object: roster pushes refresh
// the remote map + preferred version, handoff requests go to the handler
// (the ack routes back over the same socket), handoff acks resolve outbound
// waiters.
func (c *Client) readObject(conn *websocket.Conn, raw []byte) {
	var probe struct {
		Type string `json:"t"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return
	}
	switch probe.Type {
	case "roster":
		var rs rosterMsg
		if err := json.Unmarshal(raw, &rs); err != nil {
			return
		}
		remote := make(map[string]string, len(rs.Players))
		for u, sh := range rs.Players {
			if u != "" && sh != "" {
				remote[u] = sh
			}
		}
		var onPref func(string)
		var pref string
		c.mu.Lock()
		c.remote = remote
		if rs.Preferred != "" && rs.Preferred != c.preferred {
			c.preferred = rs.Preferred
			pref, onPref = rs.Preferred, c.onPreferred
		}
		c.mu.Unlock()
		if onPref != nil {
			onPref(pref)
		}
	case handoffType:
		req, err := decodeHandoff(raw)
		if err != nil {
			return
		}
		c.mu.Lock()
		fn := c.onHandoff
		c.mu.Unlock()
		ack := HandoffAck{ID: req.ID, To: req.From, Reason: "no handler"}
		if fn != nil {
			ack = fn(req)
			ack.ID = req.ID
			ack.To = req.From
		}
		if out, err := encodeHandoffAck(ack); err == nil {
			c.wmu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = conn.WriteMessage(websocket.TextMessage, out)
			c.wmu.Unlock()
		}
	case handoffAckType:
		ack, err := decodeHandoffAck(raw)
		if err != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[ack.ID]
		delete(c.pending, ack.ID)
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- ack:
			default:
			}
		}
	}
}
