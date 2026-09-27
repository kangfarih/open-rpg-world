package controller

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"rpg-world-server/internal/protocol"
)

// AdminCommands ports handleAdminCommands. Gate: rank >= Admin.
func AdminCommands(c CommandConn, command string, blocks []string, d CommandDeps) {
	if c.Rank() < CmdRankAdmin {
		return
	}

	switch command {
	// --- item/inventory -----------------------------------------------------
	case "spawn": // /spawn [key] [count]
		if len(blocks) == 0 {
			return
		}
		key := blocks[0]
		count := 1
		if len(blocks) > 1 {
			fmt.Sscanf(blocks[1], "%d", &count)
		}
		if count < 1 {
			count = 1
		}
		if !d.Inv.ItemExists(key) {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("No item with key %s exists.", key))
			return
		}
		idx := d.Inv.AddItem(c.PlayerName(), key, count)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Spawned %dx %s (slot %d).", count, key, idx))

	case "take": // /take [index] [container=bank/inventory] [username]
		var index int
		container, username := "", ""
		if len(blocks) > 0 {
			fmt.Sscanf(blocks[0], "%d", &index)
		}
		if len(blocks) > 1 {
			container = blocks[1]
		}
		if len(blocks) > 2 {
			username = strings.Join(blocks[2:], " ")
		}
		if index < 1 || username == "" {
			d.Bus.Notify(c.InstanceID(), "Invalid command, usage /take [index] [container=bank/inventory] [username]")
			return
		}
		target, ok := d.Peers.ByUsername(username)
		if !ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Player %s not found.", username))
			return
		}
		slot, ok := ContainerSlot(d, target.PlayerName(), container, index)
		if !ok || slot.Key == "" {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Player %s has no item at index %d.", username, index))
			return
		}
		ContainerRemove(d, target.PlayerName(), container, index, slot.Count)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Took %dx %s from %s.", slot.Count, slot.Key, username))

	case "takeitem": // /takeitem [key] [count] [container] [username]
		key, container, username := "", "", ""
		count := 0
		if len(blocks) > 0 {
			key = blocks[0]
		}
		if len(blocks) > 1 {
			fmt.Sscanf(blocks[1], "%d", &count)
		}
		if len(blocks) > 2 {
			container = blocks[2]
		}
		if len(blocks) > 3 {
			username = strings.Join(blocks[3:], " ")
		}
		if key == "" || username == "" || (container != "inventory" && container != "bank") {
			d.Bus.Notify(c.InstanceID(), "Invalid command, usage /takeitem [key] [count] [container=bank/inventory] [username]")
			return
		}
		if _, ok := d.Peers.ByUsername(username); !ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Player %s not found.", username))
			return
		}
		ContainerRemoveKey(d, username, container, key, count)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Took %dx %s from %s.", count, key, username))

	case "copybank", "copyinventory": // clone the target container
		username := strings.Join(blocks, " ")
		if username == "" {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Invalid command, usage /%s [username]", command))
			return
		}
		target, ok := d.Peers.ByUsername(username)
		if !ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Player %s is not online.", username))
			return
		}
		CopyContainer(d, target.PlayerName(), c.PlayerName(), command == "copybank")
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Copied %s's %s to your %s.", username, command, command))

	case "drop": // spawn a ground item at the player's feet
		if len(blocks) == 0 {
			return
		}
		key := blocks[0]
		count := 1
		if len(blocks) > 1 {
			fmt.Sscanf(blocks[1], "%d", &count)
		}
		if count < 1 {
			count = 1
		}
		d.Loot.SpawnLootAt(c.PlayerName(), key, count, c.TileX(), c.TileY())

	case "remove": // /remove [key] [count] from own inventory
		if len(blocks) < 2 {
			return
		}
		count := 0
		fmt.Sscanf(blocks[1], "%d", &count)
		if count < 1 {
			return
		}
		ContainerRemoveKey(d, c.PlayerName(), "inventory", blocks[0], count)

	case "empty":
		EmptyContainer(d, c.PlayerName(), "inventory")

	case "clear": // forEachSlot remove — same end state for the inventory
		EmptyContainer(d, c.PlayerName(), "inventory")

	case "store": // /store [key] — open any store by key (admin shortcut)
		if len(blocks) == 0 {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /store key")
			return
		}
		key := blocks[0]
		store := StoreFor(key)
		if store == nil {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("No store with key %s exists.", key))
			return
		}
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketStore, protocol.StoreOpen, SerializeStore(store)))
		c.SetStoreOpen(key)

	// --- teleport -----------------------------------------------------------
	case "teleport": // admin version accepts the withAnimation flag
		if len(blocks) < 2 {
			return
		}
		var x, y, anim int
		fmt.Sscanf(blocks[0], "%d", &x)
		fmt.Sscanf(blocks[1], "%d", &y)
		if x != 0 && y != 0 {
			_ = anim // animation flag is client-rendered; Go teleport is instant
			d.World.Teleport(c, x, y)
		}

	case "teletome":
		target, _ := resolveTarget(blocks, d)
		if target != nil {
			d.World.Teleport(target, c.TileX(), c.TileY())
		}

	case "teleto":
		target, _ := resolveTarget(blocks, d)
		if target != nil {
			d.World.Teleport(c, target.TileX(), target.TileY())
		}

	case "teleall": // every online player -> the admin's tile
		for _, name := range d.Peers.Usernames() {
			if p, ok := d.Peers.ByUsername(name); ok && p.InstanceID() != c.InstanceID() {
				d.World.Teleport(p, c.TileX(), c.TileY())
			}
		}

	case "tp":
		if len(blocks) == 0 {
			return
		}
		if spot, ok := TPSpots[blocks[0]]; ok {
			d.World.Teleport(c, spot[0], spot[1])
		}

	// --- character ----------------------------------------------------------
	case "immortal", "nohit", "invincible": // Invincible effect toggle
		ToggleEffect(c, CmdEffectInvincible, d)

	case "toggle":
		ToggleCommand(c, blocks, d)

	case "hide": // visibility flip: Despawn/Spawn to the surrounding regions
		ToggleHide(c, d)

	case "ms": // /ms [speed] -> overrideMovementSpeed (75 floor, 2000 ceiling)
		if len(blocks) == 0 {
			d.Bus.Notify(c.InstanceID(), "No movement speed specified.")
			return
		}
		speed := 0
		fmt.Sscanf(blocks[0], "%d", &speed)
		if speed <= 0 {
			d.Bus.Notify(c.InstanceID(), "Invalid movement speed specified.")
			return
		}
		speed = ClampMoveSpeed(speed)
		f := d.Flags.Load(c.PlayerName())
		f.MSpeed = speed
		d.Flags.Save(c.PlayerName(), f)
		c.SetMovementSpeed(speed) // live too (checkSpeed reads the session)

	case "noclip":
		f := d.Flags.Load(c.PlayerName())
		f.Noclip = !f.Noclip
		d.Flags.Save(c.PlayerName(), f)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Noclip: %v", f.Noclip))

	case "kill": // /kill username|instance — full-damage hit
		username := strings.Join(blocks, " ")
		if username == "" {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /kill username/instance")
			return
		}
		if target, ok := d.Peers.ByUsername(username); ok {
			d.World.DamagePlayer(target, d.World.PlayerHP(target))
			return
		}
		if m, ok := d.Mobs.MobFor(username); ok {
			d.Mobs.HitMob(m, d.Mobs.MobHP(m))
		}

	case "nuke": // kill every character in the region (players only with `all`)
		all := len(blocks) > 0 && blocks[0] != ""
		px, py := c.TileX(), c.TileY()
		for _, name := range d.Peers.Usernames() {
			p, ok := d.Peers.ByUsername(name)
			if !ok || p.InstanceID() == c.InstanceID() {
				continue
			}
			if !all {
				continue // region scan: the Go stub region == the 9-region set;
				// players on the admin's tile region only — keep it simple and
				// kill same-tile-region players (documented divergence).
			}
			if d.World.SameRegion(px, py, p.TileX(), p.TileY()) {
				d.World.DamagePlayer(p, d.World.PlayerHP(p))
			}
		}
		d.Bus.Notify(c.InstanceID(), "Congratulations, you killed everyone, are you happy with yourself?")

	case "aoe": // self-hit for 600 (commands.ts 'aoe')
		d.World.DamagePlayer(c, 600)

	// --- mobs ---------------------------------------------------------------
	case "mob": // /mob [key] -> spawnMob at the player's tile
		if len(blocks) == 0 {
			d.Bus.Notify(c.InstanceID(), "No mob specified.")
			return
		}
		inst := fmt.Sprintf("m13-%d", time.Now().UnixNano()%1000000)
		if !d.Mobs.SpawnMob(inst, blocks[0], c.TileX(), c.TileY()) {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("No mob with key %s exists.", blocks[0]))
		}

	case "movenpc": // /movenpc [instance] [x] [y] — mobs only (isMob gate)
		if len(blocks) < 3 {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /movenpc instance x y")
			return
		}
		var x, y int
		fmt.Sscanf(blocks[1], "%d", &x)
		fmt.Sscanf(blocks[2], "%d", &y)
		if m, ok := d.Mobs.MobFor(blocks[0]); ok {
			MoveMob(d, m, x, y)
		} else {
			d.Bus.Notify(c.InstanceID(), "Entity not found.")
		}

	case "nvn": // NPC vs NPC
		if len(blocks) < 2 {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /nvn instance target")
			return
		}
		a, okA := d.Mobs.MobFor(blocks[0])
		b, okB := d.Mobs.MobFor(blocks[1])
		if !okA || !okB {
			d.Bus.Notify(c.InstanceID(), "Could not find entity instances specified.")
			return
		}
		d.Mobs.Attack(a, b)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("%s is attacking %s", d.Mobs.MobKey(a), d.Mobs.MobKey(b)))

	case "allattack": // every mob in the region attacks the target instance
		if len(blocks) == 0 {
			d.Bus.Notify(c.InstanceID(), "Invalid command. Usage: /allattack [target_instance]")
			return
		}
		if _, ok := d.Mobs.MobFor(blocks[0]); !ok {
			return
		}
		px, py := c.TileX(), c.TileY()
		for _, inst := range d.Mobs.Instances() {
			m, ok := d.Mobs.MobFor(inst)
			if !ok || inst == blocks[0] {
				continue
			}
			mx, my := d.Mobs.MobPos(m)
			if d.World.SameRegion(px, py, mx, my) {
				d.Mobs.AttackTarget(m, blocks[0])
			}
		}

	case "roam":
		d.Bus.Notify(c.InstanceID(), "All mobs in the region will now roam!")
		px, py := c.TileX(), c.TileY()
		for _, inst := range d.Mobs.Instances() {
			m, ok := d.Mobs.MobFor(inst)
			if !ok {
				continue
			}
			mx, my := d.Mobs.MobPos(m)
			if d.World.SameRegion(px, py, mx, my) {
				d.Mobs.ClearTarget(m)
			}
		}

	case "find": // /find [npcKey]
		FindNPC(c, strings.Join(blocks, " "), d)

	case "talk": // /talk [instance] — trigger mob talkCallback
		if len(blocks) == 0 {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /talk instance")
			return
		}
		instance := blocks[0]
		if _, ok := d.Mobs.MobFor(instance); !ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Could not find entity with instance: %s", instance))
			return
		}
		d.Mobs.MobTalk(instance, "This is a test talking message lol")

	// --- world/info ---------------------------------------------------------
	case "getregion":
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Current Region: %d", d.World.RegionOf(c.TileX(), c.TileY())))

	case "collision": // /collision [x] [y]
		x, y := c.TileX(), c.TileY()
		if len(blocks) > 0 {
			fmt.Sscanf(blocks[0], "%d", &x)
		}
		if len(blocks) > 1 {
			fmt.Sscanf(blocks[1], "%d", &y)
		}
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("%v - index: %d", d.World.TileBlocked(x, y), y*d.World.WorldWidth()+x))

	case "distance": // Utils.getDistance is the Manhattan distance
		if len(blocks) < 2 {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /distance x y")
			return
		}
		var x, y int
		fmt.Sscanf(blocks[0], "%d", &x)
		fmt.Sscanf(blocks[1], "%d", &y)
		dx, dy := c.TileX()-x, c.TileY()-y
		if dx < 0 {
			dx = -dx
		}
		if dy < 0 {
			dy = -dy
		}
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Distance: %d", dx+dy))

	case "togglepvp": // force every player into PVP mode
		for _, name := range d.Peers.Usernames() {
			if p, ok := d.Peers.ByUsername(name); ok {
				d.World.SetPVP(p)
			}
		}

	case "countdown": // CountdownPacket broadcast to nearby regions
		if len(blocks) == 0 {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /countdown time")
			return
		}
		var t int
		fmt.Sscanf(blocks[0], "%d", &t)
		if t > 0 {
			d.World.Countdown(c, t)
		}

	case "popup": // hardcoded quest popup from commands.ts
		d.Quests.SendPopup(c, "New Quest Found!", "@blue@New @darkblue@quest @green@has@red@ been discovered!", "#00000")

	// --- quest/achievement --------------------------------------------------
	case "undostage":
		QuestStageShift(c, firstBlock(blocks), -1, d)

	case "resetquest":
		QuestReset(c, firstBlock(blocks), d)

	case "resetquests":
		QuestResetAll(c, d)

	case "finishquest":
		QuestFinish(c, firstBlock(blocks), d)

	case "resetachievements":
		AchievementsReset(c, d)

	case "finishachievement":
		AchievementFinish(c, firstBlock(blocks), d)

	case "finishachievements":
		AchievementsFinishAll(c, d)

	// --- misc ---------------------------------------------------------------
	case "timeout": // reject the commander's own connection
		d.Bus.Close(c.InstanceID())

	case "testitems": // 100x every items.json key into the bank
		TestItems(c, d)

	case "lootbag": // hardcoded loot bag (oldonesblade/froghelm/1500 gold)
		d.Loot.SpawnLootBag(c.PlayerName(), c.TileX(), c.TileY(),
			[]Drop{{Key: "oldonesblade", Count: 1}, {Key: "froghelm", Count: 1}, {Key: "gold", Count: 1500}})
	}
}

