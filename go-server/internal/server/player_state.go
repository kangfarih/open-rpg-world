// Player state — drops/loot + XP/skills + SQLite persist slice.
//
// Drops mirror packages/server mob.getDrops: one roll on the mob's personal
// `drops` list plus one roll per `dropTables` entry (tables.json), chance vs
// DROP_PROBABILITY 100000 (modules.ts:625). Single drop -> Item entity
// (type 2); multiple -> LootBag entity (type 8, take-all on Target/Step).
// Blink [26] fires LOOT_BLINK_MS before destroy at LOOT_DESPAWN_MS
// (defaults 20s/30s; truth ItemDefaults are 30s/34s - shortened so loot
// never outlives the e2e windows).
//
// XP mirrors player.handleExperience (2 XP per damage, Health 1/4 + school
// share) and resourceskill (table experience on exhaust). Thresholds are the
// RuneScape LevelExp port (loader.ts loadLevels). Level-up broadcasts Sync
// + Experience Skill, plus the Healing FX as the heal anim (no Heal packet:
// connection.ts handleHeal would render a +HP splat we never earned).
//
// Persist is SQLite via modernc.org/sqlite (pure Go, no cgo), DB_PATH or
// data.db next to the binary (go-server/data.db, gitignored via
// go-server/*.db*). WAL + NORMAL, single writer (dbMu), dirty flush every
// 10s + synchronously on disconnect + on SIGTERM/SIGINT.
package server

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/meta"
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/persist"
	"rpg-world-server/internal/player"
	"rpg-world-server/internal/resets"
	worldcore "rpg-world-server/internal/world"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// XP formula (formulas.ts LevelExp + loader.ts loadLevels, RuneScape curve).
// ---------------------------------------------------------------------------
//
// Canonical owner: internal/player (xp.go). The wrappers below keep the
// root names every other seam uses with identical values.
func expToLevel(xp int) int { return player.ExpToLevel(xp) }

func nextExp(xp int) int { return player.NextExp(xp) }

// ---------------------------------------------------------------------------
// Drop tables (packages/server/data/tables.json + mobs.json).
// ---------------------------------------------------------------------------
//
// Canonical owner: internal/entity (loot.go). The aliases + wrappers below
// keep the root names every other seam uses with identical values.
type dropEntry struct {
	Key     string `json:"key"`
	Chance  int    `json:"chance"`
	Count   int    `json:"count"`
	Quest   string `json:"quest"`
	Backend string `json:"-"`
}

type (
	dropJSON = entity.DropJSON
	dropDef  = entity.Drop
	lootBag  = entity.Loot
)

const dropProbability = entity.DropProbability

func dataPath(name string) string {
	return resourceDataPath(name)
}

func loadM5Tables() { entity.LoadLootTables() }

// rollEntry ports mob.getRandomItem (canonical owner: internal/entity
// RollEntry). Gate filtering lives in rollEntryGated (M11 quest gates
// evaluate against the killer's progression); this rolls uniformly over the
// entries given.
func rollEntry(entries []dropJSON, level int) (key string, count int, ok bool) {
	return entity.RollEntry(entries, level)
}

// getDrops ports mob.getDrops for a mob key (canonical owner:
// internal/entity GetDrops).
func getDrops(mobKey string) []dropDef {
	return entity.GetDrops(mobKey)
}

// getDropsFor ports mob.getDrops with a killer context (canonical owner:
// internal/entity GetDropsFor).
func getDropsFor(mobKey, username string) []dropDef {
	return entity.GetDropsFor(mobKey, username)
}

// rollEntryGated filters quest/achievement-gated entries by the killer's
// progression, then rolls uniformly (canonical owner: internal/entity
// RollEntryGated).
func rollEntryGated(username string, entries []dropJSON, level int) (string, int, bool) {
	return entity.RollEntryGated(username, entries, level)
}

// ---------------------------------------------------------------------------
// Loot entities (Item type 2 / LootBag type 8).
// ---------------------------------------------------------------------------
//
// Canonical owner: internal/entity (loot.go: registry, spawn paths,
// blink/destroy timers, Who payloads). The wrappers below keep the root
// names every other seam uses with identical frames/logs.
// lootWorld implements entity.LootWorld over the world Registry (position
// index + Spawn/Blink/Despawn fan-out). Frames keep their exact shapes;
// the package builds them with internal/protocol (same constructors).
type lootWorld struct{}

func (lootWorld) SetEntityPos(inst string, x, y int)     { worldcore.SetEntityPos(inst, x, y) }
func (lootWorld) EntityPos(inst string) (int, int, bool) { return worldcore.EntityPos(inst) }
func (lootWorld) RemoveEntity(inst string)               { worldcore.RemoveEntity(inst) }
func (lootWorld) Broadcast(frames ...[]any)              { worldcore.Broadcast(frames...) }

func nearWalkable(x, y int) (int, int) { return entity.NearWalkable(x, y) }

// spawnLoot drops the roll at the corpse: single -> Item, multi -> LootBag
// (take-all on Target/Step; lootbag menu Open flow deferred, logged).
func spawnLoot(mobKey string, cx, cy int, owner string) {
	entity.SpawnLoot(mobKey, cx, cy, owner)
}

