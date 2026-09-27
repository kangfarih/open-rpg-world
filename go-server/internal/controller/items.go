package controller

import (
	"fmt"
	"log"
	"sync"
	"time"

	"rpg-world-server/internal/meta"
	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// Item-use plugin system (player.ts handleContainerSelect Inventory path +
// data/plugins/items/*.ts). Previously HandleContainerSelect equipped
// straight from the inventory; the interactable/edible branches below port
// player.ts:800-811 exactly:
//
//	if (item.interactable && item.plugin?.onUse(this)) return;
//	if (item.edible && this.canEat() && item.plugin?.onUse(this)) {
//	    this.inventory.remove(fromIndex, 1);
//	    this.lastEdible = Date.now();
//	    if (item.isSmallBowl()) add bowlsmall
//	    else if (item.isMediumBowl()) add bowlmedium
//	}
//
// Effects ride the existing Heal/Effect/Points frames (no new opcodes); bowl
// returns reuse the inventory-add path (Container Add, Buy parity).
// ---------------------------------------------------------------------------

// EdibleCooldownMs mirrors Modules.Constants.EDIBLE_COOLDOWN (modules.ts:642).
const EdibleCooldownMs = int64(1500)

// SkillEating mirrors Modules.Skills.Eating (modules.ts:215-235 order:
// Lumberjacking0..Fishing8, Cooking9, Smithing10, Crafting11, Chiseling12,
// Fletching13, Smelting14, Foraging15, Eating16).
const SkillEating = 16

// Effect IDs mirror Modules.Effects (modules.ts:271-302).
const (
	EffectRunning          = 10
	EffectHotSauce         = 11
	EffectSnowPotion       = 14
	EffectFirePotion       = 15
	EffectBurning          = 16
	EffectFreezing         = 17
	EffectAccuracyBuff     = 19
	EffectStrengthBuff     = 20
	EffectDefenseBuff      = 21
	EffectMagicBuff        = 22
	EffectArcheryBuff      = 23
	EffectAccuracySuperBuf = 24
	EffectStrengthSuperBuf = 25
	EffectDefenseSuperBuf  = 26
	EffectMagicSuperBuf    = 27
	EffectArcherySuperBuf  = 28
)

// Potion cadences mirror the TS plugin sources.
const (
	SnowPotionDurationMs  = int64(60_000) // Modules.Constants.SNOW_POTION_DURATION
	FirePotionDurationMs  = int64(60_000) // Modules.Constants.FIRE_POTION_DURATION
	EffectPotionDefaultMs = int64(60_000) // effectpotion.ts duration default
	HotSauceDurationMs    = int64(15_000) // hotsauce.ts setTimeout
	BlackPotionDelayMs    = int64(5_000)  // blackpotion.ts setTimeout
)

// itemAfterFunc is time.AfterFunc (var so tests can observe scheduling).
var itemAfterFunc = time.AfterFunc

// Vitals abstracts the live hero state the item plugins need. The root
// adapter (m6.go m6vitals) implements it over gameWorld/abilities; tests use
// an in-memory fake. It is a separate field on EconomyDeps (not part of
// EconomyStore) so existing Store implementers are untouched.
type Vitals interface {
	// HeroHP reports current/max hitpoints for the conn instance.
	HeroHP(instance string) (hp, maxHP int)
	// HeroMana reports current/max mana for the conn instance.
	HeroMana(instance string) (mana, maxMana int)
	// HealHero adds hpAmount hitpoints and manaAmount mana (clamped),
	// emitting the Heal + Points frames (player.heal hitpoints/mana parity).
	HealHero(instance string, hpAmount, manaAmount int)
	// DamageHero deals dmg to the instance (Points + death funnel parity).
	DamageHero(instance string, dmg int)
	// CurePoison clears poison (character.setPoison() no-arg parity).
	CurePoison(instance string)
	// InCombat reports character.inCombat parity (has target or attackers).
	InCombat(instance string) bool
	// HasEffect reports a live status effect (status.has parity).
	HasEffect(instance string, effect int) bool
	// AddEffect records a timed status effect + Effect Add frame.
	AddEffect(instance string, effect int, durationMs int64)
	// RemoveEffect drops a status effect + Effect Remove frame (no-op
	// without a frame when the effect is absent, status.remove parity).
	RemoveEffect(instance string, effect int)
}

var (
	edibleMu   sync.Mutex
	lastEdible = map[string]int64{} // username -> last consume ms (player.lastEdible)
)

// canEat mirrors player.canEat (player.ts:2086-2088).
func canEat(username string, nowMs int64) bool {
	edibleMu.Lock()
	defer edibleMu.Unlock()
	return nowMs-lastEdible[username] > EdibleCooldownMs
}

func markAte(username string, nowMs int64) {
	edibleMu.Lock()
	defer edibleMu.Unlock()
	lastEdible[username] = nowMs
}

// HandleInventoryUse ports the plugin branches of handleContainerSelect's
// Inventory case (player.ts:800-811). It reports true when the select is
// consumed (plugin handled, or edible consumed) so the caller skips the
// equip step; false falls through to EquipFromInventory.
func HandleInventoryUse(c EconomyConn, d EconomyDeps, fromIndex int) bool {
	username := c.PlayerName()
	slot, ok := d.Store.SlotAt(username, fromIndex)
	if !ok || slot.Key == "" || slot.Count < 1 {
		return false
	}
	it := ItemInfoFor(slot.Key)
	if it == nil {
		return false
	}
	if it.Interactable && it.Plugin != "" && ItemPluginUse(c, d, it) {
		return true
	}
	if it.Edible && it.Plugin != "" {
		now := time.Now().UnixMilli()
		if canEat(username, now) && ItemPluginUse(c, d, it) {
			InventoryRemoveAt(c, d, username, fromIndex, 1)
			markAte(username, now)
			if it.SmallBowl {
				bowlReturn(c, d, username, "bowlsmall")
			} else if it.MediumBowl {
				bowlReturn(c, d, username, "bowlmedium")
			}
			d.Store.MarkDirty(username)
			log.Printf("m6: %s used %s (plugin %s)", c.InstanceID(), slot.Key, it.Plugin)
			return true
		}
	}
	return false
}

// bowlReturn ports the isSmallBowl/isMediumBowl branches (player.ts:807-810)
// over the inventory-add path (Container Add, Buy parity).
func bowlReturn(c EconomyConn, d EconomyDeps, username, bowlKey string) {
	idx := d.Store.AddItem(username, bowlKey, 1)
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
		Type: protocol.ContainerTypeInventory,
		Slot: &protocol.SlotData{Index: idx, Key: bowlKey, Count: 1, Enchantments: map[string]any{}},
	}))
}