// ---------------------------------------------------------------------------
// Admin helpers.
// ---------------------------------------------------------------------------

// ContainerSlot reads one slot by the TS 0-based array index (container.get
// returns slots[index] directly; the /take parse only rejects 0 via !index).
func ContainerSlot(d CommandDeps, username, container string, index int) (CommandSlot, bool) {
	return d.Inv.SlotAt(username, container, index)
}

// containerType maps the container name to the ContainerType (bank defaults
// the unknown-container branch like the root, which treats non-inventory as
// bank).
func containerType(container string) int {
	if container == "inventory" {
		return protocol.ContainerTypeInventory
	}
	return protocol.ContainerTypeBank
}

// ContainerRemove removes count from the slot at the TS 0-based index
// (container.remove(index, count)) and syncs the client if online.
func ContainerRemove(d CommandDeps, username, container string, index, count int) {
	key, left, ok := d.Inv.RemoveAt(username, container, index, count)
	if !ok {
		return
	}
	// container.remove fires the removeCallback -> the victim's client gets
	// a Container Remove sync (the admin sees nothing).
	if t, ok := d.Peers.ByUsername(username); ok {
		d.Bus.SendTo(t.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerRemove, protocol.ContainerData{
			Type: containerType(container),
			Slot: &protocol.SlotData{Index: index, Key: key, Count: left},
		}))
	}
}

