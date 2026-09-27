package controller

import (
	"encoding/json"
	"log"

	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// Bank (Container Select moves + bank slots).
// ---------------------------------------------------------------------------

// SeedItem adds one stack of itemKey (TESTMAP e2e hook parity): appends a
// fresh slot so the harness gets a deterministic index. Implemented via
// InventorySlots/SetInventory (not Store.SeedItem) to avoid adapter cycles.
func SeedItem(d EconomyDeps, username, itemKey string, count int) int {
	slots := d.Store.InventorySlots(username)
	slots = append(slots, Slot{Key: itemKey, Count: count})
	d.Store.SetInventory(username, slots)
	d.Store.MarkDirty(username)
	return len(slots) - 1
}

// SeedGold tops the account up to at least amount gold (TESTMAP e2e hook).
func SeedGold(d EconomyDeps, username string, amount int) {
	slots := d.Store.InventorySlots(username)
	for i, s := range slots {
		if s.Key == "gold" {
			if s.Count < amount {
				slots[i].Count = amount
				d.Store.SetInventory(username, slots)
			}
			return
		}
	}
	slots = append(slots, Slot{Key: "gold", Count: amount})
	d.Store.SetInventory(username, slots)
	d.Store.MarkDirty(username)
}

// InvSlots snapshots the inventory as serialized batch slots.
func InvSlots(d EconomyDeps, username string) []any {
	slots := d.Store.InventorySlots(username)
	out := make([]any, 0, len(slots))
	for i, s := range slots {
		out = append(out, map[string]any{
			"index": i, "key": s.Key, "count": s.Count, "enchantments": protocol.EnchAny(s.Ench),
		})
	}
	return out
}

// BankAdd stacks (stackables) or appends into the bank; returns the slot
// index or -1 when full (Modules.Constants.BANK_SIZE).
func BankAdd(d EconomyDeps, username, itemKey string, count int) int {
	return BankAddEnch(d, username, itemKey, count, nil)
}

// BankAddEnch is BankAdd carrying enchantments: plain stacks merge only
// into un-enchanted same-key slots with room under MaxStack (bank
// stackSize is MAX_STACK; container.find parity skips enchanted sides),
// everything else appends a fresh slot. Atomic: -1 returns before any
// write, so callers can restore without duplicating a partial move.
func BankAddEnch(d EconomyDeps, username, itemKey string, count int, ench protocol.Enchantments) int {
	slots := d.Store.BankSlots(username)
	if len(ench) == 0 {
		if max := MaxStack(itemKey); max > 1 {
			for i, s := range slots {
				if s.Key == itemKey && len(s.Ench) == 0 && s.Count+count <= max {
					slots[i].Count += count
					d.Store.SetBank(username, slots)
					return i
				}
			}
		}
	}
	if len(slots) >= protocol.ModulesBankSize {
		return -1
	}
	slots = append(slots, Slot{Key: itemKey, Count: count, Ench: CopyEnchantments(ench)})
	d.Store.SetBank(username, slots)
	return len(slots) - 1
}

// bankHasRoom reports whether a deposit of slot fits without mutating:
// a full-fit un-enchanted merge, or a free bank slot.
func bankHasRoom(d EconomyDeps, username string, slot Slot) bool {
	if len(slot.Ench) == 0 {
		if max := MaxStack(slot.Key); max > 1 {
			for _, s := range d.Store.BankSlots(username) {
				if s.Key == slot.Key && len(s.Ench) == 0 && s.Count+slot.Count <= max {
					return true
				}
			}
		}
	}
	return len(d.Store.BankSlots(username)) < protocol.ModulesBankSize
}

// BankBatch builds the bank Container Batch payload (bank.serialize(true)).
func BankBatch(d EconomyDeps, username string) protocol.ContainerData {
	slots := d.Store.BankSlots(username)
	out := make([]any, 0, len(slots))
	for i, s := range slots {
		out = append(out, map[string]any{
			"index": i, "key": s.Key, "count": s.Count, "enchantments": protocol.EnchAny(s.Ench),
		})
	}
	return protocol.ContainerData{Type: protocol.ContainerTypeBank, Data: &protocol.ContainerBatchPayload{Slots: out}}
}

// ClearAccess revokes bank access + closes the store (Node parity).
func ClearAccess(c EconomyConn) {
	c.SetStoreOpen("")
	c.SetCanAccess(false)
}

// OpenBank ports the banker branch: canAccessContainer = true + Container
// Batch with the serialized bank slots.
func OpenBank(c EconomyConn, d EconomyDeps) {
	c.SetCanAccess(true)
	batch := BankBatch(d, c.PlayerName())
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerBatch, batch))
	log.Printf("m6: %s opened bank (%d slots)", c.InstanceID(), len(batch.Data.Slots))
}

