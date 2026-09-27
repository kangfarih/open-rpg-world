// Package controller owns the M13 slice — player/mod/admin command tables
// (behavior-frozen move of the root m13.go commands.ts port).
//
// Ports faithfully:
//   - handlePlayerCommands guild subcommand (kick/rank/invite over the guild
//     registry, exact TS strings, silent unknown subcommands).
//   - handleModeratorCommands (mute/unmute/ban/kick/jail/unjail): rank >=
//     Moderator gate, mod hour caps, self-ban guard, ban-close semantics
//     (raw 'ban' text frame then socket close), jail teleports.
//   - handleAdminCommands (spawn/take/takeitem/copybank/copyinventory/drop/
//     remove/empty/clear/teleport/teletome/teleto/teleall/tp/immortal/
//     toggle/hide/ms/noclip/kill/nuke/aoe/mob/movenpc/nvn/allattack/roam/
//     find/getregion/collision/distance/togglepvp/countdown/popup/quest +
//     achievement admin/timeout/testitems/lootbag): rank >= Admin gate,
//     identical notify strings, clamps and packet shapes.
//   - m13flags persistence model (mute/ban/jail epoch-ms deadlines, noclip,
//     mspeed) with the identical DDL, effect toggle (/toggle + invincible),
//     hide flip, mob admin helpers and the TESTMAP dispatcher body.
//
// Transport and shared state stay with the root server: players map,
// entities, pstates/dbConn and the m5/m6/m7/m9/m10/m11 subsystems are only
// touched through the seams below (CommandConn/CommandBus/CommandPeers plus
// the Flags/GuildAdmin/AdminWorld/MobAdmin/QuestAdmin/InventoryAdmin/
// LootAdmin extensions), which the root adapter (m13.go) implements over its
// globals. Packet shapes are unchanged — frames are built with
// internal/protocol, the same constructors the root uses. The effect/hide
// maps are owned here (per-instance state, no shared-state fight).
package controller

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// TS parity constants (modules.ts + commands.ts caps).
// ---------------------------------------------------------------------------

// Modules.Ranks values the command gates need (modules.ts:327).
const (
	CmdRankModerator = 1
	CmdRankAdmin     = 2
	CmdRankLandlord  = 10
)

// Modules.Effects values used by /toggle (modules.ts:271 enum order).
const (
	CmdEffectTerror     = 3
	CmdEffectStun       = 4
	CmdEffectBurning    = 16
	CmdEffectFreezing   = 17
	CmdEffectInvincible = 18
)

// Mod caps from commands.ts (hours).
const (
	ModMuteCapHours = 168
	ModBanCapHours  = 72
	ModJailCapHours = 12
)

// TPSpots mirrors the /tp [key] switch (commands.ts 'tp'): hardcoded map
// destinations, teleporting with animation.
var TPSpots = map[string][2]int{
	"home":              {191, 166},
	"underwater":        {119, 290},
	"underwatervolcano": {194, 427},
	"kok":               {290, 357},
	"mountain":          {440, 312},
	"santa":             {526, 256},
	"ice":               {608, 332},
	"pink":              {685, 433},
	"hell":              {1112, 787},
	"sewer":             {1117, 709},
	"shroom":            {992, 631},
	"skeletonking":      {120, 794},
	"ogrelord":          {342, 166},
	"queenant":          {591, 820},
	"forestdragon":      {457, 807},
	"crystalcave":       {886, 622},
	"piratecaptain":     {930, 752},
}

// ---------------------------------------------------------------------------
// Persisted player flags (m13flags table model).
// ---------------------------------------------------------------------------

// CommandFlags is the persisted mod/admin state per username: mute/ban/jail
// ms deadlines (epoch ms; 0/absent = clear), noclip flag, movement speed
// override (0 = server default).
type CommandFlags struct {
	Mute   int64 // epoch ms
	Ban    int64 // epoch ms
	Jail   int64 // epoch ms
	Noclip bool
	MSpeed int
}

// FlagsDDL is the m13flags schema (moved verbatim; the root adapter executes
// it under dbMu like the m11 tables precedent).
const FlagsDDL = `CREATE TABLE IF NOT EXISTS m13flags(` +
	`player TEXT PRIMARY KEY, mute INT, ban INT, jail INT, noclip INT, mspeed INT)`

// Muted reports whether the mute deadline is in the future.
func (f CommandFlags) Muted(nowMs int64) bool { return f.Mute > nowMs }

// Banned reports whether the ban deadline is in the future.
func (f CommandFlags) Banned(nowMs int64) bool { return f.Ban > nowMs }

