// Package controller owns the M6 slice — stores, bank, NPC talk, containers
// and equipment (behavior-frozen move of the root m6.go economy core).
//
// Ports faithfully:
//   - stores.ts: stores.json registry keyed per store with ORDER-PRESERVING
//     item lists, Buy/Sell/Select with the exact Node check order, 20s refresh
//     ticker that restocks finite entries by stockAmount and pushes Store
//     Update to players with that store open.
//   - handler.ts handleTalkToNPC: Target Talk(0) on an NPC keys npcs.json —
//     npc.store -> Store Open + player.storeOpen, role banker ->
//     canAccessContainer + Container Batch (serialized bank slots), else the
//     npc.talk() bubble text advancing per-player talkIndex per NPC key.
//   - bank via Container Select moves between Bank(0)/Inventory(1) gated on
//     canAccessContainer (cleared on movement), persisted in SQLite and
//     restored on login as a Container Batch.
//   - equipment (Modules.Equipment 12 slots): Container Select Inventory equip
//     trigger, Equipment Unequip, stat gates, Equip echo + Sync.
//
// Transport and shared player state stay with the root server: this package
// touches them only through the Conn/Store/Bus/Peers seams (trade.go) plus
// the Economy extensions below, which the root adapter (m6.go) implements
// over its globals. Packet shapes are unchanged — frames are built with
// internal/protocol, the same constructors the root uses.
package controller

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// Economy seams (implemented by the root adapter; never by this package).
// Reuses trade.go Conn/Store/Bus/Peers where they fit, extends where needed.
// ---------------------------------------------------------------------------

// EconomyConn is the per-connection view the economy logic needs. It embeds
// Conn so the same pointer flows through and identity keeps working.
type EconomyConn interface {
	Conn
	// StoreOpen is the currently open store key ("" = none).
	StoreOpen() string
	SetStoreOpen(string)
	// CanAccess reports banker-granted bank access.
	CanAccess() bool
	SetCanAccess(bool)
	// TalkKey/TalkIndex is the plain-NPC talk bubble state.
	TalkKey() string
	SetTalkKey(string)
	TalkIndex() int
	SetTalkIndex(int)
}

// EconomyStore extends Store with the bank/equipment/total-level reads the
// economy needs. Inventory catalog reads reuse Store.ItemName/ItemType/
// MaxStack/ItemUndroppable; the richer item fields (price/skill/level/
// poisonous) live in this package's ItemInfo registry (ItemCatalog parity).
type EconomyStore interface {
	Store
	// InventorySlots snapshots the inventory (single-lock read parity).
	InventorySlots(username string) []Slot
	// SetInventory replaces the inventory (single-lock write parity).
	SetInventory(username string, slots []Slot)
	// BankSlots snapshots the bank.
	BankSlots(username string) []Slot
	// SetBank replaces the bank.
	SetBank(username string, slots []Slot)
	// EquipSlot reads one equipment slot.
	EquipSlot(username string, slotType int) (Slot, bool)
	// SetEquip writes one equipment slot.
	SetEquip(username string, slotType int, slot Slot)
	// EquipLen reports the equipment array length (12-slot normalize check).
	EquipLen(username string) int
	// TotalLevel reports the stored total level (st.Level).
	TotalLevel(username string) int
}

// EconomyBus extends Bus with global fan-out (Sync broadcasts).
type EconomyBus interface {
	Bus
	Broadcast(frames ...[]any)
}

// EconomyPeers extends Peers with the store-open scan (updatePlayers parity).
type EconomyPeers interface {
	Peers
	WithStoreOpen(key string) []EconomyConn
}

// ItemCatalog documents the item-catalogue seam: the rich fields live in
// this package's registry (LoadItems/ItemInfoFor); the Store seam exposes
// the subset trade needs (ItemName/ItemType/MaxStack/ItemUndroppable).
// Root adapters delegate those to ItemName/MaxStack/etc below.
type ItemCatalog interface {
	Price(key string) int
	Name(key string) string
	Stackable(key string) bool
}

// QuestTalk abstracts m11Talk (quest/achievement NPC hook). Returns true
// when the interaction is consumed.
type QuestTalk interface {
	Talk(c EconomyConn, npcKey string) bool
}

