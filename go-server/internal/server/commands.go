// Commands — the full commands.ts port (finishes the chat command half).
//
// Thin adapter over internal/controller (behavior-frozen move, task E8):
// all guild/player/mod/admin command tables, the mute/ban/jail/noclip
// flags model, effect toggle, mob admin helpers, quest/achievement admin
// and the TESTMAP dispatcher body live in the controller package operating
// on the CommandConn/CommandBus/CommandPeers/Flags/GuildAdmin/AdminWorld/
// MobAdmin/QuestAdmin/InventoryAdmin/LootAdmin seams below. This file only
// wires those seams to the root globals (players map, entities, pstates,
// dbConn, player-state/economy/chat/area/mob subsystems) and keeps the entry
// points main.go and other wire files call — with UNCHANGED signatures —
// delegating to the controller. Command behavior, notify strings and the
// flags schema are identical.
//
// Stayed (shared state the controller must not own): the cmdFlags SQLite
// table DDL exec + load/save over dbConn/dbMu, and the loot-spawn helpers
// (spawnLootAt/spawnLootBag) over the shared loot registry.
package server

import (
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"

	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/meta"
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/social"
	worldcore "rpg-world-server/internal/world"
)

// TS parity constants (aliases to the moved commands.ts values).
const (
	EffectTerrorStatus = controller.CmdEffectTerror
	EffectStun         = controller.CmdEffectStun
	EffectFreezingM13  = controller.CmdEffectFreezing
	EffectInvincibleM  = controller.CmdEffectInvincible
	EffectBurningM13   = controller.CmdEffectBurning
)

// Modules.Ranks Landlord value (modules.ts:327).
const (
	RankLandlordM13 = controller.CmdRankLandlord
)

// Mod caps from commands.ts (hours).
const (
	modMuteCap = controller.ModMuteCapHours
	modBanCap  = controller.ModBanCapHours
	modJailCap = controller.ModJailCapHours
)

// tpSpots mirrors the /tp [key] switch (authoritative table lives in the
// controller; shared map, never mutated).
var tpSpots = controller.TPSpots

// cmdFlags is the persisted mod/admin state (authoritative type lives in
// the controller).
type cmdFlags = controller.CommandFlags

// ---------------------------------------------------------------------------
// controller.CommandConn seam (*playerConn satisfies it, so identity is
// preserved). InstanceID/PlayerName/TileX/TileY/GrantContainerAccess come
// from the m12 seam; rank + movement speed extend it here.
// ---------------------------------------------------------------------------

// Rank is the Modules.Ranks value (chatStateFor rank parity).
func (c *playerConn) Rank() int {
	if c == nil {
		return 0
	}
	return chatStateFor(c).rank
}

// MovementSpeed is the session ms-per-tile override.
func (c *playerConn) MovementSpeed() int {
	if c == nil {
		return 0
	}
	return c.Sess.MovementSpeed
}

// SetMovementSpeed applies the /ms override live (checkSpeed reads it).
func (c *playerConn) SetMovementSpeed(v int) {
	if c == nil {
		return
	}
	c.Sess.MovementSpeed = v
}

// cmdUnwrapConn unwraps a controller.CommandConn back to the root conn (subsystem
// calls need it); falls back to an instance lookup for foreign impls.
func cmdUnwrapConn(c controller.CommandConn) *playerConn {
	if pc, ok := c.(*playerConn); ok {
		return pc
	}
	if c == nil {
		return nil
	}
	pc, _ := worldcore.Find[*playerConn](c.InstanceID())
	return pc
}

// ---------------------------------------------------------------------------
// controller.Flags seam (cmdFlags table over dbConn/dbMu; bodies verbatim).
// ---------------------------------------------------------------------------

type cmdFlagsStore struct{}

func (cmdFlagsStore) Load(username string) controller.CommandFlags { return cmdFlagsFor(username) }
func (cmdFlagsStore) Save(username string, f controller.CommandFlags) {
	saveCmdFlags(username, f)
}

