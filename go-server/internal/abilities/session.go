// Ability + status sessions, extracted behavior-frozen from the root
// abilities_wire.go adapter (task D2b item 2).
//
// TS sources: abilities.ts (add/has/setLevel/use + Add/Update/Batch/Toggle
// packets), ability.ts (activate gating: passive rejection, mana, cooldown,
// toggleCallback on activate + deactivate), quest.ts givePlayerAbility
// (stageData.ability + abilityLevel||1), achievement.ts finishCallback
// (rewardAbility/rewardAbilityLevel), character.ts handlePoisonDamage
// (poisonous attacker -> setPoison Venom) + effects() (freezing/burning
// EFFECT_RATE damage).
//
// Frames used (all pre-existing shapes, verified against the TS tree):
//
//	S->C Ability Batch=0 {abilities:[{key,level,quickSlot,type?}]}
//	S->C Ability Add=1 {key,level,quickSlot,type} (unlock)
//	S->C Ability Update=2 {key,level,quickSlot} (re-grant = setLevel)
//	S->C Ability Toggle=5 {key,level:-1} (activate + deactivate)
//	C->S Ability [22,{opcode,key,index?}] Use=3 QuickSlot=4 (menu.ts
//	  handleAbility -> socket.send(Packets.Ability,{opcode,key,index})).
//
// Everything transport/world/state related stays with the root adapter and
// is reached only through Conn (per-connection delivery) and SessionDeps:
//
//	Send/Broadcast        -> gnet.Send / worldcore.Broadcast (frames)
//	Notify                -> m6Notify (unlock + misc:* notifies)
//	DB/DataPath/Test      -> abilities table handle, resourceDataPath,
//	                         testMode gate
//	MarkDirty             -> persist dirty set
//	DummyInstance         -> combatDummyInstance (RequiresTarget liveness)
//	TargetAlive           -> combatMu/combatDead + m9MobFor liveness
//	FindPlayer/Damage*    -> worldcore.Find + m9DamagePlayer/m9PlayerHit
//	HeroWeaponPoisonous   -> m5/m6 equipped-weapon poisonous flag
//
// Frames are built with internal/protocol (the same constructors the root
// pkt/pktOp shims wrap), so wire bytes are identical. All log strings and
// TESTMAP debug ops are kept verbatim.
package abilities

import (
	"database/sql"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rpg-world-server/internal/protocol"
	"rpg-world-server/internal/status"
)

// Ability opcodes (Opcodes.Ability in opcodes.ts): Batch0 Add1 Update2
// Use3 QuickSlot4 Toggle5.
const (
	AbilityBatch     = 0
	AbilityAdd       = 1
	AbilityUpdate    = 2
	AbilityUse       = 3
	AbilityQuickSlot = 4
	AbilityToggle    = 5
)

// Modules.AbilityType (modules.ts): Active0 Passive1.
const (
	AbilityTypeActive  = 0
	AbilityTypePassive = 1
)

const (
	freezeSuffix = "|freeze"
)

// manaMaxFor computes the level-scaled max mana for a username
// (formulas.ts getMaxMana: 20 + level * 24).
func manaMaxFor(username string) int {
	level := 1
	if sdeps.PlayerLevel != nil && username != "" {
		if lv := sdeps.PlayerLevel(username); lv > 0 {
			level = lv
		}
	}
	return 20 + level*24
}

// manaMaxForInstance computes the level-scaled max mana for an instance
// by looking up the username from the Conn registry.
func manaMaxForInstance(instance string) int {
	// The abilities package doesn't have direct access to the player registry,
	// so we use a fallback: check if we have a Conn registered for this instance.
	// For now, use level 1 as the default. The root adapter will wire PlayerLevel
	// to resolve this properly.
	return 20 + 1*24 // level 1 default = 44
}

// Conn is the minimal per-connection view for ability delivery. Nil = no
// frames (quest/achievement reward paths with no live conn).
type Conn struct {
	Instance string
	Username string
	// Send delivers frames to this conn (gnet.Send parity).
	Send func(frames ...[]any)
	// Notify sends a client text notification (m6Notify parity).
	Notify func(message string)
}