// PetHooks abstracts pets_wire (drop-spawn companion flow).
type PetHooks interface {
	DropKey(c EconomyConn, index int) (mob, item string, ok bool)
	HasOwner(instance string) bool
	Grant(c EconomyConn, mob, item string)
}

// WorldLookup abstracts entity/Showcase/Sync reads.
type WorldLookup interface {
	EntityPos(instance string) (x, y int, ok bool)
	SpawnNPCKey(instance string) (key string, ok bool)
	ShowcaseKey(n int) (key string, ok bool)
	ShowcaseCount() int
	SyncFrame(instance string, x, y int) []any
}

// DropHooks abstracts world-item spawning for player inventory drops
// (container.ts drop parity: removed item -> world Item entity).
type DropHooks struct {
	// SpawnItem creates a world item entity at (x,y) for the dropped item.
	// Returns the spawned entity instance ("" on failure).
	SpawnItem func(owner, key string, count, x, y int) string
}

// EconomyDeps bundles the economy seams for one call.
type EconomyDeps struct {
	Store  EconomyStore
	Bus    EconomyBus
	Peers  EconomyPeers
	Quests QuestTalk
	Pets   PetHooks
	World  WorldLookup
	// Vitals carries live hero state for the item-use plugins (items.go).
	// Nil = plugins report unhandled (no consume); the live adapter always
	// sets it (m6deps).
	Vitals Vitals
	// Drops spawns world-item entities on player inventory drops.
	// Nil = no world entity (item is just removed from inventory).
	Drops DropHooks
}

// depsForEnchant converts economy deps to trade Deps for the shared
// OpenEnchanter call (same Store/Bus/Peers values satisfy both).
func (d EconomyDeps) tradeDeps() Deps {
	return Deps{Store: d.Store, Bus: d.Bus, Peers: d.Peers}
}

// ---------------------------------------------------------------------------
// Data path (resourceDataPath parity for items/stores/npcs.json).
// ---------------------------------------------------------------------------

func economyDataPath(name string) string {
	if p := os.Getenv("RES_" + name); p != "" {
		return p
	}
	rel := filepath.Join("..", "packages", "server", "data", name+".json")
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	if alt := filepath.Join("..", "..", "packages", "server", "data", name+".json"); true {
		if _, err := os.Stat(alt); err == nil {
			return alt
		}
	}
	return "/Users/appfuxion/repo/rpg-world-sim/packages/server/data/" + name + ".json"
}

// ---------------------------------------------------------------------------
// Item info (name/price/stackable/maxStackSize from items.json).
// ---------------------------------------------------------------------------

// ItemBonuses mirrors items.json `bonuses` (accuracy/strength/archery/magic).
type ItemBonuses struct {
	Accuracy int
	Strength int
	Archery  int
	Magic    int
}

// ItemAttackStats mirrors items.json `attackStats` (crush/slash/stab/archery/magic).
type ItemAttackStats struct {
	Crush   int
	Slash   int
	Stab    int
	Archery int
	Magic   int
}

// ItemDefenseStats mirrors items.json `defenseStats`.
type ItemDefenseStats struct {
	Crush   int
	Slash   int
	Stab    int
	Archery int
	Magic   int
}

// ItemInfo mirrors m6ItemInfo: items.json catalogue entry.
type ItemInfo struct {
	Name         string
	Description  string // items.json `description` (examine parity)
	Price        int
	Stackable    bool
	MaxStackSize int
	Type         string
	Skill        string
	Level        int
	Poisonous    bool
	Undroppable  bool
	// Item-use plugin fields (player.ts handleContainerSelect + slot.ts
	// edible/interactable plumbing + data/plugins/items/*.ts).
	Edible       bool
	Interactable bool
	Plugin       string
	HealAmount   int
	HealPercent  float64 // items.json percent (healingitem divides by 100)
	ManaAmount   int
	Effect       string // effectpotion `effect` (accuracy/strength/...)
	Duration     int64  // effectpotion `duration` ms (default 60_000)
	SmallBowl    bool
	MediumBowl   bool
	WeaponType   string // item.ts weaponType (sword/bow/staff/...) for attack styles
	AttackRange  int    // item.ts attackRange (0 = engine default)
	ManaCost     int    // item.ts manaCost (staff swings; 0 = free)
	Freezing     bool   // item.ts freezing (ice arrows)
	Burning      bool   // item.ts burning (fire arrows)
	// Combat stats (formulas.ts getMaxDamage / getDamageReduction parity).
	TwoHanded    bool             // weapon.ts isTwoHanded (bows only in items.json)
	Bonuses      ItemBonuses      // equipment totalBonuses per-item contribution
	AttackStats  ItemAttackStats  // character attack stats from weapon
	DefenseStats ItemDefenseStats // character defense stats from armor/weapon
	// Gathering tool tiers (weapon.ts lumberjacking/mining/fishing).
	// Non-zero means the item can be used for that gathering skill at the
	// given tier level; zero means the item is not a tool for that skill.
	Lumberjacking int // items.json `lumberjacking`
	Mining        int // items.json `mining`
	Fishing       int // items.json `fishing`
	// MovementModifier is the equipment movement speed multiplier
	// (items.json `movementModifier`; swiftboots=0.95). Zero means no
	// modifier; the speed pipeline treats 1.0 and 0 identically (skip).
	MovementModifier float64 `json:"movementModifier"`
}