// ItemPluginUse dispatches items.json `plugin` to its port. It reports
// whether the plugin handled the use (true = TS onUse returned true).
// Plugin names with no TS source (speedpotion, beermug) report false, the
// same outcome as TS where item.plugin is undefined for them.
func ItemPluginUse(c EconomyConn, d EconomyDeps, it *ItemInfo) bool {
	if d.Vitals == nil {
		return false
	}
	switch it.Plugin {
	case "healingitem":
		return useHealingItem(c, d, it)
	case "poisoncure":
		d.Vitals.CurePoison(c.InstanceID())
		return true
	case "firepotion":
		return useFirePotion(c, d)
	case "snowpotion":
		return useSnowPotion(c, d)
	case "blackpotion":
		return useBlackPotion(c, d)
	case "effectpotion":
		return useEffectPotion(c, d, it)
	case "hotsauce":
		return useHotSauce(c, d)
	case "chisel":
		return useCraftTool(c, d, protocol.SkillChiseling)
	case "knife":
		return useCraftTool(c, d, protocol.SkillFletching)
	}
	return false
}

// useHealingItem ports healingitem.ts onUse (healAmount/manaAmount/
// healPercent from the item data; full-HP/mana notifies + false = no
// consume; eating XP only on the flat-heal branch). The full-HP gate runs
// before any mana is applied: otherwise a hurt-mana/full-HP use would heal
// mana and then return false without consuming (free mana).
func useHealingItem(c EconomyConn, d EconomyDeps, it *ItemInfo) bool {
	inst := c.InstanceID()
	hasHeal := it.HealAmount > 0 || it.HealPercent > 0
	maxHP := 0
	if hasHeal {
		hp, max := d.Vitals.HeroHP(inst)
		maxHP = max
		if hp >= max {
			Notify(c, d, "You are already at full health.")
			return false
		}
	}
	if it.ManaAmount > 0 {
		mana, maxMana := d.Vitals.HeroMana(inst)
		if mana >= maxMana {
			Notify(c, d, "You are already at full mana.")
			return false
		}
		d.Vitals.HealHero(inst, 0, it.ManaAmount)
	}
	if hasHeal {
		if it.HealPercent > 0 {
			d.Vitals.HealHero(inst, int(float64(maxHP)*(it.HealPercent/100)), 0)
			return true
		}
		d.Vitals.HealHero(inst, it.HealAmount, 0)
		d.Store.AddXP(c, SkillEating, it.HealAmount/10)
	}
	return true
}

