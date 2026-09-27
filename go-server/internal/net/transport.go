// Package net — connection transport ownership (D2a).
//
// This file moves the root server's transport state and mechanics out of
// package main (main.go + ops_wire.go) with byte-identical behavior:
//
//   - conn/session types: Session (per-connection player grid pos + anticheat
//     state) and Conn (the per-connection record: socket, identity, session,
//     queued outbox, 9-region interest set, overflow drops). The root
//     playerConn embeds *Conn and adds game session fields (store/chat/
//     minigame); no transport type stays in the root.
//   - connection registry: Hub owns subs (admission bookkeeping), the
//     conn->IP table, the shared writeMu (gorilla/websocket forbids
//     concurrent writers), and the accept gate (update-mode flag + IP bans +
//     per-IP cap via Limiter). Semantics mirror ops_wire.go exactly.
//   - fan-out mechanics: Send (TX log + queued unicast), SendDirect (TX log +
//     immediate bulk write, spawn-burst path), WriteText (deadline write, ban
//     path), Flush (central 20Hz bulk-per-conn drain + write, tick-loop path)
//     and TryEnqueue (silent single-frame enqueue; overflow accounting shared
//     with the region router).
//   - routing predicates: RegionScoped (region-routed vs global fan-out packet
//     ids) and FrameInstance (entity instance probe of an S->C frame).
//
// Lock order (D2a contract, see also internal/world Registry):
//
//	net.Conn.mu (per-conn Regions/Dropped, leaf)
//	-> Hub.mu/subs+conns tables (admission only, leaf)
//	-> Hub.writeMu (socket writes only, leaf, never held across enqueue)
//	-> world Registry.mu (entities/players maps)
//	-> persist store (dbMu) -> subsystem state
//
// Every Hub method holds at most one mutex at a time; channel operations are
// lock-free (non-blocking select); socket writes hold only writeMu. The world
// Registry never acquires transport locks while holding its own, so the
// subsystem->registry callback nesting on the engine tick (m9Tick holds m9Mu
// across broadcast, as before) cannot deadlock.
package net

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"rpg-world-server/internal/protocol"
)

// OutboxSize is the per-connection queued-frame capacity (main.go outboxSize).
// Steady-state traffic rides Send/Broadcast into the 20Hz Flush; only the
// initial spawn burst (240 Spawn frames) bypasses it via SendDirect.
const OutboxSize = 64

// Session tracks one connection's authoritative player grid pos plus the
// movement anticheat state (main.go session). The math lives in
// internal/world; this struct is the transport-owned record of it.
type Session struct {
	PlayerX, PlayerY   int
	Target             string
	LastStep           time.Time
	MovementSpeed      int // ms per tile (Welcome default 220)
	CheatScore         int
	LastRegionChange   time.Time // region change exemption (1.5s grace)
	BypassAntiCheat    bool      // teleport bypass flag
	LatencyMs          int       // client-reported one-way latency (ms); 0 until first timestamped packet
}

// Conn is the per-connection transport record (main.go playerConn, transport
// half): socket, identity, session, queued outbox, 9-region interest set and
// overflow drops.
//
// WS/Instance/Username/Sess/Outbox are written once at accept/login on the
// connection goroutine and read from tick/engine goroutines afterwards — the
// same discipline as the root map era (registration under the world Registry
// mutex provides the happens-before edge for Instance; Username/Sess keep
// their pre-existing unlocked discipline, see package world docs).
// Regions/Dropped are guarded by mu (they were guarded by playersMu before).
type Conn struct {
	WS       *websocket.Conn
	Instance string
	Username string // login name, DB key for the persist slice
	Sess     Session
	Outbox   chan Frame // queued S->C frames, flushed by the tick loop

	mu      sync.Mutex
	regions []int // current 9-region interest set
	dropped int   // overflow drops (outbox full)
}

