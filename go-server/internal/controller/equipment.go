package controller

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// NPC registry (npcs.json) + talk routing (handler.handleTalkToNPC).
// ---------------------------------------------------------------------------

// NPCInfo mirrors m6NPCInfo (npcs.json entry).
type NPCInfo struct {
	Name  string   `json:"name"`
	Text  []string `json:"text"`
	Role  string   `json:"role"`
	Store string   `json:"store"`
}

var (
	econNPCsOnce sync.Once
	econNPCs     = map[string]*NPCInfo{}
	econNPCsOK   bool
)

// LoadNPCs loads npcs.json once (m6LoadNPCs parity).
func LoadNPCs() {
	econNPCsOnce.Do(func() {
		raw, err := os.ReadFile(economyDataPath("npcs"))
		if err != nil {
			log.Printf("m6: read npcs.json: %v (NPC talk disabled)", err)
			return
		}
		if err := json.Unmarshal(raw, &econNPCs); err != nil {
			log.Printf("m6: parse npcs.json: %v (NPC talk disabled)", err)
			return
		}
		n := 0
		for _, v := range econNPCs {
			if v.Store != "" || v.Role != "" {
				n++
			}
		}
		econNPCsOK = true
		log.Printf("m6: npcs=%d (interactive %d)", len(econNPCs), n)
	})
}

// IsNPCKey reports whether the key exists in npcs.json.
func IsNPCKey(key string) bool {
	LoadNPCs()
	return econNPCsOK && econNPCs[key] != nil
}

// NPCFor returns the registry entry (nil when unknown).
func NPCFor(key string) *NPCInfo {
	LoadNPCs()
	if !econNPCsOK {
		return nil
	}
	return econNPCs[key]
}

// NPCSnapshot returns a copy of the NPC registry (adapter sync helper).
func NPCSnapshot() map[string]*NPCInfo {
	LoadNPCs()
	out := make(map[string]*NPCInfo, len(econNPCs))
	for k, v := range econNPCs {
		out[k] = v
	}
	return out
}

// NPCsOK reports whether the NPC registry loaded.
func NPCsOK() bool {
	LoadNPCs()
	return econNPCsOK
}

// ResolveNPCKey maps a spawn instance to its npcs.json key.
func ResolveNPCKey(c EconomyConn, d EconomyDeps, instance string) string {
	if key, ok := d.World.SpawnNPCKey(instance); ok && key != "" {
		return key
	}
	// Showcase fallback: n-show-N (grid order matches showNPCs).
	if len(instance) > 7 && instance[:7] == "n-show-" {
		var n int
		if _, err := fmt.Sscanf(instance, "n-show-%d", &n); err == nil && n >= 1 && n <= d.World.ShowcaseCount() {
			if key, ok := d.World.ShowcaseKey(n - 1); ok {
				return key
			}
		}
	}
	_ = c
	return ""
}

// HandleNPCTarget ports Target Talk(0) -> handleTalkToNPC.
func HandleNPCTarget(c EconomyConn, d EconomyDeps, instance string) {
	x, y, ok := d.World.EntityPos(instance)
	if !ok {
		return
	}
	if dx, dy := absInt(c.TileX()-x), absInt(c.TileY()-y); dx > 2 || dy > 2 {
		log.Printf("m6: %s talks to %s from %d,%d tiles (ignored)", c.InstanceID(), instance, dx, dy)
		return
	}

	npcKey := ResolveNPCKey(c, d, instance)
	if npcKey == "" || !IsNPCKey(npcKey) {
		return
	}
	info := econNPCs[npcKey]
	log.Printf("m6: %s talks to %s (%s)", c.InstanceID(), npcKey, info.Name)

	// M11: quest/achievement NPCs swallow the interaction before any role
	// handling (handler.handleTalkToNPC order).
	if d.Quests != nil && d.Quests.Talk(c, npcKey) {
		return
	}
	if info.Store != "" {
		OpenStore(c, d, info.Store)
		return
	}
	if info.Role == "banker" {
		OpenBank(c, d)
		return
	}
	if info.Role == "enchanter" {
		OpenEnchanter(c, d.tradeDeps())
		return
	}
	if len(info.Text) == 0 {
		return
	}
	if c.TalkKey() != npcKey {
		c.SetTalkKey(npcKey)
		c.SetTalkIndex(0)
	}
	idx := c.TalkIndex()
	if idx >= len(info.Text) {
		idx = len(info.Text) - 1
	}
	text := info.Text[idx]
	c.SetTalkIndex(c.TalkIndex() + 1)
	inst := instance
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketNPC, protocol.NPCTalk, protocol.NpcPacketData{Instance: &inst, Text: &text}))
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---------------------------------------------------------------------------
// Equipment (Modules.Equipment 12 slots).
// ---------------------------------------------------------------------------