func blinkLoot(inst string) { entity.BlinkLoot(inst) }

func destroyLoot(inst, why string) { entity.DestroyLoot(inst, why) }

// ---------------------------------------------------------------------------
// Effect entities (type 9).
// ---------------------------------------------------------------------------
//
// Canonical owner: internal/entity (effects.go: registry, duration loading,
// spawn/destroy). The adapter below implements entity.EffectWorld over the
// same worldcore seam as lootWorld.

// effectWorld implements entity.EffectWorld over the world Registry.
type effectWorld struct{}

func (effectWorld) SetEntityPos(inst string, x, y int) { worldcore.SetEntityPos(inst, x, y) }
func (effectWorld) RemoveEntity(inst string)           { worldcore.RemoveEntity(inst) }
func (effectWorld) Broadcast(frames ...[]any)          { worldcore.Broadcast(frames...) }

// spawnEffect creates a timed effect entity at (x, y) with auto-despawn
// (effectentities.json duration or 4000ms default).
func spawnEffect(key string, x, y int) string {
	return entity.SpawnEffect(key, x, y)
}

// ---------------------------------------------------------------------------
// Player state: inventory + skills + XP.
// ---------------------------------------------------------------------------

type slotDef struct {
	Key   string
	Count int
	Ench  Enchantments // item enchantments {enchantmentId: {level}} (M12)
}