// ContainerRemoveKey removes count of key from the container (like
// container.removeItem: first-match key scan).
func ContainerRemoveKey(d CommandDeps, username, container, key string, count int) {
	removed := d.Inv.RemoveKey(username, container, key, count)
	if removed <= 0 {
		return
	}
	// The victim's client syncs via Container Remove (removeItem ->
	// removeCallback, same path as ContainerRemove).
	if t, ok := d.Peers.ByUsername(username); ok {
		d.Bus.SendTo(t.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerRemove, protocol.ContainerData{
			Type: containerType(container),
			Slot: &protocol.SlotData{Index: 0, Key: key, Count: removedCount(count, removed)},
		}))
	}
}

// removedCount reports 0 when every requested count was removed (full-stack
// clear serializes Count 0 = empty slot), else how many were removed.
func removedCount(count, removed int) int {
	if removed == count {
		return 0
	}
	return removed
}

// BankCount sums the bank stacks of one key (bank echo helper).
func BankCount(d CommandDeps, username, itemKey string) int {
	return d.Inv.BankCount(username, itemKey)
}

// EmptyContainer drops every slot (container.empty()).
func EmptyContainer(d CommandDeps, username, container string) {
	d.Inv.EmptyContainer(username, container)
}

// CopyContainer clones src's container into dst (copybank/copyinventory).
func CopyContainer(d CommandDeps, src, dst string, bank bool) {
	d.Inv.CopyContainer(src, dst, bank)
}