// Jailed reports whether the jail deadline is in the future.
func (f CommandFlags) Jailed(nowMs int64) bool { return f.Jail > nowMs }

// ClampMuteBan applies the moderator hour caps (mute 168h, ban 72h); admins
// and above bypass them (commands.ts rank == Moderator check parity).
func ClampMuteBan(command string, rank, duration int) int {
	if rank != CmdRankModerator {
		return duration
	}
	if command == "mute" && duration > ModMuteCapHours {
		return ModMuteCapHours
	}
	if command == "ban" && duration > ModBanCapHours {
		return ModBanCapHours
	}
	return duration
}

// ClampJail applies the moderator jail cap (12h); admins bypass it.
func ClampJail(rank, duration int) int {
	if rank == CmdRankModerator && duration > ModJailCapHours {
		return ModJailCapHours
	}
	return duration
}

// ClampMoveSpeed applies the /ms bounds (75 floor, 2000 ceiling; values over
// 10000 clamp to the ceiling first). Callers reject non-positive speeds
// before this (exact commands.ts order parity).
func ClampMoveSpeed(speed int) int {
	if speed > 10000 {
		speed = 2000
	}
	if speed < 75 {
		speed = 75
	}
	return speed
}

// ---------------------------------------------------------------------------
// Seams (implemented by the root adapter; never by this package).
// Reuses trade.go Conn/Store/Bus/Peers where they fit, extends where needed.
// ---------------------------------------------------------------------------

// CommandConn is the per-connection view the command tables need. It embeds
// Conn so the same pointer flows through and identity keeps working.
type CommandConn interface {
	Conn
	// Rank is the Modules.Ranks value (chatStateFor rank parity).
	Rank() int
	// MovementSpeed is the session ms-per-tile override.
	MovementSpeed() int
	SetMovementSpeed(int)
	// SetStoreOpen tracks which store the player has open (admin /store).
	SetStoreOpen(string)
}

// CommandBus extends Bus with global fan-out, the ban text frame, socket
// close and sourced notifies.
type CommandBus interface {
	Bus
	Broadcast(frames ...[]any)
	// SendBan writes the raw 'ban' UTF8 text frame (Node sendUTF8('ban')
	// parity, 2s write deadline in the adapter).
	SendBan(instance string)
	// Close drops the socket (connection.close / removeClient parity).
	Close(instance string)
	// NotifySource is the notify() variant with colour + source header
	// (m6NotifyWithSource parity, e.g. the crimsonred jail line).
	NotifySource(instance, message, colour, source string)
}

// CommandPeers abstracts live-connection lookup for commands. ByUsername is
// case-insensitive (world.getPlayerByName lowercase-compare parity);
// Usernames snapshots the online usernames (teleall/nuke/togglepvp parity).
// It deliberately re-declares (rather than embeds) Peers with the narrower
// CommandConn result so call sites never type-assert.
type CommandPeers interface {
	ByInstance(instance string) (CommandConn, bool)
	ByUsername(username string) (CommandConn, bool)
	Usernames() []string
}

// Flags abstracts the m13flags table (dbConn/dbMu stay in the root).
type Flags interface {
	Load(username string) CommandFlags
	Save(username string, f CommandFlags)
}

// GuildAdmin abstracts the socGuild* delegation.
type GuildAdmin interface {
	// InGuild reports whether the username belongs to a guild.
	InGuild(username string) bool
	Invite(c CommandConn, target string)
	Kick(c CommandConn, username string, viaCommand bool)
	RankCommand(c CommandConn, rankStr, username string)
}

// DirtyTracker abstracts markDirty (persistence dirty tracking).
type DirtyTracker interface {
	MarkDirty(username string)
}

// AdminWorld abstracts teleports, combat, region/collision reads, spawn
// frames, NPC scans, PVP, popups and countdown.
type AdminWorld interface {
	Teleport(c CommandConn, x, y int)
	DamagePlayer(c CommandConn, dmg int)
	PlayerHP(c CommandConn) int
	// SetPVP forces the player into PVP mode (m10UpdatePVP parity).
	SetPVP(c CommandConn)
	// Countdown broadcasts the CountdownPacket (instance + time).
	Countdown(c CommandConn, t int)
	SameRegion(ax, ay, bx, by int) bool
	RegionOf(x, y int) int
	TileBlocked(x, y int) bool
	WorldWidth() int
	SetEntityPos(instance string, x, y int)
	// SpawnFrame builds the Welcome Spawn frame for an instance.
	SpawnFrame(instance string) []any
	// LiveNPCPos finds the first live entity tile whose spawn key resolves
	// to npcKey (m13FindNPC entity scan parity).
	LiveNPCPos(npcKey string) (x, y int, ok bool)
}

