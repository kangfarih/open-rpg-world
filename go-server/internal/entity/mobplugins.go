// Package entity — mob behavior plugins.
//
// Faithful port of packages/server/data/plugins/mobs/ (index.ts loader +
// ant, forestdragon, hellhound, ogrelord, piratecaptain, queenant, santa,
// skeletonking, spider + default.ts base).
//
// TS hook model (mob/handler.ts wires: onMovement/onHit/onDeath/onRespawn +
// combat.onAttack/onLoop) maps onto the engine paths in mob.go:
//
//	handleHit       -> pluginOnHit   (HitMob, after Points, before death)
//	handleDeath     -> pluginOnDeath (KillMob, after quest credit)
//	handleRespawn   -> pluginOnSpawn (RespawnMob, state reset)
//	combat.onAttack -> pluginOnAttack (strikeMob tail)
//	combat.onLoop   -> PluginTick    (m9Tick second pass, adapter-driven)
//	handleMovement  -> PluginTick    (queen-ant minion follow only)
//
// Gating: every hook opens with a registry lookup by mob key; unlisted keys
// (including spider, whose TS subclass is empty, and every default mob) miss
// and return before touching anything, so the default path is byte-identical
// to the pre-plugin engine. Minion-instance checks (worker ants) are keyed
// off the tracked-minion set, so wild mobs sharing a key keep default AI.
//
// Locking: OnHit/OnDeath run on the HitMob/KillMob path (m9Mu-free), so they
// may use the full PluginHost. OnAttack runs inside strikeMob with the boss
// lock held (may use state + DamageHero(nil) + broadcasts only — never
// spawn/remove/lookup). PluginTick runs with no locks held (adapter calls it
// outside the registry lock), so the full host is available.
//
// Divergences (stub engine has no counterpart; state is kept TS-exact so the
// numbers/strings stay portable):
//   - damage types (Terror) and projectile names (fireball, gift1-6): the
//     Strike pipeline only emits HitsNormal, so these are recorded in plugin
//     state and asserted in tests but do not change frames.
//   - hellhound/forestdragon/queenant ranged checks use Manhattan distance
//     only; TS also weighs target.isRanged()/moving, which the stub tracks.
//   - queen-ant minions target/follow the queen by instance link (Mob.Target
//     only addresses players); the link lives in plugin state.
//   - minion lifetime cap uses the TS minionsSpawned counter (never
//     decremented except on boss death), not the live-minion count.
package entity