// NewConn returns a transport record with a fresh outbox.
func NewConn(ws *websocket.Conn, instance string) *Conn {
	return &Conn{
		WS:       ws,
		Instance: instance,
		Sess:     Session{PlayerX: 100, PlayerY: 96, MovementSpeed: 220},
		Outbox:   make(chan Frame, OutboxSize),
	}
}

// PeerNet returns the transport record itself (world.Peer implementation).
func (c *Conn) PeerNet() *Conn { return c }

// Regions snapshots the interest set (copy; safe for routing decisions).
func (c *Conn) Regions() []int {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.regions...)
}

// SetRegions replaces the interest set.
func (c *Conn) SetRegions(r []int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regions = append([]int(nil), r...)
}

// BumpDropped records one overflow drop and reports the total.
func (c *Conn) BumpDropped() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropped++
	return c.dropped
}

// AddrID is the limiter's per-connection key (ops_wire opsConnID):
// RemoteAddr (ip:port) is unique per conn and stays available after close,
// so release-time Forget needs no extra bookkeeping.
func AddrID(ws *websocket.Conn) string {
	if ws == nil {
		return ""
	}
	if a := ws.RemoteAddr(); a != nil {
		return a.String()
	}
	return ""
}

// ClientIP strips the port from an HTTP remote address ("1.2.3.4:5678" ->
// "1.2.3.4"); unparseable input is returned as-is (ops_wire opsClientIP).
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RegionScoped reports whether a packet id is region-scoped (interest-routed)
// vs global fan-out (main.go regionScoped, frozen set).
func RegionScoped(id int) bool {
	switch id {
	case protocol.PacketSpawn, protocol.PacketMovement, protocol.PacketAnimation,
		protocol.PacketCombat, protocol.PacketResource, protocol.PacketEffect,
		protocol.PacketChat, protocol.PacketDeath, protocol.PacketRespawn:
		return true
	}
	return false
}