var (
	econItemsOnce sync.Once
	econItems     = map[string]*ItemInfo{}
	econItemsErr  error
)

// LoadItems loads items.json once (m6LoadItems parity).
func LoadItems() error {
	econItemsOnce.Do(func() {
		raw, err := os.ReadFile(economyDataPath("items"))
		if err != nil {
			econItemsErr = err
			return
		}
		var items map[string]struct {
			Name         string  `json:"name"`
			Description  string  `json:"description"`
			Price        int     `json:"price"`
			Stackable    bool    `json:"stackable"`
			MaxStackSize int     `json:"maxStackSize"`
			Type         string  `json:"type"`
			Skill        string  `json:"skill"`
			Level        int     `json:"level"`
			Poisonous    bool    `json:"poisonous"`
			Undroppable  bool    `json:"undroppable"`
			Edible       bool    `json:"edible"`
			Interactable bool    `json:"interactable"`
			Plugin       string  `json:"plugin"`
			HealAmount   int     `json:"healAmount"`
			HealPercent  float64 `json:"healPercent"`
			ManaAmount   int     `json:"manaAmount"`
			Effect       string  `json:"effect"`
			Duration     int64   `json:"duration"`
			SmallBowl    bool    `json:"smallBowl"`
			MediumBowl   bool    `json:"mediumBowl"`
			WeaponType   string  `json:"weaponType"`
			AttackRange  int     `json:"attackRange"`
			ManaCost     int     `json:"manaCost"`
			Freezing     bool    `json:"freezing"`
			Burning      bool    `json:"burning"`
			TwoHanded    bool    `json:"twoHanded"`
			Lumberjacking int    `json:"lumberjacking"`
			Mining        int    `json:"mining"`
			Fishing       int    `json:"fishing"`
			MovementModifier float64 `json:"movementModifier"`
			Bonuses      struct {
				Accuracy int `json:"accuracy"`
				Strength int `json:"strength"`
				Archery  int `json:"archery"`
				Magic    int `json:"magic"`
			} `json:"bonuses"`
			AttackStats struct {
				Crush   int `json:"crush"`
				Slash   int `json:"slash"`
				Stab    int `json:"stab"`
				Archery int `json:"archery"`
				Magic   int `json:"magic"`
			} `json:"attackStats"`
			DefenseStats struct {
				Crush   int `json:"crush"`
				Slash   int `json:"slash"`
				Stab    int `json:"stab"`
				Archery int `json:"archery"`
				Magic   int `json:"magic"`
			} `json:"defenseStats"`
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			econItemsErr = err
			return
		}
		for k, v := range items {
			max := v.MaxStackSize
			if max <= 0 {
				max = protocol.ModulesMaxStack
			}
			econItems[k] = &ItemInfo{Name: v.Name, Description: v.Description, Price: v.Price, Stackable: v.Stackable, MaxStackSize: max,
				Type: v.Type, Skill: v.Skill, Level: v.Level, Poisonous: v.Poisonous, Undroppable: v.Undroppable,
				Edible: v.Edible, Interactable: v.Interactable, Plugin: v.Plugin,
				HealAmount: v.HealAmount, HealPercent: v.HealPercent, ManaAmount: v.ManaAmount,
				Effect: v.Effect, Duration: v.Duration, SmallBowl: v.SmallBowl, MediumBowl: v.MediumBowl,
				WeaponType: v.WeaponType, AttackRange: v.AttackRange,
				ManaCost: v.ManaCost, Freezing: v.Freezing, Burning: v.Burning,
				TwoHanded: v.TwoHanded,
				Lumberjacking: v.Lumberjacking, Mining: v.Mining, Fishing: v.Fishing,
				MovementModifier: v.MovementModifier,
				Bonuses:      ItemBonuses{Accuracy: v.Bonuses.Accuracy, Strength: v.Bonuses.Strength, Archery: v.Bonuses.Archery, Magic: v.Bonuses.Magic},
				AttackStats:  ItemAttackStats{Crush: v.AttackStats.Crush, Slash: v.AttackStats.Slash, Stab: v.AttackStats.Stab, Archery: v.AttackStats.Archery, Magic: v.AttackStats.Magic},
				DefenseStats: ItemDefenseStats{Crush: v.DefenseStats.Crush, Slash: v.DefenseStats.Slash, Stab: v.DefenseStats.Stab, Archery: v.DefenseStats.Archery, Magic: v.DefenseStats.Magic}}
		}
		log.Printf("m6: items=%d", len(econItems))
	})
	return econItemsErr
}