// useFirePotion ports firepotion.ts onUse via player.setFirePotion
// (player.ts:1538-1553): drop Burning, timed FirePotion immunity, the
// misc:FIRE_IMMUNITY notify; expiry notify mirrors the setTimeout callback.
func useFirePotion(c EconomyConn, d EconomyDeps) bool {
	inst := c.InstanceID()
	d.Vitals.RemoveEffect(inst, EffectBurning)
	d.Vitals.AddEffect(inst, EffectFirePotion, FirePotionDurationMs)
	Notify(c, d, fmt.Sprintf("misc:FIRE_IMMUNITY;duration=%d", FirePotionDurationMs/1000))
	itemAfterFunc(time.Duration(FirePotionDurationMs)*time.Millisecond, func() {
		if !d.Vitals.HasEffect(inst, EffectFirePotion) {
			d.Bus.Notify(inst, "misc:FIRE_IMMUNITY_WORN_OFF")
		}
	})
	return true
}

// useSnowPotion ports snowpotion.ts onUse via player.setSnowPotion
// (player.ts:1515-1531): drop timeout Freezing, timed SnowPotion immunity,
// the misc:FREEZE_IMMUNITY notify + worn-off callback.
func useSnowPotion(c EconomyConn, d EconomyDeps) bool {
	inst := c.InstanceID()
	d.Vitals.RemoveEffect(inst, EffectFreezing)
	d.Vitals.AddEffect(inst, EffectSnowPotion, SnowPotionDurationMs)
	Notify(c, d, fmt.Sprintf("misc:FREEZE_IMMUNITY;duration=%d", SnowPotionDurationMs/1000))
	itemAfterFunc(time.Duration(SnowPotionDurationMs)*time.Millisecond, func() {
		if !d.Vitals.HasEffect(inst, EffectSnowPotion) {
			d.Bus.Notify(inst, "misc:FREEZE_IMMUNITY_WORN_OFF")
		}
	})
	return true
}

// useBlackPotion ports blackpotion.ts onUse: the misc:BLACK_POTION notify
// (t() resolves client-side; the Go server sends the raw key like every
// other misc notify) + the 5s delayed self-hit for (hp - 1).
func useBlackPotion(c EconomyConn, d EconomyDeps) bool {
	inst := c.InstanceID()
	Notify(c, d, "misc:BLACK_POTION")
	itemAfterFunc(time.Duration(BlackPotionDelayMs)*time.Millisecond, func() {
		hp, _ := d.Vitals.HeroHP(inst)
		d.Vitals.DamageHero(inst, hp-1)
	})
	return true
}

// useEffectPotion ports effectpotion.ts onUse: timed buff effect keyed by
// the item's `effect` string + the drink/worn-off notifies.
func useEffectPotion(c EconomyConn, d EconomyDeps, it *ItemInfo) bool {
	inst := c.InstanceID()
	effect := effectIDFor(it.Effect)
	duration := it.Duration
	if duration <= 0 {
		duration = EffectPotionDefaultMs
	}
	d.Vitals.AddEffect(inst, effect, duration)
	Notify(c, d, fmt.Sprintf("You drink the %s potion.", it.Effect))
	itemAfterFunc(time.Duration(duration)*time.Millisecond, func() {
		if !d.Vitals.HasEffect(inst, effect) {
			d.Bus.Notify(inst, fmt.Sprintf("The effect of the %s potion has worn off.", it.Effect))
		}
	})
	return true
}

// effectIDFor ports effectpotion.ts getEffect (default StrengthBuff).
func effectIDFor(effect string) int {
	switch effect {
	case "accuracy":
		return EffectAccuracyBuff
	case "strength":
		return EffectStrengthBuff
	case "defense":
		return EffectDefenseBuff
	case "magic":
		return EffectMagicBuff
	case "archery":
		return EffectArcheryBuff
	case "accuracysuper":
		return EffectAccuracySuperBuf
	case "strengthsuper":
		return EffectStrengthSuperBuf
	case "defensesuper":
		return EffectDefenseSuperBuf
	case "magicsuper":
		return EffectMagicSuperBuf
	case "archerysuper":
		return EffectArcherySuperBuf
	}
	return EffectStrengthBuff
}