// MobHandle is an opaque live-mob reference (the root *m9Mob); this package
// only passes it back through MobAdmin.
type MobHandle any

// MobAdmin abstracts the m9 mob registry + combat targeting.
type MobAdmin interface {
	MobFor(instance string) (MobHandle, bool)
	MobInstance(h MobHandle) string
	MobKey(h MobHandle) string
	MobHP(h MobHandle) int
	MobPos(h MobHandle) (x, y int)
	// SetMobPos ports mob.setPosition: registry + roam origin move.
	SetMobPos(h MobHandle, x, y int)
	// Attack makes mob a attack mob b (combat.attack parity).
	Attack(a, b MobHandle)
	// AttackTarget points a mob at an arbitrary instance (player or mob).
	AttackTarget(h MobHandle, target string)
	// ClearTarget drops the target so the tick loop resumes roaming.
	ClearTarget(h MobHandle)
	// HitMob lands a full-damage hit (m9PlayerHit with nil attacker).
	HitMob(h MobHandle, dmg int)
	// Instances snapshots the live mob instance ids.
	Instances() []string
	// SpawnMob spawns key at the tile with Chase override (m9SpawnMob
	// parity); false when no mob with key exists.
	SpawnMob(instance, key string, x, y int) bool
	// MobTalk fires a speech bubble over the mob (talkCallback parity).
	MobTalk(instance, message string)
}

// QuestAdmin abstracts the m11 quest/achievement bridging.
type QuestAdmin interface {
	DirtyTracker
	QuestDef(key string) (stageCount int, ok bool)
	QuestStage(username, key string) (stage, subStage int)
	SetQuestStage(c CommandConn, username, key string, stage, subStage int)
	QuestKeys() []string
	AchDef(key string) (stageCount int, ok bool)
	AchStage(username, key string) int
	SetAchStage(username, key string, stage int)
	AchProgress(c CommandConn, username, key string)
	AchKeys(username string) []string
	// AchDefs snapshots every achievement key -> stage count (m11A parity).
	AchDefs() map[string]int
	SendAchProgress(c CommandConn, key string, stage int)
	SendPopup(c CommandConn, title, message, colour string)
}

// CommandSlot is one container slot snapshot (enchantments never survive
// admin takes; the victim sync only carries key/count like the root).
type CommandSlot struct {
	Key   string
	Count int
}

// InventoryAdmin abstracts the m5 container state the admin table mutates.
type InventoryAdmin interface {
	DirtyTracker
	ItemExists(key string) bool
	AddItem(username, key string, count int) int
	SlotAt(username, container string, index int) (CommandSlot, bool)
	// RemoveAt removes count from the slot index; returns the resulting
	// slot (empty key when cleared) for the victim Container Remove sync.
	RemoveAt(username, container string, index, count int) (key string, left int, ok bool)
	// RemoveKey removes count of key; returns how many were removed.
	RemoveKey(username, container, key string, count int) (removed int)
	EmptyContainer(username, container string)
	CopyContainer(src, dst string, bank bool)
	BankCount(username, key string) int
	InvCount(username, key string) int
	AppendBank(username, key string, count int)
	// BankSlots snapshots the bank as plain slots (/openbank parity —
	// the admin views the named player's bank).
	BankSlots(username string) []CommandSlot
}

// Drop is one exact loot item (m5Drop parity, no drop-table roll). The loot
// registry owns the canonical shape (internal/entity); this aliases it so
// the LootAdmin seam and the double-drops probe share one type.
type Drop = entity.Drop

// LootAdmin abstracts the shared loot registry (/drop + /lootbag spawn the
// exact items with no killer gate).
type LootAdmin interface {
	SpawnLootAt(owner, key string, count, x, y int)
	SpawnLootBag(owner string, x, y int, items []Drop)
}

// HomeStore abstracts the home-point registry (get/set bound coords +
// persist dirtying). The server package implements it over playerStateFor.
type HomeStore interface {
	// GetHome returns the bound home-point coordinates for the player.
	// (0,0) means no home bound (caller falls back to spawn).
	GetHome(username string) (x, y int)
	// SetHome binds the player's home-point to (x,y) and marks persist
	// dirty so the binding survives restarts.
	SetHome(username string, x, y int)
}

