package controller

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// In-memory fakes for the Store/Bus/Peers seams.
// ---------------------------------------------------------------------------

type fakeConn struct {
	instance  string
	username  string
	x, y      int
	container bool
	// EconomyConn session state.
	storeOpen string
	canAccess bool
	talkKey   string
	talkIndex int
}

func (c *fakeConn) InstanceID() string    { return c.instance }
func (c *fakeConn) PlayerName() string    { return c.username }
func (c *fakeConn) TileX() int            { return c.x }
func (c *fakeConn) TileY() int            { return c.y }
func (c *fakeConn) GrantContainerAccess() { c.container = true }

// EconomyConn seam (bank/talk session state).
func (c *fakeConn) StoreOpen() string     { return c.storeOpen }
func (c *fakeConn) SetStoreOpen(s string) { c.storeOpen = s }
func (c *fakeConn) CanAccess() bool       { return c.canAccess }
func (c *fakeConn) SetCanAccess(b bool)   { c.canAccess = b }
func (c *fakeConn) TalkKey() string       { return c.talkKey }
func (c *fakeConn) SetTalkKey(s string)   { c.talkKey = s }
func (c *fakeConn) TalkIndex() int        { return c.talkIndex }
func (c *fakeConn) SetTalkIndex(i int)    { c.talkIndex = i }

type fakeStore struct {
	inv   map[string][]Slot
	bank  map[string][]Slot
	equip map[string][]Slot
	attrs map[string]fakeAttr
	xp    map[string]map[int]int
}

type fakeAttr struct {
	undroppable bool
	name        string
	typ         string
	maxStack    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{inv: map[string][]Slot{}, bank: map[string][]Slot{}, equip: map[string][]Slot{}, attrs: map[string]fakeAttr{}, xp: map[string]map[int]int{}}
}

func (s *fakeStore) SlotAt(username string, index int) (Slot, bool) {
	inv := s.inv[username]
	if index < 0 || index >= len(inv) {
		return Slot{}, false
	}
	return inv[index], true
}

func (s *fakeStore) InventoryLen(username string) int { return len(s.inv[username]) }

func (s *fakeStore) CountItem(username, itemKey string) int {
	n := 0
	for _, sl := range s.inv[username] {
		if sl.Key == itemKey {
			n += sl.Count
		}
	}
	return n
}

func (s *fakeStore) AddItem(username, itemKey string, count int) int {
	return s.AddItemEnch(username, itemKey, count, nil)
}

func (s *fakeStore) AddItemEnch(username, itemKey string, count int, ench protocol.Enchantments) int {
	s.inv[username] = append(s.inv[username], Slot{Key: itemKey, Count: count, Ench: ench})
	return len(s.inv[username]) - 1
}

func (s *fakeStore) RemoveItem(username, itemKey string, count int) {
	inv := s.inv[username]
	for i := 0; i < len(inv) && count > 0; i++ {
		if inv[i].Key != itemKey {
			continue
		}
		take := inv[i].Count
		if take > count {
			take = count
		}
		inv[i].Count -= take
		count -= take
	}
	out := inv[:0]
	for _, sl := range inv {
		if sl.Count > 0 {
			out = append(out, sl)
		}
	}
	s.inv[username] = out
}

func (s *fakeStore) RemoveItemAt(c Conn, username string, index, count int) {
	inv := s.inv[username]
	if index < 0 || index >= len(inv) {
		return
	}
	inv[index].Count -= count
	if inv[index].Count <= 0 {
		s.inv[username] = append(inv[:index], inv[index+1:]...)
	}
}

func (s *fakeStore) SetSlotEnchantments(username string, index int, ench protocol.Enchantments) (int, bool) {
	inv := s.inv[username]
	if index < 0 || index >= len(inv) {
		return 0, false
	}
	inv[index].Ench = ench
	return inv[index].Count, true
}

func (s *fakeStore) SkillLevel(username string, skill int) int { return 1 }
func (s *fakeStore) AddXP(c Conn, skill, amount int) int {
	if s.xp[c.PlayerName()] == nil {
		s.xp[c.PlayerName()] = map[int]int{}
	}
	s.xp[c.PlayerName()][skill] += amount
	return 1
}
func (s *fakeStore) MarkDirty(username string) {}
func (s *fakeStore) SeedItem(username, itemKey string, count int) int {
	return s.AddItem(username, itemKey, count)
}
func (s *fakeStore) ClearInventoryButGold(username string) {
	out := s.inv[username][:0]
	for _, sl := range s.inv[username] {
		if sl.Key == "gold" {
			out = append(out, sl)
		}
	}
	s.inv[username] = out
}