// ensureCmdTables creates the flags table up-front (called from main()).
func ensureCmdTables() {
	if dbConn == nil {
		return
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := dbConn.Exec(controller.FlagsDDL); err != nil {
		log.Fatalf("m13: ddl: %v", err)
	}
}

// cmdFlagsFor loads the persisted flags for a username. Missing row = zero
// flags (mute/ban/jail 0, noclip false, mspeed 0 = server default).
func cmdFlagsFor(username string) cmdFlags {
	f := cmdFlags{}
	if dbConn == nil || username == "" {
		return f
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	var noclip int
	if err := dbConn.QueryRow(
		`SELECT mute,ban,jail,noclip,mspeed FROM cmdFlags WHERE player=?`, username,
	).Scan(&f.Mute, &f.Ban, &f.Jail, &noclip, &f.MSpeed); err != nil {
		return cmdFlags{}
	}
	f.Noclip = noclip != 0
	return f
}

// saveCmdFlags upserts the flags row.
func saveCmdFlags(username string, f cmdFlags) {
	if dbConn == nil || username == "" {
		return
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	noclip := 0
	if f.Noclip {
		noclip = 1
	}
	if _, err := dbConn.Exec(
		`INSERT INTO cmdFlags(player,mute,ban,jail,noclip,mspeed) VALUES(?,?,?,?,?,?) `+
			`ON CONFLICT(player) DO UPDATE SET mute=?,ban=?,jail=?,noclip=?,mspeed=?`,
		username, f.Mute, f.Ban, f.Jail, noclip, f.MSpeed,
		f.Mute, f.Ban, f.Jail, noclip, f.MSpeed); err != nil {
		log.Printf("m13: save flags %s: %v", username, err)
	}
}

// isMuted reports whether the user's mute deadline is in the future.
func isMuted(username string) bool {
	return cmdFlagsFor(username).Muted(time.Now().UnixMilli())
}

// isBanned reports whether the user's ban deadline is in the future.
func isBanned(username string) bool {
	return cmdFlagsFor(username).Banned(time.Now().UnixMilli())
}

// isJailed reports whether the user's jail deadline is in the future.
func isJailed(username string) bool {
	return cmdFlagsFor(username).Jailed(time.Now().UnixMilli())
}

// noclipAllowed reports whether the user may pass blocked tiles
// (player.noclip). Called from the movement anti-cheat path.
func noclipAllowed(username string) bool {
	return cmdFlagsFor(username).Noclip
}

// cmdMovementSpeed returns the user's overrideMovementSpeed (0 = default).
func cmdMovementSpeed(username string) int {
	return cmdFlagsFor(username).MSpeed
}

// checkBan is the login ban gate: returns true when the conn must be
// rejected (user.ban future deadline). Node sends 'ban' as a UTF8 text
// frame then closes.
func checkBan(username string) bool {
	return isBanned(username)
}

// ---------------------------------------------------------------------------
// controller.GuildAdmin seam (social_wire delegation).
// ---------------------------------------------------------------------------

type cmdGuilds struct{}

func (cmdGuilds) InGuild(username string) bool {
	_, err := social.GuildOf(username)
	return err == nil
}
func (cmdGuilds) Invite(c controller.CommandConn, target string) {
	socGuildInvite(cmdUnwrapConn(c), target)
}
func (cmdGuilds) Kick(c controller.CommandConn, username string, viaCommand bool) {
	socGuildKick(cmdUnwrapConn(c), username, viaCommand)
}
func (cmdGuilds) RankCommand(c controller.CommandConn, rankStr, username string) {
	socGuildRankCommand(cmdUnwrapConn(c), rankStr, username)
}

// ---------------------------------------------------------------------------
// controller.AdminWorld seam (teleports, combat, region/collision, spawns).
// ---------------------------------------------------------------------------

type cmdWorld struct{}

func (cmdWorld) Teleport(c controller.CommandConn, x, y int) { teleport(cmdUnwrapConn(c), x, y) }
func (cmdWorld) DamagePlayer(c controller.CommandConn, dmg int) {
	mobDamagePlayer(cmdUnwrapConn(c), dmg, nil)
}
func (cmdWorld) PlayerHP(c controller.CommandConn) int { return playerHP(cmdUnwrapConn(c)) }
func (cmdWorld) SetPVP(c controller.CommandConn) {
	if p := cmdUnwrapConn(c); p != nil {
		updatePVP(p, true)
	}
}
func (cmdWorld) Countdown(c controller.CommandConn, t int) {
	worldcore.Broadcast(pkt(PacketCountdown, map[string]any{"instance": c.InstanceID(), "time": t}))
}
func (cmdWorld) SameRegion(ax, ay, bx, by int) bool {
	return worldcore.TileRegion(ax, ay) == worldcore.TileRegion(bx, by)
}
func (cmdWorld) RegionOf(x, y int) int                  { return worldcore.TileRegion(x, y) }
func (cmdWorld) TileBlocked(x, y int) bool              { return tileBlocked(x, y) }
func (cmdWorld) WorldWidth() int                        { loadWorld(); return world.Width }
func (cmdWorld) SetEntityPos(instance string, x, y int) { worldcore.SetEntityPos(instance, x, y) }
func (cmdWorld) SpawnFrame(instance string) []any {
	return pkt(PacketSpawn, welcomePlayer(instance))
}
func (cmdWorld) LiveNPCPos(npcKey string) (int, int, bool) {
	for _, e := range worldcore.EntitySnapshot() {
		if key := resolveNPCKey(nil, e.Instance); key == npcKey {
			return e.X, e.Y, true
		}
	}
	return 0, 0, false
}

// ---------------------------------------------------------------------------
// controller.MobAdmin seam (m9 registry + combat targeting).
// ---------------------------------------------------------------------------

type cmdMobs struct{}

func mobOf(h controller.MobHandle) *mob {
	if h == nil {
		return nil
	}
	m, _ := h.(*mob)
	return m
}

func (cmdMobs) MobFor(instance string) (controller.MobHandle, bool) {
	m := mobFor(instance)
	if m == nil {
		return nil, false
	}
	return m, true
}
func (cmdMobs) MobInstance(h controller.MobHandle) string {
	if m := mobOf(h); m != nil {
		return m.instance
	}
	return ""
}
func (cmdMobs) MobKey(h controller.MobHandle) string {
	if m := mobOf(h); m != nil {
		return m.key
	}
	return ""
}
func (cmdMobs) MobHP(h controller.MobHandle) int {
	m := mobOf(h)
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hp
}
func (cmdMobs) MobPos(h controller.MobHandle) (int, int) {
	m := mobOf(h)
	if m == nil {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.x, m.y
}
func (cmdMobs) SetMobPos(h controller.MobHandle, x, y int) {
	m := mobOf(h)
	if m == nil {
		return
	}
	m.mu.Lock()
	m.x, m.y = x, y
	m.spawnX, m.spawnY = x, y
	m.mu.Unlock()
}
func (cmdMobs) Attack(a, b controller.MobHandle) {
	ma, mb := mobOf(a), mobOf(b)
	if ma == nil || mb == nil {
		return
	}
	ma.mu.Lock()
	ma.target = mb.instance
	ma.lastTgt = time.Now()
	ma.mu.Unlock()
}
func (cmdMobs) AttackTarget(h controller.MobHandle, target string) {
	m := mobOf(h)
	if m == nil {
		return
	}
	m.mu.Lock()
	m.target = target
	m.lastTgt = time.Now()
	m.mu.Unlock()
}
func (cmdMobs) ClearTarget(h controller.MobHandle) {
	m := mobOf(h)
	if m == nil {
		return
	}
	m.mu.Lock()
	m.target = ""
	m.mu.Unlock()
}
func (cmdMobs) HitMob(h controller.MobHandle, dmg int) {
	if m := mobOf(h); m != nil {
		mobPlayerHit(m, nil, dmg)
	}
}
func (cmdMobs) Instances() []string {
	mobMu.Lock()
	defer mobMu.Unlock()
	out := make([]string, 0, len(mobs))
	for inst := range mobs {
		out = append(out, inst)
	}
	return out
}
func (cmdMobs) SpawnMob(instance, key string, x, y int) bool {
	return spawnMob(instance, key, x, y, mobOverrides{Chase: true})
}
func (cmdMobs) MobTalk(instance, message string) {
	chatRouter{}.SendBubble(instance, message, true, "")
}

// ---------------------------------------------------------------------------
// controller.QuestAdmin seam (m11 state bridging).
// ---------------------------------------------------------------------------

type cmdQuests struct{}

func (cmdQuests) MarkDirty(username string) { markDirty(username) }
func (cmdQuests) QuestDef(key string) (int, bool) {
	def := questDefs[key]
	if def == nil {
		return 0, false
	}
	return def.StageCount, true
}
func (cmdQuests) QuestStage(username, key string) (int, int) {
	q := questStateFor(username).quest(key)
	return q.Stage, q.SubStage
}
func (cmdQuests) SetQuestStage(c controller.CommandConn, username, key string, stage, subStage int) {
	questSetStage(cmdUnwrapConn(c), questStateFor(username), key, stage, subStage, true)
}
func (cmdQuests) QuestKeys() []string {
	out := make([]string, 0, len(questDefs))
	for k := range questDefs {
		out = append(out, k)
	}
	return out
}
func (cmdQuests) AchDef(key string) (int, bool) {
	def := achDefs[key]
	if def == nil {
		return 0, false
	}
	return def.StageCount, true
}
func (cmdQuests) AchStage(username, key string) int { return questStateFor(username).Achs[key] }
func (cmdQuests) SetAchStage(username, key string, stage int) {
	questStateFor(username).Achs[key] = stage
}
func (cmdQuests) AchProgress(c controller.CommandConn, username, key string) {
	achProgress(cmdUnwrapConn(c), questStateFor(username), key)
}
func (cmdQuests) AchKeys(username string) []string {
	st := questStateFor(username)
	out := make([]string, 0, len(st.Achs))
	for k := range st.Achs {
		out = append(out, k)
	}
	return out
}
func (cmdQuests) AchDefs() map[string]int {
	out := make(map[string]int, len(achDefs))
	for k, def := range achDefs {
		out[k] = def.StageCount
	}
	return out
}
func (cmdQuests) SendAchProgress(c controller.CommandConn, key string, stage int) {
	sendAchievementProgress(cmdUnwrapConn(c), key, stage)
}
func (cmdQuests) SendPopup(c controller.CommandConn, title, message, colour string) {
	sendQuestPopup(cmdUnwrapConn(c), title, message, colour)
}

// ---------------------------------------------------------------------------
// controller.InventoryAdmin seam (m5 container state).
// ---------------------------------------------------------------------------

type cmdInv struct{}

func (cmdInv) MarkDirty(username string)                   { markDirty(username) }
func (cmdInv) ItemExists(key string) bool                  { return itemInfoFor(key) != nil }
func (cmdInv) AddItem(username, key string, count int) int { return addItem(username, key, count) }
func (cmdInv) SlotAt(username, container string, index int) (controller.CommandSlot, bool) {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	var slots []slotDef
	if container == "inventory" {
		slots = st.Inv
	} else {
		slots = st.Bank
	}
	if index < 0 || index >= len(slots) {
		return controller.CommandSlot{}, false
	}
	return controller.CommandSlot{Key: slots[index].Key, Count: slots[index].Count}, true
}
func (cmdInv) RemoveAt(username, container string, index, count int) (string, int, bool) {
	st := playerStateFor(username)
	pstateMu.Lock()
	var slots *[]slotDef
	if container == "inventory" {
		slots = &st.Inv
	} else {
		slots = &st.Bank
	}
	if index < 0 || index >= len(*slots) {
		pstateMu.Unlock()
		return "", 0, false
	}
	s := (*slots)[index]
	s.Count -= count
	key, left := s.Key, s.Count
	if s.Count > 0 {
		(*slots)[index] = s
	} else {
		*slots = append((*slots)[:index], (*slots)[index+1:]...)
		key, left = "", 0
	}
	pstateMu.Unlock()
	markDirty(username)
	return key, left, true
}
func (cmdInv) RemoveKey(username, container, key string, count int) int {
	remaining := count
	st := playerStateFor(username)
	pstateMu.Lock()
	var slots *[]slotDef
	if container == "inventory" {
		slots = &st.Inv
	} else {
		slots = &st.Bank
	}
	for i := len(*slots) - 1; i >= 0 && remaining > 0; i-- {
		if (*slots)[i].Key != key {
			continue
		}
		take := (*slots)[i].Count
		if take > remaining {
			take = remaining
		}
		(*slots)[i].Count -= take
		remaining -= take
		if (*slots)[i].Count <= 0 {
			*slots = append((*slots)[:i], (*slots)[i+1:]...)
		}
	}
	pstateMu.Unlock()
	if remaining < count {
		markDirty(username)
	}
	return count - remaining
}
func (cmdInv) EmptyContainer(username, container string) {
	st := playerStateFor(username)
	pstateMu.Lock()
	if container == "inventory" {
		st.Inv = nil
	} else {
		st.Bank = nil
	}
	pstateMu.Unlock()
	markDirty(username)
}
func (cmdInv) CopyContainer(src, dst string, bank bool) {
	stSrc := playerStateFor(src)
	stDst := playerStateFor(dst)
	pstateMu.Lock()
	if bank {
		clone := append([]slotDef(nil), stSrc.Bank...)
		stDst.Bank = clone
	} else {
		clone := append([]slotDef(nil), stSrc.Inv...)
		stDst.Inv = clone
	}
	pstateMu.Unlock()
	markDirty(dst)
}
func (cmdInv) BankCount(username, itemKey string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	n := 0
	for _, s := range st.Bank {
		if s.Key == itemKey {
			n += s.Count
		}
	}
	return n
}
func (cmdInv) InvCount(username, itemKey string) int { return invCount(username, itemKey) }
func (cmdInv) AppendBank(username, key string, count int) {
	st := playerStateFor(username)
	pstateMu.Lock()
	st.Bank = append(st.Bank, slotDef{Key: key, Count: count})
	pstateMu.Unlock()
}
func (cmdInv) BankSlots(username string) []controller.CommandSlot {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	out := make([]controller.CommandSlot, 0, len(st.Bank))
	for _, s := range st.Bank {
		out = append(out, controller.CommandSlot{Key: s.Key, Count: s.Count})
	}
	return out
}

// ---------------------------------------------------------------------------
// controller.LootAdmin seam (shared loot registry; bodies verbatim).
// ---------------------------------------------------------------------------

type cmdLoot struct{}

func (cmdLoot) SpawnLootAt(owner, key string, count, x, y int) {
	spawnLootAt(owner, key, count, x, y)
}
func (cmdLoot) SpawnLootBag(owner string, x, y int, items []controller.Drop) {
	drops := make([]dropDef, len(items))
	for i, d := range items {
		drops[i] = dropDef{Key: d.Key, Count: d.Count}
	}
	spawnLootBag(owner, x, y, drops)
}

// spawnLootAt wraps spawnLoot with an explicit tile (the /drop path
// spawns without a killer gate — no drop-table roll, the exact key;
// canonical owner: internal/entity SpawnLootAt).
func spawnLootAt(owner, key string, count, x, y int) {
	entity.SpawnLootAt(owner, key, count, x, y)
}

// spawnLootBag creates a loot entity with the given exact items (used by
// /drop and /lootbag; TS spawns Item/LootBag entities directly; canonical
// owner: internal/entity SpawnLootBag).
func spawnLootBag(owner string, cx, cy int, items []dropDef) {
	entity.SpawnLootBag(owner, cx, cy, items)
}

// ---------------------------------------------------------------------------
// controller.CommandBus / CommandPeers seams (send/broadcast + registry).
// ---------------------------------------------------------------------------

type cmdBus struct{}

func (cmdBus) SendTo(instance string, frames ...[]any) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	_ = gnet.Send(c.Conn, frames...)
}
func (cmdBus) Notify(instance string, message string) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		notifyPlayer(c, message)
	}
}
func (cmdBus) Broadcast(frames ...[]any) { worldcore.Broadcast(frames...) }
func (cmdBus) SendBan(instance string) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	_ = gnet.WriteText(c.Conn.WS, []byte("ban"), 2*time.Second)
}
func (cmdBus) Close(instance string) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	worldcore.RemoveClient(c.Conn.WS)
}
func (cmdBus) NotifySource(instance, message, colour, source string) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		notifyWithSource(c, message, colour, source)
	}
}

