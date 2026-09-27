// Package controller owns the M12 slice — trade, crafting, enchanting
// (finishes the M6 economy half). It is a behavior-frozen move of the root
// m12.go logic: identical frames, rolls and clamps.
//
// Trade ports game/entity/character/player/trade.ts: mutual Request (1-tile
// range), Open for both parties, per-player offers (add/remove with the
// undroppable + count clamps), acceptance reset on any offer change, and the
// double-accept exchange (remove-before-add with space checks, empty-trade
// and flag handling). Frames ride PacketTrade [33]:
//
//	C->S Request{instance} Add{index,count} Remove{index} Accept{} Close{}
//	S->C Open{instance} Add{instance,index,count,key} Remove{instance,index}
//	    Accept{message} Close{}
//
// Crafting ports controllers/crafting.ts over data/crafting/*.json (8 skill
// files). PacketCrafting [54]: S Open{type,previews} / Select{key,name,level,
// result,requirements}; C Select{key} / Craft{key,count}. Failure rolls, the
// actualCount clamp, and the Smelting→Smithing / Chiseling→Crafting skill
// mapping mirror crafting.ts exactly.
//
// Enchanting ports controllers/enchanter.ts + item.ts getAvailableEnchant-
// ments + Formulas.getEnchantChance. PacketEnchant [34]: C Select{index} /
// Confirm{index,shardIndex}; S Select{index,isShard?}. The enchanter NPC
// (role "enchanter", e.g. vendingmachine) grants canAccessContainer + NPC
// Enchant [31,3] (handler.ts:730-741).
//
// Transport and shared player state stay with the root server: this package
// touches them only through the Conn/Store/Bus/Peers seams, which the root
// adapter (m12.go) implements over its globals. Packet shapes and the DB
// schema are unchanged — frames are built with internal/protocol, the same
// constructors the root uses.
package controller

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"

	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// Seams (implemented by the root adapter; never by this package).
// ---------------------------------------------------------------------------

// Conn is the minimal per-connection view the trade/craft/enchant logic
// needs. The root *playerConn satisfies it, so the same pointer flows
// through and identity comparisons keep working.
type Conn interface {
	InstanceID() string
	PlayerName() string
	TileX() int
	TileY() int
	// GrantContainerAccess sets canAccessContainer (enchanter NPC branch).
	GrantContainerAccess()
}

// Slot is one inventory slot snapshot.
type Slot struct {
	Key   string
	Count int
	Ench  protocol.Enchantments
}

// Store abstracts player inventory/skill state, the item catalogue and
// dirty tracking. Every method maps to one root helper:
//
//	SlotAt/InventoryLen/CountItem -> m5StateFor + pstateMu reads
//	AddItem/AddItemEnch           -> m5AddItem/m5AddItemEnch
//	RemoveItem                    -> m6RemoveItem
//	RemoveItemAt                  -> m6InventoryRemoveAt (emits Container Remove)
//	SetSlotEnchantments           -> locked st.Inv[index].Ench write
//	SkillLevel/AddXP              -> m5StateFor skills read / m5AddXP
//	MarkDirty                     -> markDirty
//	SeedItem                      -> m6SeedItem
//	ClearInventoryButGold         -> the m12test "invclear" filter
//	ItemUndroppable/ItemName/ItemType/MaxStack -> m6 item catalogue
type Store interface {
	SlotAt(username string, index int) (Slot, bool)
	InventoryLen(username string) int
	CountItem(username, itemKey string) int
	AddItem(username, itemKey string, count int) int
	AddItemEnch(username, itemKey string, count int, ench protocol.Enchantments) int
	RemoveItem(username, itemKey string, count int)
	RemoveItemAt(c Conn, username string, index, count int)
	SetSlotEnchantments(username string, index int, ench protocol.Enchantments) (count int, ok bool)
	SkillLevel(username string, skill int) int
	AddXP(c Conn, skill, amount int) int
	MarkDirty(username string)
	SeedItem(username, itemKey string, count int) int
	ClearInventoryButGold(username string)
	ItemUndroppable(key string) bool
	ItemName(key string) string
	ItemType(key string) (string, bool)
	MaxStack(key string) int
}

// Bus abstracts S->C frame delivery. SendTo maps to send() unicast for the
// instance's conn; Notify maps to m6Notify (Notification Text).
type Bus interface {
	SendTo(instance string, frames ...[]any)
	Notify(instance string, message string)
}

// Peers abstracts live-connection lookup (connByInstance/connByUsername).
type Peers interface {
	ByInstance(instance string) (Conn, bool)
	ByUsername(username string) (Conn, bool)
}

// Deps bundles the three seams for one call.
type Deps struct {
	Store Store
	Bus   Bus
	Peers Peers
}

// ---------------------------------------------------------------------------
// Packet payload types (common/network/impl trade.ts / crafting.ts / enchant.ts).
// ---------------------------------------------------------------------------

