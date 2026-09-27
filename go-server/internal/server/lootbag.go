// LootBag Open/Take lifecycle adapter (objects/lootbag.ts parity).
//
// TS flow recap: stopping on a bag opens it (player.activeLootBag +
// unicast LootBag Open {items}); the client loot menu sends C LootBag
// {Take, index} per item; take() validates opener + ownership + distance,
// transfers one stack, then destroys the emptied bag (Close + despawn) or
// broadcasts Take {index} so open menus drop the row.
//
// Coexistence with the frozen take-all default (combat e2e asserts
// take-all on Target for every loot entity): single Items keep instant
// take-all on Step and Target (TS agrees — handleMovementStop adds items
// immediately). Bags diverge by entry path, because the stock client never
// Targets a bag (getTargetType returns None for lootbags; opening rides the
// Movement Stop targetInstance instead):
//   - Step onto a bag tile -> Open only (TS handleMovementStop parity).
//   - Target on a bag       -> Open + take-all (Open is the TS-faithful
//     frame; take-all stays because the combat harness requires it).
//   - C LootBag {Take}      -> single-stack transfer (new path).
package server

import (
	"encoding/json"
	"log"

	"rpg-world-server/internal/entity"
	gnet "rpg-world-server/internal/net"
	worldcore "rpg-world-server/internal/world"
)

// lootBagOpenItems builds the Open payload items (SlotData parity:
// {index,key,count,enchantments}).
func lootBagOpenItems(inst string) ([]any, bool) {
	slots, ok := entity.BagSlots(inst)
	if !ok {
		return nil, false
	}
	items := make([]any, 0, len(slots))
	for _, s := range slots {
		items = append(items, map[string]any{
			"index": s.Index, "key": s.Key, "count": s.Count,
			"enchantments": map[string]any{},
		})
	}
	return items, true
}

// sendLootBagOpen unicasts LootBag Open {items} and records the opener
// (lootbag.open parity). Reports false when inst is not a live bag.
func sendLootBagOpen(c *playerConn, inst string) bool {
	if c == nil || c.Conn == nil || inst == "" {
		return false
	}
	items, ok := lootBagOpenItems(inst)
	if !ok {
		return false
	}
	if !entity.OpenBag(c.Instance, inst) {
		return false
	}
	_ = gnet.Send(c.Conn, pktOp(PacketLootBag, LootBagOpen, map[string]any{"items": items}))
	log.Printf("lootbag: %s opened %s (%d items)", c.Instance, inst, len(items))
	return true
}

// lootBagOwnerDenied mirrors the handleMovementStop gate: a bag owned by
// someone else notifies the attempter and stays shut.
func lootBagOwnerDenied(c *playerConn, owner string) bool {
	if c == nil || owner == "" || owner == c.Username {
		return false
	}
	notifyPlayer(c, "This lootbag belongs to "+owner+".")
	return true
}

// openLootBagFor is the Step entry: owner gate, then Open only (no take —
// TS opens instead of instant-take on movement stop).
func openLootBagFor(c *playerConn, inst string) {
	if c == nil || c.Conn == nil || inst == "" {
		return
	}
	l, ok := entity.FindLoot(inst)
	if !ok || !l.Bag {
		return
	}
	if lootBagOwnerDenied(c, l.Owner) {
		return
	}
	sendLootBagOpen(c, inst)
}

// handleLootBagReq routes one C LootBag frame: [56,{opcode,index}]
// (controllers/menu.ts handleLootBagSelect parity). Only Take is handled,
// mirroring the TS handleLootBag switch.
func handleLootBagReq(c *playerConn, frame clientFrame) {
	if c == nil || len(frame) < 2 {
		return
	}
	var data struct {
		Opcode *int `json:"opcode"`
		Index  *int `json:"index"`
	}
	if err := json.Unmarshal(frame[1], &data); err != nil {
		return
	}
	if data.Opcode == nil || *data.Opcode != LootBagTake {
		return
	}
	if data.Index == nil {
		return
	}
	takeLootBagItem(c, *data.Index)
}

// takeLootBagItem ports lootbag.take(player, index) step for step: the bag
// is the player's OPEN bag (the frame carries only the index), then opener,
// ownership, distance, slot-exists, owner-failsafe, space, transfer, and
// destroy-vs-Take-broadcast.
func takeLootBagItem(c *playerConn, index int) {
	if c == nil || c.Conn == nil {
		return
	}
	inst, ok := entity.ActiveBag(c.Instance)
	if !ok {
		log.Printf("lootbag: %s take without an open bag (ignored)", c.Instance)
		return
	}
	l, found := entity.FindLoot(inst)
	if !found || !l.Bag {
		return
	}
	if l.Owner != "" && l.Owner != c.Username {
		notifyPlayer(c, "item:CANNOT_ACCESS_LOOTBAG")
		return
	}
	if dx, dy := abs(c.Sess.PlayerX-l.X), abs(c.Sess.PlayerY-l.Y); dx+dy > 1 {
		log.Printf("lootbag: take(): %s too far from %s (ignored)", c.Username, inst)
		return
	}
	// Slot-exists check before the failsafe (TS order: container lookup,
	// then the owner failsafe).
	slots, ok := entity.BagSlots(inst)
	if !ok {
		return
	}
	slotFound := false
	for _, s := range slots {
		if s.Index == index {
			slotFound = true
		}
	}
	if !slotFound {
		return
	}
	if l.Owner != "" && l.Owner != c.Username {
		notifyPlayer(c, "item:CANNOT_ACCESS_LOOTBAG")
		return
	}
	st := playerStateFor(c.Username)
	pstateMu.Lock()
	invLen := len(st.Inv)
	pstateMu.Unlock()
	if ModulesInventorySize-invLen < 1 {
		notifyPlayer(c, "misc:NO_SPACE")
		return
	}
	taken, remaining, ok := entity.TakeBagItem(inst, index)
	if !ok {
		return
	}
	idx := addItem(c.Username, taken.Key, taken.Count)
	_ = gnet.Send(c.Conn, pktOp(PacketContainer, ContainerAdd, containerData{
		Type: ContainerTypeInventory,
		Slot: &slotData{Index: idx, Key: taken.Key, Count: taken.Count, Enchantments: map[string]any{}},
	}))
	markDirty(c.Username)
	log.Printf("lootbag: %s took %s x%d from %s (%d left)", c.Instance, taken.Key, taken.Count, inst, remaining)
	if remaining == 0 {
		entity.DestroyLoot(inst, "emptied by "+c.Instance)
		return
	}
	worldcore.Broadcast(pktOp(PacketLootBag, LootBagTake, map[string]any{"index": index}))
}
