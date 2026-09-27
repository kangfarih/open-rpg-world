package server

// Chat — chat + commands (thin adapter over internal/player/chat).
//
// Ports the Node chat path (incoming.ts handleChat → player.chat →
// sendToRegions / world.globalMessage) and the player+moderator command
// subset of controllers/commands.ts that the current Go stub world can
// support. S→C frames follow common/network/impl/chat.ts: the packet
// carries no opcode, and {instance,...} = entity bubble chat while
// {source,...} = static chatbox line (connection.ts handleChat).
//
// All PURE logic — sanitization, display names, token-bucket math, global
// cooldowns, command tables, outcome strings — lives in
// internal/player/chat. This file keeps ONLY wiring: frame parsing,
// transport (send/broadcast/socRoute*), the per-conn session shape the rest
// of the root package addresses (chatStateFor(...).rank,
// playerByName/playerUsernames, teleport, notifyWithSource) and the
// m12/m13 delegation. Behavior (frames, rate limits, command outcomes) is
// frozen: main.go/ops/social call sites compile unchanged.

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/player/chat"
	worldcore "rpg-world-server/internal/world"
)

// ---------------------------------------------------------------------------
// Opcodes/modules ported from common/network.
// ---------------------------------------------------------------------------

// Opcodes.Network (opcodes.ts:52): Ping0 Pong1 Sync2.
const (
	NetworkPing = 0
	NetworkPong = 1
)

// Modules.Ranks (modules.ts:327) — subset the Go stub models. Values are the
// chat package's canonical ranks, aliased here so rank comparisons across
// the root package stay in one place.
const (
	RankNone      = chat.RankNone
	RankModerator = chat.RankModerator
	RankAdmin     = chat.RankAdmin
)

// RankTitles (modules.ts:365) for chat name prefixing (shared with the chat
// package's DisplayName helper; read-only).
var rankTitles = chat.RankTitles

// Text-pattern aliases (sanitizer/Utils.formatName parity lives in chat;
// kept here so existing references keep compiling).
var whitespaceRe = chat.WhitespaceRe

// isAdmin reports whether c carries Admin rank or higher. TESTMAP debug
// dispatchers (m8/m9/m10/m11/m12/m13/world/abilities/pets/social/handoff)
// gate on this: the TESTMAP=1 default stays ON for the dev workflow (map +
// harness depend on it), so the rank gate — not a mode flip — is what keeps
// arbitrary clients from warping/spawning/setstaging.
func isAdmin(c *playerConn) bool {
	if c == nil {
		return false
	}
	return chatStateFor(c).rank >= RankAdmin
}

// ---------------------------------------------------------------------------
// Per-connection chat state.
// ---------------------------------------------------------------------------

// chatState carries the per-connection M7 session fields. Rate limiting is
// a token bucket over the region chat (commands/global paths only tick the
// rank cooldown like Node's lastGlobalChat), so spamming cannot flood a
// region regardless of rank. Bucket math delegates to chat.AllowBucket.
type chatState struct {
	bucketMu   sync.Mutex
	tokens     float64   // refillable chat tokens
	lastRefill time.Time // last token refill timestamp

	lastGlobalChat int64 // ms, mirrors player.lastGlobalChat
	rank           int   // Modules.Ranks value (0 = None)
}

const chatBucketSize = chat.BucketSize         // burst capacity
const chatRefillPerSec = chat.RefillPerSec     // one message per 2 seconds
const globalChatCooldown = chat.GlobalCooldown // Ranks.None cooldown (player.ts getGlobalChatCooldown default)

// allowChat consumes one token, refilling elapsed-time first. Calls with no
// tokens left are rejected (Node has no equivalent — it trusts the client's
// input box — but a server clone needs the guard; keeps Node check order
// intact otherwise).
func (cs *chatState) allowChat() bool {
	cs.bucketMu.Lock()
	defer cs.bucketMu.Unlock()

	ok, tokens, refill := chat.AllowBucket(cs.tokens, cs.lastRefill, time.Now())
	cs.tokens, cs.lastRefill = tokens, refill
	return ok
}

