package entity

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// simFake is a transport-free GameWorld double: it records side-effect
// calls so mob + area orchestration can be asserted without globals,
// frames, m5 state or the combat pipeline.
type simFake struct {
	mu       sync.Mutex
	players  []PlayerView
	blocked  func(x, y int) bool
	plateau  func(x, y int) int
	heroHP   map[string]int
	entityAt map[string][2]int

	moves      []moveCall
	spawns     []MobSpawn
	despawns   []string
	removed    []string
	mobPtsInst []string
	mobPts     []ptsCall
	strikes    []strikeCall
	heroPts    []ptsCall
	deaths     []deathCall
	teleports  []tpCall
	spawnHeros []string
	respawned  []tpCall
	delays     []delayCall
	poisons    []string

	lootDrops []lootCall
	nearWalk  func(x, y int) (int, int)
	regLoot   []regLootCall
	lootItems []LootItem
	quests    []questCall

	notifies []notifyCall
	pvps     []pvpCall
	overlays []overlayCall
	cameras  []cameraCall
	musics   []musicCall
	effects  []effectCall
	freezes  []freezeCall
	chests   []ChestSpawn
	finAchs  []finAchCall

	// spawnMimicOK gates the SpawnMimic double (false = failed spawn,
	// TS spawnMob-unknown-key parity); mimicSeq numbers the instances.
	spawnMimicOK bool
	mimicSeq     int
}

type finAchCall struct {
	instance, key string
}

type moveCall struct {
	instance string
	x, y     int
}

type strikeCall struct {
	attacker, target string
	dmg              int
}

type ptsCall struct {
	instance string
	hp, max  int
}

type deathCall struct {
	player, username, mob string
}

type tpCall struct {
	instance string
	x, y     int
}

type delayCall struct {
	d  time.Duration
	fn func()
}

type lootCall struct {
	mobKey, owner string
	x, y          int
}

type regLootCall struct {
	inst, key, owner string
	count, x, y      int
}

type questCall struct {
	killer, mobKey string
}

type notifyCall struct {
	instance, msg string
}

type pvpCall struct {
	instance string
	state    bool
}

type overlayCall struct {
	instance, image, colour string
	remove                  bool
}

type cameraCall struct {
	instance string
	opcode   int
}

type musicCall struct {
	instance, song string
}

type effectCall struct {
	instance string
	add      bool
	effect   int
}

type freezeCall struct {
	instance string
	on       bool
}

func newSimFake() *simFake {
	return &simFake{
		heroHP:       map[string]int{},
		entityAt:     map[string][2]int{},
		spawnMimicOK: true,
	}
}

func (f *simFake) withPlayer(inst, user string, x, y, level, defense int) *simFake {
	f.players = append(f.players, PlayerView{
		Instance: inst, Username: user, X: x, Y: y, Level: level, Defense: defense,
	})
	return f
}

// --- MobWorld ---

func (f *simFake) Players() []PlayerView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PlayerView, len(f.players))
	copy(out, f.players)
	return out
}

func (f *simFake) PlayerPos(instance string) (int, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.players {
		if v.Instance == instance {
			return v.X, v.Y, true
		}
	}
	return 0, 0, false
}

// PlayerExists mirrors the quest hook's registry check: presence in the
// player list (a removed entry = disconnected = stale damage entry).
// removePlayer drops the entry without touching damage (m9PlayerLeave
// parity: it deletes only target/attackers).
func (f *simFake) PlayerExists(instance string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.players {
		if v.Instance == instance {
			return true
		}
	}
	return false
}

// removePlayer disconnects a player (registry drop only).
func (f *simFake) removePlayer(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.players[:0]
	for _, v := range f.players {
		if v.Instance != instance {
			kept = append(kept, v)
		}
	}
	f.players = kept
}

func (f *simFake) Blocked(x, y int) bool {
	if f.blocked != nil {
		return f.blocked(x, y)
	}
	return false
}

func (f *simFake) PlateauLevel(x, y int) int {
	if f.plateau != nil {
		return f.plateau(x, y)
	}
	return 0
}

func (f *simFake) SetEntityPos(instance string, x, y int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entityAt[instance] = [2]int{x, y}
}

func (f *simFake) Despawn(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.despawns = append(f.despawns, instance)
}

func (f *simFake) MoveMob(instance string, x, y int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entityAt[instance] = [2]int{x, y}
	f.moves = append(f.moves, moveCall{instance, x, y})
}

func (f *simFake) SpawnMobFrame(s MobSpawn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entityAt[s.Instance] = [2]int{s.X, s.Y}
	f.spawns = append(f.spawns, s)
}