type tradePacketData struct {
	Instance string `json:"instance,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Count    *int   `json:"count,omitempty"`
	Key      string `json:"key,omitempty"`
	Message  string `json:"message,omitempty"`
}

type craftingPacketData struct {
	Type         *int               `json:"type,omitempty"` // Modules.Skills interface id
	Previews     []CraftPreview     `json:"previews,omitempty"`
	Key          string             `json:"key,omitempty"`
	Name         string             `json:"name,omitempty"`
	Level        *int               `json:"level,omitempty"`
	Result       *int               `json:"result,omitempty"`
	Requirements []CraftRequirement `json:"requirements,omitempty"`
	Count        *int               `json:"count,omitempty"`
}

// CraftPreview is one crafting-menu preview row.
type CraftPreview struct {
	Key   string `json:"key"`
	Level int    `json:"level"`
}

// CraftRequirement is one crafting ingredient row.
type CraftRequirement struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
	Name  string `json:"name,omitempty"`
}

type enchantPacketData struct {
	Index      *int `json:"index,omitempty"`
	ShardIndex *int `json:"shardIndex,omitempty"`
	IsShard    bool `json:"isShard,omitempty"`
}

// ---------------------------------------------------------------------------
// Data: data/crafting/*.json (CraftingInfo shape).
// ---------------------------------------------------------------------------

// CraftItem is one crafting recipe row.
type CraftItem struct {
	Level        int                `json:"level"`
	Experience   int                `json:"experience"`
	Chance       *int               `json:"chance"` // out of 100; unset = 100
	Requirements []CraftRequirement `json:"requirements"`
	Result       struct {
		Count int `json:"count"`
	} `json:"result"`
}

var (
	craftOnce  sync.Once
	craftData  = map[string]map[string]*CraftItem{} // skill file name -> key -> item
	craftFiles int
)

// dataDir resolves a packages/server/data/<name> dir (same order m11 uses;
// m11DataDir itself needs a dir, so this mirrors it for reuse clarity).
func dataDir(name string) string {
	if p := os.Getenv("RES_" + name); p != "" {
		return p
	}
	for _, rel := range []string{
		filepath.Join("..", "packages", "server", "data", name),
		filepath.Join("..", "..", "packages", "server", "data", name),
	} {
		if st, err := os.Stat(rel); err == nil && st.IsDir() {
			return rel
		}
	}
	return name
}

// load reads the crafting tables once (craftFiles logged for the e2e).
func load() {
	craftOnce.Do(func() {
		dir := dataDir("crafting")
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("m12: read crafting dir: %v (crafting disabled)", err)
			return
		}
		for _, f := range entries {
			if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
				continue
			}
			skill := f.Name()[:len(f.Name())-5]
			raw, err := os.ReadFile(filepath.Join(dir, f.Name()))
			if err != nil {
				continue
			}
			table := map[string]*CraftItem{}
			if err := json.Unmarshal(raw, &table); err != nil {
				log.Printf("m12: parse crafting/%s: %v", f.Name(), err)
				continue
			}
			craftData[skill] = table
			craftFiles++
		}
		log.Printf("m12: crafting files=%d", craftFiles)
	})
}

// SkillForInterface ports crafting.getSkillByInterface: Smelting rewards
// Smithing XP, Chiseling rewards Crafting XP.
func SkillForInterface(iface int) int {
	switch iface {
	case protocol.SkillSmelting:
		return protocol.SkillSmithing
	case protocol.SkillChiseling:
		return protocol.SkillCraftingS
	}
	return iface
}

// SkillFileName maps the interface id to its crafting/ file name.
func SkillFileName(iface int) string {
	switch iface {
	case protocol.SkillCooking:
		return "cooking"
	case protocol.SkillSmithing, protocol.SkillSmelting:
		return "smithing"
	case protocol.SkillCraftingS, protocol.SkillChiseling:
		return "crafting"
	case protocol.SkillFletching:
		return "fletching"
	case protocol.SkillAlchemy:
		return "alchemy"
	}
	return ""
}

// IfaceName returns the Modules.Skills label for notify strings.
func IfaceName(iface int) string {
	switch iface {
	case protocol.SkillCooking:
		return "cooking"
	case protocol.SkillSmithing:
		return "smithing"
	case protocol.SkillCraftingS:
		return "crafting"
	case protocol.SkillChiseling:
		return "chiseling"
	case protocol.SkillFletching:
		return "fletching"
	case protocol.SkillSmelting:
		return "smelting"
	case protocol.SkillAlchemy:
		return "alchemy"
	}
	return fmt.Sprintf("skill%d", iface)
}

// ---------------------------------------------------------------------------
// Trade (player/trade.ts).
// ---------------------------------------------------------------------------

// OfferedItem is one offered stack.
type OfferedItem struct {
	InventoryIndex int
	MaxCount       int // slot count at offer time
	Key            string
	Count          int
	Ench           protocol.Enchantments // nil = plain (copy semantics, never shared)
}

// PlayerState carries the trade session + crafting interface (per player).
type PlayerState struct {
	Trades   map[string]*Trade // other instance -> session
	TradeReq map[string]string // other instance -> who requested last
	Iface    int               // activeCraftingInterface (-1 none)
}

var (
	stateMu sync.Mutex
	states  = map[string]*PlayerState{}
)

// stateFor returns the per-player state (lazily created).
func stateFor(key string) *PlayerState {
	stateMu.Lock()
	defer stateMu.Unlock()
	return stateLocked(key)
}

// stateLocked is stateFor for callers already holding stateMu
// (the mutex is NOT reentrant — never call stateFor under it).
func stateLocked(key string) *PlayerState {
	st, ok := states[key]
	if !ok {
		st = &PlayerState{Trades: map[string]*Trade{}, TradeReq: map[string]string{}, Iface: -1}
		states[key] = st
	}
	return st
}

// ForgetSession drops trade state when a connection leaves (world.ts
// clearActiveTrade parity happens via close; stale map entries are dropped).
func ForgetSession(key string) {
	stateMu.Lock()
	delete(states, key)
	stateMu.Unlock()
}

// Trade is one side of a session; other strings the peer's username.
type Trade struct {
	other  string
	offers map[int]*OfferedItem
	accept bool
	done   bool // exchange started (inProgress)
}

func other(c Conn, d Deps) (Conn, bool) {
	st := stateFor(c.PlayerName())
	// Find the peer connection by username.
	if t, ok := st.Trades[c.InstanceID()]; ok && t != nil {
		return d.Peers.ByUsername(t.other)
	}
	// Session keyed by the peer's instance as seen from this side.
	if t, ok := st.Trades[peerInst(c)]; ok {
		return d.Peers.ByUsername(t.other)
	}
	return nil, false
}

// peerInst returns the instance the current trade was opened under.
func peerInst(c Conn) string {
	st := stateFor(c.PlayerName())
	for inst, t := range st.Trades {
		if t != nil {
			return inst
		}
	}
	return ""
}

// pair returns both sides of the session in (me, peer) order plus the peer
// state; nil when the peer disconnected (session torn down).
func pair(c Conn, d Deps) (*Trade, Conn) {
	st := stateFor(c.PlayerName())
	stateMu.Lock()
	var t *Trade
	var peerInst string
	for inst, tr := range st.Trades {
		if tr != nil {
			t, peerInst = tr, inst
			break
		}
	}
	stateMu.Unlock()
	if t == nil {
		return nil, nil
	}
	peer, ok := d.Peers.ByInstance(peerInst)
	if !ok {
		// Fall back to username lookup (instance maps churn on reconnect).
		peer, ok = d.Peers.ByUsername(t.other)
	}
	if !ok {
		return nil, nil
	}
	return t, peer
}

func notifyTrade(d Deps, c Conn, message string) {
	d.Bus.Notify(c.InstanceID(), message)
}

// signalAdd relays an offer add to both parties (trade.signalAdd).
func signalAdd(d Deps, me, peer Conn, fromInst string, offerIndex, count int, key string) {
	payload := tradePacketData{Instance: fromInst, Index: &offerIndex, Count: &count, Key: key}
	d.Bus.SendTo(me.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeAdd, payload))
	d.Bus.SendTo(peer.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeAdd, payload))
}

// signalRemove relays an offer removal to both parties.
func signalRemove(d Deps, me, peer Conn, fromInst string, offerIndex int) {
	payload := tradePacketData{Instance: fromInst, Index: &offerIndex}
	d.Bus.SendTo(me.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeRemove, payload))
	d.Bus.SendTo(peer.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeRemove, payload))
}

// signalAccept relays acceptance to both parties (acceptCallback(message)).
func signalAccept(d Deps, me, peer Conn, message string) {
	d.Bus.SendTo(me.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeAccept, tradePacketData{Message: message}))
	d.Bus.SendTo(peer.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeAccept, tradePacketData{Message: message}))
}

// tradeClose closes the session for both parties (trade.close).
func tradeClose(d Deps, me Conn) {
	t, peer := pair(me, d)
	if t == nil {
		// No active session: still echo Close for client parity (incoming
		// Close calls trade.close() which early-returns without a frame).
		return
	}
	d.Bus.SendTo(me.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeClose, tradePacketData{}))
	if peer != nil {
		d.Bus.SendTo(peer.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeClose, tradePacketData{}))
	}
	ClearSession(me, peer, d)
}

// ClearSession drops both sides' session state.
func ClearSession(me, peer Conn, d Deps) {
	myInst := peerInst(me)
	stateMu.Lock()
	delete(stateLocked(me.PlayerName()).Trades, myInst)
	stateMu.Unlock()
	if peer != nil {
		peerInst := peerInst(peer)
		stateMu.Lock()
		delete(stateLocked(peer.PlayerName()).Trades, peerInst)
		stateMu.Unlock()
	}
}

// DisconnectClose ports player.ts disconnect trade.close(): when the
// disconnecter has an open session, the peer is resolved BEFORE clearing,
// gets the Trade Close frame ([33, Close opcode, {}] — the same construction
// tradeClose uses; the disconnecter's own send is skipped, its conn is gone),
// and both sides' state is dropped via ClearSession. No-op without an open
// session (trade.close's `if (!this.activeTrade) return`).
func DisconnectClose(me Conn, d Deps) {
	if me == nil {
		return
	}
	if !hasSession(me) {
		return
	}
	_, peer := pair(me, d)
	if peer != nil {
		d.Bus.SendTo(peer.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeClose, tradePacketData{}))
	}
	ClearSession(me, peer, d)
}

// hasSession reports whether me holds an open trade session. Read-only: it
// never creates state, so the no-session disconnect path stays a true no-op.
func hasSession(me Conn) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, ok := states[me.PlayerName()]
	if !ok || st == nil {
		return false
	}
	for _, t := range st.Trades {
		if t != nil {
			return true
		}
	}
	return false
}

// tradeRequest ports trade.request: 1-tile range, hollow-admin/cheater
// guards are stubbed out (no such flags in the Go slice); mutual requests
// open the session, otherwise notify both parties of the pending request.
func tradeRequest(d Deps, c Conn, targetInst string) {
	peer, ok := d.Peers.ByInstance(targetInst)
	if !ok || peer == c {
		return
	}
	// getDistance > 1 (Chebyshev) rejects.
	dx, dy := c.TileX()-peer.TileX(), c.TileY()-peer.TileY()
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	if dx > 1 || dy > 1 {
		return
	}
	if stateFor(peer.PlayerName()).TradeReq[c.InstanceID()] == c.InstanceID() || stateFor(peer.PlayerName()).TradeReq[peerInst(c)] == c.InstanceID() {
		tradeOpen(d, c, peer)
		return
	}
	// trade.lastRequest = target.instance (mutual request uses either key).
	st := stateFor(c.PlayerName())
	st.TradeReq[peer.InstanceID()] = peer.InstanceID()
	notifyTrade(d, peer, fmt.Sprintf("misc:TRADE_REQUEST_OTHER;username=%s", c.PlayerName()))
	notifyTrade(d, c, fmt.Sprintf("misc:TRADE_REQUEST;username=%s", peer.PlayerName()))
}

// tradeOpen ports trade.open: Trade Open to both (each with the other's
// instance), active sessions both ways, lastRequest cleared.
func tradeOpen(d Deps, me, peer Conn) {
	log.Printf("m12: opening trade between %s and %s", me.PlayerName(), peer.PlayerName())
	meInst, peerInst := me.InstanceID(), peer.InstanceID()
	d.Bus.SendTo(me.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeOpen, tradePacketData{Instance: peerInst}))
	d.Bus.SendTo(peer.InstanceID(), protocol.PktOp(protocol.PacketTrade, protocol.TradeOpen, tradePacketData{Instance: meInst}))
	stateMu.Lock()
	meSt := stateLocked(me.PlayerName())
	peerSt := stateLocked(peer.PlayerName())
	meSt.Trades[peerInst] = &Trade{other: peer.PlayerName(), offers: map[int]*OfferedItem{}}
	peerSt.Trades[meInst] = &Trade{other: me.PlayerName(), offers: map[int]*OfferedItem{}}
	delete(meSt.TradeReq, peerInst)
	delete(peerSt.TradeReq, meInst)
	stateMu.Unlock()
}

// tradeAdd ports trade.add: inventory slot -> offer slot (offerIndex ==
// inventory index in the Go port: our offers are keyed by the inventory slot
// they came from, which is what the client menu displays anyway).
func tradeAdd(d Deps, c Conn, index, count int) {
	t, peer := pair(c, d)
	if t == nil || peer == nil || t.done {
		return
	}
	if count < 1 {
		return // isNaN/count<1 guard
	}
	if t.offers[index] != nil && t.offers[index].Count == t.offers[index].MaxCount {
		return // isAdded: fully offered already
	}
	slot, ok := d.Store.SlotAt(c.PlayerName(), index)
	if !ok {
		return
	}
	if slot.Key == "" {
		return
	}
	// Undroppable items cannot be traded (misc:CANNOT_TRADE_ITEM).
	if d.Store.ItemUndroppable(slot.Key) {
		notifyTrade(d, c, "misc:CANNOT_TRADE_ITEM")
		return
	}
	offer, existed := t.offers[index]
	if !existed || offer == nil || offer.Key != slot.Key {
		// New offer (or the slot changed keys under a stale offer: the
		// old key's counts no longer describe this slot, so re-offer
		// fresh instead of stacking onto a foreign key).
		offerCount := ClampNewOffer(count, slot.Count)
		t.offers[index] = &OfferedItem{
			InventoryIndex: index, MaxCount: slot.Count, Key: slot.Key,
			Count: offerCount, Ench: CopyEnchantments(slot.Ench),
		}
		resetAccept(d, t, c, peer)
		signalAdd(d, c, peer, c.InstanceID(), index, offerCount, slot.Key)
		return
	}
	// Existing offer for the same slot: stack onto it up to maxCount.
	newCount := ClampStackedOffer(offer.Count, count, offer.MaxCount)
	offer.Count = newCount
	resetAccept(d, t, c, peer)
	signalAdd(d, c, peer, c.InstanceID(), index, newCount, slot.Key)
}

// CopyEnchantments clones an enchantment map (item.copy parity: never share
// the map between slots).
func CopyEnchantments(e protocol.Enchantments) protocol.Enchantments {
	if e == nil {
		return nil
	}
	out := protocol.Enchantments{}
	for k, v := range e {
		out[k] = v
	}
	return out
}

// resetAccept clears both sides' acceptance (any offer change).
func resetAccept(d Deps, t *Trade, me, peer Conn) {
	t.accept = false
	if pt := stateFor(peer.PlayerName()); pt != nil {
		stateMu.Lock()
		for _, other := range pt.Trades {
			if other != nil {
				other.accept = false
			}
		}
		stateMu.Unlock()
	}
}

// tradeRemove ports trade.remove.
func tradeRemove(d Deps, c Conn, index int) {
	t, peer := pair(c, d)
	if t == nil || peer == nil || t.done {
		return
	}
	if t.offers[index] == nil {
		return
	}
	delete(t.offers, index)
	resetAccept(d, t, c, peer)
	signalRemove(d, c, peer, c.InstanceID(), index)
}

// tradeAccept ports trade.accept: single accept relays ACCEPTED_TRADE to
// both; double accept runs the space check + exchange. The done
// check-and-set is atomic under stateMu so two concurrent accepts cannot
// both enter the exchange (TS inProgress parity — single-flighted here
// because the Go slice serves accepts from multiple goroutines).
func tradeAccept(d Deps, c Conn) {
	t, peer := pair(c, d)
	if t == nil || peer == nil {
		return
	}
	stateMu.Lock()
	if t.done {
		stateMu.Unlock()
		return
	}
	var other *Trade
	if pt, ok := states[peer.PlayerName()]; ok && pt != nil {
		for _, tr := range pt.Trades {
			if tr != nil {
				other = tr
				break
			}
		}
	}
	if other == nil || other.done {
		stateMu.Unlock()
		return
	}
	if !other.accept {
		t.accept = true
		stateMu.Unlock()
		signalAccept(d, c, peer, "misc:ACCEPTED_TRADE")
		signalAccept(d, peer, c, "misc:ACCEPTED_TRADE_OTHER")
		return
	}
	// Both accepted: claim the exchange under the lock, then run it
	// unlocked (exchange does Bus/Store I/O and closes the session,
	// which re-locks stateMu).
	t.done = true
	other.done = true
	stateMu.Unlock()
	tradeExchange(d, c, t, peer, other)
}

// emptySlots counts free inventory slots (inventory.getEmptySlots).
func emptySlots(d Deps, key string) int {
	return protocol.ModulesInventorySize - d.Store.InventoryLen(key)
}

// removeItemsBefore validates both sides' offered items first and only
// then removes them; it returns flagged (an offer the inventory no longer
// covers). Two-phase so a mid-exchange shortfall never destroys the items
// already taken (TS removes incrementally and can lose them on the flagged
// path — the validated-then-removed end state is identical on success).
func removeItemsBefore(d Deps, me, peer Conn, t, other *Trade) bool {
	for _, o := range t.offers {
		if o == nil {
			continue
		}
		if d.Store.CountItem(me.PlayerName(), o.Key) < o.Count {
			return true
		}
	}
	for _, o := range other.offers {
		if o == nil {
			continue
		}
		if d.Store.CountItem(peer.PlayerName(), o.Key) < o.Count {
			return true
		}
	}
	for _, o := range t.offers {
		if o == nil {
			continue
		}
		d.Store.RemoveItem(me.PlayerName(), o.Key, o.Count)
	}
	for _, o := range other.offers {
		if o == nil {
			continue
		}
		d.Store.RemoveItem(peer.PlayerName(), o.Key, o.Count)
	}
	return false
}

// ExchangeBlocked mirrors the trade.exchange space-check branch
// (accept() diff logic): positive diff = we offer more. It reports whether
// the exchange must abort plus the (me, peer) notify messages for the abort
// path. When it returns blocked=false the exchange proceeds.
func ExchangeBlocked(mySlots, peerSlots, myCount, peerCount int) (blocked bool, meMsg, peerMsg string) {
	diff := myCount - peerCount
	if diff != mySlots {
		if diff < 0 && mySlots < -diff {
			return true, "misc:NO_SPACE", "misc:NO_SPACE_OTHER"
		}
		if diff > 0 && peerSlots < diff {
			return true, "misc:NO_SPACE_OTHER", "misc:NO_SPACE"
		}
	}
	return false, "", ""
}

// tradeExchange ports trade.exchange: remove-before-add, space checks,
// empty-trade notify, then close. The done flags are claimed by tradeAccept
// under stateMu before this runs (single-flight); they are NOT set here so
// the check-and-set stays under one lock.
func tradeExchange(d Deps, me Conn, t *Trade, peer Conn, other *Trade) {
	// Space check (accept() diff logic): positive diff = we offer more.
	mySlots := emptySlots(d, me.PlayerName())
	peerSlots := emptySlots(d, peer.PlayerName())
	myCount := TotalOfferedCount(t)
	peerCount := TotalOfferedCount(other)
	if blocked, meMsg, peerMsg := ExchangeBlocked(mySlots, peerSlots, myCount, peerCount); blocked {
		resetAccept(d, t, me, peer)
		signalAccept(d, me, peer, "")
		notifyTrade(d, peer, peerMsg)
		notifyTrade(d, me, meMsg)
		tradeClose(d, me)
		return
	}

	if removeItemsBefore(d, me, peer, t, other) {
		notifyTrade(d, me, "misc:PLEASE_REPORT_BUG")
		notifyTrade(d, peer, "misc:PLEASE_REPORT_BUG")
		log.Printf("m12: trade exchange failed for %s and %s", me.PlayerName(), peer.PlayerName())
		tradeClose(d, me)
		return
	}

	total := 0
	for _, o := range other.offers { // peer's offers -> my inventory
		if o == nil {
			continue
		}
		idx := d.Store.AddItemEnch(me.PlayerName(), o.Key, o.Count, o.Ench)
		d.Bus.SendTo(me.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: idx, Key: o.Key, Count: o.Count, Enchantments: protocol.EnchAny(o.Ench)},
		}))
		total++
	}
	for _, o := range t.offers { // my offers -> peer inventory
		if o == nil {
			continue
		}
		idx := d.Store.AddItemEnch(peer.PlayerName(), o.Key, o.Count, o.Ench)
		d.Bus.SendTo(peer.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: idx, Key: o.Key, Count: o.Count, Enchantments: protocol.EnchAny(o.Ench)},
		}))
		total++
	}
	d.Store.MarkDirty(me.PlayerName())
	d.Store.MarkDirty(peer.PlayerName())

	if total == 0 {
		notifyTrade(d, me, "misc:TRADE_EMPTY")
		notifyTrade(d, peer, "misc:TRADE_EMPTY")
	} else {
		notifyTrade(d, me, "misc:TRADE_COMPLETE")
		notifyTrade(d, peer, "misc:TRADE_COMPLETE")
		log.Printf("m12: trade %s <-> %s (%d items)", me.PlayerName(), peer.PlayerName(), total)
	}
	tradeClose(d, me)
}

// TotalOfferedCount ports getTotalOfferedCount (distinct offer slots).
func TotalOfferedCount(t *Trade) int {
	n := 0
	for _, o := range t.offers {
		if o != nil {
			n++
		}
	}
	return n
}

// OfferList returns the non-nil offers sorted by inventory index (stable
// exchange order).
func OfferList(t *Trade) []*OfferedItem {
	out := make([]*OfferedItem, 0, len(t.offers))
	for _, o := range t.offers {
		if o != nil {
			out = append(out, o)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].InventoryIndex < out[j-1].InventoryIndex; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ClampNewOffer clamps a fresh offer to the slot count.
func ClampNewOffer(count, slotCount int) int {
	if count > slotCount {
		return slotCount
	}
	return count
}

// ClampStackedOffer stacks an add onto an existing offer up to maxCount.
func ClampStackedOffer(current, add, max int) int {
	n := current + add
	if n > max {
		n = max
	}
	return n
}

// ClampCraftCount ports the crafting actualCount clamp: requested count
// reduced per requirement to what the inventory covers, taking the minimum
// across requirements (a later requirement with looser supply must not
// overwrite an earlier tighter one back up past what it covers).
func ClampCraftCount(count int, reqs []CraftRequirement, have func(key string) int) int {
	actual := count
	for _, r := range reqs {
		if r.Count*count > have(r.Key) {
			if v := have(r.Key) / r.Count; v < actual {
				actual = v
			}
		}
	}
	return actual
}

// ---------------------------------------------------------------------------
// Crafting (controllers/crafting.ts).
// ---------------------------------------------------------------------------

// ClearCraftingIface resets the crafting interface to none (-1,
// player.ts handleMovementRequest activeCraftingInterface=-1 parity).
// Trade sessions are untouched: Iface lives beside, not inside, the
// Trades/TradeReq maps, so clearing it never drops a trade.
func ClearCraftingIface(c Conn) {
	if c == nil {
		return
	}
	stateFor(c.PlayerName()).Iface = -1
}

// CraftOpen ports crafting.open: previews for the interface's skill file +
// activeCraftingInterface = type.
func CraftOpen(d Deps, c Conn, iface int) {
	load()
	table := craftData[SkillFileName(iface)]
	if table == nil {
		notifyTrade(d, c, "misc:CANNOT_DO_THAT")
		return
	}
	stateFor(c.PlayerName()).Iface = iface
	previews := make([]CraftPreview, 0, len(table))
	for key, item := range table {
		previews = append(previews, CraftPreview{Key: key, Level: item.Level})
	}
	for i := 1; i < len(previews); i++ { // stable key order (deterministic e2e)
		for j := i; j > 0 && previews[j].Key < previews[j-1].Key; j-- {
			previews[j], previews[j-1] = previews[j-1], previews[j]
		}
	}
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketCrafting, protocol.CraftingOpen, craftingPacketData{
		Type: &iface, Previews: previews,
	}))
}

// craftSelect ports crafting.select: requirements + result for one key.
func craftSelect(d Deps, c Conn, key string) {
	load()
	iface := stateFor(c.PlayerName()).Iface
	table := craftData[SkillFileName(iface)]
	if table == nil {
		notifyTrade(d, c, "crafting:INVALID_DATA")
		return
	}
	item := table[key]
	if item == nil {
		notifyTrade(d, c, "crafting:INVALID_ITEM")
		return
	}
	reqs := make([]CraftRequirement, len(item.Requirements))
	for i, r := range item.Requirements {
		reqs[i] = r
		reqs[i].Name = d.Store.ItemName(r.Key)
	}
	result := item.Result.Count
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketCrafting, protocol.CraftingSelect, craftingPacketData{
		Key: key, Name: d.Store.ItemName(key), Level: &item.Level,
		Result: &result, Requirements: reqs,
	}))
}

// craft ports crafting.craft: level gate, actualCount clamp, failure rolls
// (Utils.randomInt(0,100) > (chance||100)+level per crafted unit), XP, result.
func craft(d Deps, c Conn, key string, count int) {
	load()
	iface := stateFor(c.PlayerName()).Iface
	if iface == -1 {
		notifyTrade(d, c, "misc:CANNOT_DO_THAT")
		return
	}
	if count != 1 && count != 5 && count != 10 {
		count = 1
	}
	table := craftData[SkillFileName(iface)]
	if table == nil {
		notifyTrade(d, c, "crafting:INVALID_DATA")
		return
	}
	item := table[key]
	if item == nil {
		notifyTrade(d, c, "crafting:INVALID_ITEM")
		return
	}
	skill := SkillForInterface(iface)
	level := d.Store.SkillLevel(c.PlayerName(), skill)
	if level < item.Level {
		notifyTrade(d, c, fmt.Sprintf("crafting:INVALID_LEVEL;skillText=%s;levelText=%d", IfaceName(iface), item.Level))
		return
	}
	for _, r := range item.Requirements {
		if d.Store.CountItem(c.PlayerName(), r.Key) < r.Count {
			notifyTrade(d, c, "crafting:INVALID_ITEMS")
			return
		}
	}
	actual := ClampCraftCount(count, item.Requirements, func(key string) int {
		return d.Store.CountItem(c.PlayerName(), key)
	})
	if actual < 1 {
		notifyTrade(d, c, "crafting:INVALID_ITEMS")
		return
	}
	for _, r := range item.Requirements {
		d.Store.RemoveItem(c.PlayerName(), r.Key, r.Count*actual)
	}
	chance := 100
	if item.Chance != nil && *item.Chance > 0 {
		chance = *item.Chance
	}
	failures := 0
	for i := 0; i < actual; i++ {
		// Utils.randomInt(0,100): inclusive both ends.
		if rand.Intn(101) > chance+level {
			failures++
		}
	}
	actual -= failures
	if failures > 0 {
		if failures > 1 {
			notifyTrade(d, c, fmt.Sprintf("crafting:FAILED_CRAFT;failedText=%dx %s.", failures, d.Store.ItemName(key)))
		} else {
			notifyTrade(d, c, "crafting:FAILED_CRAFT_ONE")
		}
	}
	d.Store.AddXP(c, skill, item.Experience*actual)
	for i := 0; i < actual; i++ {
		idx := d.Store.AddItem(c.PlayerName(), key, item.Result.Count)
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: idx, Key: key, Count: item.Result.Count, Enchantments: map[string]any{}},
		}))
	}
	d.Store.MarkDirty(c.PlayerName())
	log.Printf("m12: %s crafted %s x%d (failures %d, skill %d)", c.PlayerName(), key, actual*item.Result.Count, failures, skill)
}

// ---------------------------------------------------------------------------
// Enchanting (controllers/enchanter.ts + item.ts + formulas.getEnchantChance).
// ---------------------------------------------------------------------------

// enchantableTypes mirrors item.isEquippable's type list minus arrows
// (arrows stack -> count>1 check rejects them anyway; keep the list exact).
var enchantableTypes = map[string]bool{
	"helmet": true, "chestplate": true, "legplates": true, "skin": true,
	"weapon": true, "weaponarcher": true, "weaponmagic": true, "weaponskin": true,
	"pendant": true, "boots": true, "ring": true, "arrow": true, "shield": true, "cape": true,
}

// AvailableEnchantments mirrors item.getAvailableEnchantments.
func AvailableEnchantments(itemType string) []int {
	switch itemType {
	case "weapon":
		return []int{0, 1, 8} // Bloodsucking, Critical, DoubleEdged
	case "weaponarcher", "weaponmagic":
		return []int{4, 5} // Explosive, Stun
	case "armour", "armourarcher":
		return []int{2, 3, 6} // Evasion, Thorns, AntiStun
	}
	return nil
}

// EnchantChance mirrors Formulas.getEnchantChance: randomInt(0,100) < 8*tier.
func EnchantChance(tier int) bool {
	return rand.Intn(101) < 8*tier
}

// OpenEnchanter ports the handler.ts enchanter NPC branch: container
// access + NPC Enchant [31,3].
func OpenEnchanter(c Conn, d Deps) {
	c.GrantContainerAccess()
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketNPC, protocol.NPCEnchant, map[string]any{}))
}

// enchantSelect ports enchanter.select.
func enchantSelect(d Deps, c Conn, index int) {
	if index == -1 {
		notifyTrade(d, c, "enchant:CANNOT_ENCHANT")
		return
	}
	slot, ok := d.Store.SlotAt(c.PlayerName(), index)
	if !ok || slot.Key == "" {
		notifyTrade(d, c, "enchant:CANNOT_ENCHANT")
		return
	}
	key, count := slot.Key, slot.Count
	if len(key) >= 6 && key[:6] == "shardt" {
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketEnchant, protocol.EnchantSelect, enchantPacketData{Index: &index, IsShard: true}))
		return
	}
	typ, hasType := d.Store.ItemType(key)
	if !hasType || count > 1 || !enchantableTypes[typ] || d.Store.MaxStack(key) > 1 {
		notifyTrade(d, c, "enchant:CANNOT_ENCHANT")
		return
	}
	if len(AvailableEnchantments(typ)) == 0 {
		notifyTrade(d, c, "enchant:CANNOT_ENCHANT")
		return
	}
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketEnchant, protocol.EnchantSelect, enchantPacketData{Index: &index}))
}

// enchantConfirm ports enchanter.enchant: shard tier chance roll, shard
// consume, random enchantment + level, apply-when-higher, inventory resync.
func enchantConfirm(d Deps, c Conn, index, shardIndex int) {
	if index == -1 || shardIndex == -1 {
		notifyTrade(d, c, "enchant:NO_ITEM_SELECTED")
		return
	}
	itemSlot, ok := d.Store.SlotAt(c.PlayerName(), index)
	if !ok {
		itemSlot = Slot{}
	}
	shardSlot, ok := d.Store.SlotAt(c.PlayerName(), shardIndex)
	if !ok {
		shardSlot = Slot{}
	}
	itemKey, itemEnch := itemSlot.Key, itemSlot.Ench
	shardKey := shardSlot.Key
	if itemKey == "" || shardKey == "" {
		notifyTrade(d, c, "enchant:NO_ITEM_SELECTED")
		return
	}
	if len(shardKey) < 6 || shardKey[:6] != "shardt" {
		notifyTrade(d, c, "enchant:NO_SHARD")
		return
	}
	typ, hasType := d.Store.ItemType(itemKey)
	if !hasType {
		notifyTrade(d, c, "enchant:NO_ITEM_SELECTED")
		return
	}
	enchantments := AvailableEnchantments(typ)
	if len(enchantments) == 0 {
		notifyTrade(d, c, "enchant:NO_ITEM_SELECTED")
		return
	}
	tier := 1
	fmt.Sscanf(shardKey, "shardt%d", &tier)
	if tier < 1 {
		tier = 1 // packet-built shards (shardt0/bare shardt) must not reach Intn(0)
	}
	chance := EnchantChance(tier)

	// Remove one shard (inventory.remove(shardIndex, 1)). Same-index
	// use is safe: index==shardIndex always exits through the NO_SHARD
	// / NO_ITEM_SELECTED / unknown-type returns above, so the removal
	// below never eats the item being enchanted.
	RemoveItemAt(c, c.PlayerName(), shardIndex, 1, d)
	// The dense inventory compacts on removal: an item after the shard
	// shifts down one, so re-target it (TS slots are fixed, no shift).
	if shardIndex < index {
		index--
	}

	if !chance {
		notifyTrade(d, c, "enchant:FAILED_ENCHANT")
		d.Store.MarkDirty(c.PlayerName())
		return
	}
	// Random enchantment + level 1..tier (Utils.randomInt(1, tier)).
	enchantment := enchantments[rand.Intn(len(enchantments))]
	level := 1 + rand.Intn(tier)

	// canEnchant: apply only when new or strictly higher level.
	cur, has := itemEnch[enchantment]
	if has && level <= cur.Level {
		notifyTrade(d, c, "enchant:FAILED_ENCHANT")
		// Failure still consumed the shard; the item keeps its old enchant.
	} else {
		if itemEnch == nil {
			itemEnch = protocol.Enchantments{}
		}
		itemEnch[enchantment] = protocol.Enchantment{Level: level}
		d.Store.SetSlotEnchantments(c.PlayerName(), index, itemEnch)
		notifyTrade(d, c, "enchant:SUCCESSFUL_ENCHANT")
		// Synchronize the slot (inventory.loadCallback parity: Container Add
		// with the updated enchantments overwrites the client slot).
		count := 0
		if s, ok := d.Store.SlotAt(c.PlayerName(), index); ok {
			count = s.Count
		}
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: index, Key: itemKey, Count: count, Enchantments: protocol.EnchAny(itemEnch)},
		}))
	}
	d.Store.MarkDirty(c.PlayerName())
	log.Printf("m12: %s enchants %s with %s t%d -> ench %d level %d (roll %v)", c.PlayerName(), itemKey, shardKey, tier, enchantment, level, chance)
}

// RemoveItemAt removes count from one inventory slot by index (enchanter
// uses the shard's slot index directly). Re-homed from the root m6RemoveItemAt
// (which lived in m12.go despite its m6* name): it emits Container Remove.
func RemoveItemAt(c Conn, username string, index, count int, d Deps) {
	d.Store.RemoveItemAt(c, username, index, count)
}

// ---------------------------------------------------------------------------
// C->S dispatch (incoming.ts handleTrade/handleEnchant + crafting packets).
// ---------------------------------------------------------------------------

// HandleTrade ports incoming.handleTrade's opcode switch.
func HandleTrade(c Conn, data []byte, d Deps) {
	var pkt struct {
		Opcode   int    `json:"opcode"`
		Instance string `json:"instance"`
		Index    *int   `json:"index"`
		Count    *int   `json:"count"`
	}
	if err := json.Unmarshal(data, &pkt); err != nil {
		return
	}
	index := 0
	if pkt.Index != nil {
		index = *pkt.Index
	}
	count := 1
	if pkt.Count != nil {
		count = *pkt.Count
	}
	switch pkt.Opcode {
	case protocol.TradeRequest:
		tradeRequest(d, c, pkt.Instance)
	case protocol.TradeAdd:
		tradeAdd(d, c, index, count)
	case protocol.TradeRemove:
		tradeRemove(d, c, index)
	case protocol.TradeAccept:
		tradeAccept(d, c)
	case protocol.TradeClose:
		tradeClose(d, c)
	}
}

// HandleEnchant ports incoming.handleEnchant (Select/Confirm; the TS
// handler has NO canAccessContainer gate — the client only opens the menu
// from the enchanter NPC; server checks live inside the enchanter itself).
func HandleEnchant(c Conn, data []byte, d Deps) {
	var pkt struct {
		Opcode     int  `json:"opcode"`
		Index      *int `json:"index"`
		ShardIndex *int `json:"shardIndex"`
	}
	if err := json.Unmarshal(data, &pkt); err != nil {
		return
	}
	index, shard := -1, -1
	if pkt.Index != nil {
		index = *pkt.Index
	}
	if pkt.ShardIndex != nil {
		shard = *pkt.ShardIndex
	}
	switch pkt.Opcode {
	case protocol.EnchantSelect:
		enchantSelect(d, c, index)
	case protocol.EnchantConfirm:
		enchantConfirm(d, c, index, shard)
	}
}

// HandleCrafting ports incoming.handleCrafting (activeCraftingInterface
// gate + Select/Craft opcodes).
func HandleCrafting(c Conn, data []byte, d Deps) {
	var pkt struct {
		Opcode int    `json:"opcode"`
		Key    string `json:"key"`
		Count  *int   `json:"count"`
	}
	if err := json.Unmarshal(data, &pkt); err != nil {
		return
	}
	if stateFor(c.PlayerName()).Iface == -1 {
		notifyTrade(d, c, "misc:CANNOT_DO_THAT")
		return
	}
	count := 1
	if pkt.Count != nil {
		count = *pkt.Count
	}
	switch pkt.Opcode {
	case protocol.CraftingSelect:
		craftSelect(d, c, pkt.Key)
	case protocol.CraftingCraft:
		craft(d, c, pkt.Key, count)
	}
}

// ---------------------------------------------------------------------------
// TESTMAP debug dispatcher [46 {m12test}] + player commands.
// ---------------------------------------------------------------------------

// HandleTest serves the e2e harness: state probes + seeding. The testMode
// gate stays in the root adapter (it reads a root global); this body is the
// moved remainder.
func HandleTest(c Conn, data []byte, d Deps) {
	var dt struct {
		M12Test string `json:"m12test"`
		Key     string `json:"key"`
		Count   int    `json:"count"`
		Iface   int    `json:"iface"`
		Slot    int    `json:"slot"`
		Echo    string `json:"echo"`
	}
	if err := json.Unmarshal(data, &dt); err != nil || dt.M12Test == "" || c == nil {
		return
	}
	load()
	switch dt.M12Test {
	case "craftif": // set activeCraftingInterface (mirrors /crafting open)
		stateFor(c.PlayerName()).Iface = dt.Iface
	case "seed": // seedItem: append a stack, reply the slot index
		idx := d.Store.SeedItem(c.PlayerName(), dt.Key, maxInt(dt.Count, 1))
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("m12:seed:%s=%d", dt.Key, idx))
	case "invcount": // inventory.count probe
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("m12:count:%s=%d", dt.Key, d.Store.CountItem(c.PlayerName(), dt.Key)))
	case "invclear": // drop everything but gold (deterministic space checks)
		d.Store.ClearInventoryButGold(c.PlayerName())
		d.Bus.Notify(c.InstanceID(), "m12:invclear=ok")
	case "echo":
		var reply string
		switch dt.Echo {
		case "offers": // "m12:offers=me N peer N"
			t, peer := pair(c, d)
			if t == nil || peer == nil {
				reply = "m12:offers=none"
				break
			}
			pt := stateFor(peer.PlayerName())
			reply = fmt.Sprintf("m12:offers=me %d peer %d", TotalOfferedCount(t), TotalOfferedCount(pt.Trades[peerInst(c)]))
		case "accepted":
			t, _ := pair(c, d)
			if t == nil {
				reply = "m12:accepted=none"
			} else {
				reply = fmt.Sprintf("m12:accepted=%v", t.accept)
			}
		case "ench": // enchantments of inventory slot d.Slot
			ench := protocol.Enchantments{}
			if s, ok := d.Store.SlotAt(c.PlayerName(), dt.Slot); ok && s.Ench != nil {
				ench = s.Ench
			}
			raw, _ := json.Marshal(ench)
			reply = fmt.Sprintf("m12:ench:%d=%s", dt.Slot, string(raw))
		}
		if reply != "" {
			d.Bus.Notify(c.InstanceID(), reply)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// PlayerCommands ports the crafting interface open commands
// (commands.ts:1102-1121, names opencrafting/openalchemy/opencooking/
// opensmithing/opensmelting). Called from m7ParseCommand.
func PlayerCommands(c Conn, command string, d Deps) {
	switch command {
	case "opencrafting":
		CraftOpen(d, c, protocol.SkillCraftingS)
	case "openalchemy":
		CraftOpen(d, c, protocol.SkillAlchemy)
	case "opencooking":
		CraftOpen(d, c, protocol.SkillCooking)
	case "opensmithing":
		CraftOpen(d, c, protocol.SkillSmithing)
	case "opensmelting":
		CraftOpen(d, c, protocol.SkillSmelting)
	}
}