type cmdPeers struct{}

func (cmdPeers) ByInstance(instance string) (controller.CommandConn, bool) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return nil, false
	}
	return c, true
}
func (cmdPeers) ByUsername(username string) (controller.CommandConn, bool) {
	c := playerByName(username)
	if c == nil {
		return nil, false
	}
	return c, true
}
func (cmdPeers) Usernames() []string { return playerUsernames() }

// cmdHomeStore implements controller.HomeStore over the in-memory player
// state (playerStateFor). SetHome marks persist dirty so the binding
// flushes on the next 10s cycle.
type cmdHomeStore struct{}

func (cmdHomeStore) GetHome(username string) (int, int) {
	st := playerStateFor(username)
	return st.HomeX, st.HomeY
}
func (cmdHomeStore) SetHome(username string, x, y int) {
	st := playerStateFor(username)
	st.HomeX = x
	st.HomeY = y
	markDirty(username)
}

// cmdDeps wires the controller seams to the root globals.
func cmdDeps() controller.CommandDeps {
	return controller.CommandDeps{
		Flags: cmdFlagsStore{}, Guilds: cmdGuilds{}, World: cmdWorld{},
		Mobs: cmdMobs{}, Quests: cmdQuests{}, Inv: cmdInv{},
		Loot: cmdLoot{}, Bus: cmdBus{}, Peers: cmdPeers{},
		Skills: cmdSkills{}, Abilities: cmdAbilities{}, Ranks: cmdRanks{},
		Pets: cmdPets{}, Poison: cmdPoisonStore{}, Misc: cmdMisc{},
		Home: cmdHomeStore{},
	}
}

