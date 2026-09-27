package controller

// Regression tests for the controller bug-fix batch: each test fails on
// the pre-fix code and passes after. Findings verified against TS but
// intentionally left alone are documented in doc/skipped-findings.md (see
// the orchestrator report) — only behavior-frozen bug fixes land here.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// Trade: validate-before-remove (no partial destruction on flagged exchange).
// ---------------------------------------------------------------------------

func openSession(t *testing.T, s *fakeStore, bus *fakeBus, a, b *fakeConn) Deps {
	t.Helper()
	d := testDeps(s, bus, newFakePeers(a, b))
	HandleTrade(a, []byte(`{"opcode":0,"instance":"`+b.instance+`"}`), d)
	HandleTrade(b, []byte(`{"opcode":0,"instance":"`+a.instance+`"}`), d)
	if tr, _ := pair(a, d); tr == nil {
		t.Fatal("session never opened")
	}
	return d
}

func TestTradeExchangeShortfallLosesNothing(t *testing.T) {
	resetTradeState()
	s := newFakeStore()
	s.attrs["logs"] = fakeAttr{name: "Logs", typ: "material", maxStack: 10}
	s.attrs["sword"] = fakeAttr{name: "Sword", typ: "weapon"}
	s.inv["alice"] = []Slot{{Key: "logs", Count: 3}}
	s.inv["bob"] = []Slot{{Key: "sword", Count: 1}}
	a := &fakeConn{instance: "a1", username: "alice", x: 100, y: 96}
	b := &fakeConn{instance: "b1", username: "bob", x: 101, y: 96}
	bus := newFakeBus()
	d := openSession(t, s, bus, a, b)

	HandleTrade(a, []byte(`{"opcode":1,"index":0,"count":3}`), d)
	HandleTrade(b, []byte(`{"opcode":1,"index":0,"count":1}`), d)

	// Alice's stack shrinks below her offer before the double accept
	// (spent elsewhere). The exchange runs on Bob's accept with Bob's
	// offers removed first — the flagged abort must not destroy them.
	s.RemoveItem("alice", "logs", 2)

	HandleTrade(a, []byte(`{"opcode":3}`), d)
	HandleTrade(b, []byte(`{"opcode":3}`), d)

	if !bus.hasNotif("a1", "PLEASE_REPORT_BUG") || !bus.hasNotif("b1", "PLEASE_REPORT_BUG") {
		t.Fatalf("flagged exchange missing bug notifies: %v %v", bus.notifs["a1"], bus.notifs["b1"])
	}
	// Bob's offered sword must survive the aborted exchange.
	if got := s.CountItem("bob", "sword"); got != 1 {
		t.Fatalf("bob swords = %d, want 1 (partial-remove destroyed it)", got)
	}
	if got := s.CountItem("alice", "logs"); got != 1 {
		t.Fatalf("alice logs = %d, want 1 (untouched remainder)", got)
	}
	if tr, _ := pair(a, d); tr != nil {
		t.Fatal("session not closed after flagged exchange")
	}
}

// ---------------------------------------------------------------------------
// Trade: concurrent double accept single-flights the exchange.
// ---------------------------------------------------------------------------

// concStore serializes the fake seam for the concurrency test (the
// production adapter serializes via pstateMu; the plain fake would fault
// on concurrent map writes before the product lock is even reached).
type concStore struct {
	*fakeStore
	mu sync.Mutex
}

func (s *concStore) SlotAt(username string, index int) (Slot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.SlotAt(username, index)
}

func (s *concStore) InventoryLen(username string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.InventoryLen(username)
}

func (s *concStore) CountItem(username, itemKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.CountItem(username, itemKey)
}

func (s *concStore) AddItem(username, itemKey string, count int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.AddItem(username, itemKey, count)
}

func (s *concStore) AddItemEnch(username, itemKey string, count int, ench protocol.Enchantments) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeStore.AddItemEnch(username, itemKey, count, ench)
}