// ItemInfoFor returns the catalogue entry for key (nil when unknown).
func ItemInfoFor(key string) *ItemInfo {
	if err := LoadItems(); err != nil {
		return nil
	}
	return econItems[key]
}

// ItemName returns the display name (key fallback).
func ItemName(key string) string {
	if it := ItemInfoFor(key); it != nil && it.Name != "" {
		return it.Name
	}
	return key
}

// MaxStack mirrors Container maxStackSize logic.
func MaxStack(key string) int {
	it := ItemInfoFor(key)
	if it == nil || !it.Stackable {
		return 1
	}
	return it.MaxStackSize
}

// ItemPrice returns the catalogue price (0 when unknown).
func ItemPrice(key string) int {
	if it := ItemInfoFor(key); it != nil {
		return it.Price
	}
	return 0
}

// ToolTier returns the gathering tool tier for the given skill from an item.
// Returns 0 if the item is unknown or not a tool for that skill.
// Skill must be "lumberjacking", "mining", or "fishing" (not "foraging").
func ToolTier(itemKey, skill string) int {
	it := ItemInfoFor(itemKey)
	if it == nil {
		return 0
	}
	switch skill {
	case "lumberjacking":
		return it.Lumberjacking
	case "mining":
		return it.Mining
	case "fishing":
		return it.Fishing
	}
	return 0
}