// ---------------------------------------------------------------------------
// Entry points (signatures UNCHANGED; m7.go/main.go/world_wire.go call sites
// compile as-is). All logic lives in the controller.
// ---------------------------------------------------------------------------

// cmdParseCommand runs the M13 command tables after the m7/m12 ones. The
// m7 player table handles players/coords/ping/g/pm; m13 adds guild.
func cmdParseCommand(c *playerConn, command string, blocks []string) {
	controller.ParseCommand(c, command, blocks, cmdDeps())
}

// guildCommand ports the 'guild' case over the real guild registry.
func guildCommand(c *playerConn, command string, blocks []string) {
	controller.GuildCommand(c, command, blocks, cmdDeps())
}

// cmdModeratorCommands ports handleModeratorCommands. Gate: rank >= Moderator.
func cmdModeratorCommands(c *playerConn, command string, blocks []string) {
	controller.ModeratorCommands(c, command, blocks, cmdDeps())
}

// adminCommands ports handleAdminCommands. Gate: rank >= Admin.
func adminCommands(c *playerConn, command string, blocks []string) {
	controller.AdminCommands(c, command, blocks, cmdDeps())
}

// containerSlot reads one slot by the TS 0-based array index.
func containerSlot(username, container string, index int) (slotDef, bool) {
	s, ok := controller.ContainerSlot(cmdDeps(), username, container, index)
	if !ok {
		return slotDef{}, false
	}
	return slotDef{Key: s.Key, Count: s.Count}, true
}