// slotEnchJSON serializes a slot's enchantments for the inventory DB
// column ({} when none).
func slotEnchJSON(s slotDef) string {
	if s.Ench == nil {
		return "{}"
	}
	raw, err := json.Marshal(s.Ench)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// slotEnchParse restores a slot's enchantments from the DB column.
func slotEnchParse(raw string) Enchantments {
	if raw == "" || raw == "{}" {
		return nil
	}
	ench := Enchantments{}
	if err := json.Unmarshal([]byte(raw), &ench); err != nil {
		return nil
	}
	if len(ench) == 0 {
		return nil
	}
	return ench
}

type skillDef struct {
	Level int
	XP    int
}

type playerState struct {
	X, Y   int
	Level  int
	HP     int
	Rank   int // Modules.Ranks value (persist rank column parity)
	HomeX  int // home-point x (/sethome bind; respawn target)
	HomeY  int // home-point y (/sethome bind; respawn target)
	LastDailyReset  int64 // epoch ms of last daily reset (0 = never)
	LastWeeklyReset int64 // epoch ms of last weekly reset (0 = never)
	Inv    []slotDef
	Bank   []slotDef
	Equip  []slotDef // length ModulesEquipmentCount; Count 0 = empty slot
	Skills map[int]*skillDef
}

var (
	pstateMu sync.Mutex
	pstates  = map[string]*playerState{}
)

// Skill ids mirror Modules.Skills order (canonical owner: internal/player).
const (
	SkillLumberjacking = player.SkillLumberjacking
	SkillAccuracy      = player.SkillAccuracy
	SkillArchery       = player.SkillArchery
	SkillHealth        = player.SkillHealth
	SkillMagic         = player.SkillMagic
	SkillMining        = player.SkillMining
	SkillStrength      = player.SkillStrength
	SkillDefense       = player.SkillDefense
	SkillFishing       = player.SkillFishing
	SkillForaging      = player.SkillForaging
)

func skillName(id int) string { return player.SkillName(id) }

func combatSkill(id int) bool { return player.CombatSkill(id) }

func playerStateFor(key string) *playerState {
	pstateMu.Lock()
	defer pstateMu.Unlock()
	st, ok := pstates[key]
	if !ok {
		// New player: level 1, full HP (39 + 1*30 = 69).
		// Tutorial spawn (TS parity: TUTORIAL_SPAWN_POINT '133,562').
		st = &playerState{X: entity.HeroSpawnX, Y: entity.HeroSpawnY, Level: 1, HP: meta.HeroMaxHPForLevel(1),
			HomeX: entity.HeroSpawnX, HomeY: entity.HeroSpawnY,
			Skills: map[int]*skillDef{}}
		pstates[key] = st
	}
	if st.Skills == nil {
		st.Skills = map[int]*skillDef{}
	}
	// Equipment is a fixed 12-slot array (Modules.Equipment): nil or short
	// slices would panic on slot indexing, so normalize lazily here.
	if len(st.Equip) != ModulesEquipmentCount {
		eq := make([]slotDef, ModulesEquipmentCount)
		copy(eq, st.Equip)
		st.Equip = eq
	}
	return st
}

func combatLevelLocked(st *playerState) int {
	levels := make(map[int]int, len(st.Skills))
	for id, s := range st.Skills {
		levels[id] = s.Level
	}
	return player.CombatLevel(levels)
}

// connByInstance moved to internal/world (D2a): use
// worldcore.Find[*playerConn](inst) at the former call sites.

// addXP awards skill XP (canonical owner: internal/player AddXP). The
// state transition runs under pstateMu here; frames, level-up fanout and
// logs live in the package with identical shapes/text.
func addXP(c *playerConn, key string, skill, amount int) int {
	var pc *player.Conn
	if c != nil {
		cc := c
		pc = &player.Conn{
			Instance: c.Instance,
			Username: c.Username,
			Send: func(frames ...[]any) {
				_ = gnet.Send(cc.Conn, frames...)
			},
		}
	}
	return player.AddXP(xpDeps(), pc, key, skill, amount)
}

// xpDeps wires the player.XP seams to the root globals (playerStateFor/pstateMu
// state, gnet/worldcore transport, welcomePlayer Sync payload, persist
// dirty set, world XP event).
func xpDeps() player.Deps {
	return player.Deps{
		ApplyAward: func(key string, skill, amount int) player.AwardResult {
			st := playerStateFor(key)
			pstateMu.Lock()
			defer pstateMu.Unlock()
			s, ok := st.Skills[skill]
			if !ok {
				s = &skillDef{Level: 1}
				st.Skills[skill] = s
			}
			prev := s.Level
			s.XP += amount
			if s.XP < 0 {
				s.XP = 0 // TS subtraction floors at 0 (no negative XP store)
			}
			s.Level = expToLevel(s.XP)
			if s.Level < 1 {
				s.Level = 1
			}
			if combatSkill(skill) {
				st.Level = combatLevelLocked(st)
			}
			return player.AwardResult{
				Prev: prev, Level: s.Level, XP: s.XP,
				CombatLevel: st.Level, X: st.X, Y: st.Y,
			}
		},
		Broadcast: func(frames ...[]any) { worldcore.Broadcast(frames...) },
		MarkDirty: markDirty,
		SyncFrame: func(instance string, x, y, combatLevel int) []any {
			ph := welcomePlayer(instance)
			ph.X, ph.Y = x, y
			ph.Level = intp(combatLevel)
			return pkt(PacketSync, ph)
		},
		Lookup: func(instance string) (player.Conn, bool) {
			c, _ := worldcore.Find[*playerConn](instance)
			if c == nil {
				return player.Conn{}, false
			}
			cc := c
			return player.Conn{
				Instance: c.Instance,
				Username: c.Username,
				Send: func(frames ...[]any) {
					_ = gnet.Send(cc.Conn, frames...)
				},
			}, true
		},
		XPBoost: worldXPBoost,
	}
}

func percentage(xp int) float64 { return player.Percentage(xp) }

// awardCombatXP ports player.handleExperience (canonical owner:
// internal/player AwardCombatXP). The class flags mirror
// weapon.isArcher/isMagic (heroIsArcher/heroIsMagic over the equipped
// weapon, with precedence over the style switch exactly as in TS);
// Style carries the attack-style store (controller.AttackStyleFor:
// last explicit switch, else the weapon's first style) and HasMana
// the hasManaForAttack gate (player.ts:1700 — current mana >= weapon
// manaCost via the same heroManaCost lookup the swing gate uses;
// 0 for non-magic weapons so melee never halves).
func awardCombatXP(c *playerConn, key string, damage int, archer, mage bool) {
	var pc *player.Conn
	d := xpDeps()
	if c != nil {
		cc := c
		pc = &player.Conn{
			Instance: c.Instance,
			Username: c.Username,
			Send: func(frames ...[]any) {
				_ = gnet.Send(cc.Conn, frames...)
			},
		}
		d.Style = func() int { return controller.AttackStyleFor(econDeps(), cc.Username) }
		d.HasMana = func() bool { return abilities.ManaFor(cc.Instance) >= heroManaCost(cc.Username) }
	}
	player.AwardCombatXP(d, pc, key, damage, archer, mage)
}

// awardGatherXP is the M4-hook successor: table experience on exhaust
// (canonical owner: internal/player GatherXP).
func gatherXP(attackerInstance, skill string, xp int) {
	player.GatherXP(xpDeps(), attackerInstance, skill, xp)
}

// addItem stacks (items.json stackable) or appends; returns slot index.
func addItem(key, itemKey string, count int) int {
	return addItemEnch(key, itemKey, count, nil)
}

// addItemEnch adds with enchantments (M12 crafting/enchant/trade paths);
// stacking only merges when both stacks have identical enchantment maps —
// a non-nil ench always takes a fresh slot.
func addItemEnch(key, itemKey string, count int, ench Enchantments) int {
	loadM5Tables()
	st := playerStateFor(key)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if ench == nil && entity.Stackable(itemKey) {
		for i, s := range st.Inv {
			if s.Key == itemKey {
				st.Inv[i].Count += count
				return i
			}
		}
	}
	st.Inv = append(st.Inv, slotDef{Key: itemKey, Count: count, Ench: ench})
	return len(st.Inv) - 1
}

// pickup takes one loot entity for the player: inventory + Container Add +
// Despawn. Step path calls with the on-tile instance; Target path with the
// clicked instance (range-lenient, logged - truth enforces adjacency via
// getDistance, slice 1 keeps pickup observable).
func pickup(c *playerConn, inst string) bool {
	if c == nil || c.Conn == nil || inst == "" {
		return false
	}
	l, ok := entity.FindLoot(inst)
	if !ok {
		return false
	}
	// Owner gate (lootbag.ts parity): someone else's bag never takes here.
	// The Step path routes bags through openLootBagFor (Open only); the
	// Target path checks the same gate before Open — either way a denied bag
	// must not fall through to take-all.
	if l.Bag && lootBagOwnerDenied(c, l.Owner) {
		return false
	}
	dx := c.Sess.PlayerX - l.X
	if dx < 0 {
		dx = -dx
	}
	dy := c.Sess.PlayerY - l.Y
	if dy < 0 {
		dy = -dy
	}
	if dx+dy > 1 {
		log.Printf("m5: %s takes %s from %d tiles (lenient pickup)", c.Instance, inst, dx+dy)
	}
	// Inventory cap (lootbag take path parity: misc:NO_SPACE, no partial take).
	nItems := 0
	for _, it := range l.Items {
		if it.Key != "" {
			nItems++
		}
	}
	st := playerStateFor(c.Username)
	pstateMu.Lock()
	invLen := len(st.Inv)
	pstateMu.Unlock()
	if ModulesInventorySize-invLen < nItems {
		notifyPlayer(c, "misc:NO_SPACE")
		return false
	}
	for _, it := range l.Items {
		if it.Key == "" {
			continue // taken lootbag slot (hole — single-take path)
		}
		idx := addItem(c.Username, it.Key, it.Count)
		_ = gnet.Send(c.Conn, pktOp(PacketContainer, ContainerAdd, containerData{
			Type: ContainerTypeInventory,
			Slot: &slotData{Index: idx, Key: it.Key, Count: it.Count, Enchantments: map[string]any{}},
		}))
		// Statistics: owner pickups count as drops (player.ts:1268 parity —
		// only when the loot owner matches the picker).
		if l.Owner != "" && l.Owner == c.Username {
			statsAddDrop(c, it.Key, it.Count)
		}
	}
	markDirty(c.Username)
	destroyLoot(inst, "picked up by "+c.Instance)
	return true
}

// pickupAt steps onto loot: any loot on the player's tile is taken —
// single Items instantly, bags via the Open menu (lootbag.ts parity:
// handleMovementStop opens bags instead of taking them).
func pickupAt(c *playerConn) {
	pickupAtTile(c, c.Sess.PlayerX, c.Sess.PlayerY)
}

// pickupAtTile takes loot lying on (x,y) (Step destination path).
func pickupAtTile(c *playerConn, x, y int) {
	if c == nil || c.Conn == nil {
		return
	}
	if inst, ok := entity.FindLootAt(x, y); ok {
		if entity.IsBag(inst) {
			openLootBagFor(c, inst)
			return
		}
		pickup(c, inst)
	}
}

// trackPos records the authoritative tile and marks the row dirty.
// The tracked plateauLevel refreshes on the same update (handler.ts:333
// player.plateauLevel parity — every authoritative position update).
func trackPos(c *playerConn) {
	if c == nil || c.Username == "" {
		return
	}
	st := playerStateFor(c.Username)
	pstateMu.Lock()
	st.X, st.Y = c.Sess.PlayerX, c.Sess.PlayerY
	pstateMu.Unlock()
	markDirty(c.Username)
	plateauTrack(c)
}

// registerLoot adds a pre-built loot entry to the registry without any
// timers (M10 chest drops: persistent items with no blink/expiry — Node
// chest items never expire on their own). Pickup routing is shared.
func registerLoot(inst, key string, count, x, y int, owner string) {
	entity.RegisterLoot(inst, key, count, x, y, owner)
}

// isLoot reports whether id is a live loot entity.
func isLoot(id string) bool { return entity.IsLoot(id) }

// lootPayload rebuilds the Spawn payload for a loot instance (Who path;
// canonical owner: internal/entity LootPayload).
func lootPayload(inst string) (any, bool) { return entity.LootPayload(inst) }

// ---------------------------------------------------------------------------
// Player combat (C Combat {instance,target} vs killable mobs).
// ---------------------------------------------------------------------------

func mobMaxHP(instance, mobKey string) int {
	if instance == "m1" {
		return 30 // spawnFrames flat default.
	}
	if p, ok := entity.Profile(mobKey); ok && p != nil && p.HitPoints > 0 {
		return p.HitPoints
	}
	return 20
}

// handlePlayerAttack routes one hero swing at the combat rat, the boss dummy,
// or the plain-mode rat. Damage pipeline mirrors applyBossHitLocked
// (Animation + Combat Hit + Points); mob death uses the Despawn path.
// M9: engine-registered mobs (m-rat-1, m1, any m9test spawn) take the
// mob.ts/handler.ts path (Points + retaliate + engine respawn) instead of
// the legacy per-instance blocks.
func handlePlayerAttack(c *playerConn, target string) {
	if target == "" || c == nil {
		return
	}
	// Arrow gate (combat.ts:208-210 sendRangedAttack): a non-magic archer
	// with no arrows stops the loop — the per-swing Go equivalent is a
	// silent no-swing (no frames).
	if heroIsArcher(c.Username) && !heroIsMagic(c.Username) && !heroHasArrows(c.Username) {
		log.Printf("m5: %s bow swing refused (no arrows)", c.Instance)
		return
	}
	// Hero damage formula (formulas.ts getMaxDamage + getDamage parity):
	// accuracy-weighted roll on [0, maxDamage] where maxDamage =
	// (equipBonus + skillLevel) * 1.25 + 5 * style * strengthBuff.
	combatRandMu.Lock()
	dmg := heroDamageRoll(c.Username, c.Instance, target, combatRand)
	combatRandMu.Unlock()
	// M9: engine mobs first — Points/retaliate/death/respawn/loot live in
	// the engine now (mobPlayerHit -> killMob -> spawnLoot).
	// M11_HERODMG debug accelerator (mirrors M9_MOBDMG): keeps the e2e's
	// 140-HP mobs in a few-swing kill range.
	dmg = int(float64(dmg) * heroDamageMult())
	if m := mobFor(target); m != nil {
		if m.dead {
			log.Printf("m5: %s swings at dead %s (ignored)", c.Instance, target)
			return
		}
		// TS-exact plateau gate (character.ts isNearTarget): only RANGED
		// attackers (attackRange > 1) shooting UP a plateau are refused
		// (silent no-swing, see entity.RangedBlocked). The hero has no
		// range model (welcomePlayer AttackRange 1; player.sync weapon
		// recompute unmodeled), so the hero always swings as melee (1).
		m.mu.Lock()
		mobPlateau := m.plateau
		m.mu.Unlock()
		if entity.RangedBlocked(1, plateauGet(c.Instance), mobPlateau) {
			log.Printf("m5: %s swings at %s across plateaus (refused)", c.Instance, target)
			return
		}
		// Magic mana gate (player/handler.ts handleAttack): staff swings
		// need mana — LOW_MANA + no swing when short.
		if !heroMagicGate(c) {
			return
		}
		// Arrow consumption (handler.ts:238-242 + equipments.ts:188-198):
		// one arrow decremented per shot; slot clears at count 0.
		if heroIsArcher(c.Username) && !heroIsMagic(c.Username) {
			heroDecrementArrows(c.Username)
		}
		abSetTarget(c.Instance, target)
		worldcore.Broadcast(pkt(PacketAnimation, animationData{Instance: c.Instance, Action: ActionAttack}))
		// Hero damage-type roll (player.ts getDamageType): the TYPE (+ AoE
		// flag) changes the hit effect; damage is from the formula above.
		hitType, aoe := lockedHeroDamageType(c.Username)
		hit := HitData{Type: hitType, Damage: dmg}
		if aoe > 0 {
			hit.Aoe = intp(aoe)
		}
		worldcore.Broadcast(pktOp(PacketCombat, CombatHit, combatData{
			Instance: c.Instance, Target: target,
			Hit: hit,
		}))
		mobPlayerHit(m, c, dmg)
		// TS combat.ts sendAttack order: hit, then target.addStatusEffect.
		// Skip corpses (TS death clears status; the engine has no death
		// clear, so a tracker entry on a corpse would leak).
		m.mu.Lock()
		victimDead := m.dead
		m.mu.Unlock()
		if !victimDead {
			applyHitStatus(target, hitType)
		}
		// Explosive splash damages nearby mobs (character.ts handleAoE).
		if aoe > 0 {
			explosiveSplash(m, c, dmg)
		}
		// Bloodsucking proc on the attacker (character.ts handleBloodsucking
		// — inside hit(), before the death check, so it runs here too).
		if ok, level := heroBloodsucking(c.Username); ok && lockedBloodsuckRoll() {
			if heal := bloodsuckHeal(dmg, level); heal >= 1 {
				econVitals{}.HealHero(c.Instance, heal, 0)
			}
		}
		// TS combat.ts poison-on-hit: a poisonous weapon poisons the victim
		// with level-based chance (formulas.ts getPoisonChance: randomInt(0,
		// 235-level) < POISON_CHANCE(15)). No damage gate — poison applies
		// on any hit (even 0-damage rolls) if the weapon is poisonous.
		if abHeroWeaponPoisonous(c.Username) {
			mobLevel := 1
			m.mu.Lock()
			mobLevel = m.prof.Level
			m.mu.Unlock()
			if heroPoisonChance(mobLevel) {
				abApplyPoison(target)
			}
		}
		awardCombatXP(c, c.Username, dmg, heroIsArcher(c.Username), heroIsMagic(c.Username))
		return
	}
	switch target {
	case combatDummyInstance:
		combatMu.Lock()
		if combatDead {
			combatMu.Unlock()
			log.Printf("m5: %s swings at dead boss (ignored)", c.Instance)
			return
		}
		// Magic mana gate + damage-type roll (same hero-swing rules as the
		// engine-mob path; no splash victim on the legacy dummy).
		if !heroMagicGate(c) {
			combatMu.Unlock()
			return
		}
		// Arrow consumption (handler.ts:238-242 parity).
		if heroIsArcher(c.Username) && !heroIsMagic(c.Username) {
			heroDecrementArrows(c.Username)
		}
		hitType, _ := lockedHeroDamageType(c.Username)
		abSetTarget(c.Instance, target)
		applyBossHitLocked(c.Instance, dmg, hitType, nil, false, -1, true)
		combatMu.Unlock()
		if ok, level := heroBloodsucking(c.Username); ok && lockedBloodsuckRoll() {
			if heal := bloodsuckHeal(dmg, level); heal >= 1 {
				econVitals{}.HealHero(c.Instance, heal, 0)
			}
		}
		awardCombatXP(c, c.Username, dmg, heroIsArcher(c.Username), heroIsMagic(c.Username))
		// Boss loot spawns exactly once inside applyBossHitLocked on the
		// killing blow (boot.go death path) — no second spawn here (double
		// boss-loot fix: the per-swing duplicate is deleted).
	default:
		// M9: engine-registered mobs were handled above; anything else is
		// not killable (legacy note kept from slice 1).
		log.Printf("m5: %s attacks %s (not killable)", c.Instance, target)
	}
}

// ---------------------------------------------------------------------------
// SQLite persist.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// SQLite persist (single-writer Store in internal/persist).
// ---------------------------------------------------------------------------
//
// The SQL + dirty set live in internal/persist (persist.Store: Open,
// EnsureSchema, MarkDirty/MarkClean/DirtyList, WritePlayer/LoadPlayer,
// Snapshot, FlushDirty, Close — moved here verbatim, identical schema,
// WAL+NORMAL pragmas, identical log text). This section keeps the root
// names every other seam uses — dbConn/dbMu for the m11/m13/social/
// abilities/ops direct-table access, and initPlayerState/markDirty/flushDirty/
// savePlayerSync/loadPlayerState/loginWelcome with unchanged signatures — and
// delegates to the store (converting playerState <-> persist.State).
// dbConn aliases the store handle (single connection, SetMaxOpenConns(1)).
// Ticker/goroutine ownership stays in root: initPlayerState starts the 10s dirty
// flush and the SIGTERM/SIGINT final-flush handler exactly as before.
// Lock discipline: never hold dbMu and pstateMu at the same time
// (playerSnapshot needs pstateMu). flushDirty/savePlayerSync snapshot first, then
// write under dbMu; loadPlayerState holds dbMu across the store read and takes
// pstateMu only to install the result.

var (
	dbConn       *sql.DB
	dbMu         sync.Mutex
	persistStore *persist.Store
)

func dbPath() string {
	if p := os.Getenv("DB_PATH"); p != "" {
		return p
	}
	return "data.db"
}

func initPlayerState() {
	loadM5Tables()
	entity.ConfigureLoot(entity.LootDeps{
		World:       lootWorld{},
		Walkable:    func(x, y int) bool { return !blocked(x, y) },
		DoubleDrops: worldDoubleDrops,
		Gate:        questDropGated,
		DataPath:    resourceDataPath,
	})
	entity.ConfigureEffects(entity.EffectDeps{
		World: effectWorld{},
	})
	st, err := persist.Open(dbPath())
	if err != nil {
		log.Fatalf("m5: %v", err)
	}
	persistStore = st
	dbConn = st.DB()
	log.Printf("m5: sqlite open %s (WAL+NORMAL)", dbPath())
	checkWorldHashGate()   // D3: world.json sha256 vs meta.world_hash (warn; strict only)
	abConfigure()          // abilities: wire the session seams (table handle + helpers)
	petConfigure()         // entity: wire the companion seams (inventory + combat)
	socConfigure()         // social: wire friends/guilds/hub seams (tables + presence)
	worldConfigureWarps()  // controller: wire the warp-runner seams (gates + teleport)
	worldConfigureEvents() // controller: wire the event fan-out seam (global notices)
	opsConfigure()         // app: wire the ops seams (API providers + console world)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			flushDirty()
		}
	}()
	// R1 drain lifecycle (same boot step): SIGTERM/SIGINT -> DRAINING (no
	// new conns, sim continues) -> empty-or-timeout -> flush barrier ->
	// exit. With zero players this is the old final-flush-and-exit.
	startDrainDriver()
	// R1 shard role: hub Client registration (all-in-one starts none).
	startShardClient()
	// Reset ticker: 60s background sweep for daily/weekly boundary crossings.
	startResetTicker()
}