func (f *simFake) SkipFarRoam(mx, my int) bool {
	_, _ = mx, my
	return false
}

func (f *simFake) RemoveMob(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, instance)
}

func (f *simFake) MobPoints(instance string, hp, maxHP int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mobPtsInst = append(f.mobPtsInst, instance)
	f.mobPts = append(f.mobPts, ptsCall{instance, hp, maxHP})
}

func (f *simFake) StrikeMob(attacker, target string, dmg int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.strikes = append(f.strikes, strikeCall{attacker, target, dmg})
}

func (f *simFake) GetHeroHP(instance string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if hp, ok := f.heroHP[instance]; ok {
		return hp
	}
	return 69 // level 1 default
}

func (f *simFake) SetHeroHP(instance string, hp int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heroHP[instance] = hp
}

func (f *simFake) ForgetHeroHP(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.heroHP, instance)
}

func (f *simFake) HeroMaxHP(instance string) int {
	return 69 // level 1 default (39 + 1*30)
}

func (f *simFake) HeroPoints(instance string, hp, maxHP int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heroPts = append(f.heroPts, ptsCall{instance, hp, maxHP})
}

func (f *simFake) HeroDied(playerInstance, username, mobInstance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deaths = append(f.deaths, deathCall{playerInstance, username, mobInstance})
}

func (f *simFake) TeleportHero(instance string, x, y int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teleports = append(f.teleports, tpCall{instance, x, y})
}

func (f *simFake) SpawnHero(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawnHeros = append(f.spawnHeros, instance)
}

func (f *simFake) HeroRespawned(instance string, x, y int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respawned = append(f.respawned, tpCall{instance, x, y})
}

func (f *simFake) AfterDelay(d time.Duration, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, delayCall{d, fn})
}

func (f *simFake) ApplyPoison(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.poisons = append(f.poisons, instance)
}

func (f *simFake) SpawnProjectile(projectileName, ownerInst, targetInst string, x, y, tx, ty int) {
	// No-op for tests: projectile spawning is visual-only and doesn't affect
	// the mob/area orchestration being tested.
}

// --- ChestLoot ---

func (f *simFake) SpawnLoot(mobKey string, x, y int, owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lootDrops = append(f.lootDrops, lootCall{mobKey, owner, x, y})
}

func (f *simFake) NearWalkable(x, y int) (int, int) {
	if f.nearWalk != nil {
		return f.nearWalk(x, y)
	}
	return x, y
}

func (f *simFake) RegisterLoot(inst, key string, count, x, y int, owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regLoot = append(f.regLoot, regLootCall{inst, key, owner, count, x, y})
}

func (f *simFake) SpawnLootItem(i LootItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lootItems = append(f.lootItems, i)
}

// --- QuestSink ---

func (f *simFake) QuestKill(killerInstance, mobKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quests = append(f.quests, questCall{killerInstance, mobKey})
}

// --- AreaWorld ---

func (f *simFake) Notify(instance, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifies = append(f.notifies, notifyCall{instance, msg})
}

func (f *simFake) SendPVP(instance string, state bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pvps = append(f.pvps, pvpCall{instance, state})
}

func (f *simFake) SendOverlaySet(instance, image, colour string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overlays = append(f.overlays, overlayCall{instance, image, colour, false})
}

func (f *simFake) SendOverlayRemove(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overlays = append(f.overlays, overlayCall{instance, "", "", true})
}

func (f *simFake) SendCamera(instance string, opcode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cameras = append(f.cameras, cameraCall{instance, opcode})
}

func (f *simFake) SendMusic(instance, song string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.musics = append(f.musics, musicCall{instance, song})
}

func (f *simFake) SendEffect(instance string, add bool, effect int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.effects = append(f.effects, effectCall{instance, add, effect})
}

func (f *simFake) FreezeApply(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freezes = append(f.freezes, freezeCall{instance, true})
}

func (f *simFake) FreezeClear(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freezes = append(f.freezes, freezeCall{instance, false})
}

func (f *simFake) SpawnChestFrame(c ChestSpawn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entityAt[c.Instance] = [2]int{c.X, c.Y}
	f.chests = append(f.chests, c)
}

func (f *simFake) SpawnMimic(x, y int) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.spawnMimicOK {
		return "", false
	}
	f.mimicSeq++
	inst := fmt.Sprintf("mimic-test-%d", f.mimicSeq)
	f.entityAt[inst] = [2]int{x, y}
	f.spawns = append(f.spawns, MobSpawn{Instance: inst, Key: "mimic", Name: "Mimic", X: x, Y: y})
	return inst, true
}