// containerRemove removes count from the slot at the TS 0-based index
// and syncs the client if online.
func containerRemove(username, container string, index, count int) {
	controller.ContainerRemove(cmdDeps(), username, container, index, count)
}

// containerRemoveKey removes count of key from the container.
func containerRemoveKey(username, container, key string, count int) {
	controller.ContainerRemoveKey(cmdDeps(), username, container, key, count)
}

// bankCount sums the bank stacks of one key (bank echo helper).
func bankCount(username, itemKey string) int {
	return controller.BankCount(cmdDeps(), username, itemKey)
}

// emptyContainer drops every slot (container.empty()).
func emptyContainer(username, container string) {
	controller.EmptyContainer(cmdDeps(), username, container)
}

// copyContainer clones src's container into dst (copybank/copyinventory).
func copyContainer(src, dst string, bank bool) {
	controller.CopyContainer(cmdDeps(), src, dst, bank)
}

// setMobPos ports mob.setPosition: registry + roam origin move.
func setMobPos(m *mob, x, y int) {
	if m == nil {
		return
	}
	controller.MoveMob(cmdDeps(), m, x, y)
}

// mobAttack makes mob a attack mob b (combat.attack over Character refs).
func mobAttack(a, b *mob) {
	if a == nil || b == nil {
		return
	}
	cmdDeps().Mobs.Attack(a, b)
}

// mobAttackTarget points a mob at an arbitrary instance (player or mob).
func mobAttackTarget(m *mob, target string) {
	if m == nil {
		return
	}
	cmdDeps().Mobs.AttackTarget(m, target)
}

// mobRoam clears the target so the tick loop resumes roaming
// (entity.roamingCallback).
func mobRoam(m *mob) {
	if m == nil {
		return
	}
	cmdDeps().Mobs.ClearTarget(m)
}

// findNPC scans the npcs.json registry + the live entity tiles.
func findNPC(c *playerConn, npcKey string) {
	controller.FindNPC(c, npcKey, cmdDeps())
}

// toggleEffect flips a status effect with the TS notify strings.
func toggleEffect(c *playerConn, effect int) {
	controller.ToggleEffect(c, effect, cmdDeps())
}

// toggleCommand ports the /toggle switch.
func toggleCommand(c *playerConn, blocks []string) {
	controller.ToggleCommand(c, blocks, cmdDeps())
}

// hasEffect reports whether the conn carries the effect.
func hasEffect(c *playerConn, effect int) bool {
	return controller.HasEffect(c, effect)
}

// addEffect applies the effect + broadcasts Effect Add.
func addEffect(c *playerConn, effect int) {
	controller.AddEffect(c, effect, cmdDeps())
}

// removeEffect clears the effect + broadcasts Effect Remove.
func removeEffect(c *playerConn, effect int) {
	controller.RemoveEffect(c, effect, cmdDeps())
}

// clearEffects removes every effect on the conn.
func clearEffects(c *playerConn) {
	controller.ClearEffects(c, cmdDeps())
}

// forgetCmdPlayer drops the per-conn effect state on disconnect.
func forgetCmdPlayer(instance string) {
	controller.ForgetCommandPlayer(instance)
}

// toggleHide ports the 'hide' command: flip player.visible and
// Despawn/Spawn to the surrounding regions.
func toggleHide(c *playerConn) {
	controller.ToggleHide(c, cmdDeps())
}