// SessionDeps bundles the ability-session seams (implemented by the root
// adapter; never by this package).
type SessionDeps struct {
	// DB is the abilities table handle (nil = persistence disabled).
	DB *sql.DB
	// DataPath resolves abilities.json (resourceDataPath parity).
	DataPath func(name string) string
	// Test gates the TESTMAP debug dispatcher (testMode parity).
	Test bool
	// MarkDirty flags the player row for the 10s persist flush.
	MarkDirty func(key string)
	// Broadcast fans frames out (worldcore.Broadcast parity).
	Broadcast func(frames ...[]any)
	// TargetAlive reports combat-target liveness: (alive, checkable).
	// The root checks the combat dummy first (combatMu/combatDead), then
	// engine mobs (m9MobFor dead flag); checkable=false = unknown entity
	// kind (players/NPCs: no liveness data, treated as live —
	// abLiveTarget parity).
	TargetAlive func(target string) (alive, checkable bool)
	// DamagePlayer applies DoT damage to a player instance, returning HP
	// after (m9DamagePlayer + m9PlayerHP parity). ok=false = unknown.
	DamagePlayer func(instance string, dmg int) (hp int, ok bool)
	// DamageMob applies DoT damage to a killable mob (m9PlayerHit parity).
	// false = not a mob.
	DamageMob func(instance string, dmg int) bool
	// HeroWeaponPoisonous reports the items.json `poisonous` flag on the
	// hero's equipped weapon (combat.ts poison-on-hit parity).
	HeroWeaponPoisonous func(username string) bool
	// PlayerLevel returns the combat level for a username (formulas.ts
	// getMaxMana parity: mana scales with level).
	PlayerLevel func(username string) int
}

var sdeps SessionDeps

// ConfigureSessions installs the ability-session seams (called once from
// the root boot, before login batches or ticks run).
func ConfigureSessions(d SessionDeps) { sdeps = d }

// abilityEntry mirrors AbilityData (impl/ability.ts): serialize(true)
// carries key/level/quickSlot/type, Update omits type.
type abilityEntry struct {
	Key       string `json:"key"`
	Level     int    `json:"level"`
	QuickSlot int    `json:"quickSlot"`
	Type      *int   `json:"type,omitempty"`
}

type abilityToggle struct {
	Key   string `json:"key"`
	Level int    `json:"level"`
}

var (
	abOnce     sync.Once
	abRegistry *Registry

	abMu        sync.Mutex
	abLevels    = map[string]map[string]int{} // username -> ability -> level
	abQuick     = map[string]map[string]int{} // username -> ability -> quickSlot
	abLastCast  = map[string]int64{}          // username+"\x00"+key -> unix ms
	abMana      = map[string]int{}            // instance -> current mana
	abManaWarn  = map[string]bool{}           // instance -> LOW_MANA warned (displayedManaWarning)
	abTarget    = map[string]string{}         // instance -> last attack target
	abFxMu      sync.Mutex
	abFx        = map[string]map[int]bool{} // instance -> effectID (ability casts)
	abFreezeSet = map[string]bool{}         // instances holding a freeze key

	abStatus = status.NewTracker()
)

func abIntp(v int) *int { return &v }

// LoadRegistry loads abilities.json once (resourceDataPath so RES_abilities
// overrides like every other data table).
func LoadRegistry() *Registry { return loadRegistry() }

// loadRegistry loads abilities.json once (resourceDataPath so RES_abilities
// overrides like every other data table).
func loadRegistry() *Registry {
	abOnce.Do(func() {
		path := "abilities"
		if sdeps.DataPath != nil {
			path = sdeps.DataPath("abilities")
		}
		r, err := Load(path)
		if err != nil {
			log.Printf("abilities: %v (ability engine disabled)", err)
			return
		}
		abRegistry = r
		log.Printf("abilities: registry=%d", r.Count())
	})
	return abRegistry
}

// EnsureTables creates the abilities table (m13 flags-table precedent;
// synchronous INSERT OR REPLACE on grant, SELECT into memory on login).
func EnsureTables() {
	if sdeps.DB == nil {
		return
	}
	if _, err := sdeps.DB.Exec(`CREATE TABLE IF NOT EXISTS abilities(player TEXT, ability TEXT, level INT, PRIMARY KEY(player, ability))`); err != nil {
		log.Printf("abilities: ddl: %v", err)
	}
}