func (f *simFake) FinishAchievement(instance, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finAchs = append(f.finAchs, finAchCall{instance, key})
}

// fireDelays runs recorded respawn timers (manual clock).
func (f *simFake) fireDelays() {
	f.mu.Lock()
	dels := append([]delayCall(nil), f.delays...)
	f.mu.Unlock()
	for _, d := range dels {
		d.fn()
	}
}

func hasPrefix(s, pre string) bool { return strings.HasPrefix(s, pre) }

// ---------------------------------------------------------------------------
// testMob is a Mob implementation over plain fields (single live state,
// like the root m9Mob, minus transport).
// ---------------------------------------------------------------------------

type testMob struct {
	mu        sync.Mutex
	instance  string
	key       string
	prof      MobProfile
	over      MobOverrides
	spawnX    int
	spawnY    int
	x, y      int
	hp, maxHP int
	dead      bool
	target    string
	plateau   int
	lastAtk   time.Time
	lastMove  time.Time
	lastRoam  time.Time
	lastTgt   time.Time
	attackers map[string]time.Time
	dmg       DamageTable
}

func newTestMob(instance, key string, prof MobProfile, x, y int) *testMob {
	ApplyDefaults(&prof)
	now := time.Now()
	return &testMob{
		instance: instance, key: key, prof: prof,
		spawnX: x, spawnY: y, x: x, y: y,
		hp: prof.HitPoints, maxHP: prof.HitPoints,
		lastMove: now, lastRoam: now,
		attackers: map[string]time.Time{},
	}
}

func (m *testMob) Lock()                   { m.mu.Lock() }
func (m *testMob) Unlock()                 { m.mu.Unlock() }
func (m *testMob) Instance() string        { return m.instance }
func (m *testMob) MobKey() string          { return m.key }
func (m *testMob) Profile() MobProfile     { return m.prof }
func (m *testMob) Overrides() MobOverrides { return m.over }
func (m *testMob) SpawnPos() (int, int)    { return m.spawnX, m.spawnY }
func (m *testMob) Pos() (int, int)         { return m.x, m.y }
func (m *testMob) SetPos(x, y int)         { m.x, m.y = x, y }
func (m *testMob) HP() int                 { return m.hp }
func (m *testMob) MaxHP() int              { return m.maxHP }
func (m *testMob) SetHP(hp int)            { m.hp = hp }
func (m *testMob) Dead() bool              { return m.dead }
func (m *testMob) SetDead(dead bool)       { m.dead = dead }
func (m *testMob) Target() string          { return m.target }
func (m *testMob) SetTarget(t string)      { m.target = t }
func (m *testMob) Plateau() int            { return m.plateau }
func (m *testMob) LastAtk() time.Time      { return m.lastAtk }
func (m *testMob) SetLastAtk(t time.Time)  { m.lastAtk = t }
func (m *testMob) LastMove() time.Time     { return m.lastMove }
func (m *testMob) SetLastMove(t time.Time) { m.lastMove = t }
func (m *testMob) LastRoam() time.Time     { return m.lastRoam }
func (m *testMob) SetLastRoam(t time.Time) { m.lastRoam = t }
func (m *testMob) LastTgt() time.Time      { return m.lastTgt }
func (m *testMob) SetLastTgt(t time.Time)  { m.lastTgt = t }

func (m *testMob) TouchAttacker(inst string, now time.Time) { m.attackers[inst] = now }
func (m *testMob) DropAttacker(inst string)                 { delete(m.attackers, inst) }

func (m *testMob) AddDamage(inst string, dmg int, username string) {
	m.dmg.Add(inst, dmg, username)
}
func (m *testMob) DamageRank() []DamageEntry { return m.dmg.Rank() }
func (m *testMob) ClearDamage()              { m.dmg.Clear() }

func (m *testMob) Attackers() map[string]time.Time {
	out := make(map[string]time.Time, len(m.attackers))
	for k, v := range m.attackers {
		out[k] = v
	}
	return out
}

func (m *testMob) ClearAttackers() { m.attackers = map[string]time.Time{} }

// ratProfile returns a small aggressive profile (skeleton-like) for tests.
func ratProfile() MobProfile {
	return MobProfile{
		Name: "Rat", Level: 2, HitPoints: 30,
		AggroRange: 6, AttackRange: 1, AttackRate: 1000, MovementSpeed: 220,
		RespawnDelay: 4000, RoamDistance: 7,
		Aggressive: true,
	}
}