// isHidden reports whether the instance is currently hidden.
func isHidden(instance string) bool {
	return controller.IsHidden(instance)
}

// questStageShift shifts the quest stage by delta (undostage).
func questStageShift(c *playerConn, key string, delta int) {
	controller.QuestStageShift(c, key, delta, cmdDeps())
}

// questReset sets the quest to stage 0 (resetquest/resetquests).
func questReset(c *playerConn, key string) {
	controller.QuestReset(c, key, cmdDeps())
}

// questResetAll resets every quest.
func questResetAll(c *playerConn) {
	controller.QuestResetAll(c, cmdDeps())
}

// questFinish completes the quest (finishquest).
func questFinish(c *playerConn, key string) {
	controller.QuestFinish(c, key, cmdDeps())
}

// achievementsReset zeroes every achievement stage.
func achievementsReset(c *playerConn, key string) {
	controller.AchievementsReset(c, cmdDeps())
	// Dynmap: commands.ts resetachievements re-sends the region
	// (updateRegion); SetAchStage bypasses achProgress, so push here.
	maybePushDynamicMap(cmdUnwrapConn(c))
}

// achievementFinish completes one achievement.
func achievementFinish(c *playerConn, key string) {
	controller.AchievementFinish(c, key, cmdDeps())
}

// achievementsFinishAll completes every achievement.
func achievementsFinishAll(c *playerConn) {
	controller.AchievementsFinishAll(c, cmdDeps())
}

// testItems empties the bank and adds 100x of every items.json key.
func testItems(c *playerConn) {
	controller.TestItems(c, cmdDeps())
}

// handleCommandTest ports the m9test/m11test dispatcher pattern: TESTMAP-only
// seeding + echo. The testMode gate stays here; the body lives in the
// controller.
func handleCommandTest(c *playerConn, data []byte) {
	if !testMode {
		return
	}
	// Admin-rank gate (see handleMinigameTest: TESTMAP default stays ON, the gate
	// closes the any-client spawn/seed hole).
	if !isAdmin(c) {
		return
	}
	controller.HandleCommandTest(c, data, cmdDeps())
}

// ---------------------------------------------------------------------------
// controller.SkillAdmin seam (m5 skill state + XP frame paths).
// ---------------------------------------------------------------------------

type cmdSkills struct{}

func (cmdSkills) MarkDirty(username string) { markDirty(username) }

func (cmdSkills) SkillOf(c controller.CommandConn, skill int) (int, int, bool) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return 0, 1, false
	}
	st := playerStateFor(pc.Username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	s, ok := st.Skills[skill]
	if !ok || s == nil {
		return 0, 1, false
	}
	return s.XP, s.Level, true
}

func (cmdSkills) AddSkillXP(c controller.CommandConn, skill, amount int) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	addXP(pc, pc.Username, skill, amount)
}

// skillFrames builds the handleExperience withInfo pair: Experience
// Skill + Skill Update with the full serialize (level/percentage/
// nextExperience/combat).
func skillFrames(pc *playerConn, skill int, xp, level int) []gnet.Frame {
	return []gnet.Frame{
		pktOp(PacketExperience, ExperienceSkill, experienceData{
			Instance: pc.Instance, Amount: intp(0), Skill: intp(skill),
		}),
		pktOp(PacketSkill, SkillUpdate, skillData{
			Type: skill, Experience: xp, Level: intp(level),
			Percentage: floatp(percentage(xp)), NextExperience: intp(nextExp(xp)),
			Combat: boolp(combatSkill(skill)),
		}),
	}
}

// skillBatch builds the Skill Batch over every earned skill (login
// loginWelcome shape parity) plus the Experience Sync carrying the
// combat level (skills.sync parity).
func skillBatch(pc *playerConn) []gnet.Frame {
	st := playerStateFor(pc.Username)
	pstateMu.Lock()
	skills := make([]any, 0, len(st.Skills))
	for id, s := range st.Skills {
		skills = append(skills, map[string]any{
			"type": id, "experience": s.XP, "level": s.Level,
			"percentage": percentage(s.XP), "nextExperience": nextExp(s.XP),
			"combat": combatSkill(id),
		})
	}
	combatLevel := st.Level
	pstateMu.Unlock()
	return []gnet.Frame{
		pktOp(PacketSkill, SkillBatch, map[string]any{"skills": skills, "cheater": false}),
		pktOp(PacketExperience, ExperienceSync, experienceData{Instance: pc.Instance, Level: intp(combatLevel)}),
	}
}

func (cmdSkills) SetSkillXP(c controller.CommandConn, skill, xp int) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	st := playerStateFor(pc.Username)
	pstateMu.Lock()
	s, ok := st.Skills[skill]
	if !ok || s == nil {
		s = &skillDef{Level: 1}
		st.Skills[skill] = s
	}
	s.XP = xp
	s.Level = expToLevel(xp)
	if s.Level < 1 {
		s.Level = 1
	}
	if combatSkill(skill) {
		st.Level = combatLevelLocked(st)
	}
	level := s.Level
	pstateMu.Unlock()
	markDirty(pc.Username)
	_ = gnet.Send(pc.Conn, skillFrames(pc, skill, xp, level)...)
}

