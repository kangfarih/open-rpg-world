// Economy wire — stores + bank + NPC talk.
//
// Thin adapter over internal/controller (behavior-frozen move, task E6):
// all stores/bank/NPC-talk/container/equipment logic lives in the controller
// package operating on the EconomyConn/EconomyStore/EconomyBus/EconomyPeers/
// QuestTalk/PetHooks/WorldLookup seams below. This file only wires those
// seams to the root globals (players map, send/broadcast, pstates, player-state/
// quests/pets hooks) and keeps the entry points main.go and other wire files
// call — with UNCHANGED signatures — delegating to the controller. Packet
// shapes, prices, roll logic and tick cadence are identical.
package server

import (
	"encoding/json"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/protocol"
	worldcore "rpg-world-server/internal/world"
)

// Type aliases so existing names keep resolving to the moved types.
type (
	itemInfo  = controller.ItemInfo
	storeItem = controller.EconStoreItem
	storeDef     = controller.EconStore
	npcInfo   = controller.NPCInfo
)

// Registry mirrors for direct map readers (testItems/findNPC parity).
// The authoritative registries live in the controller; these copies are
// synced on load so external call sites compile untouched.
var (
	itemDefs  = map[string]*itemInfo{}
	npcDefs   = map[string]*npcInfo{}
	npcsLoaded bool
)

// ---------------------------------------------------------------------------
// controller.EconomyConn seam (*playerConn satisfies it).
// ---------------------------------------------------------------------------

func (c *playerConn) StoreOpen() string {
	if c == nil {
		return ""
	}
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	return c.storeOpen
}
func (c *playerConn) SetStoreOpen(s string) {
	if c == nil {
		return
	}
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.storeOpen = s
}
func (c *playerConn) CanAccess() bool {
	if c == nil {
		return false
	}
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	return c.canAccessContainer
}
func (c *playerConn) SetCanAccess(b bool) {
	if c == nil {
		return
	}
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.canAccessContainer = b
}
func (c *playerConn) TalkKey() string {
	if c == nil {
		return ""
	}
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	return c.talkNPC
}
func (c *playerConn) SetTalkKey(s string) {
	if c == nil {
		return
	}
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.talkNPC = s
}
func (c *playerConn) TalkIndex() int {
	if c == nil {
		return 0
	}
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	return c.talkIndex
}
func (c *playerConn) SetTalkIndex(i int) {
	if c == nil {
		return
	}
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.talkIndex = i
}

// withTalk runs fn with the talk cursor held under the session lock so
// read-modify-write cursor updates (world sign TalkWith parity) stay atomic
// with the concurrent store-ticker reads above.
func (c *playerConn) withTalk(fn func(npc *string, idx *int)) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	fn(&c.talkNPC, &c.talkIndex)
}

// resetTalk clears the talk cursor (sign debug path parity).
func (c *playerConn) resetTalk() {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.talkNPC = ""
	c.talkIndex = 0
}

// ---------------------------------------------------------------------------
// controller.EconomyStore seam (playerStateFor/pstateMu + m5 helpers).
// Store methods reuse tradeStore; bank/equip/total-level extend it.
// ---------------------------------------------------------------------------

type econStore struct{ tradeStore }

func (econStore) InventorySlots(username string) []controller.Slot {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	out := make([]controller.Slot, len(st.Inv))
	for i, s := range st.Inv {
		out[i] = controller.Slot{Key: s.Key, Count: s.Count, Ench: s.Ench}
	}
	return out
}

func (econStore) SetInventory(username string, slots []controller.Slot) {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	out := make([]slotDef, len(slots))
	for i, s := range slots {
		out[i] = slotDef{Key: s.Key, Count: s.Count, Ench: s.Ench}
	}
	st.Inv = out
}

func (econStore) BankSlots(username string) []controller.Slot {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	out := make([]controller.Slot, len(st.Bank))
	for i, s := range st.Bank {
		out[i] = controller.Slot{Key: s.Key, Count: s.Count, Ench: s.Ench}
	}
	return out
}

func (econStore) SetBank(username string, slots []controller.Slot) {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	out := make([]slotDef, len(slots))
	for i, s := range slots {
		out[i] = slotDef{Key: s.Key, Count: s.Count, Ench: s.Ench}
	}
	st.Bank = out
}

