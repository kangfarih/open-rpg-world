// Package net is the root server's connection transport + fan-out owner
// (D2a). It defines the Bus interface, the Conn/Session transport types and
// the Hub (subs registry, writeMu, accept gate, Send/SendDirect/Flush
// mechanics, RegionScoped/FrameInstance routing predicates); it imports
// nothing from the root package (that would be an import cycle).
//
// Canonical homes (D2a; the root keeps no transport globals):
//
//	Hub.AddSub/DelSub          <- root subs/subsMu map
//	Hub.writeMu                <- root writeMu
//	Hub.Upgrader               <- root upgrader
//	Hub.Accept/Release         <- root opsAccept/opsRelease (update-mode +
//	                              IP bans + per-IP cap, same log lines)
//	Hub.AllowMsg/AllowChat     <- root opsAllowMsg/opsAllowChat
//	Hub.Send/SendDirect/Flush  <- root send/sendDirect/tick-loop flush
//	                              (identical TX logs, deadlines, drop lines)
//	Conn/Session               <- root playerConn/session (transport half;
//	                              the root playerConn embeds *Conn)
//	RegionScoped/FrameInstance <- root regionScoped/frameInstance
//	Limiter                    <- root opsLimiter (limits.go, unchanged)
//
// The entity registry + region routing live in internal/world (Registry),
// which calls into this package (world -> net; never the reverse).
package net

// Frame is one S->C packet frame: [id, data] or [id, opcode, data]
// (see internal/protocol pkt/pktOp). Alias so call sites can pass the
// root's []any frames without conversion.
type Frame = []any

// Bus is the network fan-out seam the root server will adapt later.
// Each method maps to a root function it will wrap:
//
//	Broadcast  -> broadcast(frames ...[]any): region-scoped frames enqueue
//	              only to conns whose interest set includes the entity tile
//	              (clientInterested over the surroundingRegions 9-set);
//	              global frames fan out via enqueueGlobal.
//	SendTo     -> enqueueTo(conn *websocket.Conn, frames ...Frame) (via the
//	              send(conn, frames...) unicast path): queue frames for the
//	              single conn keyed by instance; flushed by the 20Hz tick loop.
//	RegionOf   -> regionOf(x, y int) int: tile -> region id
//	              ((y/mapDivisionSize)*sideLen + (x/mapDivisionSize)).
//	Interested -> regionOf + clientInterested(c *playerConn, x, y int) bool:
//	              simplified to region ids — true when the entity region is in
//	              the client's surrounding-regions interest set.
type Bus interface {
	// Broadcast routes each frame by region scope (root: broadcast).
	Broadcast(frames ...Frame)
	// SendTo unicasts frames to one player instance (root: enqueueTo/send).
	SendTo(instance string, frames ...Frame)
	// RegionOf maps a tile to its region id (root: regionOf).
	RegionOf(x, y int) int
	// Interested reports whether entity region is visible to a client
	// centered in clientRegion (root: regionOf + clientInterested).
	Interested(region, clientRegion int) bool
}

// Config carries connection-gating knobs for a future net listener.
// Zero values fall back to the Default* constants in limits.go.
type Config struct {
	// MaxConnectionsPerIP caps concurrent conns per IP.
	// Mirrors the TS MAX_CONNECTIONS=16 (see docs/GO-SERVER-PLAN.md).
	MaxConnectionsPerIP int
	// MaxMsgsPerSecond caps per-conn messages under a 1s sliding window.
	// Mirrors the TS MESSAGE_LIMIT=300 (connection.ts messageRate).
	MaxMsgsPerSecond int
	// ChatBurst is the per-conn chat token-bucket capacity.
	// Mirrors root chatBucketSize=3.
	ChatBurst float64
	// ChatRefillPerSec is the per-conn chat token refill rate.
	// Mirrors root chatRefillPerSec=0.5 (one msg per 2s).
	ChatRefillPerSec float64
}