// ItemKeys returns the sorted catalogue keys (m13TestItems parity helper).
func ItemKeys() []string {
	if err := LoadItems(); err != nil {
		return nil
	}
	keys := make([]string, 0, len(econItems))
	for k := range econItems {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// Store registry (stores.ts load/serialize + stores.json).
// ---------------------------------------------------------------------------

// EconStoreItem is one ordered store list entry.
type EconStoreItem struct {
	Key    string
	Name   string
	Count  int
	Price  int
	Stock  int
	MaxCnt int
}

// EconStore is one stores.json store entry.
type EconStore struct {
	Key          string
	Currency     string
	Restricted   bool
	AllowedItems []string
	Refresh      time.Duration
	LastUpdate   time.Time
	Items        []*EconStoreItem
	// Original marks the keys shipped in stores.json (sold-out
	// player-sold entries vanish; originals linger at 0 as out of
	// stock — stores.ts isOriginalItem parity, note the inverted
	// name there: true means NOT in the original stock).
	Original map[string]bool
}

func (s *EconStore) itemAllowed(key string) bool {
	for _, k := range s.AllowedItems {
		if k == key {
			return true
		}
	}
	return false
}

var (
	econStoresOnce sync.Once
	econStores     = map[string]*EconStore{}
	econStoresErr  error
	// econStoresMu guards the live store lists: the refresh ticker
	// mutates counts off-request while Buy/Sell mutate + read them.
	// The registry map itself is stable after load (sync.Once), so
	// StoreFor's map read stays lock-free; everything touching Items
	// or LastUpdate goes through this mutex. Lock order is always
	// econStoresMu -> Store seam (pstateMu) — the seam never calls
	// back into the registry, so no inversion is possible.
	econStoresMu sync.RWMutex
)

// LoadStores loads stores.json once (m6LoadStores parity).
func LoadStores() error {
	econStoresOnce.Do(func() {
		if err := LoadItems(); err != nil {
			econStoresErr = err
			return
		}
		raw, err := os.ReadFile(economyDataPath("stores"))
		if err != nil {
			econStoresErr = err
			return
		}
		var rawStores map[string]struct {
			Items []struct {
				Key         string `json:"key"`
				Count       int    `json:"count"`
				Price       int    `json:"price"`
				StockAmount int    `json:"stockAmount"`
			} `json:"items"`
			Refresh      int      `json:"refresh"`
			Currency     string   `json:"currency"`
			Restricted   bool     `json:"restricted"`
			AllowedItems []string `json:"allowedItems"`
			AllowedLower []string `json:"alloweditems"`
		}
		if err := json.Unmarshal(raw, &rawStores); err != nil {
			econStoresErr = err
			return
		}
		for key, st := range rawStores {
			store := &EconStore{
				Key:          key,
				Currency:     st.Currency,
				Restricted:   st.Restricted,
				AllowedItems: st.AllowedItems,
				Refresh:      time.Duration(st.Refresh) * time.Millisecond,
				LastUpdate:   time.Now(),
			}
			store.AllowedItems = append(store.AllowedItems, st.AllowedLower...)
			store.Original = map[string]bool{}
			seen := map[string]bool{}
			for _, it := range st.Items {
				if seen[it.Key] {
					log.Printf("m6: store %s duplicate item %s skipped", key, it.Key)
					continue
				}
				seen[it.Key] = true
				store.Original[it.Key] = true
				price := it.Price
				if price == 0 {
					price = ItemPrice(it.Key)
				}
				name := ItemName(it.Key)
				if name == it.Key {
					name = ""
				}
				store.Items = append(store.Items, &EconStoreItem{
					Key: it.Key, Name: name, Count: it.Count,
					Price: price, Stock: it.StockAmount, MaxCnt: it.Count,
				})
			}
			econStores[key] = store
		}
		log.Printf("m6: stores=%d", len(econStores))
	})
	return econStoresErr
}

// StoreFor returns the store registry entry (nil when unknown).
func StoreFor(key string) *EconStore {
	if err := LoadStores(); err != nil {
		return nil
	}
	return econStores[key]
}

// StartStoreTicker mirrors Stores constructor: every 20s, each store whose
// refresh elapsed restocks finite entries by stockAmount and pushes Store
// Update to players with it open.
func StartStoreTicker(d EconomyDeps) {
	if err := LoadStores(); err != nil {
		log.Fatalf("m6: load stores: %v", err)
	}
	go func() {
		t := time.NewTicker(protocol.StoreRefreshInterval * time.Second)
		defer t.Stop()
		for range t.C {
			for key, store := range econStores {
				econStoresMu.Lock()
				if time.Since(store.LastUpdate) <= store.Refresh {
					econStoresMu.Unlock()
					continue
				}
				for _, it := range store.Items {
					if it.Count == -1 || it.Count >= it.MaxCnt || it.Stock <= 0 {
						continue
					}
					it.Count += it.Stock
					if it.Count > it.MaxCnt {
						it.Count = it.MaxCnt
					}
				}
				store.LastUpdate = time.Now()
				econStoresMu.Unlock()
				UpdatePlayers(key, d)
			}
		}
	}()
}

// StoreItems snapshots the ordered item list.
func StoreItems(store *EconStore) []EconStoreItem {
	econStoresMu.RLock()
	defer econStoresMu.RUnlock()
	items := make([]EconStoreItem, len(store.Items))
	for i, it := range store.Items {
		items[i] = *it
	}
	return items
}

// indexStoreItem is the lock-free position lookup; callers must hold
// (at least) econStoresMu.RLock, or own the store outright (tests).
func indexStoreItem(store *EconStore, itemKey string) int {
	for i, it := range store.Items {
		if it.Key == itemKey {
			return i
		}
	}
	return -1
}

// FindStoreItem returns the position of itemKey in the store list or -1.
func FindStoreItem(store *EconStore, itemKey string) int {
	econStoresMu.RLock()
	defer econStoresMu.RUnlock()
	return indexStoreItem(store, itemKey)
}

// SerializeStore mirrors stores.ts serialize.
func SerializeStore(store *EconStore) protocol.StorePacketData {
	econStoresMu.RLock()
	defer econStoresMu.RUnlock()
	items := make([]protocol.StoreItemData, 0, len(store.Items))
	for _, it := range store.Items {
		name := it.Name
		if name == "" {
			name = it.Key
		}
		items = append(items, protocol.StoreItemData{Key: it.Key, Name: name, Count: it.Count, Price: it.Price})
	}
	key, currency := store.Key, store.Currency
	return protocol.StorePacketData{Key: &key, Currency: &currency, Items: items}
}

// UpdatePlayers pushes Store Update to everyone with that store open.
func UpdatePlayers(key string, d EconomyDeps) {
	store := StoreFor(key)
	if store == nil {
		return
	}
	data := SerializeStore(store)
	for _, c := range d.Peers.WithStoreOpen(key) {
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketStore, protocol.StoreUpdate, data))
	}
}