func markDirty(key string) {
	if key == "" || dbConn == nil || persistStore == nil {
		return
	}
	// Guest sessions are never persisted (player.ts:2358 save() early-returns
	// for isGuest). This is the choke point every gameplay path funnels
	// through, so one guard covers quests, skills, inventory and statistics.
	// Exception: device-identified guests (persistentGuests) DO persist.
	if isGuestKey(key) && !isPersistentGuest(key) {
		return
	}
	persistStore.MarkDirty(key)
}

func playerSnapshot(key string) *playerState {
	pstateMu.Lock()
	defer pstateMu.Unlock()
	st, ok := pstates[key]
	if !ok {
		return nil
	}
	cp := &playerState{X: st.X, Y: st.Y, Level: st.Level, HP: st.HP, Rank: st.Rank,
		HomeX: st.HomeX, HomeY: st.HomeY,
		LastDailyReset: st.LastDailyReset, LastWeeklyReset: st.LastWeeklyReset,
		Skills: map[int]*skillDef{}}
	cp.Inv = append(cp.Inv, st.Inv...)
	cp.Bank = append(cp.Bank, st.Bank...)
	cp.Equip = append(cp.Equip, st.Equip...)
	for id, s := range st.Skills {
		cp.Skills[id] = &skillDef{Level: s.Level, XP: s.XP}
	}
	return cp
}