// HandleContainerSelect ports handleContainerSelect's type switch.
func HandleContainerSelect(c EconomyConn, msg *protocol.ClientContainer, d EconomyDeps) {
	if msg.Type == nil || msg.FromIndex == nil {
		return
	}
	if *msg.Type == protocol.ContainerTypeInventory {
		if HandleInventoryUse(c, d, *msg.FromIndex) {
			return
		}
		EquipFromInventory(c, d, *msg.FromIndex)
		return
	}
	if *msg.Type != protocol.ContainerTypeBank {
		return
	}
	if !c.CanAccess() {
		Notify(c, d, "misc:CANNOT_DO_THAT")
		return
	}
	if msg.FromContainer == nil || msg.ToContainer == nil {
		return
	}
	from, to, fromIndex := *msg.FromContainer, *msg.ToContainer, *msg.FromIndex
	if from == to || (from != protocol.ContainerTypeBank && from != protocol.ContainerTypeInventory) ||
		(to != protocol.ContainerTypeBank && to != protocol.ContainerTypeInventory) {
		return
	}
	username := c.PlayerName()

	switch {
	case from == protocol.ContainerTypeInventory && to == protocol.ContainerTypeBank:
		slots := d.Store.InventorySlots(username)
		if fromIndex < 0 || fromIndex >= len(slots) {
			return
		}
		slot := slots[fromIndex]
		if slot.Key == "" || slot.Count < 1 {
			return
		}
		// Capacity is checked before touching the inventory (TS move is
		// atomic: a full bank leaves the source slot alone).
		if !bankHasRoom(d, username, slot) {
			Notify(c, d, "Bank is full.")
			return
		}
		nslots := append(slots[:fromIndex], slots[fromIndex+1:]...)
		d.Store.SetInventory(username, nslots)

		bankIdx := BankAddEnch(d, username, slot.Key, slot.Count, slot.Ench)
		if bankIdx < 0 {
			// Defensive only (pre-checked above): put the stack back
			// and resync the client, so the move never loses items.
			invIdx := d.Store.AddItemEnch(username, slot.Key, slot.Count, slot.Ench)
			d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
				Type: protocol.ContainerTypeInventory,
				Slot: &protocol.SlotData{Index: invIdx, Key: slot.Key, Count: slot.Count, Enchantments: protocol.EnchAny(slot.Ench)},
			}))
			d.Store.MarkDirty(username)
			Notify(c, d, "Bank is full.")
			return
		}
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerRemove, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: fromIndex, Key: slot.Key, Count: 0, Enchantments: protocol.EnchAny(slot.Ench)},
		}))
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
			Type: protocol.ContainerTypeBank,
			Slot: &protocol.SlotData{Index: bankIdx, Key: slot.Key, Count: slot.Count, Enchantments: protocol.EnchAny(slot.Ench)},
		}))
		d.Store.MarkDirty(username)
		log.Printf("m6: %s deposit %s x%d (bank slot %d)", c.InstanceID(), slot.Key, slot.Count, bankIdx)

	case from == protocol.ContainerTypeBank && to == protocol.ContainerTypeInventory:
		bslots := d.Store.BankSlots(username)
		if fromIndex < 0 || fromIndex >= len(bslots) {
			return
		}
		slot := bslots[fromIndex]
		if slot.Key == "" || slot.Count < 1 {
			return
		}

		if protocol.ModulesInventorySize-d.Store.InventoryLen(username) < 1 && !HasItem(d, username, slot.Key) {
			Notify(c, d, "Your inventory is full.")
			return
		}

		// The bank slot is dropped only after the inventory add lands:
		// a failed add must not destroy the bank stack.
		invIdx := d.Store.AddItemEnch(username, slot.Key, slot.Count, slot.Ench)
		if invIdx < 0 {
			Notify(c, d, "Your inventory is full.")
			return
		}
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: invIdx, Key: slot.Key, Count: slot.Count, Enchantments: protocol.EnchAny(slot.Ench)},
		}))

		nb := append(bslots[:fromIndex], bslots[fromIndex+1:]...)
		d.Store.SetBank(username, nb)
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerRemove, protocol.ContainerData{
			Type: protocol.ContainerTypeBank,
			Slot: &protocol.SlotData{Index: fromIndex, Key: slot.Key, Count: 0, Enchantments: protocol.EnchAny(slot.Ench)},
		}))
		d.Store.MarkDirty(username)
		log.Printf("m6: %s withdraw %s x%d (bank slot %d)", c.InstanceID(), slot.Key, slot.Count, fromIndex)
	}
}