// useHotSauce ports hotsauce.ts onUse via player.setRunning(running,
// hotSauce): duplicate HotSauce notifies + false; otherwise drop Running,
// add HotSauce, and after 15s drop both + the faded notify. Movement Speed
// frames have no Go counterpart (no speed engine) — the status/effect
// pipeline carries the buff.
func useHotSauce(c EconomyConn, d EconomyDeps) bool {
	inst := c.InstanceID()
	if d.Vitals.HasEffect(inst, EffectHotSauce) {
		Notify(c, d, "I really shouldn't be drinking multiple of these...")
		return false
	}
	Notify(c, d, "You feel an intense rush of adrenaline, you feel like you can run forever.")
	d.Vitals.RemoveEffect(inst, EffectRunning)
	d.Vitals.AddEffect(inst, EffectHotSauce, HotSauceDurationMs)
	itemAfterFunc(time.Duration(HotSauceDurationMs)*time.Millisecond, func() {
		d.Vitals.RemoveEffect(inst, EffectRunning)
		d.Vitals.RemoveEffect(inst, EffectHotSauce)
		d.Bus.Notify(inst, "The hot sauce effect has faded.")
	})
	return true
}

// useCraftTool ports chisel.ts/knife.ts onUse: combat gates on the
// fletching-menu notify (the TS string says "fletching" in both files),
// otherwise the crafting menu opens on the tool's interface. Tools are
// interactable, never consumed.
func useCraftTool(c EconomyConn, d EconomyDeps, iface int) bool {
	if d.Vitals.InCombat(c.InstanceID()) {
		Notify(c, d, "You cannot activate the fletching menu while in combat.")
		return false
	}
	CraftOpen(d.tradeDeps(), c, iface)
	return true
}

// ---------------------------------------------------------------------------
// Attack style (the deferred EquipmentStyle case + hero damage wiring).
//
// TS flow: C->S [8,{opcode:3,style}] -> incoming.handleEquipment ->
// equipment.updateAttackStyle (hasAttackStyle gate + sync + callback) ->
// handler.handleAttackStyle echoes [8,3,{attackStyle, attackRange}].
// Damage: formulas.getMaxDamage switches on character.getAttackStyle
// (Slash/Focused x1.1, Crush/Hack x1.05, Shared x1.03).
//
// Persistence note: TS persists attackStyle inside the equipment DB blob
// (equipments.serialize). The Go equipment table has no such column and the
// schema stays frozen, so styles live in this session map (username keyed).
// ---------------------------------------------------------------------------

// AttackStyle IDs mirror Modules.AttackStyle (modules.ts:158-179).
const (
	AttackStyleNone      = 0
	AttackStyleStab      = 1
	AttackStyleSlash     = 2
	AttackStyleDefensive = 3
	AttackStyleCrush     = 4
	AttackStyleShared    = 5
	AttackStyleHack      = 6
	AttackStyleChop      = 7
	AttackStyleAccurate  = 8
	AttackStyleFast      = 9
	AttackStyleFocused   = 10
	AttackStyleLongRange = 11
)

// ArcherAttackRange mirrors Modules.Constants.ARCHER_ATTACK_RANGE
// (modules.ts:644): the bow default when items.json carries no attackRange.
const ArcherAttackRange = 8

var (
	styleMu      sync.Mutex
	attackStyles = map[string]int{} // username -> selected style
)