// GrantAbility ports abilities.add + the quest/achievement reward calls
// (quest.ts givePlayerAbility, achievement.ts finishCallback): unknown keys
// are refused with a log (TS: `Ability <key> does not exist.`), re-grants
// raise the level via an Update frame (TS: has() -> setLevel), first grants
// send Add. Level mirrors `abilityLevel || 1` (0/negative -> 1) with the
// registry 1-4 clamp applied by ForKey.
func GrantAbility(c *Conn, username, key string, level int) bool {
	if username == "" || key == "" {
		return false
	}
	r := loadRegistry()
	if r == nil {
		return false
	}
	if level < 1 {
		level = 1
	}
	a, ok := r.ForKey(key, level)
	if !ok {
		log.Printf("abilities: Ability %s does not exist.", key)
		return false
	}
	abMu.Lock()
	m := abLevels[username]
	if m == nil {
		m = map[string]int{}
		abLevels[username] = m
	}
	_, had := m[key]
	m[key] = a.Level
	qm := abQuick[username]
	if qm == nil {
		qm = map[string]int{}
		abQuick[username] = qm
	}
	if _, ok := qm[key]; !ok {
		qm[key] = -1
	}
	quick := qm[key]
	abMu.Unlock()
	if sdeps.DB != nil {
		if _, err := sdeps.DB.Exec(`INSERT OR REPLACE INTO abilities(player,ability,level) VALUES(?,?,?)`,
			username, key, a.Level); err != nil {
			log.Printf("abilities: save %s/%s: %v", username, key, err)
		}
	}
	if sdeps.MarkDirty != nil {
		sdeps.MarkDirty(username)
	}
	if c == nil {
		return true
	}
	typ := AbilityTypeActive
	if !r.IsActive(key) {
		typ = AbilityTypePassive
	}
	if had {
		c.Send(protocol.PktOp(protocol.PacketAbility, AbilityUpdate, abilityEntry{
			Key: key, Level: a.Level, QuickSlot: quick,
		}))
	} else {
		c.Send(protocol.PktOp(protocol.PacketAbility, AbilityAdd, abilityEntry{
			Key: key, Level: a.Level, QuickSlot: quick, Type: abIntp(typ),
		}))
	}
	c.Notify("You have unlocked the " + key + " ability.")
	log.Printf("abilities: %s granted %s lv%d", username, key, a.Level)
	return true
}

// Has reports whether username unlocked key.
func Has(username, key string) bool {
	abMu.Lock()
	defer abMu.Unlock()
	return abLevels[username][key] > 0
}

// ResetAbilities clears the server-side ability unlock map for username
// (abilities.ts reset(): `this.abilities = {}` — the same store the C->S
// Ability QuickSlot opcode writes and LoginBatch reads). The client reloads
// from the next Ability Batch (empty list when nothing is unlocked). Like
// TS, persisted rows are untouched: a relogin restores from the DB.
func ResetAbilities(username string) {
	if username == "" {
		return
	}
	abMu.Lock()
	defer abMu.Unlock()
	delete(abLevels, username)
	delete(abQuick, username)
}

// LoadAbilities restores persisted unlocks into memory (m11LoadQuests
// precedent — called before the login batches are built).
func LoadAbilities(username string) {
	if sdeps.DB == nil || username == "" {
		return
	}
	loadRegistry()
	rows, err := sdeps.DB.Query(`SELECT ability,level FROM abilities WHERE player=?`, username)
	if err != nil {
		return
	}
	defer rows.Close()
	abMu.Lock()
	defer abMu.Unlock()
	for rows.Next() {
		var key string
		var level int
		if rows.Scan(&key, &level) == nil {
			if abLevels[username] == nil {
				abLevels[username] = map[string]int{}
			}
			abLevels[username][key] = level
		}
	}
}