// VerifyStore ports verifyStore: store must exist AND be the player's
// currently open one.
func VerifyStore(c EconomyConn, key string) *EconStore {
	if c.StoreOpen() != key {
		log.Printf("m6: %s store action on %s but storeOpen=%q", c.InstanceID(), key, c.StoreOpen())
		return nil
	}
	store := StoreFor(key)
	if store == nil {
		log.Printf("m6: %s unknown store %s", c.InstanceID(), key)
	}
	return store
}

// Notify sends the client Text notification (player.notify parity).
func Notify(c EconomyConn, d EconomyDeps, message string) {
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketNotification, protocol.NotificationText, protocol.NotificationPacketData{Message: message}))
}

// ---------------------------------------------------------------------------
// Store open/purchase/sell/select (stores.ts exact order of checks).
// ---------------------------------------------------------------------------

// OpenStore ports open(): Store Open + player.storeOpen = key.
func OpenStore(c EconomyConn, d EconomyDeps, key string) {
	store := StoreFor(key)
	if store == nil {
		log.Printf("m6: %s INVALID_STORE %s", c.InstanceID(), key)
		return
	}
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketStore, protocol.StoreOpen, SerializeStore(store)))
	c.SetStoreOpen(key)
	log.Printf("m6: %s opened store %s", c.InstanceID(), key)
}

// HasItem reports whether the inventory holds at least one of itemKey.
func HasItem(d EconomyDeps, username, itemKey string) bool {
	for _, s := range d.Store.InventorySlots(username) {
		if s.Key == itemKey {
			return true
		}
	}
	return false
}

// FindCurrency ports inventory.getIndex(item, count).
func FindCurrency(d EconomyDeps, username, itemKey string, count int) int {
	slots := d.Store.InventorySlots(username)
	for i, s := range slots {
		if s.Key == itemKey && s.Count >= count {
			return i
		}
	}
	return -1
}

// InventoryRemoveAt removes count from the slot at index (emits Container
// Remove) and drops emptied slots (dense list parity).
func InventoryRemoveAt(c EconomyConn, d EconomyDeps, username string, index, count int) {
	slots := d.Store.InventorySlots(username)
	if index < 0 || index >= len(slots) {
		return
	}
	s := slots[index]
	s.Count -= count
	if s.Count > 0 {
		slots[index] = s
		d.Store.SetInventory(username, slots)
		d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerRemove, protocol.ContainerData{
			Type: protocol.ContainerTypeInventory,
			Slot: &protocol.SlotData{Index: index, Key: s.Key, Count: s.Count, Enchantments: protocol.EnchAny(s.Ench)},
		}))
		return
	}
	nslots := append(slots[:index], slots[index+1:]...)
	d.Store.SetInventory(username, nslots)
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerRemove, protocol.ContainerData{
		Type: protocol.ContainerTypeInventory,
		Slot: &protocol.SlotData{Index: index, Key: s.Key, Count: 0, Enchantments: protocol.EnchAny(s.Ench)},
	}))
}