func (cmdSkills) ResetSkills(c controller.CommandConn) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	st := playerStateFor(pc.Username)
	pstateMu.Lock()
	for _, id := range controller.ProgressionSkillIDs {
		s, ok := st.Skills[id]
		if !ok || s == nil {
			s = &skillDef{Level: 1}
			st.Skills[id] = s
		}
		s.XP, s.Level = 0, 1 // setExperience(0) parity (silent part)
	}
	st.Level = combatLevelLocked(st)
	pstateMu.Unlock()
	markDirty(pc.Username)
	// addExperience(0) per skill is silent at level 1 (no level-up), then
	// skills.sync() — the Batch + Experience Sync pair.
	_ = gnet.Send(pc.Conn, skillBatch(pc)...)
}

func (cmdSkills) MaxSkills(c controller.CommandConn) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	st := playerStateFor(pc.Username)
	pstateMu.Lock()
	for _, id := range controller.ProgressionSkillIDs {
		s, ok := st.Skills[id]
		if !ok || s == nil {
			s = &skillDef{Level: 1}
			st.Skills[id] = s
		}
		s.XP, s.Level = 0, 1 // setExperience(0) first, like TS
	}
	pstateMu.Unlock()
	for _, id := range controller.ProgressionSkillIDs {
		addXP(pc, pc.Username, id, controller.MaxAwardXP)
	}
}

func (cmdSkills) SyncSkills(c controller.CommandConn) {
	if pc := cmdUnwrapConn(c); pc != nil {
		_ = gnet.Send(pc.Conn, skillBatch(pc)...)
	}
}

func (cmdSkills) LevelsToExperience(fromLevel, toLevel int) int {
	tbl := meta.BuildLevelExp(meta.MaxLevel)
	if len(tbl) == 0 {
		return 0
	}
	if fromLevel < 0 {
		fromLevel = 0
	}
	if toLevel < 0 {
		toLevel = 0
	}
	if fromLevel >= len(tbl) {
		fromLevel = len(tbl) - 1
	}
	if toLevel >= len(tbl) {
		toLevel = len(tbl) - 1
	}
	return tbl[toLevel] - tbl[fromLevel] // Formulas.levelsToExperience
}

// ---------------------------------------------------------------------------
// controller.AbilityAdmin seam (abilities registry grant paths).
// ---------------------------------------------------------------------------

type cmdAbilities struct{}

func (cmdAbilities) MarkDirty(username string) { markDirty(username) }
func (cmdAbilities) HasAbility(username, key string) bool {
	return abHas(username, key)
}
func (cmdAbilities) GrantAbility(c controller.CommandConn, username, key string, level int) bool {
	return abGrantAbility(cmdUnwrapConn(c), username, key, level)
}
func (cmdAbilities) QuickSlotAbility(c controller.CommandConn, username, key string, slot int) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	// Same store the C->S Ability QuickSlot opcode writes (HandleAbility
	// QuickSlot branch, owned-ability gate included).
	raw, _ := json.Marshal(map[string]any{"opcode": AbilityQuickSlot, "key": key, "index": slot})
	abHandleAbility(pc, raw)
	markDirty(username)
}
func (cmdAbilities) ResetAbilities(c controller.CommandConn) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	// abilities.reset() parity: clear the server-side unlock map, then the
	// client reload signal (loadCallback -> Ability Batch, empty now).
	abResetAbilities(pc.Username)
	_ = gnet.Send(pc.Conn, abLoginBatch(pc.Username))
	markDirty(pc.Username)
	log.Printf("m13: %s reset abilities", pc.Username)
}

// ---------------------------------------------------------------------------
// controller.RankAdmin seam (player rank sets).
// ---------------------------------------------------------------------------

type cmdRanks struct{}