// AttackStylesFor ports item.getAttackStyles (item.ts:405-482): styles by
// weaponType; non-weapons and unknown types carry none.
func AttackStylesFor(itemKey string) []int {
	it := ItemInfoFor(itemKey)
	if it == nil {
		return nil
	}
	switch it.Type {
	case "weapon", "weaponarcher", "weaponmagic":
	default:
		return nil
	}
	switch it.WeaponType {
	case "sword":
		return []int{AttackStyleStab, AttackStyleSlash, AttackStyleShared, AttackStyleDefensive}
	case "bigsword", "scythe":
		return []int{AttackStyleSlash, AttackStyleCrush, AttackStyleDefensive}
	case "axe":
		return []int{AttackStyleHack, AttackStyleChop, AttackStyleDefensive}
	case "pickaxe":
		return []int{AttackStyleStab, AttackStyleDefensive}
	case "blunt":
		return []int{AttackStyleCrush, AttackStyleShared, AttackStyleDefensive}
	case "spear":
		return []int{AttackStyleStab, AttackStyleSlash, AttackStyleDefensive}
	case "bow":
		return []int{AttackStyleAccurate, AttackStyleFast, AttackStyleLongRange}
	case "whip":
		return []int{AttackStyleSlash, AttackStyleDefensive}
	case "staff":
		return []int{AttackStyleFocused, AttackStyleLongRange}
	}
	return nil
}

// AttackRangeFor ports the weapon attackRange update (item.ts:142 +
// weapon.ts updateAttackStyle: LongRange +2 for archer/magic weapons).
func AttackRangeFor(itemKey string, style int) int {
	base := 1
	ranged := false
	if it := ItemInfoFor(itemKey); it != nil {
		if it.AttackRange > 0 {
			base = it.AttackRange
		} else if it.Type == "weaponarcher" || it.Type == "weaponmagic" {
			base = ArcherAttackRange
		}
		ranged = it.Type == "weaponarcher" || it.Type == "weaponmagic"
	}
	if style == AttackStyleLongRange && ranged {
		base += 2
	}
	return base
}

// StyleDamageKey maps an AttackStyle to the meta.MaxDamageFloat style
// string (formulas.getMaxDamage switch parity).
func StyleDamageKey(style int) string {
	switch style {
	case AttackStyleSlash, AttackStyleFocused:
		return "slash"
	case AttackStyleCrush, AttackStyleHack:
		return "crush"
	case AttackStyleShared:
		return "shared"
	}
	return ""
}

// StyleDamageMult returns the max-damage multiplier for a style through
// meta.MaxDamageFloat (read-only reuse): the styled over the unstyled
// ratio, so the factors stay defined once (slash 1.1, crush 1.05,
// shared 1.03, everything else 1.0).
func StyleDamageMult(style int) float64 {
	base := meta.MaxDamageFloat(0, 0, "", false)
	return meta.MaxDamageFloat(0, 0, StyleDamageKey(style), false) / base
}

// AttackStyleFor reports the player's current style: the last explicit
// switch, else the equipped weapon's first style (equip() default parity),
// else None.
func AttackStyleFor(d EconomyDeps, username string) int {
	styleMu.Lock()
	style, ok := attackStyles[username]
	styleMu.Unlock()
	if ok {
		return style
	}
	if e, ok := d.Store.EquipSlot(username, protocol.EquipmentWeapon); ok && e.Key != "" {
		if styles := AttackStylesFor(e.Key); len(styles) > 0 {
			return styles[0]
		}
	}
	return AttackStyleNone
}

// ForgetAttackStyle drops the session style (disconnect parity).
func ForgetAttackStyle(username string) {
	styleMu.Lock()
	delete(attackStyles, username)
	styleMu.Unlock()
}

// UpdateAttackStyle ports equipments.updateAttackStyle + the
// handleAttackStyle echo: hasAttackStyle gate (warning + silent return),
// store, sync broadcast, [8,3,{attackStyle, attackRange}] echo.
func UpdateAttackStyle(c EconomyConn, d EconomyDeps, style int) {
	username := c.PlayerName()
	weapon := ""
	if e, ok := d.Store.EquipSlot(username, protocol.EquipmentWeapon); ok {
		weapon = e.Key
	}
	valid := false
	for _, s := range AttackStylesFor(weapon) {
		if s == style {
			valid = true
			break
		}
	}
	if !valid {
		log.Printf("m6: [%s] Invalid attack style.", username)
		return
	}
	styleMu.Lock()
	attackStyles[username] = style
	styleMu.Unlock()
	attackRange := AttackRangeFor(weapon, style)
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketEquipment, protocol.EquipmentStyle, map[string]any{
		"attackStyle": style, "attackRange": attackRange,
	}))
	d.Bus.Broadcast(d.World.SyncFrame(c.InstanceID(), c.TileX(), c.TileY()))
	log.Printf("m6: %s attack style %d (range %d)", c.InstanceID(), style, attackRange)
}