// Buy ports purchase() with the exact Node check order.
func Buy(c EconomyConn, d EconomyDeps, key string, index, count int) {
	store := VerifyStore(c, key)
	if store == nil {
		return
	}
	items := StoreItems(store)
	if index < 0 || index >= len(items) {
		log.Printf("m6: %s PURCHASE_INVALID_STORE %s index %d", c.InstanceID(), key, index)
		return
	}
	item := items[index]
	if count < 1 {
		count = 1
	}

	// First and foremost check the user has enough space.
	if protocol.ModulesInventorySize-d.Store.InventoryLen(c.PlayerName()) < 1 && !HasItem(d, c.PlayerName(), item.Key) {
		Notify(c, d, "store:NOT_ENOUGH_SPACE")
		return
	}

	if item.Count != -1 {
		if item.Count < 1 {
			Notify(c, d, "store:ITEM_OUT_OF_STOCK")
			return
		}
		if item.Count < count {
			count = item.Count
		}
	}

	need := item.Price * count
	curIdx := FindCurrency(d, c.PlayerName(), store.Currency, need)
	if curIdx < 0 {
		Notify(c, d, "store:NOT_ENOUGH_CURRENCY")
		return
	}

	amount := d.Store.AddItem(c.PlayerName(), item.Key, count)
	if amount < 0 {
		return
	}
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
		Type: protocol.ContainerTypeInventory,
		Slot: &protocol.SlotData{Index: amount, Key: item.Key, Count: count, Enchantments: map[string]any{}},
	}))

	// Decrement by the bought count (TS `item.count -= amount` where
	// amount is the added count — the seam always adds the full count).
	// `amount` above is the inventory slot INDEX, never the count, so it
	// must not touch stock or the <1 failure check (slot 0 is success).
	econStoresMu.Lock()
	if idx := indexStoreItem(store, item.Key); idx >= 0 && store.Items[idx].Count > 0 {
		store.Items[idx].Count -= count
		// Sold-out entries that were never original stock vanish
		// (stores.ts purchase filter via isOriginalItem); originals
		// linger at 0 as ITEM_OUT_OF_STOCK. Infinite stock (-1)
		// never reaches here.
		if store.Items[idx].Count < 1 && !store.Original[item.Key] {
			store.Items = append(store.Items[:idx], store.Items[idx+1:]...)
		}
	}
	econStoresMu.Unlock()

	InventoryRemoveAt(c, d, c.PlayerName(), curIdx, need)
	d.Store.MarkDirty(c.PlayerName())

	log.Printf("m6: %s purchased %d %s for %d %s (store %s)", c.InstanceID(), count, item.Key, need, store.Currency, key)
	UpdatePlayers(key, d)
}

// GetTotalCost ports getTotalCost exactly.
func GetTotalCost(count, price, storeCount int) int {
	const limit = 10
	if storeCount > limit {
		return int(0.2 * float64(price) * float64(count))
	}
	total := 0.0
	remaining := count
	for i := 0; i < count; i++ {
		if i > limit {
			break
		}
		sc := storeCount + i
		if sc > limit {
			sc = limit
		}
		total += float64(50-3*sc) / 100.0 * float64(price)
		remaining--
	}
	total += 0.2 * float64(price) * float64(remaining)
	return int(total)
}