func (s *fakeStore) ItemUndroppable(key string) bool { return s.attrs[key].undroppable }
func (s *fakeStore) ItemName(key string) string {
	if n := s.attrs[key].name; n != "" {
		return n
	}
	return key
}
func (s *fakeStore) ItemType(key string) (string, bool) {
	if _, ok := s.attrs[key]; !ok {
		return "", false
	}
	return s.attrs[key].typ, true
}
func (s *fakeStore) MaxStack(key string) int {
	if m := s.attrs[key].maxStack; m > 0 {
		return m
	}
	return 1
}

// EconomyStore seam (bank/equipment/total-level).
func (s *fakeStore) InventorySlots(username string) []Slot {
	out := make([]Slot, len(s.inv[username]))
	copy(out, s.inv[username])
	return out
}

func (s *fakeStore) SetInventory(username string, slots []Slot) {
	out := make([]Slot, len(slots))
	copy(out, slots)
	s.inv[username] = out
}

func (s *fakeStore) BankSlots(username string) []Slot {
	out := make([]Slot, len(s.bank[username]))
	copy(out, s.bank[username])
	return out
}

func (s *fakeStore) SetBank(username string, slots []Slot) {
	out := make([]Slot, len(slots))
	copy(out, slots)
	s.bank[username] = out
}

func (s *fakeStore) EquipSlot(username string, slotType int) (Slot, bool) {
	eq := s.equip[username]
	if slotType < 0 || slotType >= len(eq) {
		return Slot{}, false
	}
	return eq[slotType], true
}

func (s *fakeStore) SetEquip(username string, slotType int, slot Slot) {
	eq := s.equip[username]
	for len(eq) <= slotType {
		eq = append(eq, Slot{})
	}
	eq[slotType] = slot
	s.equip[username] = eq
}

func (s *fakeStore) EquipLen(username string) int { return protocol.ModulesEquipmentCount }

func (s *fakeStore) TotalLevel(username string) int { return 1 }

type frame struct {
	id     int
	opcode int
	data   []byte
}

type fakeBus struct {
	sent   map[string][]frame
	notifs map[string][]string
}

func newFakeBus() *fakeBus {
	return &fakeBus{sent: map[string][]frame{}, notifs: map[string][]string{}}
}

func (b *fakeBus) SendTo(instance string, frames ...[]any) {
	for _, f := range frames {
		var fr frame
		if len(f) > 0 {
			if id, ok := f[0].(int); ok {
				fr.id = id
			}
		}
		if len(f) > 2 {
			if op, ok := f[1].(int); ok {
				fr.opcode = op
			}
			raw, _ := json.Marshal(f[2])
			fr.data = raw
		}
		b.sent[instance] = append(b.sent[instance], fr)
	}
}

func (b *fakeBus) Notify(instance string, message string) {
	b.notifs[instance] = append(b.notifs[instance], message)
}

// EconomyBus seam (Sync broadcasts).
func (b *fakeBus) Broadcast(frames ...[]any) {
	for _, f := range frames {
		var fr frame
		if len(f) > 0 {
			if id, ok := f[0].(int); ok {
				fr.id = id
			}
		}
		if len(f) > 2 {
			if op, ok := f[1].(int); ok {
				fr.opcode = op
			}
			raw, _ := json.Marshal(f[2])
			fr.data = raw
		}
		b.sent["broadcast"] = append(b.sent["broadcast"], fr)
	}
}

func (b *fakeBus) hasOpcode(instance string, id, opcode int) bool {
	for _, f := range b.sent[instance] {
		if f.id == id && f.opcode == opcode {
			return true
		}
	}
	return false
}