func (econStore) EquipSlot(username string, slotType int) (controller.Slot, bool) {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if slotType < 0 || slotType >= len(st.Equip) {
		return controller.Slot{}, false
	}
	s := st.Equip[slotType]
	return controller.Slot{Key: s.Key, Count: s.Count, Ench: s.Ench}, true
}

func (econStore) SetEquip(username string, slotType int, slot controller.Slot) {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if slotType < 0 || slotType >= len(st.Equip) {
		return
	}
	st.Equip[slotType] = slotDef{Key: slot.Key, Count: slot.Count, Ench: slot.Ench}
}

func (econStore) EquipLen(username string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	return len(st.Equip)
}

func (econStore) TotalLevel(username string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	return st.Level
}

// ---------------------------------------------------------------------------
// controller.EconomyBus / EconomyPeers seams.
// ---------------------------------------------------------------------------

type econBus struct{ tradeBus }

func (econBus) Broadcast(frames ...[]any) { worldcore.Broadcast(frames...) }

type econPeers struct{ tradePeers }

func (econPeers) WithStoreOpen(key string) []controller.EconomyConn {
	var out []controller.EconomyConn
	for _, c := range worldcore.AllOf[*playerConn]() {
		// StoreOpen() takes the session read lock: the 20s ticker reads
		// off-goroutine while conn goroutines write via SetStoreOpen
		// (controller ClearAccess/OpenStore path). Never read c.storeOpen
		// directly here (-race under e2e/m6 load).
		if c.StoreOpen() == key {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// controller.QuestTalk / PetHooks / WorldLookup seams.
// ---------------------------------------------------------------------------

type econQuests struct{}

func (econQuests) Talk(c controller.EconomyConn, npcKey string) bool {
	return questTalk(tradeConn(c), npcKey)
}

type econPets struct{}

func (econPets) DropKey(c controller.EconomyConn, index int) (string, string, bool) {
	return petDropKey(tradeConn(c), index)
}
func (econPets) HasOwner(instance string) bool { return petHasOwner(instance) }
func (econPets) Grant(c controller.EconomyConn, mob, item string) {
	petGrant(tradeConn(c), mob, item)
}

type econWorld struct{}

func (econWorld) EntityPos(instance string) (int, int, bool) {
	return worldcore.EntityPos(instance)
}
func (econWorld) SpawnNPCKey(instance string) (string, bool) {
	payload, ok := spawnPayload(instance)
	if !ok {
		return "", false
	}
	var probe struct {
		Type int    `json:"type"`
		Key  string `json:"key"`
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", false
	}
	if probe.Type == EntityNPC && probe.Key != "" {
		return probe.Key, true
	}
	return "", false
}
func (econWorld) ShowcaseKey(n int) (string, bool) {
	if n < 0 || n >= len(showNPCs) {
		return "", false
	}
	return showNPCs[n], true
}
func (econWorld) ShowcaseCount() int { return len(showNPCs) }
func (econWorld) SyncFrame(instance string, x, y int) []any {
	ph := welcomePlayer(instance)
	ph.X, ph.Y = x, y
	return pkt(PacketSync, ph)
}

func econDeps() controller.EconomyDeps {
	return controller.EconomyDeps{
		Store: econStore{}, Bus: econBus{}, Peers: econPeers{},
		Quests: econQuests{}, Pets: econPets{}, World: econWorld{},
		Vitals: econVitals{},
		Drops: controller.DropHooks{
			SpawnItem: func(owner, key string, count, x, y int) string {
				return entity.SpawnLootAt(owner, key, count, x, y)
			},
		},
	}
}

// ---------------------------------------------------------------------------
// controller.Vitals seam (item-use plugins: healing/poison/effects).
// ---------------------------------------------------------------------------

// econVitals implements controller.Vitals over the hero HP store
// (gameWorldAdapter Get/SetHeroHP + HeroPoints), the ability mana/status
// tracker (abilities.*) and the engine mob targets (mobTargeting).
type econVitals struct{}

func (econVitals) HeroHP(instance string) (int, int) {
	return gameWorld.GetHeroHP(instance), gameWorld.HeroMaxHP(instance)
}

func (econVitals) HeroMana(instance string) (int, int) {
	return abilities.ManaState(instance)
}

// HealHero ports player.heal(amount, 'hitpoints'/'mana') (player.ts:504-541):
// clamp into the pool, Heal frame with the requested amount + Points sync.
func (econVitals) HealHero(instance string, hpAmount, manaAmount int) {
	if hpAmount > 0 {
		hp := gameWorld.GetHeroHP(instance)
		maxHP := gameWorld.HeroMaxHP(instance)
		if hp < maxHP {
			want := hp + hpAmount
			if want > maxHP {
				want = maxHP
			}
			gameWorld.SetHeroHP(instance, want)
			worldcore.Broadcast(protocol.Pkt(protocol.PacketHeal, protocol.HealData{
				Instance: instance, Type: "hitpoints", Amount: hpAmount,
			}))
			gameWorld.HeroPoints(instance, want, maxHP)
		}
	}
	if manaAmount > 0 {
		if applied, _, _ := abilities.HealMana(instance, manaAmount); applied > 0 {
			worldcore.Broadcast(protocol.Pkt(protocol.PacketHeal, protocol.HealData{
				Instance: instance, Type: "mana", Amount: manaAmount,
			}))
		}
	}
}

// DamageHero ports the black-potion delayed self-hit (player.hit parity via
// entity.DamageHero: Points + the exactly-once death funnel).
func (econVitals) DamageHero(instance string, dmg int) {
	if dmg < 0 {
		dmg = 0
	}
	// Invincible parity: block all damage including black-potion self-hit.
	if abilities.HasStatusEffect(instance, 18) { // fxInvincible
		return
	}
	username := ""
	if c, ok := worldcore.Find[*playerConn](instance); ok && c != nil {
		username = c.Username
	}
	entity.DamageHero(gameWorld, instance, username, dmg, nil)
}

func (econVitals) CurePoison(instance string) { abRemovePoison(instance) }

// InCombat ports character.inCombat (character.ts:769-776: has target or
// attackers) over the hero's live target plus engine mob attackers.
func (econVitals) InCombat(instance string) bool {
	return abLiveTarget(instance) != "" || mobTargeting(instance)
}

func (econVitals) HasEffect(instance string, effect int) bool {
	return abilities.HasStatusEffect(instance, effect)
}

func (econVitals) AddEffect(instance string, effect int, durationMs int64) {
	abilities.ApplyStatusEffect(instance, effect, durationMs)
}

func (econVitals) RemoveEffect(instance string, effect int) {
	abilities.RemoveStatusEffect(instance, effect)
}

// ---------------------------------------------------------------------------
// Entry points (signatures UNCHANGED; main.go/m5/m11/m12/m13 call sites
// compile as-is). All logic lives in the controller.
// ---------------------------------------------------------------------------

func loadItems() error {
	if err := controller.LoadItems(); err != nil {
		return err
	}
	for _, k := range controller.ItemKeys() {
		itemDefs[k] = controller.ItemInfoFor(k)
	}
	return nil
}

func itemInfoFor(key string) *itemInfo { return controller.ItemInfoFor(key) }

func itemName(key string) string { return controller.ItemName(key) }

func maxStack(key string) int { return controller.MaxStack(key) }

func loadStores() error { return controller.LoadStores() }

func itemPrice(key string) int { return controller.ItemPrice(key) }

func toolTier(itemKey, skill string) int { return controller.ToolTier(itemKey, skill) }

func storeFor(key string) *storeDef { return controller.StoreFor(key) }

func startStoreTicker() { controller.StartStoreTicker(econDeps()) }

func storeItems(store *storeDef) []storeItem { return controller.StoreItems(store) }

func findStoreItem(store *storeDef, itemKey string) int {
	return controller.FindStoreItem(store, itemKey)
}

func serializeStore(store *storeDef) storePacketData { return controller.SerializeStore(store) }

func updateStorePlayers(key string) { controller.UpdatePlayers(key, econDeps()) }

func verifyStore(c *playerConn, key string) *storeDef {
	if c == nil {
		return nil
	}
	return controller.VerifyStore(c, key)
}

func notifyPlayer(c *playerConn, message string) {
	if c == nil {
		return
	}
	controller.Notify(c, econDeps(), message)
}

func openStore(c *playerConn, key string) {
	if c == nil {
		return
	}
	controller.OpenStore(c, econDeps(), key)
}

func inventoryHasItem(key, itemKey string) bool {
	return controller.HasItem(econDeps(), key, itemKey)
}

func findCurrency(key, itemKey string, count int) int {
	return controller.FindCurrency(econDeps(), key, itemKey, count)
}

func inventoryRemoveAt(c *playerConn, key string, index, count int) {
	if c == nil {
		return
	}
	controller.InventoryRemoveAt(c, econDeps(), key, index, count)
}

func storeBuy(c *playerConn, key string, index, count int) {
	if c == nil {
		return
	}
	controller.Buy(c, econDeps(), key, index, count)
}

func getTotalCost(count, price, storeCount int) int {
	return controller.GetTotalCost(count, price, storeCount)
}

func storeSell(c *playerConn, key string, index, count int) {
	if c == nil {
		return
	}
	controller.Sell(c, econDeps(), key, index, count)
}

func storeSelect(c *playerConn, key string, index, count int) {
	if c == nil {
		return
	}
	controller.Select(c, econDeps(), key, index, count)
}

func handleStore(c *playerConn, frame clientFrame) {
	if c == nil || len(frame) < 2 {
		return
	}
	controller.HandleStore(c, []byte(frame[1]), econDeps())
}

func seedItem(key, itemKey string, count int) int {
	return controller.SeedItem(econDeps(), key, itemKey, count)
}

func seedGold(key string, amount int) { controller.SeedGold(econDeps(), key, amount) }

func invSlots(key string) []any { return controller.InvSlots(econDeps(), key) }

func bankAdd(key, itemKey string, count int) int {
	return controller.BankAdd(econDeps(), key, itemKey, count)
}

func bankBatch(key string) containerData { return controller.BankBatch(econDeps(), key) }

func clearContainerAccess(c *playerConn) {
	if c == nil {
		return
	}
	controller.ClearAccess(c)
}

func openBank(c *playerConn) {
	if c == nil {
		return
	}
	controller.OpenBank(c, econDeps())
}

func handleContainerSelect(c *playerConn, msg *clientContainer) {
	if c == nil || msg == nil {
		return
	}
	controller.HandleContainerSelect(c, msg, econDeps())
}

func handleContainerSwap(c *playerConn, fromIndex, toIndex int) {
	if c == nil {
		return
	}
	controller.HandleContainerSwap(c, econDeps(), fromIndex, toIndex)
}

func handleContainer(c *playerConn, frame clientFrame) {
	if c == nil || len(frame) < 2 {
		return
	}
	controller.HandleContainer(c, []byte(frame[1]), econDeps())
}

func loadNPCs() {
	controller.LoadNPCs()
	for k, v := range controller.NPCSnapshot() {
		npcDefs[k] = v
	}
	npcsLoaded = controller.NPCsOK()
}

func isNPCKey(key string) bool { return controller.IsNPCKey(key) }

func resolveNPCKey(c *playerConn, instance string) string {
	var ec controller.EconomyConn
	if c != nil {
		ec = c
	}
	return controller.ResolveNPCKey(ec, econDeps(), instance)
}

func handleNPCTarget(c *playerConn, instance string) {
	if c == nil {
		return
	}
	controller.HandleNPCTarget(c, econDeps(), instance)
}

// min helper for the talk index clamp (shared with m11.go).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func equipmentType(itemType string) int { return controller.EquipmentType(itemType) }

func isEquippable(itemType string) bool { return controller.IsEquippable(itemType) }

func equipmentData(slotType int, key string, count int, clientInfo bool) map[string]any {
	return controller.EquipmentData(slotType, key, count, clientInfo)
}

func equipmentSlots(key string) []any { return controller.EquipmentSlots(econDeps(), key) }

func skillLevelFor(key string, skillID int) (string, int, int) {
	return controller.SkillLevelFor(econDeps(), key, skillID)
}

func canEquip(c *playerConn, key string) bool {
	if c == nil {
		return false
	}
	return controller.CanEquip(c, econDeps(), key)
}

func skillIDFor(key string) (int, bool) { return controller.SkillIDFor(key) }

func unequipType(c *playerConn, slotType int) {
	if c == nil {
		return
	}
	controller.UnequipType(c, econDeps(), slotType)
}

func equipFromInventory(c *playerConn, fromIndex int) {
	if c == nil {
		return
	}
	controller.EquipFromInventory(c, econDeps(), fromIndex)
}

func handleEquipment(c *playerConn, frame clientFrame) {
	if c == nil || len(frame) < 2 {
		return
	}
	controller.HandleEquipment(c, []byte(frame[1]), econDeps())
}