// globalChatReady ports canGlobalChat(): the rank-based cooldown between
// global messages (default rank = 60s, mods/admins = 5s).
func (cs *chatState) globalChatReady() bool {
	return chat.GlobalReady(cs.rank, cs.lastGlobalChat, nowMillis())
}

// globalChatDuration ports getGlobalChatDuration(): whole minutes left on
// the cooldown, minimum 1 (player.ts).
func (cs *chatState) globalChatDuration() int {
	return chat.GlobalDuration(cs.rank, cs.lastGlobalChat, nowMillis())
}

func nowMillis() int64 {
	return chat.NowMillis()
}

// chatStateFor returns the M7 state attached to a playerConn, creating it
// lazily (chatConn state lives beside the M6 store/talk fields).
func chatStateFor(c *playerConn) *chatState {
	if c.chat == nil {
		c.chat = &chatState{rank: c.rank}
	}
	return c.chat
}

// ---------------------------------------------------------------------------
// S→C payload shapes (common/network/impl/chat.ts + client handleChat).
// ---------------------------------------------------------------------------

// chatPacketData mirrors ChatPacketData. Instance-based frames carry a
// bubble over the entity; source-based frames are static chatbox lines.
type chatPacketData struct {
	Instance   string `json:"instance,omitempty"`
	Message    string `json:"message"`
	WithBubble bool   `json:"withBubble,omitempty"`
	Colour     string `json:"colour,omitempty"`
	Source     string `json:"source,omitempty"`
}

// ---------------------------------------------------------------------------
// Transport seams (chat.Moderation/chat.Router over live root state).
// ---------------------------------------------------------------------------

// chatModeration implements chat.Moderation over the persisted m13 mute flags.
type chatModeration struct{}

func (chatModeration) IsMuted(username string) bool { return isMuted(username) }

// chatRouter implements chat.Router over the live transports: region bubble
// broadcast, global Router fan-out, and sourced unicasts.
type chatRouter struct{}

var _ chat.Router = chatRouter{}

func (chatRouter) SendBubble(instance, message string, withBubble bool, colour string) {
	worldcore.Broadcast(pkt(PacketChat, chatPacketData{
		Instance:   instance,
		Message:    message,
		WithBubble: withBubble,
		Colour:     colour,
	}))
}

func (chatRouter) SendGlobal(source, message, colour string) {
	socRouteGlobal(pkt(PacketChat, chatPacketData{
		Source:  source,
		Message: message,
		Colour:  colour,
	}))
}

func (chatRouter) SendSourced(username, message, colour, source string) {
	if t := playerByName(username); t != nil {
		notifyWithSource(t, message, colour, source)
	}
}

var defaultRouter = chatRouter{}

// ---------------------------------------------------------------------------
// Chat entry point (incoming.ts handleChat + player.chat).
// ---------------------------------------------------------------------------

// handleChat is the PacketChat dispatcher (C→S Chat frame = [text]).
// Gate order is frozen: sanitize → visible-text → command bypass → region
// bucket → ops limiter → mute check → region chat.
func handleChat(c *playerConn, frame clientFrame) {
	if len(frame) < 2 {
		return
	}
	var raw []string
	if err := json.Unmarshal(frame[1], &raw); err != nil || len(raw) == 0 {
		return
	}

	text := sanitize(raw[0])
	if !chat.HasVisibleText(text) {
		return
	}

	// Commands (/ or ; prefix) bypass chat entirely (incoming.ts:476).
	if chat.IsCommand(text) {
		parseCommand(c, text)
		return
	}

	cs := chatStateFor(c)

	// Rate limit before the mute check so floods cannot burn CPU on the
	// filter path (Node has no bucket; this is the Go hardening slice).
	if !cs.allowChat() {
		return
	}

	// Ops limiter: shared per-conn chat bucket (same silent drop as the
	// bucket-exhaust above — no notify).
	if !gnet.AllowChat(c.Conn) {
		return
	}

	// Mute gate (incoming.ts:479): the m13 slice persists user.mute in the
	// players.data blob and rejects chat while the deadline is in the future.
	if (chatModeration{}).IsMuted(c.Username) {
		notifyPlayer(c, chat.MutedText())
		return
	}

	// Profanity filter (incoming.ts:481): mute check first, then clean.
	text = chat.Clean(text)

	sendChat(c, text, false, true, "")
}