// EquipmentType ports item.getEquipmentType.
func EquipmentType(itemType string) int {
	switch itemType {
	case "helmet":
		return protocol.EquipmentHelmet
	case "chestplate":
		return protocol.EquipmentChestplate
	case "legplates":
		return protocol.EquipmentLegplates
	case "skin":
		return protocol.EquipmentArmourSkin
	case "weapon", "weaponarcher", "weaponmagic":
		return protocol.EquipmentWeapon
	case "weaponskin":
		return protocol.EquipmentWeaponSkin
	case "pendant":
		return protocol.EquipmentPendant
	case "boots":
		return protocol.EquipmentBoots
	case "ring":
		return protocol.EquipmentRing
	case "arrow":
		return protocol.EquipmentArrows
	case "shield":
		return protocol.EquipmentShield
	case "cape":
		return protocol.EquipmentCape
	}
	return -1
}

// IsEquippable ports item.isEquippable.
func IsEquippable(itemType string) bool {
	return EquipmentType(itemType) >= 0
}

// EquipmentData builds the EquipmentData payload for one slot.
func EquipmentData(slotType int, key string, count int, clientInfo bool) map[string]any {
	if count < 1 {
		count = -1
	}
	data := map[string]any{
		"type":         slotType,
		"key":          key,
		"count":        count,
		"enchantments": map[string]any{},
	}
	if clientInfo {
		data["name"] = ItemName(key)
		if it := ItemInfoFor(key); it != nil {
			data["poisonous"] = it.Poisonous
		}
	}
	return data
}

// EquipmentSlots snapshots the equipped slots as serialized entries.
func EquipmentSlots(d EconomyDeps, username string) []any {
	eqs := make([]any, 0, protocol.ModulesEquipmentCount)
	for t := 0; t < protocol.ModulesEquipmentCount; t++ {
		e, ok := d.Store.EquipSlot(username, t)
		if !ok || e.Key == "" || e.Count < 1 {
			continue
		}
		eqs = append(eqs, EquipmentData(t, e.Key, e.Count, true))
	}
	return eqs
}

// skillNameFor maps a skill id to its display name (m5SkillName parity).
func skillNameFor(id int) string {
	switch id {
	case 0:
		return "Lumberjacking"
	case 1:
		return "Accuracy"
	case 2:
		return "Archery"
	case 3:
		return "Health"
	case 4:
		return "Magic"
	case 5:
		return "Mining"
	case 6:
		return "Strength"
	case 7:
		return "Defense"
	case 8:
		return "Fishing"
	case 15:
		return "Foraging"
	}
	return fmt.Sprintf("Skill%d", id)
}

// SkillLevelFor maps an items.json requirement skill key to the stored skill
// id (stubs are all level 1 parity).
func SkillLevelFor(d EconomyDeps, username string, skillID int) (string, int, int) {
	name := skillNameFor(skillID)
	level := d.Store.SkillLevel(username, skillID)
	return name, level, level
}

// SkillIDFor maps an items.json skill key to the skill id.
func SkillIDFor(key string) (int, bool) {
	switch key {
	case "lumberjacking":
		return 0, true
	case "accuracy":
		return 1, true
	case "archery":
		return 2, true
	case "health":
		return 3, true
	case "magic":
		return 4, true
	case "mining":
		return 5, true
	case "strength":
		return 6, true
	case "defense":
		return 7, true
	case "fishing":
		return 8, true
	case "foraging":
		return 15, true
	}
	return 0, false
}

// CanEquip ports item.canEquip.
func CanEquip(c EconomyConn, d EconomyDeps, key string) bool {
	it := ItemInfoFor(key)
	if it == nil {
		return false
	}
	requirement := it.Level
	if requirement == 0 {
		requirement = 0
	}
	if it.Skill != "" {
		skillID, ok := SkillIDFor(it.Skill)
		if ok {
			name, level, _ := SkillLevelFor(d, c.PlayerName(), skillID)
			if level < requirement {
				Notify(c, d, fmt.Sprintf("item:SKILL_LEVEL_REQUIREMENT_EQUIP;skill=%s;level=%d", name, requirement))
				return false
			}
			return true
		}
	}
	if d.Store.TotalLevel(c.PlayerName()) < requirement {
		Notify(c, d, fmt.Sprintf("item:TOTAL_LEVEL_REQUIREMENT;level=%d", requirement))
		return false
	}
	return true
}

// canStackUnequip reports whether the equipped stack merges into the
// inventory without a free slot (container.find parity: same key, room
// under MaxStack, neither side enchanted).
func canStackUnequip(d EconomyDeps, username string, e Slot) bool {
	if len(e.Ench) != 0 || MaxStack(e.Key) <= 1 {
		return false
	}
	max := MaxStack(e.Key)
	for _, s := range d.Store.InventorySlots(username) {
		if s.Key == e.Key && len(s.Ench) == 0 && s.Count < max {
			return true
		}
	}
	return false
}