func (s *concStore) RemoveItem(username, itemKey string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fakeStore.RemoveItem(username, itemKey, count)
}

type concBus struct {
	*fakeBus
	mu sync.Mutex
}

func (b *concBus) SendTo(instance string, frames ...[]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fakeBus.SendTo(instance, frames...)
}

func (b *concBus) Notify(instance string, message string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fakeBus.Notify(instance, message)
}

func (b *concBus) Broadcast(frames ...[]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fakeBus.Broadcast(frames...)
}

func TestTradeAcceptConcurrentSingleFlight(t *testing.T) {
	for round := 0; round < 5; round++ {
		resetTradeState()
		inner := newFakeStore()
		inner.attrs["logs"] = fakeAttr{name: "Logs", typ: "material", maxStack: 10}
		inner.attrs["sword"] = fakeAttr{name: "Sword", typ: "weapon"}
		inner.inv["alice"] = []Slot{{Key: "logs", Count: 3}}
		inner.inv["bob"] = []Slot{{Key: "sword", Count: 1}}
		s := &concStore{fakeStore: inner}
		a := &fakeConn{instance: "a1", username: "alice", x: 100, y: 96}
		b := &fakeConn{instance: "b1", username: "bob", x: 101, y: 96}
		bus := &concBus{fakeBus: newFakeBus()}
		d := testDeps(s, bus, newFakePeers(a, b))
		HandleTrade(a, []byte(`{"opcode":0,"instance":"b1"}`), d)
		HandleTrade(b, []byte(`{"opcode":0,"instance":"a1"}`), d)

		HandleTrade(a, []byte(`{"opcode":1,"index":0,"count":3}`), d)
		HandleTrade(b, []byte(`{"opcode":1,"index":0,"count":1}`), d)

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(2)
			go func() { defer wg.Done(); tradeAccept(d, a) }()
			go func() { defer wg.Done(); tradeAccept(d, b) }()
		}
		wg.Wait()

		if got := inner.CountItem("alice", "sword"); got != 1 {
			t.Fatalf("round %d: alice swords = %d, want 1 (exchange ran twice?)", round, got)
		}
		if got := inner.CountItem("bob", "logs"); got != 3 {
			t.Fatalf("round %d: bob logs = %d, want 3", round, got)
		}
		if got := inner.CountItem("alice", "logs"); got != 0 {
			t.Fatalf("round %d: alice logs = %d, want 0", round, got)
		}
		completes := 0
		for _, m := range append(bus.notifs["a1"], bus.notifs["b1"]...) {
			if len(m) >= len("misc:TRADE_COMPLETE") && m[:len("misc:TRADE_COMPLETE")] == "misc:TRADE_COMPLETE" {
				completes++
			}
		}
		if completes != 2 {
			t.Fatalf("round %d: TRADE_COMPLETE notifies = %d, want 2 (single exchange)", round, completes)
		}
	}
}

// ---------------------------------------------------------------------------
// Trade: stacked add onto a slot that changed keys re-offers fresh.
// ---------------------------------------------------------------------------

func TestTradeAddSlotKeyChangeReoffers(t *testing.T) {
	resetTradeState()
	s := newFakeStore()
	s.attrs["logs"] = fakeAttr{name: "Logs", typ: "material", maxStack: 10}
	s.attrs["sword"] = fakeAttr{name: "Sword", typ: "weapon"}
	s.inv["alice"] = []Slot{{Key: "logs", Count: 5}}
	s.inv["bob"] = []Slot{{Key: "logs", Count: 1}}
	a := &fakeConn{instance: "a1", username: "alice", x: 100, y: 96}
	b := &fakeConn{instance: "b1", username: "bob", x: 101, y: 96}
	bus := newFakeBus()
	d := openSession(t, s, bus, a, b)

	HandleTrade(a, []byte(`{"opcode":1,"index":0,"count":2}`), d)
	// Slot 0 now holds a different item (used + replaced elsewhere).
	s.inv["alice"] = []Slot{{Key: "sword", Count: 1}}
	HandleTrade(a, []byte(`{"opcode":1,"index":0,"count":1}`), d)

	tr, _ := pair(a, d)
	if tr == nil || tr.offers[0] == nil {
		t.Fatalf("offer missing: %+v", tr)
	}
	if tr.offers[0].Key != "sword" || tr.offers[0].Count != 1 || tr.offers[0].MaxCount != 1 {
		t.Fatalf("stale stacked offer: %+v (want fresh sword x1)", tr.offers[0])
	}
}