// toPersist converts an in-memory player state to the persist snapshot
// (inventory + equipment enchantments serialized to the DB column format;
// bank rows carry no enchantments, matching the bank table).
func toPersist(st *playerState) persist.State {
	ps := persist.State{
		X: st.X, Y: st.Y, Level: st.Level, HP: st.HP, Rank: st.Rank,
		HomeX: st.HomeX, HomeY: st.HomeY,
		LastDailyReset: st.LastDailyReset, LastWeeklyReset: st.LastWeeklyReset,
		Skills: make(map[int]persist.Skill, len(st.Skills)),
	}
	for _, s := range st.Inv {
		ps.Inv = append(ps.Inv, persist.Slot{Key: s.Key, Count: s.Count, Ench: slotEnchJSON(s)})
	}
	for _, s := range st.Bank {
		ps.Bank = append(ps.Bank, persist.Slot{Key: s.Key, Count: s.Count})
	}
	for _, e := range st.Equip {
		ps.Equip = append(ps.Equip, persist.Slot{Key: e.Key, Count: e.Count, Ench: slotEnchJSON(e)})
	}
	for id, s := range st.Skills {
		ps.Skills[id] = persist.Skill{Level: s.Level, XP: s.XP}
	}
	return ps
}

// persistToM5 converts a persist snapshot back to the in-memory state,
// normalizing the fixed ModulesEquipmentCount slot array (slot indexing
// must never panic) exactly like the old loadPlayerState tail.
func persistToM5(ps persist.State) *playerState {
	st := &playerState{X: ps.X, Y: ps.Y, Level: ps.Level, HP: ps.HP, Rank: ps.Rank,
		HomeX: ps.HomeX, HomeY: ps.HomeY,
		LastDailyReset: ps.LastDailyReset, LastWeeklyReset: ps.LastWeeklyReset,
		Skills: map[int]*skillDef{}}
	for _, s := range ps.Inv {
		st.Inv = append(st.Inv, slotDef{Key: s.Key, Count: s.Count, Ench: slotEnchParse(s.Ench)})
	}
	for _, s := range ps.Bank {
		st.Bank = append(st.Bank, slotDef{Key: s.Key, Count: s.Count})
	}
	eslots := make([]slotDef, 0, len(ps.Equip))
	for _, s := range ps.Equip {
		eslots = append(eslots, slotDef{Key: s.Key, Count: s.Count, Ench: slotEnchParse(s.Ench)})
	}
	eq := make([]slotDef, ModulesEquipmentCount)
	copy(eq, eslots)
	st.Equip = eq
	for id, s := range ps.Skills {
		st.Skills[id] = &skillDef{Level: s.Level, XP: s.XP}
	}
	return st
}