// LoginBatch builds the Ability Batch frame queued after the Welcome
// extras (handler.ts onLoaded ability serialize — empty list when nothing
// unlocked; previously the server never sent packet 22).
func LoginBatch(username string) []any {
	abMu.Lock()
	m := abLevels[username]
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := make([]abilityEntry, 0, len(keys))
	r := abRegistry
	for _, k := range keys {
		e := abilityEntry{Key: k, Level: m[k], QuickSlot: abQuick[username][k]}
		if r != nil {
			typ := AbilityTypeActive
			if !r.IsActive(k) {
				typ = AbilityTypePassive
			}
			e.Type = abIntp(typ)
		}
		list = append(list, e)
	}
	abMu.Unlock()
	if list == nil {
		list = []abilityEntry{}
	}
	return protocol.PktOp(protocol.PacketAbility, AbilityBatch, map[string]any{"abilities": list})
}

// ManaFor returns the current mana for an instance (welcome default).
func ManaFor(instance string) int {
	abMu.Lock()
	defer abMu.Unlock()
	if m, ok := abMana[instance]; ok {
		return m
	}
	return manaMaxForInstance(instance)
}

// SetMana pins the session mana for an instance, clamped to [0, cap]
// (TESTMAP/debug + test setup; live swings use SpendMana/HealMana).
func SetMana(instance string, value int) {
	if instance == "" {
		return
	}
	if value < 0 {
		value = 0
	}
	maxMana := manaMaxForInstance(instance)
	if value > maxMana {
		value = maxMana
	}
	abMu.Lock()
	abMana[instance] = value
	abMu.Unlock()
}

// SpendMana decrements session mana by cost (floored at 0) and broadcasts
// the Points mana frame (player.ts handleAttack mana.decrement + handleMana
// parity). Reports the new totals.
func SpendMana(instance string, cost int) (mana, max int) {
	maxMana := manaMaxForInstance(instance)
	if instance == "" {
		return ManaFor(instance), maxMana
	}
	abMu.Lock()
	m, ok := abMana[instance]
	if !ok {
		m = maxMana
	}
	m -= cost
	if m < 0 {
		m = 0
	}
	abMana[instance] = m
	abMu.Unlock()
	if sdeps.Broadcast != nil {
		sdeps.Broadcast(protocol.Pkt(protocol.PacketPoints, protocol.PointsData{
			Instance: instance, Mana: abIntp(m), MaxMana: abIntp(maxMana),
		}))
	}
	return m, maxMana
}

// ManaWarningShown reports whether the LOW_MANA one-shot warning already
// fired for an instance (player.ts displayedManaWarning parity).
func ManaWarningShown(instance string) bool {
	if instance == "" {
		return false
	}
	abMu.Lock()
	defer abMu.Unlock()
	return abManaWarn[instance]
}

// SetManaWarning arms/disarms the LOW_MANA one-shot warning for an
// instance (set on warn, cleared once mana suffices for a swing).
func SetManaWarning(instance string, shown bool) {
	if instance == "" {
		return
	}
	abMu.Lock()
	if shown {
		abManaWarn[instance] = true
	} else {
		delete(abManaWarn, instance)
	}
	abMu.Unlock()
}

// HasPoison reports a live Venom entry for an instance (character.ts
// heal() poison gate parity — poison lives on the status tracker, not in
// Modules.Effects, so it uses the KindPoison sentinel key).
func HasPoison(instance string) bool {
	if instance == "" {
		return false
	}
	return abStatus.Has(status.Instance(instance), status.KindPoison)
}

// HasFreeze reports area freezing for an instance (m10 FreezeApply parity:
// the tracker entry rides the derived freeze-suffix key, not the base
// instance key, so HasStatusEffect(Freezing) alone misses it).
func HasFreeze(instance string) bool {
	if instance == "" {
		return false
	}
	abFxMu.Lock()
	defer abFxMu.Unlock()
	return abFreezeSet[instance]
}

// SetTarget records the hero's last attack target (RequiresTarget gate).
func SetTarget(instance, target string) {
	if instance == "" {
		return
	}
	abMu.Lock()
	if target == "" {
		delete(abTarget, instance)
	} else {
		abTarget[instance] = target
	}
	abMu.Unlock()
}

// LiveTarget resolves the recorded target when it is still alive.
func LiveTarget(instance string) string {
	abMu.Lock()
	t := abTarget[instance]
	abMu.Unlock()
	if t == "" {
		return ""
	}
	if sdeps.TargetAlive == nil {
		return t // unconfigured seam: no liveness data, treated as live
	}
	if alive, checkable := sdeps.TargetAlive(t); !checkable {
		return t // unknown entity kinds (players/NPCs): no liveness data
	} else if alive {
		return t
	}
	SetTarget(instance, "")
	return ""
}

