// Package net — rate-limit hardening (transport-free).
//
// Conventions mirrored (read-only, DO NOT import):
//   - per-IP cap: TS Modules.Constants.MAX_CONNECTIONS=16
//     (packages/common/network/modules.ts:645); socket bookkeeping in
//     packages/server/src/network/sockethandler.ts (addAddress/removeAddress,
//     isMaxConnections). bus.go Config.MaxConnectionsPerIP models the knob.
//   - per-conn msg/s: packages/server/src/network/connection.ts messageRate
//     reset every 1000ms via setInterval, enforced in
//     packages/server/src/network/sockets/uws.ts handleMessage against
//     config.messageLimit (MESSAGE_LIMIT=300 in .env.defaults).
//   - chat token bucket: root m7.go chatState.allowChat — capacity
//     chatBucketSize=3.0, refill chatRefillPerSec=0.5/s (one msg per 2s).
//
// This file is ADDITIVE ONLY: it introduces Limiter without touching bus.go
// or any root file. Transport-free means: no sockets, no goroutines, no
// time.After/tickers — the caller passes nowMs (milliseconds, e.g.
// time.Now().UnixMilli()) so tests can inject a synthetic clock.
package net

import "sync"

// Default limits mirroring the TS/Go conventions above.
const (
	// DefaultMaxConnectionsPerIP mirrors TS MAX_CONNECTIONS=16.
	DefaultMaxConnectionsPerIP = 16
	// DefaultMaxMsgsPerSecond mirrors config MESSAGE_LIMIT=300 per 1s window.
	DefaultMaxMsgsPerSecond = 300
	// DefaultChatBurst mirrors root m7.go chatBucketSize=3.
	DefaultChatBurst = 3.0
	// DefaultChatRefillPerSec mirrors root m7.go chatRefillPerSec=1/2s.
	DefaultChatRefillPerSec = 0.5
	// msgWindowMs is the sliding-window width for per-conn msg/s
	// (mirrors the TS 1000ms rateInterval reset).
	msgWindowMs = int64(1000)
)

// Limits carries the Limiter knobs. Zero values fall back to the Defaults
// above so NewLimiterWithLimits(Limits{}) behaves like NewLimiter().
type Limits struct {
	MaxConnectionsPerIP int
	MaxMsgsPerSecond    int
	ChatBurst           float64
	ChatRefillPerSec    float64
}

// DefaultLimits returns the convention-mirroring knob set.
func DefaultLimits() Limits {
	return Limits{
		MaxConnectionsPerIP: DefaultMaxConnectionsPerIP,
		MaxMsgsPerSecond:    DefaultMaxMsgsPerSecond,
		ChatBurst:           DefaultChatBurst,
		ChatRefillPerSec:    DefaultChatRefillPerSec,
	}
}

// normalize fills zero/negative knobs with defaults.
func (l Limits) normalize() Limits {
	if l.MaxConnectionsPerIP <= 0 {
		l.MaxConnectionsPerIP = DefaultMaxConnectionsPerIP
	}
	if l.MaxMsgsPerSecond <= 0 {
		l.MaxMsgsPerSecond = DefaultMaxMsgsPerSecond
	}
	if l.ChatBurst <= 0 {
		l.ChatBurst = DefaultChatBurst
	}
	if l.ChatRefillPerSec <= 0 {
		l.ChatRefillPerSec = DefaultChatRefillPerSec
	}
	return l
}

// msgWindow is one conn's sliding-window message timestamps (ms).
type msgWindow struct {
	times []int64
}

// chatBucket is one conn's token bucket (mirrors m7.go chatState).
type chatBucket struct {
	tokens float64
	lastMs int64
	init   bool
}

// Limiter is a transport-free rate limiter combining:
//   - per-IP concurrent-connection counting (Acquire/Release),
//   - per-conn messages-per-second sliding window (AllowMsg),
//   - per-conn chat token bucket (AllowChat).
//
// All state is guarded by mu; methods are synchronous and spawn no
// goroutines. Callers pass nowMs (Unix milliseconds) for AllowMsg/AllowChat;
// time is never sampled inside, keeping tests deterministic.
type Limiter struct {
	mu       sync.Mutex
	maxPerIP int
	maxMsgs  int
	chatCap  float64
	chatFill float64

	ips   map[string]int
	msgs  map[string]*msgWindow
	chats map[string]*chatBucket
}

