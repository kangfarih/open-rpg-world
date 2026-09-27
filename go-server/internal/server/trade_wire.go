// Trade wire — trade, crafting, enchanting (finishes the economy half).
//
// Thin adapter over internal/controller (behavior-frozen move, task E1a):
// all trade/craft/enchant logic lives in the controller package operating on
// the Conn/Store/Bus/Peers seams below. This file only wires those seams to
// the root globals (players map, send, pstates, m5/m6 helpers) and keeps the
// entry points main.go/m6.go/m7.go call — with UNCHANGED signatures —
// delegating to the controller. Packet shapes and DB schema are identical.
//
// Re-homed: removeItemAt (defined here despite its m6* name) now delegates
// to controller.RemoveItemAt.
package server

import (
	"rpg-world-server/internal/controller"
	gnet "rpg-world-server/internal/net"
	worldcore "rpg-world-server/internal/world"
)

// Type aliases so existing names keep resolving to the moved types.
type (
	tradePlayerState      = controller.PlayerState
	tradeSession            = controller.Trade
	tradeOfferedItem      = controller.OfferedItem
	craftItem        = controller.CraftItem
	craftPreview     = controller.CraftPreview
	craftRequirement = controller.CraftRequirement
)

// ---------------------------------------------------------------------------
// controller.Conn seam (*playerConn satisfies it, so identity is preserved).
// ---------------------------------------------------------------------------

func (c *playerConn) InstanceID() string {
	if c == nil {
		return ""
	}
	return c.Instance
}
func (c *playerConn) PlayerName() string {
	if c == nil {
		return ""
	}
	return c.Username
}
func (c *playerConn) TileX() int {
	if c == nil {
		return 0
	}
	return c.Sess.PlayerX
}
func (c *playerConn) TileY() int {
	if c == nil {
		return 0
	}
	return c.Sess.PlayerY
}

// GrantContainerAccess sets canAccessContainer (enchanter NPC branch).
func (c *playerConn) GrantContainerAccess() { c.SetCanAccess(true) }

// tradeConn unwraps the controller.Conn back to the root conn (frame-emitting
// store ops need it); falls back to an instance lookup for foreign impls.
func tradeConn(c controller.Conn) *playerConn {
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
// controller.Store seam (playerStateFor/addItem/markDirty + m6 item helpers).
// ---------------------------------------------------------------------------

type tradeStore struct{}

func (tradeStore) SlotAt(username string, index int) (controller.Slot, bool) {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if index < 0 || index >= len(st.Inv) {
		return controller.Slot{}, false
	}
	s := st.Inv[index]
	return controller.Slot{Key: s.Key, Count: s.Count, Ench: s.Ench}, true
}

func (tradeStore) InventoryLen(username string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	return len(st.Inv)
}

func (tradeStore) CountItem(username, itemKey string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	n := 0
	for _, s := range st.Inv {
		if s.Key == itemKey {
			n += s.Count
		}
	}
	return n
}

func (tradeStore) AddItem(username, itemKey string, count int) int {
	return addItem(username, itemKey, count)
}

func (tradeStore) AddItemEnch(username, itemKey string, count int, ench Enchantments) int {
	return addItemEnch(username, itemKey, count, ench)
}

func (tradeStore) RemoveItem(username, itemKey string, count int) {
	invRemoveItem(username, itemKey, count)
}

func (tradeStore) RemoveItemAt(c controller.Conn, username string, index, count int) {
	inventoryRemoveAt(tradeConn(c), username, index, count)
}

func (tradeStore) SetSlotEnchantments(username string, index int, ench Enchantments) (int, bool) {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if index < 0 || index >= len(st.Inv) {
		return 0, false
	}
	st.Inv[index].Ench = ench
	return st.Inv[index].Count, true
}

func (tradeStore) SkillLevel(username string, skill int) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if s := st.Skills[skill]; s != nil {
		return s.Level
	}
	return 1
}

func (tradeStore) AddXP(c controller.Conn, skill, amount int) int {
	return addXP(tradeConn(c), c.PlayerName(), skill, amount)
}

func (tradeStore) MarkDirty(username string) { markDirty(username) }

func (tradeStore) SeedItem(username, itemKey string, count int) int {
	return seedItem(username, itemKey, count)
}

func (tradeStore) ClearInventoryButGold(username string) {
	st := playerStateFor(username)
	pstateMu.Lock()
	out := st.Inv[:0]
	for _, s := range st.Inv {
		if s.Key == "gold" {
			out = append(out, s)
		}
	}
	st.Inv = out
	pstateMu.Unlock()
	markDirty(username)
}