// MoveMob ports mob.setPosition: registry + roam origin move (adapter seam),
// entity position + Teleport broadcast.
func MoveMob(d CommandDeps, m MobHandle, x, y int) {
	d.Mobs.SetMobPos(m, x, y)
	inst := d.Mobs.MobInstance(m)
	d.World.SetEntityPos(inst, x, y)
	d.Bus.Broadcast(protocol.Pkt(protocol.PacketTeleport,
		protocol.TeleportData{Instance: inst, X: x, Y: y}))
}

// FindNPC scans the npcs.json registry + the live entity tiles.
func FindNPC(c CommandConn, npcKey string, d CommandDeps) {
	LoadNPCs()
	if info := NPCFor(npcKey); info != nil {
		// Find the first live entity whose key resolves to npcKey.
		if x, y, ok := d.World.LiveNPCPos(npcKey); ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Found NPC: %s at x: %d, y: %d", info.Name, x, y))
			return
		}
		d.Bus.Notify(c.InstanceID(), "Could not find NPC.")
		return
	}
	d.Bus.Notify(c.InstanceID(), "Could not find NPC.")
}

// ToggleEffect flips a status effect with the TS notify strings.
func ToggleEffect(c CommandConn, effect int, d CommandDeps) {
	if HasEffect(c, effect) {
		RemoveEffect(c, effect, d)
		d.Bus.Notify(c.InstanceID(), "You are no longer invincible.")
		return
	}
	AddEffect(c, effect, d)
	d.Bus.Notify(c.InstanceID(), "You are now invincible.")
}