import (
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Seeded RNG (Utils.randomInt parity: min and max both inclusive).
// ---------------------------------------------------------------------------

var pluginRand = rand.New(rand.NewSource(time.Now().UnixNano()))

// PluginRandInt mirrors Utils.randomInt(min, max): uniform in [min, max].
func PluginRandInt(min, max int) int {
	if max <= min {
		return min
	}
	return min + pluginRand.Intn(max-min+1)
}

// SetPluginRandSeed reseeds the plugin RNG (deterministic unit tests).
func SetPluginRandSeed(seed int64) {
	pluginRand = rand.New(rand.NewSource(seed))
}

// ---------------------------------------------------------------------------
// Registry (TS index.ts map) + per-tick set.
// ---------------------------------------------------------------------------

// mobPluginKeys mirrors the index.ts default export. Spider is absent on
// purpose: its TS subclass adds nothing over Default, so it keeps the
// byte-identical default path.
var mobPluginKeys = map[string]bool{
	"ogrelord":      true,
	"skeletonking":  true,
	"queenant":      true,
	"ant":           true,
	"forestdragon":  true,
	"santa":         true,
	"piratecaptain": true,
	"hellhound":     true,
}

// mobPluginTickKeys lists bosses with per-tick logic in TS: combatLoop
// overrides (forestdragon, hellhound, queenant), the ant heal interval, and
// the ogrelord dialogue interval. Santa/piratecaptain/skeletonking have no
// tick logic, so StepMob never consults them.
var mobPluginTickKeys = map[string]bool{
	"ogrelord":     true,
	"queenant":     true,
	"ant":          true,
	"forestdragon": true,
	"hellhound":    true,
}

// HasMobPlugin reports whether key has a behavior plugin (gate for hit,
// death, spawn and attack hooks).
func HasMobPlugin(key string) bool { return mobPluginKeys[key] }

// HasMobPluginTick reports whether key needs the per-tick hook.
func HasMobPluginTick(key string) bool { return mobPluginTickKeys[key] }

// ---------------------------------------------------------------------------
// TS-exact per-boss constants.
// ---------------------------------------------------------------------------

var (
	skPositions  = [][2]int{{139, 782}, {125, 785}}
	skMaxMinions = 6

	hhPositions  = [][2]int{{1059, 765}, {1047, 768}}
	hhMaxMinions = 8

	ogrePositions = [][2]int{{338, 160}, {346, 164}, {342, 169}, {335, 164}}
	ogreKeys      = []string{"ogre", "ironogre"}
	ogreDialogues = []string{
		"The great ogre lord will trample over you!",
		"No, do not touch my onions!",
		"Me smash you!",
	}
	ogreWaveTalk     = "My minions will surely help defeat you!"
	ogreDialogueWait = 15 * time.Second

	pirateMaxMinions = 8
	pirateTeleportN  = 12 // canTeleport: randomInt(0,12)==4 (code-exact 1/13)
	pirateSpawnN     = 8  // canSpawnMob: randomInt(0,8)==4 (code-exact 1/9)
	pirateTeleports  = [][2]int{{935, 743}, {930, 755}, {920, 753}, {918, 743}}
	pirateMinions    = [][2]int{{923, 745}, {931, 746}, {929, 754}, {923, 752}}

	queenAttackRate = 750
	queenAoE        = 4

	santaHealDivisor = 3
	santaHealTalk    = "The power of Christmas heals me!"

	antHealAmount = 35
)

// ---------------------------------------------------------------------------
// PluginHost: world mutations the adapter (or test fake) provides.
// GameWorld is intentionally left unchanged; hooks type-assert to this.
// ---------------------------------------------------------------------------

// MinionOpts mirrors Default.spawn post-conditions: non-respawning, boss
// aggro range, forced aggression, plus per-boss roam/attack-range/roaming.
type MinionOpts struct {
	AggroRange       int
	RoamDistance     int
	AttackRange      int // 0 = profile default
	AlwaysAggressive bool
	NoRespawn        bool
	NoRoam           bool
}

// PluginHost abstracts boss/minion mutation across the engine/adapter seam.
type PluginHost interface {
	SpawnMinion(bossInstance, key string, x, y int, opts MinionOpts) string
	KillMinion(instance string)
	MinionDied(bossInstance, minionInstance string)
	SetMobTarget(mobInstance, playerInstance string)
	TeleportMob(mobInstance string, x, y int)
	ClearMobCombat(mobInstance string)
	MobPos(instance string) (x, y int, ok bool)
	MobTalk(mobInstance, message string)
	HealMob(instance string, amount int)
	FollowStep(mobInstance string, tx, ty int)
}

// ---------------------------------------------------------------------------
// Per-instance state.
// ---------------------------------------------------------------------------

type bossState struct {
	minions map[string]bool

	spawned int // lifetime counter for MAX caps (TS minionsSpawned)

	firstWave, secondWave bool // ogrelord
	queenSpawned          bool // queenant one-shot wave
	healed                bool // santa one-shot heal
	special               bool // forestdragon / queenant special-attack latch
	aoe                   int  // queenant / santa AoE marker (visual-only)
	projectile            string

	atkRangeOverride int // -1 = none
	atkRateOverride  int // 0 = none

	lastTeleport    [2]int
	hasLastTeleport bool // piratecaptain

	healTarget   string        // ant worker: queen instance to heal
	healInterval time.Duration // ant worker: fixed 1-5s interval (TS ctor)
	nextHeal     time.Time
	lastDialogue time.Time // ogrelord 15s dialogue clock
}

var (
	pluginMu     sync.Mutex
	pluginStates = map[string]*bossState{}
	minionOwners = map[string]string{} // minion instance -> boss instance
)

func newBossState() *bossState {
	return &bossState{minions: map[string]bool{}, atkRangeOverride: -1}
}

func stateFor(inst string) *bossState {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	st, ok := pluginStates[inst]
	if !ok {
		st = newBossState()
		pluginStates[inst] = st
	}
	return st
}

func getState(inst string) (*bossState, bool) {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	st, ok := pluginStates[inst]
	return st, ok
}

func clearState(inst string) {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	delete(pluginStates, inst)
	for min, owner := range minionOwners {
		if owner == inst {
			delete(minionOwners, min)
		}
	}
}

func isMinion(inst string) bool {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	_, ok := minionOwners[inst]
	return ok
}

// noteMinionSpawned records a live minion; queen-spawned ants also link
// their heal target (TS: minion.setTarget(queen)).
func noteMinionSpawned(boss, min, key string) {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	st, ok := pluginStates[boss]
	if !ok {
		st = newBossState()
		pluginStates[boss] = st
	}
	st.minions[min] = true
	minionOwners[min] = boss
	if key == "ant" {
		if ms, ok := pluginStates[min]; ok {
			ms.healTarget = boss
		} else {
			ms = newBossState()
			ms.healTarget = boss
			pluginStates[min] = ms
		}
	}
}

// liveMinions snapshots the boss's tracked live minions.
func liveMinions(boss string) []string {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	st, ok := pluginStates[boss]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(st.minions))
	for m := range st.minions {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// pickRandomAttacker mirrors Default.getTarget (uniform over attackers).
func pickRandomAttacker(atk map[string]time.Time) (string, bool) {
	if len(atk) == 0 {
		return "", false
	}
	keys := make([]string, 0, len(atk))
	for k := range atk {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[PluginRandInt(0, len(keys)-1)], true
}

func halfHealth(hp, maxHP int) bool {
	if maxHP <= 0 {
		return false
	}
	return float64(hp)/float64(maxHP) <= 0.5
}

func quarterHealth(hp, maxHP int) bool {
	if maxHP <= 0 {
		return false
	}
	return float64(hp)/float64(maxHP) <= 0.25
}

// spawnMinion is the shared Default.spawn port: host spawn + tracking +
// optional aggro transfer. Returns "" when no host or spawn failed (the
// caller must not count it — TS spawn always succeeds).
func spawnMinion(h PluginHost, boss, key string, x, y int, opts MinionOpts, attackers map[string]time.Time, w GameWorld) string {
	inst := h.SpawnMinion(boss, key, x, y, opts)
	if inst == "" {
		return ""
	}
	noteMinionSpawned(boss, inst, key)
	if tgt, ok := pickRandomAttacker(attackers); ok {
		h.SetMobTarget(inst, tgt)
	}
	return inst
}

// ---------------------------------------------------------------------------
// Hit hook (HitMob, after Points, before the death branch).
// ---------------------------------------------------------------------------

// pluginHitSnap is the unlocked post-hit snapshot HitMob builds (gated).
type pluginHitSnap struct {
	key, inst    string
	hp, maxHP    int
	x, y, sx, sy int
	prof         MobProfile
	attackers    map[string]time.Time
}

func pluginOnHit(s pluginHitSnap, w GameWorld) {
	if !HasMobPlugin(s.key) {
		return
	}
	h, ok := w.(PluginHost)
	if !ok {
		return
	}
	switch s.key {
	case "skeletonking":
		// 1/4 on-hit spawn of 'skeleton', lifetime cap 6.
		if PluginRandInt(1, 4) != 2 {
			return
		}
		st := stateFor(s.inst)
		pluginMu.Lock()
		n := st.spawned
		pluginMu.Unlock()
		if n >= skMaxMinions {
			return
		}
		pos := skPositions[PluginRandInt(0, len(skPositions)-1)]
		if spawnMinion(h, s.inst, "skeleton", pos[0], pos[1], MinionOpts{
			AggroRange: s.prof.AggroRange, RoamDistance: 24,
			AlwaysAggressive: true, NoRespawn: true,
		}, s.attackers, w) == "" {
			return
		}
		pluginMu.Lock()
		st.spawned++
		pluginMu.Unlock()

	case "hellhound":
		// 1/6 on-hit spawn, lifetime cap 8. darkwolf spawns on the boss,
		// blackwizard at a fixed spot with attackRange 16.
		if PluginRandInt(1, 6) != 2 {
			return
		}
		st := stateFor(s.inst)
		pluginMu.Lock()
		n := st.spawned
		pluginMu.Unlock()
		if n >= hhMaxMinions {
			return
		}
		key, px, py, atkRange := "darkwolf", s.x, s.y, 0
		if PluginRandInt(0, 1) != 0 {
			key = "blackwizard"
			pos := hhPositions[PluginRandInt(0, len(hhPositions)-1)]
			px, py, atkRange = pos[0], pos[1], 16
		}
		if spawnMinion(h, s.inst, key, px, py, MinionOpts{
			AggroRange: s.prof.AggroRange, RoamDistance: s.prof.RoamDistance,
			AttackRange: atkRange, AlwaysAggressive: true, NoRespawn: true,
		}, s.attackers, w) == "" {
			return
		}
		pluginMu.Lock()
		st.spawned++
		pluginMu.Unlock()

	case "ogrelord":
		// Wave at half HP ('ogre' x4), wave at quarter HP ('ironogre' x4).
		st := stateFor(s.inst)
		pluginMu.Lock()
		first, second := st.firstWave, st.secondWave
		pluginMu.Unlock()
		wave := -1
		if halfHealth(s.hp, s.maxHP) && !first {
			wave = 0
		} else if quarterHealth(s.hp, s.maxHP) && !second {
			wave = 1
		}
		if wave < 0 {
			return
		}
		for _, pos := range ogrePositions {
			spawnMinion(h, s.inst, ogreKeys[wave], pos[0], pos[1], MinionOpts{
				AggroRange: s.prof.AggroRange, RoamDistance: 24,
				AlwaysAggressive: true, NoRespawn: true,
			}, s.attackers, w)
		}
		pluginMu.Lock()
		if wave == 0 {
			st.firstWave = true
		} else {
			st.secondWave = true
		}
		pluginMu.Unlock()
		h.MobTalk(s.inst, ogreWaveTalk)

	case "queenant":
		// One-shot wave of 4 'ant' workers at half HP + attackRate 750.
		st := stateFor(s.inst)
		pluginMu.Lock()
		done := st.queenSpawned
		pluginMu.Unlock()
		if !halfHealth(s.hp, s.maxHP) || done {
			return
		}
		positions := [][2]int{
			{s.sx + 3, s.sy}, {s.sx - 3, s.sy},
			{s.sx, s.sy + 3}, {s.sx, s.sy - 3},
		}
		for _, pos := range positions {
			spawnMinion(h, s.inst, "ant", pos[0], pos[1], MinionOpts{
				AggroRange: s.prof.AggroRange, RoamDistance: s.prof.RoamDistance,
				AlwaysAggressive: true, NoRespawn: true, NoRoam: true,
			}, nil, w)
		}
		pluginMu.Lock()
		st.queenSpawned = true
		st.atkRateOverride = queenAttackRate
		pluginMu.Unlock()

	case "santa":
		// One-shot 1/3-max-HP heal at half HP + talk.
		st := stateFor(s.inst)
		pluginMu.Lock()
		done := st.healed
		pluginMu.Unlock()
		if done || !halfHealth(s.hp, s.maxHP) {
			return
		}
		pluginMu.Lock()
		st.healed = true
		pluginMu.Unlock()
		h.HealMob(s.inst, s.maxHP/santaHealDivisor)
		h.MobTalk(s.inst, santaHealTalk)

	case "piratecaptain":
		// Spawn check first (1/9), else teleport check (1/13).
		st := stateFor(s.inst)
		if PluginRandInt(0, pirateSpawnN) == 4 {
			pluginMu.Lock()
			n := st.spawned
			pluginMu.Unlock()
			if n < pirateMaxMinions {
				pos := pirateMinions[PluginRandInt(0, len(pirateMinions)-1)]
				if spawnMinion(h, s.inst, "pirateskeleton", pos[0], pos[1], MinionOpts{
					AggroRange: s.prof.AggroRange, RoamDistance: 20,
					AlwaysAggressive: true, NoRespawn: true,
				}, s.attackers, w) != "" {
					pluginMu.Lock()
					st.spawned++
					pluginMu.Unlock()
				}
			}
			return
		}
		if PluginRandInt(0, pirateTeleportN) != 4 {
			return
		}
		pos := pirateTeleports[PluginRandInt(0, len(pirateTeleports)-1)]
		pluginMu.Lock()
		last, has := st.lastTeleport, st.hasLastTeleport
		pluginMu.Unlock()
		if has && last == pos {
			return
		}
		h.ClearMobCombat(s.inst)
		pluginMu.Lock()
		st.atkRangeOverride = 11
		st.lastTeleport, st.hasLastTeleport = pos, true
		pluginMu.Unlock()
		h.TeleportMob(s.inst, pos[0], pos[1])
	}
}

// pluginSuppressRetaliate ports ant.ts handleHit: worker ants (tracked
// queen minions) never respond to attacks. Wild ants keep default AI.
func pluginSuppressRetaliate(key, inst string) bool {
	if key != "ant" {
		return false
	}
	return isMinion(inst)
}

// ---------------------------------------------------------------------------
// Death hook (KillMob, after quest credit) + minion-death note.
// ---------------------------------------------------------------------------

func pluginOnDeath(key, inst string, w GameWorld) {
	if !HasMobPlugin(key) {
		return
	}
	h, ok := w.(PluginHost)
	if !ok {
		clearState(inst)
		return
	}
	for _, min := range liveMinions(inst) {
		h.KillMinion(min)
	}
	clearState(inst)
}

// pluginNoteMinionDeath mirrors minion.onDeathImpl: prune the boss list and,
// for the queen, restore the mobs.json attackRate once all ants are gone.
// Afterwards the adapter drops the registry entry (no respawn for minions).
func pluginNoteMinionDeath(inst string, w GameWorld) {
	pluginMu.Lock()
	owner, ok := minionOwners[inst]
	if !ok {
		pluginMu.Unlock()
		return
	}
	delete(minionOwners, inst)
	boss, ok := pluginStates[owner]
	if ok {
		delete(boss.minions, inst)
		if len(boss.minions) == 0 {
			boss.atkRateOverride = 0
		}
	}
	pluginMu.Unlock()
	if h, ok := w.(PluginHost); ok {
		h.MinionDied(owner, inst)
	}
}

// ---------------------------------------------------------------------------
// Spawn hook (RespawnMob): fresh life resets all plugin state, matching the
// TS death-reset comments ("reset the boss back to default status").
// ---------------------------------------------------------------------------

func pluginOnSpawn(key, inst string) {
	if !HasMobPlugin(key) {
		return
	}
	clearState(inst)
}

// ---------------------------------------------------------------------------
// Attack hook (strikeMob tail, boss lock held): state + DamageHero(nil) only.
// ---------------------------------------------------------------------------

func pluginOnAttack(m Mob, p MobProfile, w GameWorld) {
	key := m.MobKey()
	if !HasMobPlugin(key) {
		return
	}
	switch key {
	case "forestdragon":
		st := stateFor(m.Instance())
		pluginMu.Lock()
		defer pluginMu.Unlock()
		// Second consecutive swing resets the special (TS early return).
		if st.special {
			st.special = false
			st.atkRangeOverride = 1
			st.projectile = "" // clear override, use profile default (fireball)
			return
		}
		if PluginRandInt(1, 6) != 2 {
			st.projectile = "" // clear override, use profile default (fireball)
			return
		}
		// Terror special: ranged terror attack (damage type visual-only).
		st.atkRangeOverride = 9
		st.special = true
		st.projectile = "terror"

	case "queenant":
		st := stateFor(m.Instance())
		pluginMu.Lock()
		if st.special {
			st.special = false
			st.atkRangeOverride = 1
			pluginMu.Unlock()
			return
		}
		pluginMu.Unlock()
		if PluginRandInt(1, 6) != 2 {
			return
		}
		// 1/12 AoE rider alongside the terror attackAll (markers only).
		if PluginRandInt(1, 12) == 3 {
			pluginMu.Lock()
			st.aoe = queenAoE
			pluginMu.Unlock()
		}
		attackAllTerror(m, p, w)
		pluginMu.Lock()
		st.special = true
		pluginMu.Unlock()

	case "santa":
		// Random gift projectile per swing; gift6 carries AoE 4.
		n := PluginRandInt(1, 6)
		name := "gift"
		if n != 1 {
			name = map[int]string{2: "gift2", 3: "gift3", 4: "gift4", 5: "gift5", 6: "gift6"}[n]
		}
		st := stateFor(m.Instance())
		pluginMu.Lock()
		st.projectile = name
		if n == 6 {
			st.aoe = 4
		}
		pluginMu.Unlock()
	}
}

// attackAllTerror ports Default.attackAll(Terror): roll per-attacker damage
// through the existing hero-damage pipeline (nil source — lock-safe).
func attackAllTerror(m Mob, p MobProfile, w GameWorld) {
	byInst := map[string]PlayerView{}
	for _, v := range w.Players() {
		byInst[v.Instance] = v
	}
	for inst := range m.Attackers() {
		v, ok := byInst[inst]
		if !ok {
			continue
		}
		def := 1
		if v.Defense > def {
			def = v.Defense
		}
		defStats := [5]int{v.DefCrush, v.DefSlash, v.DefStab, v.DefMagic, v.DefArchery}
		dmg := RollMobDamage(p, def, MobDamageMult(), defStats)
		// Damage reduction (formulas.ts getDamageReduction).
		if v.DamageReduction > 0 && v.DamageReduction < 1 {
			dmg = int(math.Floor(float64(dmg) * v.DamageReduction))
		}
		if hp := w.GetHeroHP(inst); dmg > hp {
			dmg = hp
		}
		if dmg < 0 {
			dmg = 0
		}
		DamageHero(w, inst, v.Username, dmg, nil)
	}
}

// ---------------------------------------------------------------------------
// Plugin projectile override (strikeMob reads after pluginOnAttack).
// ---------------------------------------------------------------------------

// setPluginProjectile records a per-swing projectile override for the mob
// (santa gift cycle, forestdragon terror special). Empty clears.
func setPluginProjectile(mobInstance, projectile string) {
	st := stateFor(mobInstance)
	pluginMu.Lock()
	st.projectile = projectile
	pluginMu.Unlock()
}

// getPluginProjectile returns the per-swing projectile override set by
// pluginOnAttack, or empty when none.
func getPluginProjectile(mobInstance string) string {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	if st, ok := pluginStates[mobInstance]; ok {
		return st.projectile
	}
	return ""
}

// ---------------------------------------------------------------------------
// Combat override (StepMob, gated single lookup; miss = default profile).
// ---------------------------------------------------------------------------

func pluginCombatOverride(key, inst string) (atkRange, atkRate int, ok bool) {
	if !HasMobPlugin(key) {
		return 0, 0, false
	}
	st, found := getState(inst)
	if !found {
		return 0, 0, false
	}
	pluginMu.Lock()
	defer pluginMu.Unlock()
	return st.atkRangeOverride, st.atkRateOverride, true
}

// ---------------------------------------------------------------------------
// Tick hook (adapter second pass, no locks held): combatLoop ports, queen
// follow, ant healing, ogre dialogue.
// ---------------------------------------------------------------------------

// pluginTickSnap is the unlocked boss snapshot PluginTick dispatches on.
type pluginTickSnap struct {
	key, inst, target string
	hp, maxHP         int
	x, y              int
	prof              MobProfile
	now               time.Time
}

// PluginTick runs the per-tick boss logic for tick-registered keys.
func PluginTick(m Mob, w GameWorld, now time.Time) {
	key := m.MobKey()
	if !HasMobPluginTick(key) {
		return
	}
	m.Lock()
	if m.Dead() {
		m.Unlock()
		return
	}
	snap := pluginTickSnap{
		key: key, inst: m.Instance(), target: m.Target(),
		hp: m.HP(), maxHP: m.MaxHP(),
		prof: m.Profile(), now: now,
	}
	snap.x, snap.y = m.Pos()
	m.Unlock()

	h, ok := w.(PluginHost)
	if !ok {
		return
	}
	switch key {
	case "forestdragon", "hellhound", "queenant":
		combatLoopRange(snap, w, key)
		if key == "queenant" {
			queenFollow(snap, h)
		}
	case "ant":
		antHealTick(snap, h)
	case "ogrelord":
		ogreDialogueTick(snap, h)
	}
}

// combatLoopRange ports handleCombatLoop: hold the special-attack range,
// else pick melee/ranged by Manhattan distance (TS also weighs the target's
// ranged flag and movement, which the stub does not track). No state is
// created for idle/in-special mobs (orphan window).
func combatLoopRange(s pluginTickSnap, w GameWorld, key string) {
	if s.target == "" {
		return
	}
	if st, ok := getState(s.inst); ok {
		pluginMu.Lock()
		special := st.special
		pluginMu.Unlock()
		if special {
			return
		}
	}
	tx, ty, ok := w.PlayerPos(s.target)
	if !ok {
		return
	}
	useRanged := Manhattan(s.x, s.y, tx, ty) > 1
	want := 1
	if useRanged {
		want = 10
		if key == "hellhound" {
			want = 12
		}
	}
	st := stateFor(s.inst)
	pluginMu.Lock()
	st.atkRangeOverride = want
	pluginMu.Unlock()
}

// queenFollow ports handleMovement: minions trail the queen, one chase-step
// per tick through the existing engine helper.
func queenFollow(s pluginTickSnap, h PluginHost) {
	qx, qy, ok := h.MobPos(s.inst)
	if !ok {
		qx, qy = s.x, s.y
	}
	for _, min := range liveMinions(s.inst) {
		h.FollowStep(min, qx, qy)
	}
}

// antHealTick ports the worker-ant heal interval: fixed 1-5s cadence, follow
// the queen, heal 35 when near.
func antHealTick(s pluginTickSnap, h PluginHost) {
	st := stateFor(s.inst)
	pluginMu.Lock()
	target, interval, next := st.healTarget, st.healInterval, st.nextHeal
	pluginMu.Unlock()
	if target == "" {
		return // wild ant: default AI only
	}
	if interval == 0 {
		interval = time.Duration(PluginRandInt(1000, 5000)) * time.Millisecond
		pluginMu.Lock()
		st.healInterval = interval
		st.nextHeal = s.now.Add(interval)
		pluginMu.Unlock()
		return
	}
	if s.now.Before(next) {
		return
	}
	pluginMu.Lock()
	st.nextHeal = s.now.Add(interval)
	pluginMu.Unlock()
	qx, qy, ok := h.MobPos(target)
	if !ok {
		return
	}
	h.FollowStep(s.inst, qx, qy)
	ax, ay, ok := h.MobPos(s.inst)
	if !ok {
		return
	}
	if Manhattan(ax, ay, qx, qy) > 2 {
		return
	}
	h.HealMob(target, antHealAmount)
}

// ogreDialogueTick ports the 15s random-dialogue interval (combat-gated).
func ogreDialogueTick(s pluginTickSnap, h PluginHost) {
	if s.target == "" {
		return
	}
	st := stateFor(s.inst)
	pluginMu.Lock()
	last := st.lastDialogue
	pluginMu.Unlock()
	if !last.IsZero() && s.now.Sub(last) < ogreDialogueWait {
		return
	}
	pluginMu.Lock()
	st.lastDialogue = s.now
	pluginMu.Unlock()
	h.MobTalk(s.inst, ogreDialogues[PluginRandInt(0, len(ogreDialogues)-1)])
}

// ---------------------------------------------------------------------------
// Test introspection (in-package unit tests + adapter tick gating).
// ---------------------------------------------------------------------------

// pluginMinionCount returns the tracked live minions (test helper).
func pluginMinionCount(inst string) int { return len(liveMinions(inst)) }

// pluginSpawnedTotal returns the lifetime spawn counter (test helper).
func pluginSpawnedTotal(inst string) int {
	st, ok := getState(inst)
	if !ok {
		return 0
	}
	pluginMu.Lock()
	defer pluginMu.Unlock()
	return st.spawned
}

// resetPluginStates clears all plugin state (test isolation helper).
func resetPluginStates() {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	pluginStates = map[string]*bossState{}
	minionOwners = map[string]string{}
}