func (tradeStore) ItemUndroppable(key string) bool {
	info := itemInfoFor(key)
	return info != nil && info.Undroppable
}

func (tradeStore) ItemName(key string) string { return itemName(key) }

func (tradeStore) ItemType(key string) (string, bool) {
	info := itemInfoFor(key)
	if info == nil {
		return "", false
	}
	return info.Type, true
}

func (tradeStore) MaxStack(key string) int { return maxStack(key) }

// ---------------------------------------------------------------------------
// controller.Bus seam (send unicast + notify) and controller.Peers seam.
// ---------------------------------------------------------------------------

type tradeBus struct{}

func (tradeBus) SendTo(instance string, frames ...[]any) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	_ = gnet.Send(c.Conn, frames...)
}

func (tradeBus) Notify(instance string, message string) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	notifyPlayer(c, message)
}

type tradePeers struct{}

func (tradePeers) ByInstance(instance string) (controller.Conn, bool) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return nil, false
	}
	return c, true
}

// connByUsername finds the live connection for a username (trade peers are
// players, so usernames are unique among connections here).
func (tradePeers) ByUsername(username string) (controller.Conn, bool) {
	for _, c := range worldcore.AllOf[*playerConn]() {
		if c.Username == username {
			return c, true
		}
	}
	return nil, false
}

// tradeDeps wires the controller seams to the root globals.
func tradeDeps() controller.Deps {
	return controller.Deps{Store: tradeStore{}, Bus: tradeBus{}, Peers: tradePeers{}}
}

// ---------------------------------------------------------------------------
// Entry points (signatures UNCHANGED; main.go/m6.go/m7.go call sites compile
// as-is). The testMode gate stays here — it reads a root global.
// ---------------------------------------------------------------------------

// handleTrade ports incoming.handleTrade's opcode switch.
func handleTrade(c *playerConn, data []byte) {
	controller.HandleTrade(c, data, tradeDeps())
}

// handleEnchant ports incoming.handleEnchant (Select/Confirm; the TS
// handler has NO canAccessContainer gate — the client only opens the menu
// from the enchanter NPC; server checks live inside the enchanter itself).
func handleEnchant(c *playerConn, data []byte) {
	controller.HandleEnchant(c, data, tradeDeps())
}

// handleCrafting ports incoming.handleCrafting (activeCraftingInterface
// gate + Select/Craft opcodes).
func handleCrafting(c *playerConn, data []byte) {
	controller.HandleCrafting(c, data, tradeDeps())
}

// handleTradeTest serves the e2e harness: state probes + seeding.
func handleTradeTest(c *playerConn, data []byte) {
	if !testMode {
		return
	}
	// Admin-rank gate (see handleMinigameTest: TESTMAP default stays ON, the gate
	// closes the any-client spawn/seed hole).
	if !isAdmin(c) {
		return
	}
	controller.HandleTest(c, data, tradeDeps())
}

// clearTradeSession drops both sides' session state.
func clearTradeSession(me, peer *playerConn) {
	var p controller.Conn
	if peer != nil {
		p = peer
	}
	controller.ClearSession(me, p, tradeDeps())
}

// disconnectTrade ports player.ts disconnect trade.close(): the open peer
// (if any) gets Trade Close and both sides are cleared. No-op without a session.
func disconnectTrade(me *playerConn) {
	if me == nil {
		return
	}
	controller.DisconnectClose(me, tradeDeps())
}

// forgetTradeSession drops trade state when a connection leaves (world.ts
// clearActiveTrade parity happens via close; stale map entries are dropped).
func forgetTradeSession(key string) {
	controller.ForgetSession(key)
}

// openEnchanter ports the handler.ts enchanter NPC branch: container
// access + NPC Enchant [31,3].
func openEnchanter(c *playerConn) {
	controller.OpenEnchanter(c, tradeDeps())
}

// removeItemAt removes count from one inventory slot by index (enchanter
// uses the shard's slot index directly). Emits Container Remove.
func removeItemAt(c *playerConn, username string, index, count int) {
	controller.RemoveItemAt(c, username, index, count, tradeDeps())
}

// craftingCommands ports the crafting interface open commands
// (commands.ts:1102-1121, names opencrafting/openalchemy/opencooking/
// opensmithing/opensmelting). Called from parseCommand.
func craftingCommands(c *playerConn, command string) {
	controller.PlayerCommands(c, command, tradeDeps())
}