func (cmdRanks) MarkDirty(username string) { markDirty(username) }
func (cmdRanks) SetRank(target controller.CommandConn, rank int) {
	pc := cmdUnwrapConn(target)
	if pc == nil {
		return
	}
	pc.rank = rank
	chatStateFor(pc).rank = rank
	_ = gnet.Send(pc.Conn, pkt(PacketRank, rank)) // RankPacket(rank)
	// player.sync() region fanout (SyncPacket serialize parity).
	st := playerStateFor(pc.Username)
	pstateMu.Lock()
	x, y, level := st.X, st.Y, st.Level
	st.Rank = rank // durable across relogin via the persist rank column
	pstateMu.Unlock()
	ph := welcomePlayer(pc.Instance)
	ph.X, ph.Y = x, y
	ph.Level = intp(level)
	worldcore.Broadcast(pkt(PacketSync, ph))
	markDirty(pc.Username)
}
func (cmdRanks) SetRankOffline(username string, rank int) {
	// database.setRank parity: persist the rank for an offline player so the
	// login path restores it. Missing row = warn like TS (`No player found
	// with the username ...`), no stub insert.
	if dbConn == nil || username == "" {
		return
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	res, err := dbConn.Exec(`UPDATE players SET rank=? WHERE instance=?`, rank, username)
	if err != nil {
		log.Printf("m13: setrank offline %s: %v", username, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		log.Printf("m13: No player found with the username %s.", username)
		return
	}
	log.Printf("m13: setrank offline %s rank=%d", username, rank)
}

// ---------------------------------------------------------------------------
// controller.PetAdmin seam (pet grant path).
// ---------------------------------------------------------------------------

type cmdPets struct{}

func (cmdPets) GrantPet(c controller.CommandConn, key string) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	mob, item := petResolveKey(key)
	petGrant(pc, mob, item) // duplicate-pet notify lives in the grant
}

// ---------------------------------------------------------------------------
// controller.PoisonAdmin seam (status Tracker pipeline + region scan).
// Poison Apply/Clear ride the status engine (abApplyPoison/abRemovePoison,
// the same calls the poisonous-weapon hook rides via
// gameWorldAdapter.ApplyPoison); the toggle bookkeeping below mirrors the
// controller-side expiry map.
// ---------------------------------------------------------------------------

type cmdPoisonStore struct{}

var (
	poisonMu  sync.Mutex
	poisoned  = map[string]int64{}
	poisonGen int64
)

func (cmdPoisonStore) PoisonHas(instance string) bool {
	poisonMu.Lock()
	defer poisonMu.Unlock()
	return poisoned[instance] != 0
}
func (cmdPoisonStore) PoisonApply(instance string) {
	if instance == "" {
		return
	}
	abApplyPoison(instance)
	poisonMu.Lock()
	poisonGen++
	gen := poisonGen
	poisoned[instance] = gen
	poisonMu.Unlock()
	// Natural Venom expiry clears the toggle state (30s default).
	time.AfterFunc(controller.PoisonExpiry, func() {
		poisonMu.Lock()
		defer poisonMu.Unlock()
		if poisoned[instance] == gen {
			delete(poisoned, instance)
		}
	})
}
func (cmdPoisonStore) PoisonClear(instance string) {
	// Early cure (character.ts setPoison() with no argument): drop the Venom
	// DoT in the status engine, not just the toggle bookkeeping below —
	// otherwise the 30s ticks keep hitting after the cure notify.
	abRemovePoison(instance)
	poisonMu.Lock()
	delete(poisoned, instance)
	poisonMu.Unlock()
}
func (cmdPoisonStore) EntityKind(instance string) string {
	if mobFor(instance) != nil {
		return "mob"
	}
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		return "player"
	}
	if _, _, ok := worldcore.EntityPos(instance); ok {
		return "other"
	}
	return ""
}
func (cmdPoisonStore) RegionCharInstances(c controller.CommandConn) []string {
	adminRegion := worldcore.TileRegion(c.TileX(), c.TileY())
	var out []string
	for _, e := range worldcore.EntitySnapshot() {
		if worldcore.TileRegion(e.X, e.Y) != adminRegion {
			continue
		}
		if mobFor(e.Instance) != nil {
			out = append(out, e.Instance)
			continue
		}
		if p, _ := worldcore.Find[*playerConn](e.Instance); p != nil {
			out = append(out, e.Instance)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// controller.MiscAdmin seam (attack range, debug frame, region resend,
// IP bans shared with the stdin console form).
// ---------------------------------------------------------------------------

type cmdMisc struct{}

// attackRange is the stub's canonical attack range (welcomePlayer
// AttackRange intp(1) everywhere; player.sync recomputes it from the
// weapon in TS, which the stub does not model).
func (cmdMisc) AttackRange(c controller.CommandConn) int { return 1 }

func (cmdMisc) SendDebug(c controller.CommandConn) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	// CommandPacket {command:'debug'}: [Packets.Command, data] (packet.ts
	// serialize with no opcode; Packets.Command = 20).
	_ = gnet.Send(pc.Conn, pkt(PacketCommand, map[string]any{"command": "debug"}))
}

func (cmdMisc) ResendRegions(c controller.CommandConn) {
	pc := cmdUnwrapConn(c)
	if pc == nil {
		return
	}
	// regionsLoaded = [] + updateRegion() parity: recompute interest,
	// then the login region-load burst scoped to the admin (List Spawns +
	// Positions, then one Spawn per surrounding-region entity).
	worldcore.UpdateRegion(pc, pc.Sess.PlayerX, pc.Sess.PlayerY)
	// Dynmap: commands.ts resetregions ends in updateRegion, which
	// re-sends the per-player region (signature-gated here).
	maybePushDynamicMap(pc)
	handleList(pc)
	regions := pc.Conn.Regions()
	regionSet := make(map[int]bool, len(regions))
	for _, r := range regions {
		regionSet[r] = true
	}
	n := 0
	for _, e := range worldcore.EntitySnapshot() {
		if !regionSet[worldcore.TileRegion(e.X, e.Y)] {
			continue
		}
		p, ok := spawnPayload(e.Instance)
		if !ok {
			continue
		}
		_ = gnet.Send(pc.Conn, pkt(PacketSpawn, p))
		n++
	}
	log.Printf("m13: resetregions resent %d spawns to %s", n, pc.Instance)
}

func (cmdMisc) PlayerIP(username string) (string, bool) {
	target := playerByName(username)
	if target == nil {
		return "", false
	}
	return connIP(target), true
}

// connIP resolves the remote host of a conn (ops_wire BanIP parity).
func connIP(target *playerConn) string {
	host, _, err := net.SplitHostPort(gnet.AddrID(target.Conn.WS))
	if err != nil {
		host = gnet.AddrID(target.Conn.WS)
	}
	return host
}

func (cmdMisc) BanIP(ip string) { banIP(ip) }

// banIP records the IP ban + drops matching conns — the same list and
// drop the stdin console /ipban drives (ops_wire BanIP closure parity;
// kept as one helper so both forms share the implementation).
func banIP(ip string) {
	gnet.BanIP(ip)
	for _, k := range worldcore.AllWS() {
		host, _, err := net.SplitHostPort(gnet.AddrID(k))
		if err != nil {
			host = gnet.AddrID(k)
		}
		if host == ip {
			worldcore.RemoveClient(k)
		}
	}
}