// sanitize ports sanitizer.escape + sanitize (delegates to chat.Sanitize).
func sanitize(s string) string {
	return chat.Sanitize(s)
}

// formatName ports Utils.formatName (delegates to chat.FormatName).
func formatName(name string) string {
	return chat.FormatName(name)
}

// sendChat ports player.chat(message, global, withBubble, colour): rank
// prefix + global cooldown → region bubble or world broadcast.
func sendChat(c *playerConn, message string, global bool, withBubble bool, colour string) {
	cs := chatStateFor(c)

	if global {
		if !cs.globalChatReady() {
			notifyPlayer(c, chat.GlobalCooldownNotice(cs.globalChatDuration()))
			return
		}
		cs.lastGlobalChat = nowMillis()
	}

	name := chat.DisplayName(c.Username, cs.rank)
	colour = chat.ResolveColour(cs.rank, colour)

	if global {
		// world.globalMessage: [Global] prefix source frame, no bubble.
		// All-in-one hub routing: resolve the online set via the Router and
		// unicast; fall back to the existing broadcast when nobody resolves.
		defaultRouter.SendGlobal(chat.GlobalSource(name), message, colour)
		return
	}

	// Region-scoped bubble (player.chat → sendToRegions): the broadcast
	// helper resolves c.Instance's tile and fans out to the 9-region
	// interest sets, matching world.push(Regions).
	defaultRouter.SendBubble(c.Instance, message, withBubble, colour)
}

// ---------------------------------------------------------------------------
// Commands (controllers/commands.ts).
// ---------------------------------------------------------------------------

// parseCommand ports Commands.parse (delegates prefix/split to
// chat.SplitCommand), then runs the player/mod command tables in order plus
// the M12 crafting and M13 guild/mod/admin tables.
func parseCommand(c *playerConn, rawText string) {
	command, args, ok := chat.SplitCommand(rawText)
	if !ok {
		return
	}

	chatPlayerCommands(c, command, args)
	moderatorCommands(c, command, args)
	craftingCommands(c, command)     // M12: crafting interface opens (/crafting etc.)
	cmdParseCommand(c, command, args) // M13: guild + full mod/admin tables
}

// chatPlayerCommands ports handlePlayerCommands (the subset meaningful in the
// Go stub world): players, coords, g/gc/global, pm/msg.
func chatPlayerCommands(c *playerConn, command string, blocks []string) {
	cmd, ok := chat.ClassifyPlayer(command)
	if !ok {
		return
	}
	switch cmd {
	case chat.CmdPlayers:
		names := playerUsernames()
		notifyPlayer(c, chat.PlayersSummary(len(names)))
		if chatStateFor(c).rank == RankAdmin {
			notifyPlayer(c, strings.Join(names, ", "))
		}

	case chat.CmdCoords:
		notifyPlayer(c, chat.CoordsText(c.Sess.PlayerX, c.Sess.PlayerY))

	case chat.CmdPing:
		// player.ping(): Network Ping frame, bypassing the outbox queue.
		_ = gnet.Send(c.Conn, pktOp(PacketNetwork, NetworkPing, nil))

	case chat.CmdGlobal:
		sendChat(c, strings.Join(blocks, " "), true, false, chat.GlobalColour)

	case chat.CmdPM:
		username, message, ok := chat.ParsePrivateMessage(blocks)
		if !ok {
			return
		}
		sendPrivateMessage(c, username, message)
	}
}