func (b *fakeBus) hasNotif(instance, substr string) bool {
	for _, m := range b.notifs[instance] {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

type fakePeers struct {
	byInst map[string]Conn
	byName map[string]Conn
}

func newFakePeers(conns ...Conn) *fakePeers {
	p := &fakePeers{byInst: map[string]Conn{}, byName: map[string]Conn{}}
	for _, c := range conns {
		p.byInst[c.InstanceID()] = c
		p.byName[c.PlayerName()] = c
	}
	return p
}

func (p *fakePeers) ByInstance(instance string) (Conn, bool) {
	c, ok := p.byInst[instance]
	return c, ok
}

func (p *fakePeers) ByUsername(username string) (Conn, bool) {
	c, ok := p.byName[username]
	return c, ok
}

func testDeps(s Store, b Bus, p Peers) Deps { return Deps{Store: s, Bus: b, Peers: p} }

func resetTradeState() {
	stateMu.Lock()
	states = map[string]*PlayerState{}
	stateMu.Unlock()
}

// ---------------------------------------------------------------------------
// Pure offer/exchange math.
// ---------------------------------------------------------------------------

func TestSkillMappings(t *testing.T) {
	if got := SkillForInterface(protocol.SkillSmelting); got != protocol.SkillSmithing {
		t.Fatalf("smelting -> %d, want smithing %d", got, protocol.SkillSmithing)
	}
	if got := SkillForInterface(protocol.SkillChiseling); got != protocol.SkillCraftingS {
		t.Fatalf("chiseling -> %d, want crafting %d", got, protocol.SkillCraftingS)
	}
	if got := SkillForInterface(protocol.SkillCooking); got != protocol.SkillCooking {
		t.Fatalf("cooking passthrough -> %d", got)
	}
	cases := map[int]string{
		protocol.SkillCooking:   "cooking",
		protocol.SkillSmithing:  "smithing",
		protocol.SkillSmelting:  "smithing",
		protocol.SkillCraftingS: "crafting",
		protocol.SkillChiseling: "crafting",
		protocol.SkillFletching: "fletching",
		protocol.SkillAlchemy:   "alchemy",
		9999:                    "",
	}
	for iface, want := range cases {
		if got := SkillFileName(iface); got != want {
			t.Fatalf("SkillFileName(%d) = %q, want %q", iface, got, want)
		}
	}
}

func TestAvailableEnchantments(t *testing.T) {
	if got := AvailableEnchantments("weapon"); fmt.Sprint(got) != "[0 1 8]" {
		t.Fatalf("weapon = %v", got)
	}
	if got := AvailableEnchantments("weaponarcher"); fmt.Sprint(got) != "[4 5]" {
		t.Fatalf("weaponarcher = %v", got)
	}
	if got := AvailableEnchantments("armourarcher"); fmt.Sprint(got) != "[2 3 6]" {
		t.Fatalf("armourarcher = %v", got)
	}
	if got := AvailableEnchantments("helmet"); got != nil {
		t.Fatalf("helmet = %v, want nil", got)
	}
}

func TestCopyEnchantments(t *testing.T) {
	if CopyEnchantments(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	src := protocol.Enchantments{0: protocol.Enchantment{Level: 2}}
	cp := CopyEnchantments(src)
	cp[0] = protocol.Enchantment{Level: 9}
	if src[0].Level != 2 {
		t.Fatal("copy shares the map with the source")
	}
}

func TestOfferClamps(t *testing.T) {
	if got := ClampNewOffer(5, 3); got != 3 {
		t.Fatalf("new offer clamp = %d, want 3", got)
	}
	if got := ClampNewOffer(2, 3); got != 2 {
		t.Fatalf("new offer clamp = %d, want 2", got)
	}
	if got := ClampStackedOffer(2, 5, 4); got != 4 {
		t.Fatalf("stacked offer clamp = %d, want 4", got)
	}
	if got := ClampStackedOffer(1, 1, 4); got != 2 {
		t.Fatalf("stacked offer clamp = %d, want 2", got)
	}
}

func TestTotalOfferedCountAndOfferList(t *testing.T) {
	tr := &Trade{offers: map[int]*OfferedItem{
		5: {InventoryIndex: 5, Key: "a", Count: 1},
		2: {InventoryIndex: 2, Key: "b", Count: 1},
		7: nil,
	}}
	if got := TotalOfferedCount(tr); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	list := OfferList(tr)
	if len(list) != 2 || list[0].InventoryIndex != 2 || list[1].InventoryIndex != 5 {
		t.Fatalf("offer list not sorted by slot: %+v", list)
	}
}

func TestExchangeBlocked(t *testing.T) {
	// diff == mySlots -> proceed (no check trips).
	if blocked, _, _ := ExchangeBlocked(3, 3, 1, 1); blocked {
		t.Fatal("equal diff/slots must proceed")
	}
	// diff<0 and I lack space -> abort with (me NO_SPACE, peer NO_SPACE_OTHER).
	blocked, me, peer := ExchangeBlocked(0, 5, 0, 2)
	if !blocked || me != "misc:NO_SPACE" || peer != "misc:NO_SPACE_OTHER" {
		t.Fatalf("incoming-space abort = %v %q %q", blocked, me, peer)
	}
	// diff>0 and peer lacks space -> abort swapped.
	blocked, me, peer = ExchangeBlocked(5, 0, 2, 0)
	if !blocked || me != "misc:NO_SPACE_OTHER" || peer != "misc:NO_SPACE" {
		t.Fatalf("outgoing-space abort = %v %q %q", blocked, me, peer)
	}
	// diff != slots but both sides fit -> proceed.
	if blocked, _, _ := ExchangeBlocked(5, 5, 1, 0); blocked {
		t.Fatal("fitting trade must proceed")
	}
}

func TestClampCraftCount(t *testing.T) {
	reqs := []CraftRequirement{{Key: "a", Count: 2}, {Key: "b", Count: 1}}
	have := func(k string) int { return map[string]int{"a": 5, "b": 9}[k] }
	// a: 2*5 > 5 -> candidate 5/2 = 2; b: 1*5 <= 9 -> no clamp. Min = 2.
	if got := ClampCraftCount(5, reqs, have); got != 2 {
		t.Fatalf("clamp = %d, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Functional trade round-trip over the fakes.
// ---------------------------------------------------------------------------

func TestTradeRoundTrip(t *testing.T) {
	resetTradeState()
	s := newFakeStore()
	s.attrs["logs"] = fakeAttr{name: "Logs", typ: "material", maxStack: 10}
	s.attrs["bronzesword"] = fakeAttr{name: "Bronze sword", typ: "weapon"}
	s.inv["alice"] = []Slot{{Key: "logs", Count: 3}}
	s.inv["bob"] = []Slot{{Key: "bronzesword", Count: 1}}

	a := &fakeConn{instance: "a1", username: "alice", x: 100, y: 96}
	b := &fakeConn{instance: "b1", username: "bob", x: 101, y: 96}
	bus := newFakeBus()
	d := testDeps(s, bus, newFakePeers(a, b))

	// Mutual request opens the session for both parties.
	HandleTrade(a, []byte(`{"opcode":0,"instance":"b1"}`), d)
	HandleTrade(b, []byte(`{"opcode":0,"instance":"a1"}`), d)
	if !bus.hasOpcode("a1", protocol.PacketTrade, protocol.TradeOpen) {
		t.Fatal("alice never got Trade Open")
	}
	if !bus.hasOpcode("b1", protocol.PacketTrade, protocol.TradeOpen) {
		t.Fatal("bob never got Trade Open")
	}

	// Offer more than the slot holds -> clamped to the slot count.
	HandleTrade(a, []byte(`{"opcode":1,"index":0,"count":99}`), d)
	tr, _ := pair(a, d)
	if tr == nil || tr.offers[0] == nil || tr.offers[0].Count != 3 {
		t.Fatalf("alice offer not clamped to 3: %+v", tr)
	}
	HandleTrade(b, []byte(`{"opcode":1,"index":0,"count":1}`), d)

	// Single accept relays (Trade Accept frames to both); double accept exchanges.
	HandleTrade(a, []byte(`{"opcode":3}`), d)
	if !bus.hasOpcode("a1", protocol.PacketTrade, protocol.TradeAccept) {
		t.Fatal("no ACCEPTED_TRADE relay frames")
	}
	HandleTrade(b, []byte(`{"opcode":3}`), d)
	if s.CountItem("alice", "bronzesword") != 1 {
		t.Fatalf("alice inv: %+v", s.inv["alice"])
	}
	if s.CountItem("bob", "logs") != 3 {
		t.Fatalf("bob inv: %+v", s.inv["bob"])
	}
	if !bus.hasNotif("a1", "TRADE_COMPLETE") || !bus.hasNotif("b1", "TRADE_COMPLETE") {
		t.Fatal("missing TRADE_COMPLETE")
	}
	// Session closed on both sides.
	if tr, _ := pair(a, d); tr != nil {
		t.Fatal("session not closed after exchange")
	}
}

func TestTradeUndroppableRejected(t *testing.T) {
	resetTradeState()
	s := newFakeStore()
	s.attrs["questitem"] = fakeAttr{name: "Quest item", typ: "material", undroppable: true}
	s.inv["alice"] = []Slot{{Key: "questitem", Count: 1}}
	s.inv["bob"] = []Slot{{Key: "logs", Count: 1}}
	s.attrs["logs"] = fakeAttr{name: "Logs", typ: "material", maxStack: 10}

	a := &fakeConn{instance: "a1", username: "alice", x: 100, y: 96}
	b := &fakeConn{instance: "b1", username: "bob", x: 101, y: 96}
	bus := newFakeBus()
	d := testDeps(s, bus, newFakePeers(a, b))

	HandleTrade(a, []byte(`{"opcode":0,"instance":"b1"}`), d)
	HandleTrade(b, []byte(`{"opcode":0,"instance":"a1"}`), d)
	HandleTrade(a, []byte(`{"opcode":1,"index":0,"count":1}`), d)
	if !bus.hasNotif("a1", "CANNOT_TRADE_ITEM") {
		t.Fatalf("undroppable offer not rejected: %v", bus.notifs["a1"])
	}
	if tr, _ := pair(a, d); tr != nil && TotalOfferedCount(tr) != 0 {
		t.Fatal("rejected offer was recorded")
	}
}

func TestOpenEnchanterGrantsAccess(t *testing.T) {
	s := newFakeStore()
	bus := newFakeBus()
	a := &fakeConn{instance: "a1", username: "alice"}
	d := testDeps(s, bus, newFakePeers(a))
	OpenEnchanter(a, d)
	if !a.container {
		t.Fatal("container access not granted")
	}
	if !bus.hasOpcode("a1", protocol.PacketNPC, protocol.NPCEnchant) {
		t.Fatal("no NPC Enchant frame")
	}
}

func TestDisconnectCloseNotifiesPeerAndClearsBoth(t *testing.T) {
	resetTradeState()
	s := newFakeStore()
	a := &fakeConn{instance: "a1", username: "alice", x: 100, y: 96}
	b := &fakeConn{instance: "b1", username: "bob", x: 101, y: 96}
	bus := newFakeBus()
	d := testDeps(s, bus, newFakePeers(a, b))

	// Mutual request opens the session for both parties.
	HandleTrade(a, []byte(`{"opcode":0,"instance":"b1"}`), d)
	HandleTrade(b, []byte(`{"opcode":0,"instance":"a1"}`), d)
	if tr, _ := pair(a, d); tr == nil {
		t.Fatal("session never opened")
	}

	// Alice disconnects: Bob must get Trade Close, both sides cleared.
	DisconnectClose(a, d)
	if !bus.hasOpcode("b1", protocol.PacketTrade, protocol.TradeClose) {
		t.Fatal("peer never got Trade Close on disconnect")
	}
	if bus.hasOpcode("a1", protocol.PacketTrade, protocol.TradeClose) {
		t.Fatal("disconnecter must not get a Close frame (conn is gone)")
	}
	if tr, _ := pair(a, d); tr != nil {
		t.Fatal("disconnecter's session not cleared")
	}
	if tr, _ := pair(b, d); tr != nil {
		t.Fatal("peer's session not cleared")
	}

	// The cleared peer can immediately open a fresh trade.
	bus.sent = map[string][]frame{}
	c := &fakeConn{instance: "c1", username: "carol", x: 102, y: 96}
	d.Peers.(*fakePeers).byInst["c1"] = c
	d.Peers.(*fakePeers).byName["carol"] = c
	HandleTrade(b, []byte(`{"opcode":0,"instance":"c1"}`), d)
	HandleTrade(c, []byte(`{"opcode":0,"instance":"b1"}`), d)
	if !bus.hasOpcode("b1", protocol.PacketTrade, protocol.TradeOpen) {
		t.Fatal("peer could not open a fresh trade after disconnect clear")
	}
}

func TestDisconnectCloseNoSessionNoOp(t *testing.T) {
	resetTradeState()
	s := newFakeStore()
	a := &fakeConn{instance: "a1", username: "alice", x: 100, y: 96}
	bus := newFakeBus()
	d := testDeps(s, bus, newFakePeers(a))

	DisconnectClose(a, d) // must not panic, must send nothing
	for inst, frames := range bus.sent {
		if len(frames) != 0 {
			t.Fatalf("no-session disconnect sent %d frames to %s", len(frames), inst)
		}
	}
	for inst, notifs := range bus.notifs {
		if len(notifs) != 0 {
			t.Fatalf("no-session disconnect sent %d notifs to %s", len(notifs), inst)
		}
	}
	DisconnectClose(nil, d) // nil-safe, must not panic
}

func TestDisconnectClosePeerGoneClearsStale(t *testing.T) {
	resetTradeState()
	s := newFakeStore()
	a := &fakeConn{instance: "a1", username: "alice"}
	bus := newFakeBus()
	d := testDeps(s, bus, newFakePeers(a)) // bob is not connected
	stateFor("alice").Trades["b9"] = &Trade{other: "bob", offers: map[int]*OfferedItem{}}

	DisconnectClose(a, d) // nobody to notify: no frames, stale entry dropped
	for inst, frames := range bus.sent {
		if len(frames) != 0 {
			t.Fatalf("gone-peer disconnect sent %d frames to %s", len(frames), inst)
		}
	}
	if got := len(stateFor("alice").Trades); got != 0 {
		t.Fatalf("stale session not cleared (%d entries left)", got)
	}
}