// CommandDeps bundles the command seams for one call.
type CommandDeps struct {
	Flags     Flags
	Guilds    GuildAdmin
	World     AdminWorld
	Mobs      MobAdmin
	Quests    QuestAdmin
	Inv       InventoryAdmin
	Loot      LootAdmin
	Bus       CommandBus
	Peers     CommandPeers
	Skills    SkillAdmin
	Abilities AbilityAdmin
	Ranks     RankAdmin
	Pets      PetAdmin
	Poison    PoisonAdmin
	Misc      MiscAdmin
	Home      HomeStore
}

// ---------------------------------------------------------------------------
// Parse entry (extends the m7 dispatcher).
// ---------------------------------------------------------------------------

// ParseCommand runs the command tables after the m7/m12 ones. The m7 player
// table handles players/coords/ping/g/pm; this adds guild + the full
// moderator/admin tables.
func ParseCommand(c CommandConn, command string, blocks []string, d CommandDeps) {
	HomeCommands(c, command, blocks, d)
	GuildCommand(c, command, blocks, d)
	ModeratorCommands(c, command, blocks, d)
	AdminCommands(c, command, blocks, d)
	AdminProgressionCommands(c, command, blocks, d)
}

// firstBlock returns blocks[0] or "" (undostage/resetquest arg parity).
func firstBlock(blocks []string) string {
	if len(blocks) == 0 {
		return ""
	}
	return blocks[0]
}

// ---------------------------------------------------------------------------
// Player commands (no rank gate — available to every player).
// ---------------------------------------------------------------------------

// HomeCommands handles /home and /sethome (home-point recall system).
// No rank gate: every authenticated player can bind and recall to their
// home point. /sethome saves the current tile; /home teleports back.
func HomeCommands(c CommandConn, command string, blocks []string, d CommandDeps) {
	if d.Home == nil {
		return
	}
	switch command {
	case "sethome":
		x, y := c.TileX(), c.TileY()
		d.Home.SetHome(c.PlayerName(), x, y)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Home bound to %d, %d", x, y))
	case "home":
		hx, hy := d.Home.GetHome(c.PlayerName())
		if hx == 0 && hy == 0 {
			d.Bus.Notify(c.InstanceID(), "No home bound. Use /sethome first.")
			return
		}
		d.World.Teleport(c, hx, hy)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Recalled to home (%d, %d)", hx, hy))
	}
}

// ---------------------------------------------------------------------------
// Effect state (per-instance map, PacketEffect Add/Remove parity).
// ---------------------------------------------------------------------------

var (
	cmdEffMu sync.Mutex
	cmdEffs  = map[string]map[int]bool{}
)

// HasEffect reports whether the instance carries the effect.
func HasEffect(c CommandConn, effect int) bool {
	cmdEffMu.Lock()
	defer cmdEffMu.Unlock()
	return cmdEffs[c.InstanceID()][effect]
}

// AddEffect applies the effect + broadcasts Effect Add.
func AddEffect(c CommandConn, effect int, d CommandDeps) {
	cmdEffMu.Lock()
	if cmdEffs[c.InstanceID()] == nil {
		cmdEffs[c.InstanceID()] = map[int]bool{}
	}
	cmdEffs[c.InstanceID()][effect] = true
	cmdEffMu.Unlock()
	d.Bus.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectAdd,
		protocol.EffectData{Instance: c.InstanceID(), Effect: effect}))
}

// RemoveEffect clears the effect + broadcasts Effect Remove.
func RemoveEffect(c CommandConn, effect int, d CommandDeps) {
	cmdEffMu.Lock()
	delete(cmdEffs[c.InstanceID()], effect)
	cmdEffMu.Unlock()
	d.Bus.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectRemove,
		protocol.EffectData{Instance: c.InstanceID(), Effect: effect}))
}

// ClearEffects removes every effect on the instance.
func ClearEffects(c CommandConn, d CommandDeps) {
	cmdEffMu.Lock()
	effs := cmdEffs[c.InstanceID()]
	cmdEffs[c.InstanceID()] = nil
	cmdEffMu.Unlock()
	for e := range effs {
		d.Bus.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectRemove,
			protocol.EffectData{Instance: c.InstanceID(), Effect: e}))
	}
}

// ForgetCommandPlayer drops the per-conn effect state on disconnect.
func ForgetCommandPlayer(instance string) {
	cmdEffMu.Lock()
	delete(cmdEffs, instance)
	cmdEffMu.Unlock()
}

// ---------------------------------------------------------------------------
// Hide state (player.visible flip parity).
// ---------------------------------------------------------------------------

var (
	cmdVisMu  sync.Mutex
	cmdHidden = map[string]bool{}
)