// Sell ports sell() with the exact Node check order.
func Sell(c EconomyConn, d EconomyDeps, key string, index, count int) {
	store := VerifyStore(c, key)
	if store == nil {
		return
	}
	if count < 1 {
		Notify(c, d, "store:INVALID_ITEM_COUNT")
		return
	}
	slot, ok := d.Store.SlotAt(c.PlayerName(), index)
	if !ok || slot.Key == "" || slot.Count < 1 {
		log.Printf("m6: %s INVALID_ITEM_SELECTION (sell idx %d)", c.InstanceID(), index)
		return
	}

	if len(store.AllowedItems) > 0 && !store.itemAllowed(slot.Key) {
		Notify(c, d, "store:RESTRICTED_ITEM")
		return
	}
	if store.Restricted {
		Notify(c, d, "store:RESTRICTED_STORE")
		return
	}
	if slot.Key == store.Currency {
		Notify(c, d, "store:CANNOT_SELL_ITEM")
		return
	}

	// Temporary UI fix: count comes from the slot itself.
	count = slot.Count

	price := ItemPrice(slot.Key)
	storeIdx := FindStoreItem(store, slot.Key)
	if storeIdx >= 0 && store.Items[storeIdx].Price > 0 {
		price = store.Items[storeIdx].Price
	}
	storeCount := 0
	if storeIdx >= 0 {
		storeCount = store.Items[storeIdx].Count
	}
	totalCoins := GetTotalCost(count, price, storeCount)
	// Zero-coin sales are allowed (< 0 rejects) — stores.ts sell parity,
	// not a missing clamp: haggling a worthless item down to 0 still
	// moves it into the shop stock.
	if totalCoins < 0 {
		Notify(c, d, "store:CANNOT_SELL_ITEM")
		return
	}

	InventoryRemoveAt(c, d, c.PlayerName(), index, count)
	coinIdx := d.Store.AddItem(c.PlayerName(), store.Currency, totalCoins)
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
		Type: protocol.ContainerTypeInventory,
		Slot: &protocol.SlotData{Index: coinIdx, Key: store.Currency, Count: totalCoins, Enchantments: map[string]any{}},
	}))
	d.Store.MarkDirty(c.PlayerName())

	// Increment the item count or add to store (key re-resolved under
	// the lock: the snapshot index may have shifted under restock or a
	// concurrent sale). Infinite stock (-1) never changes.
	econStoresMu.Lock()
	if idx := indexStoreItem(store, slot.Key); idx < 0 {
		store.Items = append(store.Items, &EconStoreItem{
			Key: slot.Key, Name: ItemName(slot.Key), Count: count,
			Price: price, Stock: 1, MaxCnt: count,
		})
	} else if store.Items[idx].Count != -1 {
		store.Items[idx].Count += count
	}
	econStoresMu.Unlock()

	log.Printf("m6: %s sold %d %s for %d %s (store %s)", c.InstanceID(), count, slot.Key, totalCoins, store.Currency, key)
	UpdatePlayers(key, d)
}

// Select ports select(): quote the sell price of the selected inventory slot.
func Select(c EconomyConn, d EconomyDeps, key string, index, count int) {
	store := VerifyStore(c, key)
	if store == nil {
		return
	}
	slot, ok := d.Store.SlotAt(c.PlayerName(), index)
	if !ok || slot.Key == "" || slot.Count < 1 {
		log.Printf("m6: %s INVALID_ITEM_SELECTION (select idx %d)", c.InstanceID(), index)
		return
	}

	if slot.Key == store.Currency {
		Notify(c, d, "store:CANNOT_SELL_ITEM")
		return
	}

	count = slot.Count // temporary UI fix

	price := ItemPrice(slot.Key)
	storeIdx := FindStoreItem(store, slot.Key)
	if storeIdx >= 0 && store.Items[storeIdx].Price > 0 {
		price = store.Items[storeIdx].Price
	}
	storeCount := 0
	if storeIdx >= 0 {
		storeCount = store.Items[storeIdx].Count
	}
	totalCoins := GetTotalCost(count, price, storeCount)
	if totalCoins < 1 {
		Notify(c, d, "store:CANNOT_SELL_ITEM")
		return
	}

	idx := index
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketStore, protocol.StoreSelect, protocol.StorePacketData{
		Key: &key,
		Item: &protocol.StoreItemData{
			Key: slot.Key, Name: ItemName(slot.Key), Count: count, Price: totalCoins, Index: &idx,
		},
	}))
	log.Printf("m6: %s select %s x%d -> %d %s", c.InstanceID(), slot.Key, count, totalCoins, store.Currency)
}

// HandleStore routes the C->S Store frame: [40,{opcode,key,index,count}].
func HandleStore(c EconomyConn, data []byte, d EconomyDeps) {
	var msg struct {
		Opcode *int   `json:"opcode"`
		Key    string `json:"key"`
		Index  *int   `json:"index"`
		Count  *int   `json:"count"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	if msg.Opcode == nil || msg.Index == nil || *msg.Index < 0 {
		return
	}
	index := *msg.Index
	count := 1
	if msg.Count != nil && *msg.Count > 1 {
		count = *msg.Count
	}
	switch *msg.Opcode {
	case protocol.StoreBuy:
		Buy(c, d, msg.Key, index, count)
	case protocol.StoreSell:
		Sell(c, d, msg.Key, index, count)
	case protocol.StoreSelect:
		Select(c, d, msg.Key, index, count)
	}
}

// HandleStoreFrame routes a decoded clientFrame pair (adapter helper).
func HandleStoreFrame(c EconomyConn, frame []json.RawMessage, d EconomyDeps) {
	if len(frame) < 2 {
		return
	}
	HandleStore(c, []byte(frame[1]), d)
}