// ToggleCommand ports the /toggle switch: freeze/fire/terror/stun with
// addWithTimeout(60s), hide routes to the client-side command, and the
// default clears all effects.
func ToggleCommand(c CommandConn, blocks []string, d CommandDeps) {
	if len(blocks) == 0 {
		// TS: key = blocks.shift()! -> undefined -> switch default ->
		// status.clear().
		ClearEffects(c, d)
		return
	}
	effect := -1
	switch blocks[0] {
	case "cold", "freeze", "freezing":
		effect = CmdEffectFreezing
	case "fire", "burn", "burning":
		effect = CmdEffectBurning
	case "terror":
		effect = CmdEffectTerror
	case "stun":
		effect = CmdEffectStun
	case "hide":
		// client-side hide command (CommandPacket) — Go stub has no client
		// command channel; notify instead (divergence note).
		d.Bus.Notify(c.InstanceID(), "hide")
		return
	}
	if effect < 0 {
		ClearEffects(c, d)
		return
	}
	if HasEffect(c, effect) {
		RemoveEffect(c, effect, d)
		return
	}
	AddEffect(c, effect, d)
	// addWithTimeout(60_000): auto-remove after a minute.
	time.AfterFunc(time.Minute, func() {
		if HasEffect(c, effect) {
			RemoveEffect(c, effect, d)
		}
	})
}

// ToggleHide ports the 'hide' command: flip player.visible and
// Despawn/Spawn to the surrounding regions.
func ToggleHide(c CommandConn, d CommandDeps) {
	cmdVisMu.Lock()
	vis := !cmdHidden[c.InstanceID()]
	cmdHidden[c.InstanceID()] = vis
	cmdVisMu.Unlock()
	if vis {
		// hidden = true -> notify "You are now invisible." + Despawn.
		d.Bus.Notify(c.InstanceID(), "You are now invisible.")
		d.Bus.Broadcast(protocol.Pkt(protocol.PacketDespawn,
			protocol.DespawnData{Instance: c.InstanceID()}))
		return
	}
	d.Bus.Notify(c.InstanceID(), "You are now visible.")
	d.Bus.Broadcast(d.World.SpawnFrame(c.InstanceID()))
}