// FrameInstance extracts the entity instance from an S->C frame's data
// payload (main.go frameInstance).
func FrameInstance(frame Frame) string {
	if len(frame) < 2 {
		return ""
	}
	data := frame[len(frame)-1]
	raw, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	var probe struct {
		Instance string `json:"instance"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Instance
}

// Hub owns the connection registry and the accept gate (main.go subs/writeMu
// + ops_wire.go limiter/conns/bans/accepting). Construct via NewHub; the
// process-wide DefaultHub backs the package-level wrappers below.
type Hub struct {
	mu   sync.Mutex
	subs map[*websocket.Conn]struct{}

	writeMu sync.Mutex

	Upgrader websocket.Upgrader

	limiter *Limiter

	accepting atomic.Bool

	connsMu sync.Mutex
	conns   map[*websocket.Conn]string // admitted conn -> client IP

	ipMu   sync.Mutex
	ipBans map[string]bool
}

// NewHub returns an admitting Hub with default limits (16/IP, 300 msg/s,
// chat 3 burst @ 0.5/s — the opsLimiter budgets).
func NewHub() *Hub {
	h := &Hub{
		subs:    make(map[*websocket.Conn]struct{}),
		limiter: NewLimiterFromConfig(Config{}),
		conns:   make(map[*websocket.Conn]string),
		ipBans:  make(map[string]bool),
		Upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
	}
	h.accepting.Store(true)
	return h
}

// DefaultHub is the process-wide transport registry (replaces the root
// subs/writeMu/opsLimiter/opsAccepting/opsConns/opsIPBans globals).
var DefaultHub = NewHub()

// AddSub records an admitted connection (main.go handleConn entry).
func (h *Hub) AddSub(ws *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ws] = struct{}{}
}

// DelSub forgets an admitted connection (main.go removeClient).
func (h *Hub) DelSub(ws *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ws)
}

// Accept gates one HTTP request before the WS upgrade (ops_wire opsAccept):
// update-mode and IP bans reject first, then the per-IP cap (reject + log
// over 16), then the upgrade. A failed upgrade releases the acquired slot.
func (h *Hub) Accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, bool) {
	if !h.accepting.Load() {
		http.Error(w, "server updating", http.StatusServiceUnavailable)
		return nil, false
	}
	ip := ClientIP(r)
	h.ipMu.Lock()
	banned := h.ipBans[ip]
	h.ipMu.Unlock()
	if banned {
		log.Printf("ops: reject banned ip=%s", ip)
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	if !h.limiter.Acquire(ip) {
		log.Printf("ops: reject ip=%s over per-IP cap (%d)", ip, DefaultMaxConnectionsPerIP)
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return nil, false
	}
	conn, err := h.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		h.limiter.Release(ip)
		return nil, false
	}
	h.connsMu.Lock()
	h.conns[conn] = ip
	h.connsMu.Unlock()
	log.Printf("client connected: %s", r.RemoteAddr)
	return conn, true
}

// Release frees the limiter slot and per-conn msg/chat state for a conn whose
// handleConn loop has returned (ops_wire opsRelease: every disconnect path
// funnels there — read errors, kicks, bans and the deferred removeClient).
func (h *Hub) Release(ws *websocket.Conn) {
	if ws == nil {
		return
	}
	h.connsMu.Lock()
	ip, ok := h.conns[ws]
	if ok {
		delete(h.conns, ws)
	}
	h.connsMu.Unlock()
	if !ok {
		return
	}
	h.limiter.Forget(AddrID(ws))
	h.limiter.Release(ip)
}

// AllowMsg reports whether one inbound frame from id may be processed (drops
// over the per-message budget; caller logs the drop — ops_wire opsAllowMsg).
func (h *Hub) AllowMsg(id string) bool {
	if id == "" {
		return true
	}
	return h.limiter.AllowMsg(id, time.Now().UnixMilli())
}

// AllowChat reports whether conn may send one chat message under the limiter
// bucket. Rejection drops silently, matching the chatState bucket-exhaust
// path (ops_wire opsAllowChat).
func (h *Hub) AllowChat(c *Conn) bool {
	if c == nil || c.WS == nil {
		return true
	}
	return h.limiter.AllowChat(AddrID(c.WS), time.Now().UnixMilli())
}

// TryEnqueue queues one frame for conn, reporting false on overflow (silent;
// the caller owns overflow accounting). Channel operations are lock-free.
func TryEnqueue(c *Conn, f Frame) bool {
	if c == nil {
		return false
	}
	select {
	case c.Outbox <- f:
		return true
	default:
		return false
	}
}

// enqueueLocked is the drop+count path shared by Send and the world region
// router: overflow logs the identical "outbox overflow" line.
func enqueueLogged(c *Conn, f Frame) {
	if TryEnqueue(c, f) {
		return
	}
	n := c.BumpDropped()
	log.Printf("outbox overflow instance=%s dropped=%d", c.Instance, n)
}

// Send queues unicast frames for one conn (flushed by the tick loop) and logs
// the bulk (main.go send, identical signature minus the conn lookup).
func (h *Hub) Send(c *Conn, frames ...Frame) error {
	msg := protocol.Bulk(frames...)
	fmt.Printf("TX %s\n", msg)
	if c == nil {
		return nil
	}
	for _, f := range frames {
		enqueueLogged(c, f)
	}
	return nil
}

// SendDirect writes one bulk message immediately (5s deadline), bypassing the
// tick outbox (main.go sendDirect, identical). Used only for the initial
// spawn burst (240 Spawn frames exceed the 64-slot outbox); all steady-state
// traffic goes via Send.
func (h *Hub) SendDirect(ws *websocket.Conn, frames ...Frame) error {
	msg := protocol.Bulk(frames...)
	fmt.Printf("TX %s\n", msg)
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return ws.WriteMessage(websocket.TextMessage, msg)
}

// WriteText writes one deadline-guarded text frame immediately (ban path:
// main.go login gate + m13 SendBan, identical 2s deadline).
func (h *Hub) WriteText(ws *websocket.Conn, msg []byte, d time.Duration) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(d))
	return ws.WriteMessage(websocket.TextMessage, msg)
}

// Flush drains every conn's queued frames as a single bulk write (main.go
// startTickLoop body, identical TX logging, 5s deadline, failure log). It
// returns the conns whose write failed; the caller drops them via the world
// Registry (which runs the disconnect fanout).
func (h *Hub) Flush(conns []*Conn) []*Conn {
	var failed []*Conn
	for _, c := range conns {
		var frames []Frame
		for {
			select {
			case f := <-c.Outbox:
				frames = append(frames, f)
			default:
				goto drained
			}
		}
	drained:
		if len(frames) == 0 {
			continue
		}
		msg := protocol.Bulk(frames...)
		fmt.Printf("TX %s\n", msg)
		h.writeMu.Lock()
		_ = c.WS.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := c.WS.WriteMessage(websocket.TextMessage, msg)
		h.writeMu.Unlock()
		if err != nil {
			log.Printf("tick write failed instance=%s: %v", c.Instance, err)
			failed = append(failed, c)
		}
	}
	return failed
}

// SetAccepting flips the update-mode gate (console /update parity).
func (h *Hub) SetAccepting(b bool) { h.accepting.Store(b) }

// BanIP records an IP ban (console /ipban parity).
func (h *Hub) BanIP(ip string) {
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	h.ipBans[ip] = true
}

// UnbanIP clears an IP ban (console /unbanip parity:
// database.setIpBan(ip, false)). Same-IP conns stay connected — unban
// never drops; only the ban path drops matching conns.
func (h *Hub) UnbanIP(ip string) {
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	delete(h.ipBans, ip)
}

// BannedIPs lists banned IPs (console /ipban list parity, sorted).
func (h *Hub) BannedIPs() []string {
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	out := make([]string, 0, len(h.ipBans))
	for k := range h.ipBans {
		out = append(out, k)
	}
	return out
}

// Package-level wrappers over DefaultHub (terse root call sites).

// AddSub records an admitted connection.
func AddSub(ws *websocket.Conn) { DefaultHub.AddSub(ws) }

// DelSub forgets an admitted connection.
func DelSub(ws *websocket.Conn) { DefaultHub.DelSub(ws) }

// Accept gates one HTTP request before the WS upgrade.
func Accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, bool) {
	return DefaultHub.Accept(w, r)
}

// Release frees the limiter slot and per-conn state for a closed conn.
func Release(ws *websocket.Conn) { DefaultHub.Release(ws) }

// AllowMsg reports whether one inbound frame from id may be processed.
func AllowMsg(id string) bool { return DefaultHub.AllowMsg(id) }

// AllowChat reports whether conn may send one chat message.
func AllowChat(c *Conn) bool { return DefaultHub.AllowChat(c) }

// Send queues unicast frames for one conn and logs the bulk.
func Send(c *Conn, frames ...Frame) error { return DefaultHub.Send(c, frames...) }

// SendDirect writes one bulk message immediately.
func SendDirect(ws *websocket.Conn, frames ...Frame) error {
	return DefaultHub.SendDirect(ws, frames...)
}

// WriteText writes one deadline-guarded text frame immediately.
func WriteText(ws *websocket.Conn, msg []byte, d time.Duration) error {
	return DefaultHub.WriteText(ws, msg, d)
}

// Flush drains every conn's queued frames as single bulk writes.
func Flush(conns []*Conn) []*Conn { return DefaultHub.Flush(conns) }

// SetAccepting flips the update-mode gate.
func SetAccepting(b bool) { DefaultHub.SetAccepting(b) }

// BanIP records an IP ban.
func BanIP(ip string) { DefaultHub.BanIP(ip) }

// UnbanIP clears an IP ban (same-IP conns stay connected).
func UnbanIP(ip string) { DefaultHub.UnbanIP(ip) }

// BannedIPs lists banned IPs.
func BannedIPs() []string { return DefaultHub.BannedIPs() }