// IsHidden reports whether the instance is currently hidden.
func IsHidden(instance string) bool {
	cmdVisMu.Lock()
	defer cmdVisMu.Unlock()
	return cmdHidden[instance]
}

// ---------------------------------------------------------------------------
// TESTMAP debug dispatcher.
// ---------------------------------------------------------------------------

// testInput mirrors the m13test probe shape.
type testInput struct {
	M13Test  string `json:"m13test"`
	Username string `json:"username"`
	Op       string `json:"op"`
	Key      string `json:"key"`
	Value    int    `json:"value"`
}

// offlineConn is a synthetic username carrier for offline TESTMAP targets —
// flag/echo reads hit the table, notifies go to the requester.
type offlineConn struct{ username string }

func (c *offlineConn) InstanceID() string    { return "" }
func (c *offlineConn) PlayerName() string    { return c.username }
func (c *offlineConn) TileX() int            { return 0 }
func (c *offlineConn) TileY() int            { return 0 }
func (c *offlineConn) GrantContainerAccess() {}
func (c *offlineConn) Rank() int             { return 0 }
func (c *offlineConn) MovementSpeed() int    { return 0 }
func (c *offlineConn) SetMovementSpeed(int)  {}
func (c *offlineConn) SetStoreOpen(string)   {}

// HandleCommandTest ports the m9test/m11test dispatcher pattern: TESTMAP-only
// seeding + echo back through notifies (the e2e greps these). The testMode
// gate stays in the root adapter (it reads a root global); this body is the
// moved remainder.
func HandleCommandTest(c CommandConn, data []byte, d CommandDeps) {
	var in testInput
	if err := json.Unmarshal(data, &in); err != nil {
		return
	}
	var target CommandConn = c
	if in.Username != "" {
		if t, ok := d.Peers.ByUsername(in.Username); ok {
			target = t
		} else {
			target = &offlineConn{username: in.Username}
		}
	}
	switch in.Op {
	case "seed":
		// Seed the flag set wholesale (mute/ban/jail epoch ms + noclip).
		f := d.Flags.Load(target.PlayerName())
		if in.Key == "mute" {
			f.Mute = int64(in.Value)
		}
		if in.Key == "ban" {
			f.Ban = int64(in.Value)
		}
		if in.Key == "jail" {
			f.Jail = int64(in.Value)
		}
		d.Flags.Save(target.PlayerName(), f)
	case "echo":
		// State echo for the e2e: mute/ban/jail/noclip/mspeed.
		f := d.Flags.Load(target.PlayerName())
		verb := "cleared"
		if f.Mute > 0 {
			verb = fmt.Sprintf("until %d", f.Mute)
		}
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("m13:flags user=%s mute=%s ban=%d jail=%d noclip=%v mspeed=%d",
			target.PlayerName(), verb, f.Ban, f.Jail, f.Noclip, f.MSpeed))
	case "seedinv":
		// Seed an inventory stack for the target (TESTMAP container legs).
		d.Inv.AddItem(target.PlayerName(), in.Key, in.Value)
		d.Inv.MarkDirty(target.PlayerName())
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("m13:inv %s=%d", in.Key, d.Inv.InvCount(target.PlayerName(), in.Key)))
	case "inv":
		// Inventory count echo (m13:inv <key>=<n>) for the container legs.
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("m13:inv %s=%d", in.Key, d.Inv.InvCount(target.PlayerName(), in.Key)))
	case "bank":
		// Bank count echo (m13:bank <key>=<n>).
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("m13:bank %s=%d", in.Key, d.Inv.BankCount(target.PlayerName(), in.Key)))
	case "quest":
		// Echo quest state: m11:quest:<key>=<stage>/<stageCount>.
		count, ok := d.Quests.QuestDef(in.Key)
		if !ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("m13:noquest %s", in.Key))
			return
		}
		stage, _ := d.Quests.QuestStage(target.PlayerName(), in.Key)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("m11:quest:%s=%d/%d", in.Key, stage, count))
	}
}

// resolveTarget lowercases + joins blocks then looks the player up
// (m7PlayerByName lowercase-compare parity).
func resolveTarget(blocks []string, d CommandDeps) (CommandConn, string) {
	name := strings.ToLower(strings.Join(blocks, " "))
	if name == "" {
		return nil, ""
	}
	t, ok := d.Peers.ByUsername(name)
	if !ok {
		return nil, name
	}
	return t, name
}

// nowMs is time.Now (deadline math parity with the root clock).
func nowMs() time.Time { return time.Now() }

// hoursDuration converts whole hours to a Duration (mute/ban/jail parity).
func hoursDuration(hours int) time.Duration { return time.Duration(hours) * time.Hour }