// NewLimiter returns a Limiter with DefaultLimits().
func NewLimiter() *Limiter {
	return NewLimiterWithLimits(DefaultLimits())
}

// NewLimiterWithLimits returns a Limiter with the given knobs
// (zero values fall back to defaults).
func NewLimiterWithLimits(l Limits) *Limiter {
	l = l.normalize()
	return &Limiter{
		maxPerIP: l.MaxConnectionsPerIP,
		maxMsgs:  l.MaxMsgsPerSecond,
		chatCap:  l.ChatBurst,
		chatFill: l.ChatRefillPerSec,
		ips:      make(map[string]int),
		msgs:     make(map[string]*msgWindow),
		chats:    make(map[string]*chatBucket),
	}
}

// NewLimiterFromConfig builds a Limiter from the bus Config seam, passing
// all knobs through (zero values fall back to defaults via normalize).
func NewLimiterFromConfig(cfg Config) *Limiter {
	return NewLimiterWithLimits(Limits{
		MaxConnectionsPerIP: cfg.MaxConnectionsPerIP,
		MaxMsgsPerSecond:    cfg.MaxMsgsPerSecond,
		ChatBurst:           cfg.ChatBurst,
		ChatRefillPerSec:    cfg.ChatRefillPerSec,
	})
}

// Acquire admits one connection from ip, returning false when the per-IP cap
// is already reached. The caller is expected to drop/reject the conn on false
// (mirrors TS isMaxConnections gating the upgrade path).
func (l *Limiter) Acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ips[ip] >= l.maxPerIP {
		return false
	}
	l.ips[ip]++
	return true
}

// Release drops one connection slot for ip. It never drives the count
// negative and deletes the entry at zero (mirrors removeAddress).
func (l *Limiter) Release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.ips[ip]
	if !ok || n <= 0 {
		delete(l.ips, ip)
		return
	}
	if n == 1 {
		delete(l.ips, ip)
		return
	}
	l.ips[ip] = n - 1
}

// Count reports current concurrent connections for ip (observability helper).
func (l *Limiter) Count(ip string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ips[ip]
}

// AllowMsg reports whether conn id may send one message at nowMs under the
// per-conn sliding-window budget (width msgWindowMs). Allowed calls consume
// one slot; rejected calls consume nothing.
func (l *Limiter) AllowMsg(id string, nowMs int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.msgs[id]
	if !ok {
		w = &msgWindow{}
		l.msgs[id] = w
	}
	// Prune timestamps outside (nowMs-1000ms, nowMs]. Timestamps in the
	// future (clock skew) are kept: nowMs-t < 0 < window.
	kept := w.times[:0]
	for _, t := range w.times {
		if nowMs-t < msgWindowMs {
			kept = append(kept, t)
		}
	}
	// Zero the dropped tail so paged-out conns don't retain references.
	for i := len(kept); i < len(w.times); i++ {
		w.times[i] = 0
	}
	w.times = kept
	if len(w.times) >= l.maxMsgs {
		return false
	}
	w.times = append(w.times, nowMs)
	return true
}

// AllowChat reports whether conn id may send one chat message at nowMs under
// the token-bucket budget: capacity chatCap, refill chatFill tokens/sec
// (mirrors m7.go allowChat with an injected clock). Allowed calls consume one
// token; rejected calls consume nothing. Negative clock steps refill nothing.
func (l *Limiter) AllowChat(id string, nowMs int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.chats[id]
	if !ok {
		b = &chatBucket{}
		l.chats[id] = b
	}
	if !b.init {
		b.init = true
		b.tokens = l.chatCap
		b.lastMs = nowMs
	} else {
		elapsed := float64(nowMs-b.lastMs) / 1000.0
		if elapsed > 0 {
			b.tokens += elapsed * l.chatFill
			if b.tokens > l.chatCap {
				b.tokens = l.chatCap
			}
		}
		b.lastMs = nowMs
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Forget drops per-conn msg/chat state for id. Call on disconnect so idle
// conn ids don't accumulate (the IP slot itself is freed via Release).
func (l *Limiter) Forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.msgs, id)
	delete(l.chats, id)
}