// moderatorCommands ports handleModeratorCommands (subset): /teleport.
// Rank gate mirrors the isMod/isAdmin/isHollowAdmin early return.
func moderatorCommands(c *playerConn, command string, blocks []string) {
	if !chat.ModAllowed(chatStateFor(c).rank) {
		return
	}
	if cmd, ok := chat.ClassifyMod(command); ok && cmd == chat.ModTeleport {
		if x, y, ok := chat.ParseTeleportArgs(blocks); ok {
			teleport(c, x, y)
		}
	}
}

// sendPrivateMessage ports player.sendPrivateMessage + sendMessage: an
// offline target notifies misc:NOT_ONLINE; delivery is an aquamarine
// Notification with a [From <name>] source (both sides for the sender).
func sendPrivateMessage(c *playerConn, playerName string, message string) {
	// All-in-one hub routing: resolve the direct target via the Router first;
	// an offline target falls back to the existing misc:NOT_ONLINE notify.
	target := socRouteChat(playerName)
	if target == nil {
		notifyPlayer(c, chat.PMOffline(playerName))
		return
	}
	formatted := formatName(c.Username)
	fromSource, toSource := chat.PMSources(formatted, formatName(target.Username))
	defaultRouter.SendSourced(target.Username, message, chat.PMColour, fromSource)
	defaultRouter.SendSourced(c.Username, message, chat.PMColour, toSource)
}

// teleport ports character.teleport: set position, Teleport frame to the
// surrounding regions (which the Go broadcast scopes by the entity tile).
// The tracked plateauLevel refreshes on the landing tile (door/teleport
// destinations can sit on a different plateau than the origin), and the
// authoritative tile is tracked via trackPos (persist parity: saves must
// record the post-teleport tile, not the stale pre-teleport one). All
// server-side teleports (doors, mod/admin teleport/teleall/teletome/
// teleto/tp) funnel through here.
func teleport(c *playerConn, x, y int) {
	if c == nil || c.Conn == nil {
		return
	}
	withTeleportBypass(c, func() {
		c.Sess.PlayerX = x
		c.Sess.PlayerY = y
		c.regionsLoaded = nil // region streaming: force re-stream on landing
		worldcore.SetEntityPos(c.Instance, x, y)
		worldcore.UpdateRegion(c, x, y)
		worldcore.Broadcast(pkt(PacketTeleport, teleportData{Instance: c.Instance, X: x, Y: y}))
		c.markTeleported()
		plateauTrack(c)
		trackPos(c)
	})
}

// ---------------------------------------------------------------------------
// Player registry helpers (world/entities equivalents over the Go stub).
// ---------------------------------------------------------------------------

// playerUsernames snapshots online usernames.
func playerUsernames() []string {
	var names []string
	for _, c := range worldcore.AllOf[*playerConn]() {
		if c.Username != "" {
			names = append(names, c.Username)
		}
	}
	return names
}

// playerByName finds an online conn by username (case-insensitive, like
// world.getPlayerByName's lowercase compare).
func playerByName(name string) *playerConn {
	lower := strings.ToLower(name)
	for _, c := range worldcore.AllOf[*playerConn]() {
		if strings.ToLower(c.Username) == lower {
			return c
		}
	}
	return nil
}

// notifyWithSource is the notify() variant with a source header
// (player.notify(message, colour, message.title) → Notification Text). The
// payload shape lives in the chat package (chat.SourceNotice); this wrapper
// keeps the established call sites (m7 PM path, m13 jail path) compiling.
func notifyWithSource(c *playerConn, message string, colour string, source string) {
	n := chat.Notice(message, colour, source)
	col := n.Colour
	src := n.Source
	_ = gnet.Send(c.Conn, pktOp(PacketNotification, NotificationText, notificationPacketData{
		Message: n.Message,
		Colour:  &col,
		Source:  &src,
	}))
}