// ---------------------------------------------------------------------------
// Craft: clamp takes the minimum across requirements.
// ---------------------------------------------------------------------------

func TestClampCraftCountTakesMinimum(t *testing.T) {
	reqs := []CraftRequirement{{Key: "a", Count: 2}, {Key: "b", Count: 1}}
	have := func(k string) int { return map[string]int{"a": 2, "b": 5}[k] }
	// a allows 2/2 = 1; b allows 5/1 = 5. Min = 1 (overwrite would say 5,
	// crafting more than the a-supply covers).
	if got := ClampCraftCount(10, reqs, have); got != 1 {
		t.Fatalf("clamp = %d, want 1 (minimum)", got)
	}
}

// ---------------------------------------------------------------------------
// Enchant: failed chance consumes the shard and stops (no apply, no
// SUCCESS); tier clamps so shardt0 never panics; post-removal slot shift
// re-targets the item.
// ---------------------------------------------------------------------------

func enchDeps(s *fakeStore, bus *fakeBus, c *fakeConn) Deps {
	return testDeps(s, bus, newFakePeers(c))
}

func seedEnchant(s *fakeStore, itemKey, shardKey string) {
	s.inv["u"] = []Slot{{Key: shardKey, Count: 1}, {Key: itemKey, Count: 1}}
}

func TestEnchantFailedChanceStops(t *testing.T) {
	succeeded, failed := false, false
	for i := 0; i < 200 && !(succeeded && failed); i++ {
		s := newFakeStore()
		s.attrs["steelsword"] = fakeAttr{name: "Steel sword", typ: "weapon"}
		seedEnchant(s, "steelsword", "shardt5")
		c := &fakeConn{instance: "i1", username: "u"}
		bus := newFakeBus()
		d := enchDeps(s, bus, c)
		enchantConfirm(d, c, 1, 0)
		slot, _ := s.SlotAt("u", 0)
		if bus.hasNotif("i1", "enchant:SUCCESSFUL_ENCHANT") {
			succeeded = true
			if len(slot.Ench) == 0 {
				t.Fatalf("iter %d: SUCCESS without enchantment applied (slot shift lost it)", i)
			}
			if slot.Key != "steelsword" {
				t.Fatalf("iter %d: enchant landed on %q, want steelsword", i, slot.Key)
			}
		} else if bus.hasNotif("i1", "enchant:FAILED_ENCHANT") {
			failed = true
			if len(slot.Ench) != 0 {
				t.Fatalf("iter %d: FAILED chance still applied an enchantment", i)
			}
			if bus.hasNotif("i1", "enchant:SUCCESSFUL_ENCHANT") {
				t.Fatalf("iter %d: FAILED chance also notified SUCCESS", i)
			}
			if got := s.CountItem("u", "shardt5"); got != 0 {
				t.Fatalf("iter %d: failed enchant did not consume the shard", i)
			}
		} else {
			t.Fatalf("iter %d: neither SUCCESS nor FAILED notified", i)
		}
	}
	if !succeeded || !failed {
		t.Fatalf("did not observe both outcomes in 200 rolls (succeeded=%v failed=%v)", succeeded, failed)
	}
}

func TestEnchantTierZeroClampsNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("enchantConfirm panicked on shardt0: %v", r)
		}
	}()
	s := newFakeStore()
	s.attrs["steelsword"] = fakeAttr{name: "Steel sword", typ: "weapon"}
	seedEnchant(s, "steelsword", "shardt0")
	c := &fakeConn{instance: "i1", username: "u"}
	bus := newFakeBus()
	enchantConfirm(enchDeps(s, bus, c), c, 1, 0)
	if got := s.CountItem("u", "shardt0"); got != 0 {
		t.Fatalf("shardt0 not consumed: %d left", got)
	}
}

// ---------------------------------------------------------------------------
// Bank: full-bank deposit keeps the inventory; enchants survive both moves
// and the batch; failed withdraw keeps the bank stack.
// ---------------------------------------------------------------------------

type failAddStore struct {
	*fakeStore
}

func (s *failAddStore) AddItem(username, itemKey string, count int) int { return -1 }
func (s *failAddStore) AddItemEnch(username, itemKey string, count int, ench protocol.Enchantments) int {
	return -1
}

func bankDeps(s EconomyStore, bus *fakeBus, c *fakeConn) EconomyDeps {
	c.SetCanAccess(true) // banker-granted container access (Talk parity)
	return EconomyDeps{
		Store: s, Bus: bus, Peers: econPeers{newFakePeers(c)},
		Quests: nil, Pets: nil, World: econWorld{}, Vitals: nil,
	}
}

func bankSelectMsg(from, to, fromIndex int) *protocol.ClientContainer {
	return &protocol.ClientContainer{
		Type:          &[]int{protocol.ContainerTypeBank}[0],
		FromContainer: &from, ToContainer: &to, FromIndex: &fromIndex,
	}
}

func TestBankDepositFullKeepsInventory(t *testing.T) {
	s := newFakeStore()
	c := &fakeConn{instance: "i1", username: "u"}
	// Fill the bank to capacity with unmergeable singletons.
	for i := 0; i < protocol.ModulesBankSize; i++ {
		s.bank["u"] = append(s.bank["u"], Slot{Key: fmt.Sprintf("junk%d", i), Count: 1})
	}
	s.inv["u"] = []Slot{{Key: "logs", Count: 3}, {Key: "sword", Count: 1}}
	bus := newFakeBus()
	d := bankDeps(s, bus, c)

	HandleContainerSelect(c, bankSelectMsg(protocol.ContainerTypeInventory, protocol.ContainerTypeBank, 0), d)

	if !hasNotifText(bus, "i1", "Bank is full.") {
		t.Fatalf("missing Bank-full notify: %v", bus.sent["i1"])
	}
	got := s.InventorySlots("u")
	if len(got) != 2 || got[0].Key != "logs" || got[0].Count != 3 || got[1].Key != "sword" {
		t.Fatalf("inventory disturbed by refused deposit: %+v", got)
	}
	for _, f := range bus.sent["i1"] {
		if f.id == protocol.PacketContainer && f.opcode == protocol.ContainerRemove {
			t.Fatal("deposit emitted Container Remove despite the full bank")
		}
	}
}