// UnequipType ports equipments.unequip(type).
func UnequipType(c EconomyConn, d EconomyDeps, slotType int) {
	if slotType < 0 || slotType >= protocol.ModulesEquipmentCount {
		return
	}
	username := c.PlayerName()
	e, ok := d.Store.EquipSlot(username, slotType)
	if !ok || e.Key == "" {
		return
	}
	// A full inventory keeps the equipment on (container.add returns
	// <1 there, and unequip stops before clearing the slot — silently,
	// no notify in TS either).
	if protocol.ModulesInventorySize-d.Store.InventoryLen(username) < 1 && !canStackUnequip(d, username, e) {
		return
	}
	invIdx := d.Store.AddItemEnch(username, e.Key, e.Count, e.Ench)
	if invIdx < 0 {
		return
	}
	count := e.Count
	d.Store.SetEquip(username, slotType, Slot{})
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketEquipment, protocol.EquipmentUnequip, map[string]any{
		"type": slotType, "count": count,
	}))
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
		Type: protocol.ContainerTypeInventory,
		Slot: &protocol.SlotData{Index: invIdx, Key: e.Key, Count: e.Count, Enchantments: protocol.EnchAny(e.Ench)},
	}))
	d.Bus.Broadcast(d.World.SyncFrame(c.InstanceID(), c.TileX(), c.TileY()))
	d.Store.MarkDirty(username)
	log.Printf("m6: %s unequip type=%d %s x%d -> inv[%d]", c.InstanceID(), slotType, e.Key, e.Count, invIdx)
}

// EquipFromInventory ports equipments.equip(item, fromIndex).
func EquipFromInventory(c EconomyConn, d EconomyDeps, fromIndex int) {
	username := c.PlayerName()
	slot, ok := d.Store.SlotAt(username, fromIndex)
	if !ok {
		return
	}
	if slot.Key == "" || slot.Count < 1 {
		return
	}
	it := ItemInfoFor(slot.Key)
	if it == nil || !IsEquippable(it.Type) {
		return
	}
	if !CanEquip(c, d, slot.Key) {
		return
	}
	slotType := EquipmentType(it.Type)
	if slotType < 0 || slotType >= protocol.ModulesEquipmentCount || d.Store.EquipLen(username) < protocol.ModulesEquipmentCount {
		return
	}

	// Two-handed weapon auto-swap (equipments.ts:116-132 parity):
	// Equipping a two-handed weapon with a shield equipped auto-unequips
	// the shield; equipping a shield with a two-handed weapon auto-unequips
	// the weapon. Both need inventory space for the displaced item.
	if it.TwoHanded && slotType == protocol.EquipmentWeapon {
		if shield, _ := d.Store.EquipSlot(username, protocol.EquipmentShield); shield.Key != "" {
			if protocol.ModulesInventorySize-d.Store.InventoryLen(username) < 1 {
				Notify(c, d, "misc:NO_SPACE")
				return
			}
			UnequipType(c, d, protocol.EquipmentShield)
		}
	}
	if slotType == protocol.EquipmentShield {
		if weapon, _ := d.Store.EquipSlot(username, protocol.EquipmentWeapon); weapon.Key != "" {
			if wit := ItemInfoFor(weapon.Key); wit != nil && wit.TwoHanded {
				if protocol.ModulesInventorySize-d.Store.InventoryLen(username) < 1 {
					Notify(c, d, "misc:NO_SPACE")
					return
				}
				UnequipType(c, d, protocol.EquipmentWeapon)
			}
		}
	}

	InventoryRemoveAt(c, d, username, fromIndex, slot.Count)

	old, _ := d.Store.EquipSlot(username, slotType)
	var oldIdx int = -1
	if old.Key != "" {
		oldIdx = d.Store.AddItem(username, old.Key, old.Count)
	}
	d.Store.SetEquip(username, slotType, Slot{Key: slot.Key, Count: slot.Count})

	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketEquipment, protocol.EquipmentEquip, map[string]any{
		"data": EquipmentData(slotType, slot.Key, slot.Count, true),
	}))
	d.Bus.Broadcast(d.World.SyncFrame(c.InstanceID(), c.TileX(), c.TileY()))
	if oldIdx >= 0 {
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: oldIdx, Key: old.Key, Count: old.Count, Enchantments: map[string]any{}},
		}))
	}
	d.Store.MarkDirty(username)
	log.Printf("m6: %s equip %s x%d -> type %d (old %q returned at %d)",
		c.InstanceID(), slot.Key, slot.Count, slotType, old.Key, oldIdx)
}

// HandleEquipment routes the C->S Equipment frame [8,{opcode,type|style}].
func HandleEquipment(c EconomyConn, data []byte, d EconomyDeps) {
	var msg protocol.ClientEquipment
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	switch msg.Opcode {
	case protocol.EquipmentUnequip:
		if msg.Type == nil {
			return
		}
		UnequipType(c, d, *msg.Type)
	case protocol.EquipmentStyle:
		if msg.Style == nil {
			return
		}
		UpdateAttackStyle(c, d, *msg.Style)
	}
}

// HandleEquipmentFrame routes a decoded clientFrame pair (adapter helper).
func HandleEquipmentFrame(c EconomyConn, frame []json.RawMessage, d EconomyDeps) {
	if len(frame) < 2 {
		return
	}
	HandleEquipment(c, []byte(frame[1]), d)
}