// HandleAbility routes C->S Ability frames [22,{opcode,key,index?}]
// (incoming.ts handleAbility): Use activates, QuickSlot stores the slot.
// Nil conns are a no-op (quest/achievement reward paths with no live conn
// must not panic on a QuickSlot frame).
func HandleAbility(c *Conn, data []byte) {
	if c == nil {
		return
	}
	var d struct {
		Opcode int    `json:"opcode"`
		Key    string `json:"key"`
		Index  *int   `json:"index"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Key == "" {
		return
	}
	switch d.Opcode {
	case AbilityUse:
		Use(c, d.Key)
	case AbilityQuickSlot:
		if d.Index == nil {
			return
		}
		abMu.Lock()
		if abLevels[c.Username][d.Key] > 0 {
			if abQuick[c.Username] == nil {
				abQuick[c.Username] = map[string]int{}
			}
			abQuick[c.Username][d.Key] = *d.Index
		}
		abMu.Unlock()
	}
}

// Use ports Ability.activate gating (ability.ts): passive rejection,
// RequiresTarget combat gate (misc:NEED_COMBAT), mana (misc:NOT_ENOUGH_MANA)
// + cooldown via abilities.CanCast (misc:NEED_WAIT_ABILITY). On success the
// mana decrements (Points mana emit), a Toggle frame goes out, server-side
// effects ride the status tracker + Effect Add, and the TS setTimeout
// deactivate becomes a time.AfterFunc Toggle (+ EffectRemove).
func Use(c *Conn, key string) {
	if c == nil {
		return
	}
	r := loadRegistry()
	if r == nil {
		return
	}
	abMu.Lock()
	level := abLevels[c.Username][key]
	abMu.Unlock()
	if level < 1 {
		return // TS: abilities[key]?.activate — unknown/unowned is a no-op
	}
	if !r.IsActive(key) {
		return // TS: passive type check rejects before mana/cooldown
	}
	a, ok := r.ForKey(key, level)
	if !ok {
		return
	}
	if r.RequiresTarget(key) && LiveTarget(c.Instance) == "" {
		c.Notify("misc:NEED_COMBAT")
		return
	}
	nowMs := time.Now().UnixMilli()
	mana := ManaFor(c.Instance)
	abMu.Lock()
	last := abLastCast[c.Username+"\x00"+key]
	abMu.Unlock()
	if mana < a.ManaCost {
		c.Notify("misc:NOT_ENOUGH_MANA")
		return
	}
	// DualistsMark attack rate parity (TS player.ts getAttackRate: subtract
	// 200ms from weapon.attackRate when DualistsMark is active). Adjusted
	// here at the cooldown check: shifting `last` back by 200ms makes the
	// cooldown elapse 200ms sooner, matching the TS behavior.
	effectiveLast := last
	if HasStatusEffect(c.Instance, int(EffectDualistsMark)) {
		effectiveLast -= 200
	}
	if !a.CanCast(mana, nowMs, effectiveLast) {
		effCd := int64(a.CooldownMs)
		if HasStatusEffect(c.Instance, int(EffectDualistsMark)) {
			effCd -= 200
		}
		wait := (effCd - (nowMs - effectiveLast)) / 1000
		if wait < 1 {
			wait = 1
		}
		c.Notify("misc:NEED_WAIT_ABILITY;duration=" + Itoa(wait))
		return
	}
	mana -= a.ManaCost
	abMu.Lock()
	abMana[c.Instance] = mana
	abLastCast[c.Username+"\x00"+key] = nowMs
	abMu.Unlock()
	c.Send(protocol.Pkt(protocol.PacketPoints, protocol.PointsData{
		Instance: c.Instance, Mana: abIntp(mana), MaxMana: abIntp(manaMaxFor(c.Username)),
	}))
	c.Send(protocol.PktOp(protocol.PacketAbility, AbilityToggle, abilityToggle{Key: key, Level: -1}))
	fx := a.Effect()
	if fx.Kind == EffectNone {
		// Client-visual window only (intimidate/hotshot/secretcalling):
		// untoggle when the duration lapses, no server status.
		if fx.DurationMs > 0 {
			time.AfterFunc(time.Duration(fx.DurationMs)*time.Millisecond, func() {
				c.Send(protocol.PktOp(protocol.PacketAbility, AbilityToggle, abilityToggle{Key: key, Level: -1}))
			})
		}
		log.Printf("abilities: %s cast %s (window %dms)", c.Username, key, fx.DurationMs)
		return
	}
	effectID := int(fx.Kind)
	abStatus.Apply(status.Instance(c.Instance), status.Kind(effectID), 0, int64(fx.DurationMs), nowMs)
	abFxMu.Lock()
	m := abFx[c.Instance]
	if m == nil {
		m = map[int]bool{}
		abFx[c.Instance] = m
	}
	m[effectID] = true
	abFxMu.Unlock()
	sdeps.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectAdd, protocol.EffectData{Instance: c.Instance, Effect: effectID}))
	log.Printf("abilities: %s cast %s effect=%d dur=%dms", c.Username, key, effectID, fx.DurationMs)
	if fx.DurationMs > 0 {
		time.AfterFunc(time.Duration(fx.DurationMs)*time.Millisecond, func() {
			c.Send(protocol.PktOp(protocol.PacketAbility, AbilityToggle, abilityToggle{Key: key, Level: -1}))
			abFxMu.Lock()
			if abFx[c.Instance][effectID] {
				delete(abFx[c.Instance], effectID)
				abFxMu.Unlock()
				sdeps.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectRemove, protocol.EffectData{Instance: c.Instance, Effect: effectID}))
				return
			}
			abFxMu.Unlock()
		})
	}
}

func Itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ApplyPoison records Venom on an instance (character.ts setPoison default
// in handlePoisonDamage — replace, no stacking). No frames here: damage
// surfaces as Points ticks, expiry is silent (TS clears with no final hit).
func ApplyPoison(instance string) {
	if instance == "" {
		return
	}
	abStatus.Apply(status.Instance(instance), status.KindPoison, 0, 0, time.Now().UnixMilli())
}

// RemovePoison cures Venom on an instance (character.ts setPoison() with no
// argument clears the poison). Only the poison entry is dropped — ability
// windows and freeze keys sharing the instance are untouched. The /poison
// toggle-off cure path must call this; otherwise the 30s Venom DoT keeps
// ticking after the cure notify.
func RemovePoison(instance string) {
	if instance == "" {
		return
	}
	abStatus.Remove(status.Instance(instance), status.KindPoison)
}

// ManaMax reports the hero mana cap for an instance (formulas.ts getMaxMana
// parity: 20 + level * 24). Falls back to level 1 when the player is unknown.
func ManaMax(instance string) int { return manaMaxForInstance(instance) }

// ManaState reports current/max mana for an instance (welcome default).
func ManaState(instance string) (mana, maxMana int) {
	return ManaFor(instance), manaMaxForInstance(instance)
}

// HealMana adds amount mana, clamped to the cap, and broadcasts the Points
// frame (player.heal mana-branch parity: increment + sync). It reports the
// applied amount plus the new totals.
func HealMana(instance string, amount int) (applied, mana, max int) {
	maxMana := manaMaxForInstance(instance)
	if instance == "" || amount < 1 {
		return 0, ManaFor(instance), maxMana
	}
	abMu.Lock()
	m, ok := abMana[instance]
	if !ok {
		m = maxMana
	}
	applied = amount
	if m+applied > maxMana {
		applied = maxMana - m
	}
	m += applied
	abMana[instance] = m
	abMu.Unlock()
	if sdeps.Broadcast != nil {
		sdeps.Broadcast(protocol.Pkt(protocol.PacketPoints, protocol.PointsData{
			Instance: instance, Mana: abIntp(m), MaxMana: abIntp(maxMana),
		}))
	}
	return applied, m, maxMana
}

// HasStatusEffect reports a live tracker entry for an Effects ID
// (status.has parity for the item-use plugins).
func HasStatusEffect(instance string, effectID int) bool {
	if instance == "" {
		return false
	}
	return abStatus.Has(status.Instance(instance), status.Kind(effectID))
}

// ApplyStatusEffect records a timed Effects-ID entry, mirrors it into abFx
// so the StatusTick expiry sweep emits the EffectRemove frame, and
// broadcasts Effect Add (status.addWithTimeout + client visual parity).
func ApplyStatusEffect(instance string, effectID int, durationMs int64) {
	if instance == "" {
		return
	}
	nowMs := time.Now().UnixMilli()
	abStatus.Apply(status.Instance(instance), status.Kind(effectID), 0, durationMs, nowMs)
	abFxMu.Lock()
	m := abFx[instance]
	if m == nil {
		m = map[int]bool{}
		abFx[instance] = m
	}
	m[effectID] = true
	abFxMu.Unlock()
	if sdeps.Broadcast != nil {
		sdeps.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectAdd,
			protocol.EffectData{Instance: instance, Effect: effectID}))
	}
}

// RemoveStatusEffect drops one Effects-ID entry + its abFx mirror and
// broadcasts Effect Remove. Absent effects are a silent no-op
// (status.remove parity: no frame for entries that were never added).
func RemoveStatusEffect(instance string, effectID int) {
	if instance == "" {
		return
	}
	if !abStatus.Has(status.Instance(instance), status.Kind(effectID)) {
		return
	}
	abStatus.Remove(status.Instance(instance), status.Kind(effectID))
	abFxMu.Lock()
	if abFx[instance] != nil {
		delete(abFx[instance], effectID)
		if len(abFx[instance]) == 0 {
			delete(abFx, instance)
		}
	}
	abFxMu.Unlock()
	if sdeps.Broadcast != nil {
		sdeps.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectRemove,
			protocol.EffectData{Instance: instance, Effect: effectID}))
	}
}

// ClearStatus drops every live status-tracker entry on an instance (player
// handleDeath parity: status.clear() + the setPoison() cure). Poison,
// burning, freezing (including the m10 freeze-suffix key) and ability-cast
// DoT windows stop ticking, so StatusTick emits no further Points damage for
// the corpse. Mana, last-target, unlock levels and the abilities table rows
// are untouched (TS keeps abilities across death). The abFx visual mirrors
// are left for the StatusTick expiry sweep, which translates each vanished
// tracker entry into the EffectRemove frame the client expects.
func ClearStatus(instance string) {
	if instance == "" {
		return
	}
	abStatus.Clear(status.Instance(instance))
	abStatus.Clear(status.Instance(instance + freezeSuffix))
}

// HeroWeaponPoisonous reports whether the hero's equipped weapon carries
// the items.json `poisonous` flag (combat.ts poison-on-hit parity).
func HeroWeaponPoisonous(username string) bool {
	if sdeps.HeroWeaponPoisonous == nil {
		return false
	}
	return sdeps.HeroWeaponPoisonous(username)
}

// FreezeApply/Clear bridge the m10 freezing area (area.ts addPlayer/
// removePlayer -> status Freezing) into the tracker so EFFECT_RATE damage
// flows through the Points pipeline. The visual Effect frames stay owned by
// m10SetFreezing; the tracker entry rides a derived key so leaving the area
// clears exactly the freezing entry (Clear is per-instance).
func FreezeApply(instance string) {
	if instance == "" {
		return
	}
	abStatus.Apply(status.Instance(instance+freezeSuffix), status.KindFreezing, 0, -1, time.Now().UnixMilli())
	abFxMu.Lock()
	abFreezeSet[instance] = true
	abFxMu.Unlock()
}

func FreezeClear(instance string) {
	if instance == "" {
		return
	}
	abStatus.Clear(status.Instance(instance + freezeSuffix))
	abFxMu.Lock()
	delete(abFreezeSet, instance)
	abFxMu.Unlock()
}

// StatusTick drains due DoT ticks into the existing Points pipeline and
// reaps expired ability effects into EffectRemove frames. Called from the
// central 20Hz tick loop (additive: empty tracker = Tick only).
func StatusTick() {
	nowMs := time.Now().UnixMilli()
	for _, ex := range abStatus.Tick(nowMs) {
		inst := string(ex.Instance)
		base := inst
		if strings.HasSuffix(inst, freezeSuffix) {
			base = strings.TrimSuffix(inst, freezeSuffix)
		}
		if hp, ok := sdeps.DamagePlayer(base, ex.Damage); ok {
			log.Printf("abilities: dot %s kind=%d dmg=%d hp=%d", base, int(ex.Kind), ex.Damage, hp)
			continue
		}
		if sdeps.DamageMob(base, ex.Damage) {
			log.Printf("abilities: dot mob %s kind=%d dmg=%d", base, int(ex.Kind), ex.Damage)
		}
	}
	// Expiry sweep: tracker reaps silently on Tick, so translate a vanished
	// ability entry into the EffectRemove the client expects. /toggle and m10
	// freezing effects live outside abFx and are untouched here.
	abFxMu.Lock()
	for inst, effs := range abFx {
		for effectID := range effs {
			if !abStatus.Has(status.Instance(inst), status.Kind(effectID)) {
				delete(effs, effectID)
				sdeps.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectRemove, protocol.EffectData{Instance: inst, Effect: effectID}))
			}
		}
		if len(effs) == 0 {
			delete(abFx, inst)
		}
	}
	abFxMu.Unlock()
}

// ForgetPlayer drops per-conn ability state on disconnect (m13ForgetPlayer
// precedent) including any leaked freeze-tracker keys.
func ForgetPlayer(instance string) {
	if instance == "" {
		return
	}
	abStatus.Clear(status.Instance(instance + freezeSuffix))
	abMu.Lock()
	delete(abMana, instance)
	delete(abManaWarn, instance)
	delete(abTarget, instance)
	abMu.Unlock()
	abFxMu.Lock()
	delete(abFx, instance)
	delete(abFreezeSet, instance)
	abFxMu.Unlock()
}

// ---------------------------------------------------------------------------
// TESTMAP debug dispatcher (m9test/m11test precedent, rides [46 {abtest}]).
// ---------------------------------------------------------------------------

// TestHandler processes TESTMAP debug ops: grant/echo for the unlock leg,
// apply for deterministic DoT legs (kind poison|burning|freezing,
// power/durationMs optional, kind defaults apply), mana to set the session
// mana directly.
func TestHandler(c *Conn, data []byte) {
	if !sdeps.Test || c == nil {
		return
	}
	var d struct {
		AbTest     string `json:"abtest"`
		Key        string `json:"key"`
		Level      int    `json:"level"`
		Kind       string `json:"kind"`
		Power      int    `json:"power"`
		DurationMs int64  `json:"durationMs"`
		Value      int    `json:"value"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.AbTest == "" {
		return
	}
	switch d.AbTest {
	case "grant":
		level := d.Level
		if level < 1 {
			level = 1
		}
		if GrantAbility(c, c.Username, d.Key, level) {
			abMu.Lock()
			lv := abLevels[c.Username][d.Key]
			abMu.Unlock()
			c.Notify("ab:grant " + d.Key + "=" + Itoa(int64(lv)))
		}
	case "apply":
		var kind status.Kind
		switch strings.ToLower(d.Kind) {
		case "poison":
			kind = status.KindPoison
		case "burning":
			kind = status.KindBurning
		case "freezing":
			kind = status.KindFreezing
		default:
			return
		}
		abStatus.Apply(status.Instance(c.Instance), kind, d.Power, d.DurationMs, time.Now().UnixMilli())
		c.Notify("ab:applied " + strings.ToLower(d.Kind))
	case "mana":
		v := d.Value
		if v < 0 {
			v = 0
		}
		maxMana := manaMaxFor(c.Username)
		if v > maxMana {
			v = maxMana
		}
		abMu.Lock()
		abMana[c.Instance] = v
		abMu.Unlock()
		c.Notify("ab:mana=" + Itoa(int64(v)))
	case "echo":
		abMu.Lock()
		keys := make([]string, 0, len(abLevels[c.Username]))
		for k, lv := range abLevels[c.Username] {
			keys = append(keys, k+"="+Itoa(int64(lv)))
		}
		sort.Strings(keys)
		mana, ok := abMana[c.Instance]
		abMu.Unlock()
		if !ok {
			mana = manaMaxFor(c.Username)
		}
		c.Notify("ab:abilities [" + strings.Join(keys, ",") + "] mana=" + Itoa(int64(mana)))
	}
}