func TestBankDepositWithdrawPreserveEnchant(t *testing.T) {
	s := newFakeStore()
	c := &fakeConn{instance: "i1", username: "u"}
	ench := protocol.Enchantments{0: protocol.Enchantment{Level: 2}}
	s.inv["u"] = []Slot{{Key: "steelsword", Count: 1, Ench: ench}}
	bus := newFakeBus()
	d := bankDeps(s, bus, c)

	HandleContainerSelect(c, bankSelectMsg(protocol.ContainerTypeInventory, protocol.ContainerTypeBank, 0), d)

	bslots := s.BankSlots("u")
	if len(bslots) != 1 || len(bslots[0].Ench) == 0 {
		t.Fatalf("bank slot lost enchantments: %+v", bslots)
	}
	batch := BankBatch(d, "u")
	found := false
	for _, raw := range batch.Data.Slots {
		if m, ok := raw.(map[string]any); ok {
			if m["key"] == "steelsword" {
				if em, ok := m["enchantments"].(map[string]any); ok && len(em) > 0 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("BankBatch dropped enchantments: %+v", batch.Data.Slots)
	}

	// Withdraw it back: inventory side must carry the enchantments too.
	bus2 := newFakeBus()
	d2 := bankDeps(s, bus2, c)
	HandleContainerSelect(c, bankSelectMsg(protocol.ContainerTypeBank, protocol.ContainerTypeInventory, 0), d2)
	slots := s.InventorySlots("u")
	if len(slots) != 1 || len(slots[0].Ench) == 0 {
		t.Fatalf("withdraw lost enchantments: %+v", slots)
	}
}

func TestBankWithdrawFailedAddKeepsBank(t *testing.T) {
	inner := newFakeStore()
	inner.bank["u"] = []Slot{{Key: "logs", Count: 3}}
	inner.inv["u"] = []Slot{}
	s := &failAddStore{fakeStore: inner}
	c := &fakeConn{instance: "i1", username: "u"}
	bus := newFakeBus()
	d := bankDeps(s, bus, c)

	HandleContainerSelect(c, bankSelectMsg(protocol.ContainerTypeBank, protocol.ContainerTypeInventory, 0), d)

	if got := len(inner.bank["u"]); got != 1 {
		t.Fatalf("bank slots = %d, want 1 (failed add destroyed the stack)", got)
	}
	if !hasNotifText(bus, "i1", "Your inventory is full.") {
		t.Fatal("missing inventory-full notify on failed withdraw")
	}
}

// ---------------------------------------------------------------------------
// Equipment: full inventory keeps the equipment on; enchants survive.
// ---------------------------------------------------------------------------

func TestUnequipFullInventoryKeepsEquipment(t *testing.T) {
	s := newFakeStore()
	c := &fakeConn{instance: "i1", username: "u", x: 1, y: 1}
	for i := 0; i < protocol.ModulesInventorySize; i++ {
		s.inv["u"] = append(s.inv["u"], Slot{Key: fmt.Sprintf("junk%d", i), Count: 1})
	}
	s.SetEquip("u", protocol.EquipmentWeapon, Slot{Key: "steelsword", Count: 1})
	bus := newFakeBus()
	d := bankDeps(s, bus, c)

	UnequipType(c, d, protocol.EquipmentWeapon)

	if e, _ := s.EquipSlot("u", protocol.EquipmentWeapon); e.Key == "" {
		t.Fatal("equipment cleared despite a full inventory")
	}
	if len(s.inv["u"]) != protocol.ModulesInventorySize {
		t.Fatalf("inventory slots = %d, want %d", len(s.inv["u"]), protocol.ModulesInventorySize)
	}
	if bus.hasOpcode("i1", protocol.PacketEquipment, protocol.EquipmentUnequip) {
		t.Fatal("unequip frame emitted despite a full inventory")
	}
}

func TestUnequipPreservesEnchant(t *testing.T) {
	s := newFakeStore()
	c := &fakeConn{instance: "i1", username: "u", x: 1, y: 1}
	ench := protocol.Enchantments{0: protocol.Enchantment{Level: 3}}
	s.SetEquip("u", protocol.EquipmentWeapon, Slot{Key: "steelsword", Count: 1, Ench: ench})
	bus := newFakeBus()
	d := bankDeps(s, bus, c)

	UnequipType(c, d, protocol.EquipmentWeapon)

	slots := s.InventorySlots("u")
	if len(slots) != 1 || len(slots[0].Ench) == 0 {
		t.Fatalf("unequip dropped enchantments: %+v", slots)
	}
}

// ---------------------------------------------------------------------------
// Stores: slot-0 buys succeed + charge/decrement by count; sold-out
// non-original entries vanish, originals linger; swap resyncs via Batch.
// ---------------------------------------------------------------------------

func writeTestStores(t *testing.T, doc string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "stores.json")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RES_stores", p)
}

const testStoresDoc = `{
	"testshop": {
		"items": [
			{"key": "bronzesword", "count": 5, "price": 10, "stockAmount": 1},
			{"key": "oldonesblade", "count": 1, "price": 50, "stockAmount": 1}
		],
		"refresh": 20000,
		"currency": "gold",
		"restricted": false
	}
}`

func openTestStore(c *fakeConn, key string) {
	c.storeOpen = key
}

func TestBuySlotZeroChargesAndDecrements(t *testing.T) {
	writeTestStores(t, testStoresDoc)
	s := newFakeStore()
	c := &fakeConn{instance: "i1", username: "u"}
	openTestStore(c, "testshop")
	// Empty inventory: the bought stack lands at slot 0 (the old
	// `amount < 1` check mistook that index for a failed add and let
	// the item go free with no charge and no stock decrement).
	s.inv["u"] = []Slot{{Key: "gold", Count: 100}}
	bus := newFakeBus()
	d := bankDeps(s, bus, c)

	Buy(c, d, "testshop", 0, 2)

	if got := s.CountItem("u", "gold"); got != 80 {
		t.Fatalf("gold = %d, want 80 (2x10 charged)", got)
	}
	if got := s.CountItem("u", "bronzesword"); got != 2 {
		t.Fatalf("swords = %d, want 2", got)
	}
	st := StoreFor("testshop")
	if st.Items[0].Count != 3 {
		t.Fatalf("stock = %d, want 3 (5-2 by count, not by slot index)", st.Items[0].Count)
	}
}

func TestBuySoldOutNonOriginalRemoved(t *testing.T) {
	writeTestStores(t, testStoresDoc)
	s := newFakeStore()
	c := &fakeConn{instance: "i1", username: "u"}
	openTestStore(c, "testshop")
	s.inv["u"] = []Slot{{Key: "gold", Count: 1000}}
	bus := newFakeBus()
	d := bankDeps(s, bus, c)

	// oldonesblade is original stock here: buying it out lingers at 0.
	Buy(c, d, "testshop", 1, 1)
	st := StoreFor("testshop")
	if idx := FindStoreItem(st, "oldonesblade"); idx < 0 || st.Items[idx].Count != 0 {
		t.Fatalf("original sold-out entry wrong: %+v", st.Items)
	}

	// A player-sold entry is not original: buying it out removes it.
	econStoresMu.Lock()
	st.Items = append(st.Items, &EconStoreItem{Key: "frogsleg", Name: "Frog leg", Count: 1, Price: 5, Stock: 1, MaxCnt: 1})
	econStoresMu.Unlock()
	s.inv["u"] = append(s.inv["u"], Slot{Key: "gold", Count: 1000})
	Buy(c, d, "testshop", len(StoreItems(st))-1, 1)
	if idx := FindStoreItem(st, "frogsleg"); idx >= 0 {
		t.Fatalf("sold-out non-original entry lingered: %+v", st.Items)
	}
}

func TestSwapEmitsBatch(t *testing.T) {
	s := newFakeStore()
	c := &fakeConn{instance: "i1", username: "u"}
	s.inv["u"] = []Slot{{Key: "a", Count: 1}, {Key: "b", Count: 2}}
	bus := newFakeBus()
	d := bankDeps(s, bus, c)

	HandleContainerSwap(c, d, 0, 1)

	if got := s.InventorySlots("u"); got[0].Key != "b" || got[1].Key != "a" {
		t.Fatalf("swap did not reorder: %+v", got)
	}
	if !bus.hasOpcode("i1", protocol.PacketContainer, protocol.ContainerBatch) {
		t.Fatal("swap emitted no Container Batch resync")
	}
}

// ---------------------------------------------------------------------------
// Drops: undroppable items are rejected with the TS-exact string.
// ---------------------------------------------------------------------------

type stubPets struct{}

func (stubPets) DropKey(c EconomyConn, index int) (string, string, bool) { return "", "", false }
func (stubPets) HasOwner(instance string) bool                           { return false }
func (stubPets) Grant(c EconomyConn, mob, item string)                   {}

func TestDropUndroppableRejected(t *testing.T) {
	s := newFakeStore()
	s.attrs["questitem"] = fakeAttr{name: "Quest item", typ: "material", undroppable: true}
	s.inv["u"] = []Slot{{Key: "questitem", Count: 1}}
	c := &fakeConn{instance: "i1", username: "u"}
	bus := newFakeBus()
	d := bankDeps(s, bus, c)
	d.Pets = stubPets{}

	v, idx := 1, 0
	typ := protocol.ContainerTypeInventory
	HandleContainer(c, []byte(`{"opcode":2,"type":`+itoa(typ)+`,"fromIndex":`+itoa(idx)+`,"value":`+itoa(v)+`}`), d)

	if got := s.CountItem("u", "questitem"); got != 1 {
		t.Fatalf("undroppable item dropped: %d left", got)
	}
	if !hasNotifText(bus, "i1", "You cannot drop this item.") {
		t.Fatal("missing undroppable drop notify")
	}
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// ---------------------------------------------------------------------------
// Healing: a full-HP use never banks free mana before refusing.
// ---------------------------------------------------------------------------

func TestHealingFullHPNoFreeMana(t *testing.T) {
	s, bus, v := newFakeStore(), newFakeBus(), newVitalsFake()
	v.hp = v.maxHP // full HP...
	v.mana = 10    // ...but hurt mana.
	c := &fakeConn{instance: "i1", username: "u1"}
	d := useDeps(s, bus, v, c)

	// Dual restorative: HP gate must fire before any mana is applied.
	if useHealingItem(c, d, &ItemInfo{HealAmount: 50, ManaAmount: 30}) {
		t.Fatal("dual item at full HP reported handled")
	}
	if len(v.heals) != 0 {
		t.Fatalf("free mana applied before the full-HP refusal: %v", v.heals)
	}
	if !hasNotifText(bus, "i1", "You are already at full health.") {
		t.Fatal("missing full-health notify")
	}

	// Pure-mana items still heal at full HP (no heal component to gate).
	bus2 := newFakeBus()
	d2 := useDeps(s, bus2, v, c)
	if !useHealingItem(c, d2, &ItemInfo{ManaAmount: 30}) {
		t.Fatal("pure-mana item at full HP refused")
	}
	if len(v.heals) != 1 || v.heals[0] != [2]int{0, 30} {
		t.Fatalf("mana heals = %v, want [{0 30}]", v.heals)
	}
}

// ---------------------------------------------------------------------------
// Setrank: unknown ranks never persist offline (zero value).
// ---------------------------------------------------------------------------

func TestSetrankOfflineInvalidRank(t *testing.T) {
	admin := newCmdConn("a1", "admin", CmdRankAdmin)
	bus := newCmdBus()
	d, p := progTestDeps(bus, newCmdPeers(admin), newCmdFlags())

	AdminProgressionCommands(admin, "setrank", []string{"Nope", "ghost"}, d)

	if !bus.hasNotif("a1", "Invalid rank: Nope") {
		t.Fatalf("missing invalid-rank notify: %v", bus.notifs["a1"])
	}
	if len(p.ranks.offline) != 0 {
		t.Fatalf("invalid rank persisted offline: %v", p.ranks.offline)
	}
}

// ---------------------------------------------------------------------------
// Warps: nil seams never panic.
// ---------------------------------------------------------------------------

func TestDoWarpNilDepsNoPanic(t *testing.T) {
	old := warpDeps
	warpDeps = WarpDeps{}
	defer func() { warpDeps = old }()

	gated := &WarpEntry{Name: "aynor", X: 1, Y: 1, W: 2, H: 2, Level: 5}
	if DoWarp(WarpConn{Instance: "i1", Username: "u"}, gated) {
		t.Fatal("gated warp allowed with nil seams")
	}
	open := &WarpEntry{Name: "mudwich", X: 10, Y: 10, W: 3, H: 3}
	if !DoWarp(WarpConn{Instance: "i1", Username: "u2"}, open) {
		t.Fatal("open warp denied with nil seams")
	}
}