// HandleContainerSwap ports handleContainerSwap: reorder two inventory slots.
func HandleContainerSwap(c EconomyConn, d EconomyDeps, fromIndex, toIndex int) {
	if fromIndex < 0 || toIndex < 0 || fromIndex == toIndex {
		return
	}
	slots := d.Store.InventorySlots(c.PlayerName())
	if fromIndex >= len(slots) || toIndex >= len(slots) {
		return
	}
	slots[fromIndex], slots[toIndex] = slots[toIndex], slots[fromIndex]
	d.Store.SetInventory(c.PlayerName(), slots)
	d.Store.MarkDirty(c.PlayerName())
	// Resync the client (container.swap fires loadCallback on both
	// containers, which the handler fans out as a Container Batch).
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerBatch, inventoryBatch(c.PlayerName(), slots)))
	log.Printf("m6: %s swap inventory %d <-> %d", c.InstanceID(), fromIndex, toIndex)
}

// inventoryBatch builds the inventory Container Batch payload for slots
// (inventory.serialize(true) parity for the swap resync).
func inventoryBatch(username string, slots []Slot) protocol.ContainerData {
	out := make([]any, 0, len(slots))
	for i, s := range slots {
		out = append(out, map[string]any{
			"index": i, "key": s.Key, "count": s.Count, "enchantments": protocol.EnchAny(s.Ench),
		})
	}
	return protocol.ContainerData{Type: protocol.ContainerTypeInventory, Data: &protocol.ContainerBatchPayload{Slots: out}}
}

// HandleContainer routes the C->S Container frame.
func HandleContainer(c EconomyConn, data []byte, d EconomyDeps) {
	var msg protocol.ClientContainer
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	if msg.Opcode == nil {
		return
	}
	switch *msg.Opcode {
	case protocol.ContainerSelect:
		HandleContainerSelect(c, &msg, d)
	case protocol.ContainerSwap:
		if msg.FromIndex == nil || msg.Value == nil {
			return
		}
		if msg.Type != nil && *msg.Type == protocol.ContainerTypeBank {
			return
		}
		HandleContainerSwap(c, d, *msg.FromIndex, *msg.Value)
	case protocol.ContainerRemove:
		if msg.Type == nil || msg.FromIndex == nil || msg.Value == nil || *msg.Value < 1 {
			return
		}
		if *msg.Type != protocol.ContainerTypeInventory {
			return
		}
		// Quest/special items cannot be dropped (inventory.remove
		// undroppable+drop gate parity). Sell and bank moves have no
		// such gate in TS — intentionally unchecked there too.
		droppedSlot, droppedOk := d.Store.SlotAt(c.PlayerName(), *msg.FromIndex)
		if droppedOk && droppedSlot.Key != "" && d.Store.ItemUndroppable(droppedSlot.Key) {
			Notify(c, d, "You cannot drop this item.")
			return
		}
		if mob, item, ok := d.Pets.DropKey(c, *msg.FromIndex); ok {
			if d.Pets.HasOwner(c.InstanceID()) {
				Notify(c, d, "misc:ALREADY_HAVE_PET")
				return
			}
			InventoryRemoveAt(c, d, c.PlayerName(), *msg.FromIndex, *msg.Value)
			d.Store.MarkDirty(c.PlayerName())
			log.Printf("pets: %s drop-spawned %s (%s)", c.InstanceID(), item, mob)
			d.Pets.Grant(c, mob, item)
			return
		}
		InventoryRemoveAt(c, d, c.PlayerName(), *msg.FromIndex, *msg.Value)
		d.Store.MarkDirty(c.PlayerName())
		// World item spawn (container.ts drop parity): the removed item
		// becomes a world Item entity other players can see and pick up.
		if d.Drops.SpawnItem != nil && droppedOk && droppedSlot.Key != "" {
			d.Drops.SpawnItem(c.PlayerName(), droppedSlot.Key, *msg.Value, c.TileX(), c.TileY())
		}
		log.Printf("m6: %s drop idx %d x%d", c.InstanceID(), *msg.FromIndex, *msg.Value)
	}
}

// HandleContainerFrame routes a decoded clientFrame pair (adapter helper).
func HandleContainerFrame(c EconomyConn, frame []json.RawMessage, d EconomyDeps) {
	if len(frame) < 2 {
		return
	}
	HandleContainer(c, []byte(frame[1]), d)
}