func writePlayer(key string, st *playerState) {
	if persistStore == nil {
		return
	}
	// Statistics counters ride the same row write as a JSON blob in the
	// additive `statistics` table (key-aware call site: the converters stay
	// key-agnostic so handoff.go keeps compiling untouched).
	ps := toPersist(st)
	snap := statsCopyOf(key)
	ps.Stats = persist.StatsBlob{
		MobKills: snap.MobKills, MobExamines: snap.MobExamines,
		Resources: snap.Resources, Drops: snap.Drops,
		PvPKills: snap.PvPKills, PvPDeaths: snap.PvPDeaths,
		CreationTime: snap.CreationTime, TotalTimePlayed: snap.TotalTimePlayed,
		LastLogin: snap.LastLogin, LoginCount: snap.LoginCount,
	}
	_ = persistStore.WritePlayer(key, ps)
}

func flushDirty() {
	if dbConn == nil || persistStore == nil {
		return
	}
	// Lock discipline: never hold dbMu and pstateMu at the same time
	// (playerSnapshot needs pstateMu). Snapshot first, then write under dbMu.
	keys := persistStore.DirtyList()
	for _, key := range keys {
		st := playerSnapshot(key)
		dbMu.Lock()
		if st != nil {
			writePlayer(key, st)
		}
		persistStore.MarkClean(key)
		dbMu.Unlock()
	}
}