// ---------------------------------------------------------------------------
// Quest/achievement admin commands (m11 state bridging).
// ---------------------------------------------------------------------------

// QuestStageShift shifts the quest stage by delta (undostage).
func QuestStageShift(c CommandConn, key string, delta int, d CommandDeps) {
	if key == "" {
		d.Bus.Notify(c.InstanceID(), "No quest specified.")
		return
	}
	if _, ok := d.Quests.QuestDef(key); !ok {
		d.Bus.Notify(c.InstanceID(), "Could not find quest.")
		return
	}
	stage, sub := d.Quests.QuestStage(c.PlayerName(), key)
	if stage+delta < 0 {
		return
	}
	d.Quests.SetQuestStage(c, c.PlayerName(), key, stage+delta, sub)
}

// QuestReset sets the quest to stage 0 (resetquest/resetquests).
func QuestReset(c CommandConn, key string, d CommandDeps) {
	if key == "" {
		d.Bus.Notify(c.InstanceID(), "No quest specified.")
		return
	}
	if _, ok := d.Quests.QuestDef(key); !ok {
		d.Bus.Notify(c.InstanceID(), "No quest specified.")
		return
	}
	d.Quests.SetQuestStage(c, c.PlayerName(), key, 0, 0)
}

// QuestResetAll resets every quest (resetquests: setStage(0, ..., true)).
func QuestResetAll(c CommandConn, d CommandDeps) {
	for _, key := range d.Quests.QuestKeys() {
		d.Quests.SetQuestStage(c, c.PlayerName(), key, 0, 0)
	}
}

// QuestFinish sets the stage past the last (setStage(9999) — clamped to
// StageCount so the completion path fires exactly once).
func QuestFinish(c CommandConn, key string, d CommandDeps) {
	if key == "" {
		d.Bus.Notify(c.InstanceID(), "Malformed command, expected /finishquest questKey")
		return
	}
	count, ok := d.Quests.QuestDef(key)
	if !ok {
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Could not find quest with key: %s", key))
		return
	}
	d.Quests.SetQuestStage(c, c.PlayerName(), key, count, 0)
}

// AchievementsReset zeroes every achievement stage.
func AchievementsReset(c CommandConn, d CommandDeps) {
	for _, key := range d.Quests.AchKeys(c.PlayerName()) {
		d.Quests.SetAchStage(c.PlayerName(), key, 0)
	}
	d.Quests.MarkDirty(c.PlayerName())
	for _, key := range d.Quests.AchKeys(c.PlayerName()) {
		d.Quests.SendAchProgress(c, key, d.Quests.AchStage(c.PlayerName(), key))
	}
}

// AchievementFinish completes one achievement (achievement.finish()).
func AchievementFinish(c CommandConn, key string, d CommandDeps) {
	if key == "" {
		d.Bus.Notify(c.InstanceID(), "Malformed command, expected /finishachievement achievementKey")
		return
	}
	count, ok := d.Quests.AchDef(key)
	if !ok {
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Could not find achievement with key: %s", key))
		return
	}
	for d.Quests.AchStage(c.PlayerName(), key) < count {
		d.Quests.AchProgress(c, c.PlayerName(), key)
	}
}

// AchievementsFinishAll completes every achievement.
func AchievementsFinishAll(c CommandConn, d CommandDeps) {
	for key, count := range d.Quests.AchDefs() {
		for d.Quests.AchStage(c.PlayerName(), key) < count {
			d.Quests.AchProgress(c, c.PlayerName(), key)
		}
	}
}

// TestItems empties the bank and adds 100x of every items.json key.
func TestItems(c CommandConn, d CommandDeps) {
	EmptyContainer(d, c.PlayerName(), "bank")
	if err := LoadItems(); err != nil {
		return
	}
	keys := ItemKeys()
	sort.Strings(keys)
	for _, k := range keys {
		d.Inv.AppendBank(c.PlayerName(), k, 100)
	}
	d.Inv.MarkDirty(c.PlayerName())
	d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Bank filled with %d items.", len(keys)))
}