// savePlayerSync flushes one player immediately (disconnect path).
// Lock discipline: never hold dbMu and pstateMu at the same time.
func savePlayerSync(key string) {
	if dbConn == nil || persistStore == nil || key == "" {
		return
	}
	// Guest sessions are never written (player.ts:2358 save() isGuest guard);
	// this is the synchronous disconnect/flush path, so it needs the same
	// guard as markDirty. Exception: device-identified guests DO persist.
	if isGuestKey(key) && !isPersistentGuest(key) {
		return
	}
	st := playerSnapshot(key)
	dbMu.Lock()
	if st != nil {
		writePlayer(key, st)
	}
	persistStore.MarkClean(key)
	dbMu.Unlock()
}

// loadPlayerState restores a player row (Welcome from DB when the instance is known).
func loadPlayerState(key string) (*playerState, bool) {
	if dbConn == nil || persistStore == nil || key == "" {
		return nil, false
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	ps, ok := persistStore.LoadPlayer(key)
	if !ok {
		return nil, false
	}
	st := persistToM5(ps)
	// Statistics counters restore into the stats registry (statistics.load
	// parity for the gameplay fields).
	statsInstall(key, ps.Stats)
	pstateMu.Lock()
	pstates[key] = st
	pstateMu.Unlock()
	log.Printf("m5: loaded %s (pos %d,%d level %d inv %d bank %d skills %d)", key, st.X, st.Y, st.Level, len(st.Inv), len(st.Bank), len(st.Skills))
	return st, true
}

// loginWelcome builds the Welcome payload: DB row when the login username
// is known, else the fresh hero. Also queues Container + Skill batches so a
// reconnect visibly restores inventory/skills.
func loginWelcome(c *playerConn, username string) (PlayerData, [][]any) {
	key := sanitizeUsername(username)
	if key == "" {
		// Instance fallback (legacy behaviour for a login with no name);
		// instance IDs are generated and never trigger the filter.
		key = sanitizeUsername(c.Instance)
	}
	c.Username = key
	var st *playerState
	if c.Guest {
		if isPersistentGuest(key) {
			// Device-identified returning guest: try to load saved state.
			if loaded, ok := loadPlayerState(key); ok {
				st = loaded
			} else {
				// First login for this device: fresh state, enable persistence.
				st = playerStateFor(key)
				markDirty(key)
			}
		} else {
			// TS guest (incoming.ts:235): player.load(Creator.serialize(player)) —
			// a fresh default state, never restored from and never written to the
			// database (player.ts:2358 save() returns early for guests).
			st = playerStateFor(key)
			markGuestKey(key)
		}
	} else if loaded, ok := loadPlayerState(key); ok {
		st = loaded
	} else {
		st = playerStateFor(key)
		markDirty(key)
	}
	// Statistics + reset stamps are database writes, so ephemeral guests skip
	// them. Device-identified persistent guests get full stats tracking.
	var loginResetFired []resets.Kind
	if !c.Guest || isPersistentGuest(key) {
		// Statistics: record login lifecycle fields (creationTime on first
		// login, lastLogin, loginCount) after the counters are restored.
		statsRecordLogin(key)
		// Daily/weekly reset: check at login so a player who logs in after a
		// boundary crossing sees the reset immediately (the 60s ticker handles
		// players already online). Notifications are sent after the Welcome
		// frame lands (notifyPlayer needs a connected client).
		loginResetFired = checkLoginResets(key, time.Now())
	}
	// Rank durability (database.setRank parity): a persisted offline /setrank
	// lands on the session at login. Fresh rows carry 0 (None), a no-op.
	c.rank = st.Rank
	chatStateFor(c).rank = st.Rank
	c.Sess.PlayerX, c.Sess.PlayerY = st.X, st.Y
	worldcore.SetEntityPos(c.Instance, st.X, st.Y)
	worldcore.UpdateRegion(c, st.X, st.Y)
	ph := welcomePlayer(c.Instance)
	ph.X, ph.Y = st.X, st.Y
	if st.Level > 0 {
		ph.Level = intp(st.Level)
	}
	if st.HP > 0 {
		ph.HitPoints = intp(st.HP)
	}
	var extra [][]any
	pstateMu.Lock()
	slots := make([]any, 0, len(st.Inv))
	for i, s := range st.Inv {
		slots = append(slots, map[string]any{
			"index": i, "key": s.Key, "count": s.Count, "enchantments": enchAny(s.Ench),
		})
	}
	skills := make([]any, 0, len(st.Skills))
	for id, s := range st.Skills {
		skills = append(skills, map[string]any{
			"type": id, "experience": s.XP, "level": s.Level,
			"percentage": percentage(s.XP), "nextExperience": nextExp(s.XP),
			"combat": combatSkill(id),
		})
	}
	pstateMu.Unlock()
	if len(slots) > 0 {
		extra = append(extra, pktOp(PacketContainer, ContainerBatch, containerData{
			Type: ContainerTypeInventory, Data: &containerBatch{Slots: slots},
		}))
	}
	if len(skills) > 0 {
		extra = append(extra, pktOp(PacketSkill, SkillBatch, map[string]any{
			"skills": skills, "cheater": false,
		}))
	}
	if len(st.Bank) > 0 {
		bslots := make([]any, 0, len(st.Bank))
		for i, s := range st.Bank {
			bslots = append(bslots, map[string]any{
				"index": i, "key": s.Key, "count": s.Count, "enchantments": enchAny(s.Ench),
			})
		}
		extra = append(extra, pktOp(PacketContainer, ContainerBatch, containerData{
			Type: ContainerTypeBank, Data: &containerBatch{Slots: bslots},
		}))
	}
	// Equipment restore: Batch frame with the equipped entries (client
	// player.equip each -> sprites + profile). Empty slots are omitted,
	// matching Node's DB loader (skips falsy keys).
	eqs := make([]any, 0, len(st.Equip))
	for t, e := range st.Equip {
		if e.Key == "" || e.Count < 1 {
			continue
		}
		data := equipmentData(t, e.Key, e.Count, true)
		data["enchantments"] = enchAny(e.Ench)
		eqs = append(eqs, data)
	}
	if len(eqs) > 0 {
		extra = append(extra, pktOp(PacketEquipment, EquipmentBatch, equipBatchData{Equipments: eqs}))
	}
	// Daily/weekly reset notifications: appended to the login frame batch so
	// the client sees them right after Welcome+Map+Inventory+Skills.
	for _, k := range loginResetFired {
		label := "daily"
		if k == resets.Weekly {
			label = "weekly"
		}
		extra = append(extra, pktOp(PacketNotification, NotificationText, notificationPacketData{
			Message: "reset:" + label,
		}))
	}
	return ph, extra
}
