// ---------------------------------------------------------------------------
// Mob engine — generic mob AI engine (worlds setup slice 1): thin root adapter.
//
// The engine lives in internal/entity (mob.go); this file keeps the live
// state + the world wiring with UNCHANGED public signatures so main.go,
// player_state.go, quests_wire.go, commands.go, pets_wire.go, ops_wire.go
// and abilities_wire.go compile untouched.
//
// STAYED here (commands.go/player_state.go/abilities_wire.go touch these
// directly):
//   - mob struct (all fields), mobMu/mobs registry, mobProf/mobSpawn
//     tables, playerHPs hero HP store.
//   - The shared entity.World implementation (gameWorld): broadcast/send,
//     entities/players maps, setEntityPos, blocked, player-state loot/XP,
//     quest kill, combatMu-adjacent lookups — packet shapes frozen here.
//   - mobEngine/adoptExistingMobs (mode globals), handleMobTest (TESTMAP
//     debug frames), ratEntity/m.data (EntityData payload shape).
//
// MOVED to entity: profiles/load/merge/defaults, distances, damage roll,
// roam pick, chase step, aggro/leash/attack tick, retaliate, kill credit,
// respawn timers, hero HP/death hooks. The old mob<->area call cycle
// (spawnMob->chestAreaAt/addChestMob; killHooks->m9 state) is
// gone: both engines live in ONE package (internal/entity) behind the
// GameWorld seam.
// ---------------------------------------------------------------------------

package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/meta"
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/sim"
	worldcore "rpg-world-server/internal/world"
)

// mobProfile/spawnMobOverride/mobOverrides are entity-owned now (same
// JSON/shape, type aliases — all existing references keep compiling).
type (
	mobProfile    = entity.MobProfile
	spawnMobOverride = entity.SpawnOverride
	mobOverrides     = entity.MobOverrides
)

// mob is one live AI mob instance (Node Mob + handler state).
type mob struct {
	instance string
	key      string
	prof     mobProfile // mobs.json merged with spawns.json overrides

	spawnX, spawnY int
	x, y           int
	hp, maxHP      int
	dead           bool

	// plateau is the bound plateau level from the spawn tile
	// (mob.ts:148 parity; roam steps + incoming swings gate on it).
	plateau int

	mu        sync.Mutex
	target    string // player instance (combat.target)
	lastAtk   time.Time
	lastMove  time.Time
	lastRoam  time.Time
	lastTgt   time.Time
	attackers map[string]time.Time // instance -> last hit (addAttacker)
	// dmg is the TS damageTable port (entity.DamageTable): per-attacker
	// clamped damage totals for top-damager kill credit. Lifecycle
	// differs from attackers on purpose (TS parity): pruning/leash touch
	// only attackers; death clears dmg (entity.KillMob). Value struct,
	// zero-value ready — struct literals need no init.
	dmg entity.DamageTable

	over mobOverrides
}

// Engine registry. Lock order: mobMu -> m.mu (never reversed).
var (
	mobMu    sync.Mutex
	mobs  = map[string]*mob{}
	mobProf  = map[string]*mobProfile{}
	mobSpawn = map[string]*spawnMobOverride{}
)

// ---------------------------------------------------------------------------
// entity.Mob implementation (locked-context accessors; callers hold m.mu
// exactly where the old code held it).
// ---------------------------------------------------------------------------

func (m *mob) Lock()   { m.mu.Lock() }
func (m *mob) Unlock() { m.mu.Unlock() }

func (m *mob) Instance() string               { return m.instance }
func (m *mob) MobKey() string                 { return m.key }
func (m *mob) Profile() entity.MobProfile     { return m.prof }
func (m *mob) Overrides() entity.MobOverrides { return m.over }

func (m *mob) SpawnPos() (int, int) { return m.spawnX, m.spawnY }
func (m *mob) Pos() (int, int)      { return m.x, m.y }
func (m *mob) SetPos(x, y int)      { m.x, m.y = x, y }

func (m *mob) HP() int           { return m.hp }
func (m *mob) MaxHP() int        { return m.maxHP }
func (m *mob) SetHP(hp int)      { m.hp = hp }
func (m *mob) Dead() bool        { return m.dead }
func (m *mob) SetDead(dead bool) { m.dead = dead }

func (m *mob) Target() string     { return m.target }
func (m *mob) SetTarget(t string) { m.target = t }

// Plateau reports the bound spawn plateau level (mob.ts:148 parity; the
// caller must hold m.mu like every other accessor).
func (m *mob) Plateau() int { return m.plateau }

func (m *mob) LastAtk() time.Time      { return m.lastAtk }
func (m *mob) SetLastAtk(t time.Time)  { m.lastAtk = t }
func (m *mob) LastMove() time.Time     { return m.lastMove }
func (m *mob) SetLastMove(t time.Time) { m.lastMove = t }
func (m *mob) LastRoam() time.Time     { return m.lastRoam }
func (m *mob) SetLastRoam(t time.Time) { m.lastRoam = t }
func (m *mob) LastTgt() time.Time      { return m.lastTgt }
func (m *mob) SetLastTgt(t time.Time)  { m.lastTgt = t }

func (m *mob) TouchAttacker(inst string, now time.Time) { m.attackers[inst] = now }
func (m *mob) DropAttacker(inst string)                 { delete(m.attackers, inst) }

// AddDamage/DamageRank/ClearDamage delegate to the embedded damage table
// (pruning DropAttacker deliberately does NOT drop damage — TS
// removeAttacker touches only the attackers list).
func (m *mob) AddDamage(inst string, dmg int, username string) {
	m.dmg.Add(inst, dmg, username)
}
func (m *mob) DamageRank() []entity.DamageEntry { return m.dmg.Rank() }
func (m *mob) ClearDamage()                     { m.dmg.Clear() }

func (m *mob) Attackers() map[string]time.Time {
	out := make(map[string]time.Time, len(m.attackers))
	for k, v := range m.attackers {
		out[k] = v
	}
	return out
}

func (m *mob) ClearAttackers() { m.attackers = map[string]time.Time{} }

// ---------------------------------------------------------------------------
// Shared entity.GameWorld implementation (M9+M10 world seam).
// ---------------------------------------------------------------------------

// gameWorldAdapter implements entity.GameWorld with the root helpers.
// Packet shapes are frozen here (identical frame builders to the old
// m9.go/m10.go bodies).
type gameWorldAdapter struct{}

var gameWorld = gameWorldAdapter{}

func (gameWorldAdapter) Players() []entity.PlayerView {
	out := []entity.PlayerView{}
	for _, c := range worldcore.AllOf[*playerConn]() {
		// Corpses never aggro (TS: dead players drop out of combat;
		// cleanCombat + the hit() dead-guard). Without this the next
		// tick re-acquires the corpse and re-strikes it, rebroadcasting
		// Points-0 and re-firing HeroDied. Fresh conns read full HP
		// (GetHeroHP default) and are unaffected.
		if gameWorld.GetHeroHP(c.Instance) <= 0 {
			continue
		}
		lvl, def := 0, 0
		if st := playerStateFor(c.Username); st != nil {
			lvl = st.Level
			if sk := st.Skills[SkillDefense]; sk != nil {
				def = sk.Level
			}
		}
		pv := entity.PlayerView{
			Instance: c.Instance, Username: c.Username,
			X: c.Sess.PlayerX, Y: c.Sess.PlayerY,
			Level: lvl, Defense: def,
			Plateau: plateauGet(c.Instance),
		}
		populatePlayerCombatStats(&pv, c.Username, c.Instance)
		out = append(out, pv)
	}
	return out
}

func (gameWorldAdapter) PlayerPos(instance string) (int, int, bool) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		return c.Sess.PlayerX, c.Sess.PlayerY, true
	}
	return 0, 0, false
}

// PlayerExists is the kill-credit existence check (TS
// world.entities.get(instance) + isPlayer parity at handleDeath time):
// the registry lookup the quest hook uses (QuestKill -> worldcore.Find),
// so corpses still count and only disconnects drop out. Nil-safe.
func (gameWorldAdapter) PlayerExists(instance string) bool {
	if instance == "" {
		return false
	}
	c, _ := worldcore.Find[*playerConn](instance)
	return c != nil
}

func (gameWorldAdapter) Blocked(x, y int) bool { return blocked(x, y) }

func (gameWorldAdapter) PlateauLevel(x, y int) int { return plateauLevelOf(x, y) }

func (gameWorldAdapter) SetEntityPos(instance string, x, y int) {
	worldcore.SetEntityPos(instance, x, y)
}

func (gameWorldAdapter) Despawn(instance string) {
	worldcore.Broadcast(pkt(PacketDespawn, despawnData{Instance: instance}))
}

func (gameWorldAdapter) MoveMob(instance string, x, y int) {
	worldcore.SetEntityPos(instance, x, y)
	worldcore.Broadcast(pktOp(PacketMovement, MovementMove, serverMovement{
		Instance: instance, X: intp(x), Y: intp(y),
	}))
}

func (gameWorldAdapter) SpawnMobFrame(s entity.MobSpawn) {
	worldcore.SetEntityPos(s.Instance, s.X, s.Y)
	worldcore.Broadcast(pkt(PacketSpawn, EntityData{
		Instance: s.Instance, Type: EntityMob, Key: s.Key,
		Name: s.Name, X: s.X, Y: s.Y,
		Orientation: intp(OrientationDown),
		Level:       intp(s.Level),
		HitPoints:   intp(s.HP), MaxHitPoints: intp(s.MaxHP),
		MovementSpeed: intp(s.MoveSpeed),
		AttackRange:   intp(s.AttackRange),
	}))
}

// SkipFarRoam ports the entities.ts load roam-interval guard (the
// setInterval body over forEachMob): roaming only sleeps when players are
// absent from the mob's region AND more than 30 players are online. With
// 30 or fewer players (every test and small server) it always reports
// false, so roam behavior there is unchanged.
func (gameWorldAdapter) SkipFarRoam(mx, my int) bool {
	players := gameWorld.Players()
	if len(players) <= 30 {
		return false
	}
	mr := worldcore.TileRegion(mx, my)
	for _, v := range players {
		if worldcore.TileRegion(v.X, v.Y) == mr {
			return false
		}
	}
	return true
}

func (gameWorldAdapter) MobPoints(instance string, hp, maxHP int) {
	worldcore.Broadcast(pkt(PacketPoints, pointsData{
		Instance: instance, HitPoints: intp(hp), MaxHitPoints: intp(maxHP),
	}))
}

func (gameWorldAdapter) StrikeMob(attacker, target string, dmg int) {
	worldcore.Broadcast(pktOp(PacketCombat, CombatHit, combatData{
		Instance: attacker, Target: target,
		Hit: HitData{Type: HitsNormal, Damage: dmg},
	}))
}

// deathFired marks instances whose HeroDied funnel already ran, so the
// funnel is exactly-once per life (TS character.hit dead-guard parity:
// hits on a corpse are silent — no Points-0 rebroadcasts, no duplicate
// Death/Despawn/save). Lock-free sync.Map: HeroDied runs on the engine
// tick (holding mobMu + the killer's m.mu), the StatusTick loop and admin
// intake, so it can take no subsystem mutexes of its own. Cleared on
// respawn (handleMobRespawn) and disconnect (mobPlayerLeave) so the next
// life dies loudly again.
var deathFired sync.Map // instance -> true

func (gameWorldAdapter) HeroDied(playerInstance, username, mobInstance string) {
	// Player handleDeath parity (player/handler.ts handleDeath): status
	// clear, Despawn broadcast, pet despawn, persist flush, Death unicast
	// to self. Attacker release: the killer's target already cleared in
	// DamageHero; every other mob targeting the victim releases it on its
	// next tick through the existing gone-target path (the corpse leaves
	// the Players scan set below) — the world.ts cleanCombat outcome with
	// no new locks. PvP accounting (pvpDeaths/killCallback), the
	// damageTable reset and the skills/combat stops have no Go counterparts
	// (no damage table, no hero combat loop — single-swing dispatch — and
	// gathering is per-swing with no continuous action to stop) and stay
	// omitted.
	//
	// LOCK DISCIPLINE: the strike path calls this holding mobMu (mobTick)
	// and the killer's m.mu — take neither here (self-deadlock). Every
	// seam below is lock-free or leaf-ordered (tracker/registry/pstateMu
	// follow the pre-existing mobMu-outer order; m5 paths never take mobMu).
	_ = mobInstance
	if _, dup := deathFired.LoadOrStore(playerInstance, true); dup {
		return
	}
	abClearStatus(playerInstance) // status.clear() + setPoison() cure; unlocks kept
	c, _ := worldcore.Find[*playerConn](playerInstance)
	worldcore.Broadcast(pkt(PacketDespawn, despawnData{Instance: playerInstance}))
	if c != nil {
		petForgetPlayer(c) // disconnect removePet parity; no-op without a pet
	}
	savePlayerSync(username) // disconnect persist path reused, not duplicated
	if c != nil {
		// Death goes to the victim only (TS sends Death to self);
		// observers learn of the death via the Despawn above.
		_ = gnet.Send(c.Conn, pkt(PacketDeath, playerInstance))
	}
}

func (gameWorldAdapter) TeleportHero(instance string, x, y int) {
	worldcore.Broadcast(pkt(PacketTeleport, teleportData{Instance: instance, X: x, Y: y}))
}

func (gameWorldAdapter) SpawnHero(instance string) {
	worldcore.Broadcast(pkt(PacketSpawn, welcomePlayer(instance)))
}

func (gameWorldAdapter) HeroRespawned(instance string, x, y int) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		_ = gnet.Send(c.Conn, pkt(PacketRespawn, respawnData{X: x, Y: y}))
	}
}

func (gameWorldAdapter) AfterDelay(d time.Duration, fn func()) {
	time.AfterFunc(d, fn)
}

func (gameWorldAdapter) ApplyPoison(instance string) {
	abApplyPoison(instance)
}

func (gameWorldAdapter) SpawnLoot(mobKey string, x, y int, owner string) {
	spawnLoot(mobKey, x, y, owner)
}

func (gameWorldAdapter) NearWalkable(x, y int) (int, int) {
	return nearWalkable(x, y)
}

func (gameWorldAdapter) RegisterLoot(inst, key string, count, x, y int, owner string) {
	registerLoot(inst, key, count, x, y, owner)
}

func (gameWorldAdapter) SpawnLootItem(i entity.LootItem) {
	worldcore.Broadcast(pkt(PacketSpawn, EntityData{
		Instance: i.Instance, Type: EntityItem, Key: i.Key, Name: i.Key,
		X: i.X, Y: i.Y, Count: intp(i.Count),
	}))
}

func (gameWorldAdapter) QuestKill(killerInstance, mobKey string) {
	killer, _ := worldcore.Find[*playerConn](killerInstance)
	questKill(killer, mobKey) // nil-safe (questKill guards nil)
	// Statistics kill counter rides the same death signal (handler.ts:765
	// addMobKill parity — counter only, no achievement).
	statsRecordKill(killer, mobKey) // nil-safe
}

func (gameWorldAdapter) Notify(instance, msg string) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		notifyPlayer(c, msg)
	}
}

func (gameWorldAdapter) SendPVP(instance string, state bool) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		_ = gnet.Send(c.Conn, []any{PacketPVP, nil, map[string]any{"state": state}})
	}
}

func (gameWorldAdapter) SendOverlaySet(instance, image, colour string) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		_ = gnet.Send(c.Conn, pktOp(PacketOverlay, entity.OverlaySet, map[string]any{
			"image":  image,
			"colour": colour,
		}))
	}
}

func (gameWorldAdapter) SendOverlayRemove(instance string) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		_ = gnet.Send(c.Conn, pktOp(PacketOverlay, entity.OverlayRemove, nil))
	}
}

func (gameWorldAdapter) SendCamera(instance string, opcode int) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		_ = gnet.Send(c.Conn, pktOp(PacketCamera, opcode, nil))
	}
}

func (gameWorldAdapter) SendMusic(instance, song string) {
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		_ = gnet.Send(c.Conn, []any{PacketMusic, nil, song})
	}
}

func (gameWorldAdapter) SendEffect(instance string, add bool, effect int) {
	op := EffectRemove
	if add {
		op = EffectAdd
	}
	if c, _ := worldcore.Find[*playerConn](instance); c != nil {
		_ = gnet.Send(c.Conn, pktOp(PacketEffect, op, effectData{Instance: instance, Effect: effect}))
	}
}

func (gameWorldAdapter) FreezeApply(instance string) {
	abFreezeApply(instance)
}

func (gameWorldAdapter) FreezeClear(instance string) {
	abFreezeClear(instance)
}

func (gameWorldAdapter) SpawnChestFrame(c entity.ChestSpawn) {
	worldcore.SetEntityPos(c.Instance, c.X, c.Y)
	worldcore.Broadcast(pkt(PacketSpawn, EntityData{
		Instance: c.Instance, Type: EntityChest, Key: "chest", Name: "Chest", X: c.X, Y: c.Y,
	}))
}

func (gameWorldAdapter) FinishAchievement(instance, key string) {
	c, _ := worldcore.Find[*playerConn](instance)
	finishAchievement(c, key) // nil-safe (unknown instance/achievement ignored)
}

// playerViewFor builds one PlayerView for a live conn (mobOnPlayerMoved).
func playerViewFor(c *playerConn) entity.PlayerView {
	lvl, def := 0, 0
	if st := playerStateFor(c.Username); st != nil {
		lvl = st.Level
		if sk := st.Skills[SkillDefense]; sk != nil {
			def = sk.Level
		}
	}
	pv := entity.PlayerView{
		Instance: c.Instance, Username: c.Username,
		X: c.Sess.PlayerX, Y: c.Sess.PlayerY,
		Level: lvl, Defense: def,
		Plateau: plateauGet(c.Instance),
	}
	populatePlayerCombatStats(&pv, c.Username, c.Instance)
	return pv
}

// populatePlayerCombatStats fills the equipment defense stats and damage
// reduction fields on a PlayerView (server-layer data that the entity
// layer needs for triangle advantage and damage reduction).
func populatePlayerCombatStats(pv *entity.PlayerView, username, instance string) {
	ds := heroTotalDefenseStats(username)
	pv.DefCrush = ds.Crush
	pv.DefSlash = ds.Slash
	pv.DefStab = ds.Stab
	pv.DefMagic = ds.Magic
	pv.DefArchery = ds.Archery
	pv.DamageReduction = heroDamageReduction(username, instance)
}

// mobAlive reports whether m is still the registered mob (respawn guard).
func mobAlive(m *mob) bool {
	return mobFor(m.instance) == m
}

// killerView maps a killer conn to a PlayerView (nil-safe).
func killerView(killer *playerConn) *entity.PlayerView {
	if killer == nil {
		return nil
	}
	v := entity.PlayerView{Instance: killer.Instance, Username: killer.Username}
	return &v
}

// ---------------------------------------------------------------------------
// Tables / spawn / registry.
// ---------------------------------------------------------------------------

func loadMobTables() {
	mobMu.Lock()
	defer mobMu.Unlock()
	if len(mobProf) > 0 {
		return
	}
	for name, dst := range map[string]any{"mobs": &mobProf, "spawns": &mobSpawn} {
		raw, err := os.ReadFile(resourceDataPath(name))
		if err != nil {
			log.Printf("m9: read %s.json: %v (engine disabled)", name, err)
			return
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			log.Printf("m9: parse %s.json: %v (engine disabled)", name, err)
			return
		}
	}
	log.Printf("m9: mobs.json=%d profiles, spawns.json=%d overrides", len(mobProf), len(mobSpawn))
}

// spawnMob registers + broadcasts a mob (entities.ts spawnMob shape).
// World boot adoption and the m9test dispatcher both land here.
func spawnMob(instance, key string, x, y int, over mobOverrides) bool {
	return spawnMobInner(instance, key, x, y, over, true)
}

// spawnMobQuiet registers a mob identically to spawnMob but skips the
// Spawn broadcast: boot-time seeding only (zero subscribers at boot; late
// joiners discover the mob through the region-scoped List + Who, the TS
// updateEntityList path). Chest-area adoption, plateau bind and spawns.json
// overrides are identical.
func spawnMobQuiet(instance, key string, x, y int, over mobOverrides) bool {
	return spawnMobInner(instance, key, x, y, over, false)
}

func spawnMobInner(instance, key string, x, y int, over mobOverrides, broadcast bool) bool {
	loadMobTables()
	mobMu.Lock()
	prof := entity.ProfileFor(mobProf, mobSpawn, key, x, y)
	if prof == nil {
		mobMu.Unlock()
		log.Printf("m9: unknown mob key %s (spawn skipped)", key)
		return false
	}
	entity.ApplyDefaults(prof)
	m := &mob{
		instance: instance, key: key, prof: *prof,
		spawnX: x, spawnY: y, x: x, y: y,
		maxHP: prof.HitPoints, hp: prof.HitPoints,
		plateau:   plateauLevelOf(x, y), // mob.ts:148 spawn plateau bind
		lastMove:  time.Now(),
		lastRoam:  time.Now(),
		attackers: map[string]time.Time{},
		over:      over,
	}
	if over.Aggro > 0 {
		m.prof.AggroRange = over.Aggro
	}
	if over.Leash > 0 {
		m.prof.RoamDistance = over.Leash
	}
	mobs[instance] = m
	payload := m.data()
	mobMu.Unlock()

	worldcore.SetEntityPos(instance, x, y)
	if broadcast {
		worldcore.Broadcast(pkt(PacketSpawn, payload))
	}
	// M10: Mob.addToChestArea parity — a mob spawning inside a chest area
	// registers with it (addEntity; removes any unlooted reward chest).
	if area := chestAreaAt(x, y); area != nil {
		addChestMob(area, instance, m.respawnDelay())
	}
	return true
}

// removeMob drops a mob from the registry (no despawn frame; callers that
// need one broadcast it themselves — Node despawn/destroy split).
func removeMob(instance string) {
	mobMu.Lock()
	delete(mobs, instance)
	mobMu.Unlock()
}

func mobFor(instance string) *mob {
	mobMu.Lock()
	defer mobMu.Unlock()
	return mobs[instance]
}

// data mirrors Mob.serialize: hitPoints/maxHitPoints/attackRange/level.
func (m *mob) data() EntityData {
	return EntityData{
		Instance: m.instance, Type: EntityMob, Key: m.key,
		Name: m.prof.Name, X: m.x, Y: m.y,
		Orientation: intp(OrientationDown),
		Level:       intp(m.prof.Level),
		HitPoints:   intp(m.hp), MaxHitPoints: intp(m.maxHP),
		MovementSpeed: intp(m.prof.MovementSpeed),
		AttackRange:   intp(m.prof.AttackRange),
	}
}

// respawnDelay mirrors Mob.respawn: override > profile > MobDefaults.
func (m *mob) respawnDelay() time.Duration {
	return entity.RespawnDelayFor(m.over, m.prof)
}

// ---------------------------------------------------------------------------
// Engine tick / hit intake (orchestration lives in entity).
// ---------------------------------------------------------------------------

// mobTick is the 500ms AI pass over every live mob.
func mobTick() {
	mobMu.Lock()
	now := time.Now()
	var plugMobs []*mob
	for _, m := range mobs {
		if m.dead {
			continue
		}
		entity.StepMob(m, gameWorld, now)
		// Tick-plugin mobs are collected for a second pass AFTER the
		// registry lock is released: PluginTick uses the full PluginHost
		// (spawn/remove/lookup take mobMu), so it must never run under it.
		if entity.HasMobPluginTick(m.key) {
			plugMobs = append(plugMobs, m)
		}
	}
	mobMu.Unlock()
	for _, m := range plugMobs {
		entity.PluginTick(m, gameWorld, time.Now())
	}
}

// mobPlayerHit applies hero damage to a mob: Points, retaliate, death.
func mobPlayerHit(m *mob, attacker *playerConn, dmg int) {
	entity.HitMob(m, killerView(attacker), dmg, gameWorld, time.Now(), func() bool {
		return mobAlive(m)
	})
}

// killMob ports handler.handleDeath: despawn + kill credit (M5 loot) +
// destroy (respawn timer restores full HP at spawn).
func killMob(m *mob, killer *playerConn) {
	entity.KillMob(m, killerView(killer), gameWorld, func() bool {
		return mobAlive(m)
	})
}

// respawnMob ports handler.handleRespawn: full HP, back at spawn, Spawn frame.
func respawnMob(m *mob) {
	if mobFor(m.instance) != m {
		return // removed while dead
	}
	entity.RespawnMob(m, gameWorld)
	log.Printf("m9: %s respawned full HP=%d", m.instance, m.maxHP)
}

// ---------------------------------------------------------------------------
// Mob-plugin host (entity.PluginHost over the live registry).
//
// Minions spawn through the existing spawnMob path (Spawn broadcast +
// chest-area registration + plateau bind) with the TS Default.spawn
// post-conditions layered on: non-respawning, boss aggro range, forced
// aggression. Cleanup runs the full killerless KillMob pipeline (Despawn
// broadcast + unowned loot + nil-safe quest hook, no respawn timer) and then
// drops the registry entry (TS destroy). Mob talk reuses the existing Chat
// bubble frame (TS talkCallback surface). No new packet shapes.
// ---------------------------------------------------------------------------

// minionSeq disambiguates minion instances per boss.
var minionSeq atomic.Int64

// mimicSeq disambiguates mimic instances per opened chest.
var mimicSeq atomic.Int64

// SpawnMimic spawns a 'mimic' mob at (x, y) for a mimic-chest open
// (entities.ts onOpen spawnMob('mimic')): the full spawnMob path (Spawn
// broadcast + chest-area adoption + plateau bind) with the TS
// post-conditions layered on (non-respawning via the NoRespawn override;
// the chest link itself lives in entity, set by OpenChest on success).
// Reports ok=false when the profile is unknown (TS `if (mimic)` parity).
func (gameWorldAdapter) SpawnMimic(x, y int) (string, bool) {
	inst := fmt.Sprintf("mimic-%d", mimicSeq.Add(1))
	if !spawnMob(inst, "mimic", x, y, mobOverrides{NoRespawn: true}) {
		return "", false
	}
	return inst, true
}

// RemoveMob drops a dead non-respawning mob from the registry (TS destroy
// for the mimic; no despawn frame — entity.KillMob already sent it).
func (gameWorldAdapter) RemoveMob(instance string) {
	removeMob(instance)
}

func (gameWorldAdapter) SpawnMinion(bossInstance, key string, x, y int, opts entity.MinionOpts) string {
	inst := fmt.Sprintf("%s-minion-%d", bossInstance, minionSeq.Add(1))
	over := mobOverrides{Aggro: opts.AggroRange, Leash: opts.RoamDistance, NoRespawn: true}
	if !spawnMob(inst, key, x, y, over) {
		return ""
	}
	if m := mobFor(inst); m != nil {
		m.mu.Lock()
		if opts.AlwaysAggressive {
			m.prof.AlwaysAggro = true
		}
		if opts.AttackRange > 0 {
			m.prof.AttackRange = opts.AttackRange
		}
		if opts.NoRoam {
			f := false
			m.prof.Roaming = &f
		}
		m.mu.Unlock()
	}
	return inst
}

func (gameWorldAdapter) KillMinion(instance string) {
	m := mobFor(instance)
	if m == nil {
		return
	}
	entity.KillMob(m, nil, gameWorld, func() bool { return mobAlive(m) })
	removeMob(instance) // TS destroy: no registry entry, no respawn
}

func (gameWorldAdapter) MinionDied(bossInstance, minionInstance string) {
	_ = bossInstance
	removeMob(minionInstance)
}

func (gameWorldAdapter) SetMobTarget(mobInstance, playerInstance string) {
	m := mobFor(mobInstance)
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dead {
		return
	}
	m.target = playerInstance
	m.attackers[playerInstance] = time.Now()
}

func (gameWorldAdapter) TeleportMob(mobInstance string, x, y int) {
	m := mobFor(mobInstance)
	if m == nil {
		return
	}
	m.mu.Lock()
	m.x, m.y = x, y
	m.mu.Unlock()
	gameWorld.MoveMob(mobInstance, x, y)
}

func (gameWorldAdapter) ClearMobCombat(mobInstance string) {
	m := mobFor(mobInstance)
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.target = ""
	m.attackers = map[string]time.Time{}
}

func (gameWorldAdapter) MobPos(instance string) (int, int, bool) {
	m := mobFor(instance)
	if m == nil {
		return 0, 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.x, m.y, true
}

func (gameWorldAdapter) MobTalk(mobInstance, message string) {
	chatRouter{}.SendBubble(mobInstance, message, true, "")
}

func (gameWorldAdapter) HealMob(instance string, amount int) {
	m := mobFor(instance)
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.dead {
		m.mu.Unlock()
		return
	}
	hp := m.hp + amount
	if hp > m.maxHP {
		hp = m.maxHP
	}
	m.hp = hp
	max := m.maxHP
	m.mu.Unlock()
	gameWorld.MobPoints(instance, hp, max)
}

func (gameWorldAdapter) FollowStep(mobInstance string, tx, ty int) {
	m := mobFor(mobInstance)
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.dead {
		m.mu.Unlock()
		return
	}
	nx, ny, ok := entity.ChaseStep(m.x, m.y, tx, ty, 1, blocked)
	if ok {
		m.x, m.y = nx, ny
	}
	m.mu.Unlock()
	if ok {
		gameWorld.MoveMob(mobInstance, nx, ny)
	}
}

// ---------------------------------------------------------------------------
// Mob projectile spawning (visual-only; damage already applied by strikeMob).
// ---------------------------------------------------------------------------

// Mob projectile registry for in-flight mob shots (Who/List resolution
// between Spawn and impact Despawn). Separate from the player combat
// projPayloads registry to avoid key collisions.
var (
	mobProjMu       sync.Mutex
	mobProjSeq      int
	mobProjPayloads = map[string]EntityData{}
)

func (gameWorldAdapter) SpawnProjectile(projectileName, ownerInst, targetInst string, x, y, tx, ty int) {
	if projectileName == "" {
		return
	}
	mobProjSeq++
	inst := fmt.Sprintf("mpr-%d", mobProjSeq)
	// Flight time follows the projectile.ts rule (distance*90ms, internal/sim).
	travel := sim.TravelBetween(x, y, tx, ty)
	p := EntityData{
		Instance: inst, Type: EntityProjectile, Key: projectileName, Name: projectileName,
		X: x, Y: y, OwnerInstance: ownerInst, TargetInstance: targetInst,
	}
	mobProjMu.Lock()
	mobProjPayloads[inst] = p
	mobProjMu.Unlock()
	worldcore.SetEntityPos(inst, x, y)
	worldcore.Broadcast(pkt(PacketAnimation, animationData{Instance: ownerInst, Action: ActionAttack}))
	worldcore.Broadcast(pkt(PacketSpawn, p))
	log.Printf("m9: %s launched %s (%s) travel=%v", ownerInst, inst, projectileName, travel)
	time.AfterFunc(travel, func() {
		mobProjMu.Lock()
		delete(mobProjPayloads, inst)
		mobProjMu.Unlock()
		worldcore.RemoveEntity(inst)
		worldcore.Broadcast(pkt(PacketDespawn, despawnData{Instance: inst}))
	})
}

// ---------------------------------------------------------------------------
// Player HP / death / respawn (character.hitPoints + player.respawn).
// ---------------------------------------------------------------------------

// heroHPEntry stores both current and max HP for a hero instance.
// Max HP is level-scaled (formulas.ts getMaxHitPoints: 39 + level * 30).
type heroHPEntry struct {
	hp, maxHP int
}

var playerHPs sync.Map // instance -> heroHPEntry

// heroMaxHPFor looks up the level-scaled max HP for a username.
// Falls back to level-1 (69) when the player state is unavailable.
func heroMaxHPFor(username string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	level := st.Level
	pstateMu.Unlock()
	return meta.HeroMaxHPForLevel(level)
}

func (gameWorldAdapter) GetHeroHP(instance string) int {
	if v, ok := playerHPs.Load(instance); ok {
		return v.(heroHPEntry).hp
	}
	// Default: look up the player's level-scaled max HP.
	// Instance -> username resolution via worldcore.Find.
	if c, ok := worldcore.Find[*playerConn](instance); ok && c != nil {
		return heroMaxHPFor(c.Username)
	}
	return meta.HeroMaxHPForLevel(1) // fallback: level 1
}

func (gameWorldAdapter) SetHeroHP(instance string, hp int) {
	if v, ok := playerHPs.Load(instance); ok {
		entry := v.(heroHPEntry)
		entry.hp = hp
		playerHPs.Store(instance, entry)
		return
	}
	// First store: initialize with level-scaled max HP.
	maxHP := meta.HeroMaxHPForLevel(1)
	if c, ok := worldcore.Find[*playerConn](instance); ok && c != nil {
		maxHP = heroMaxHPFor(c.Username)
	}
	playerHPs.Store(instance, heroHPEntry{hp: hp, maxHP: maxHP})
}

func (gameWorldAdapter) ForgetHeroHP(instance string) {
	playerHPs.Delete(instance)
}

// HeroMaxHP returns the level-scaled max HP for a hero instance
// (formulas.ts getMaxHitPoints parity: 39 + level * 30).
func (gameWorldAdapter) HeroMaxHP(instance string) int {
	if v, ok := playerHPs.Load(instance); ok {
		return v.(heroHPEntry).maxHP
	}
	// Default: look up the player's level-scaled max HP.
	if c, ok := worldcore.Find[*playerConn](instance); ok && c != nil {
		return heroMaxHPFor(c.Username)
	}
	return meta.HeroMaxHPForLevel(1) // fallback: level 1
}

func (gameWorldAdapter) HeroPoints(instance string, hp, maxHP int) {
	worldcore.Broadcast(pkt(PacketPoints, pointsData{
		Instance: instance, HitPoints: intp(hp), MaxHitPoints: intp(maxHP),
	}))
}

func playerHP(c *playerConn) int {
	return gameWorld.GetHeroHP(c.Instance)
}

// mobDamagePlayer applies mob damage: Points frame, Death on empty.
func mobDamagePlayer(c *playerConn, dmg int, from *mob) {
	mobDamagePlayerThorns(c, dmg, from, false)
}

// mobDamagePlayerThorns ports player/handler.ts handleHit thorns block with
// the TS shape mirrored exactly: the dead/no-attacker guard and the
// isThorns loop guard live on RECEIPT, and the reflect call itself passes
// no thorns flag (mob receipt never reflects, so no loop is possible).
func mobDamagePlayerThorns(c *playerConn, dmg int, from *mob, isThorns bool) {
	if c == nil {
		return
	}
	// Invincible parity (TS character.hit: early return when
	// status.has(Modules.Effects.Invincible)). Blocks ALL incoming damage
	// including mob hits, DoT ticks, and admin damage commands.
	if abilities.HasStatusEffect(c.Instance, fxInvincible) {
		return
	}
	var f entity.Mob
	if from != nil {
		f = from
	}
	entity.DamageHero(gameWorld, c.Instance, c.Username, dmg, f)
	// Prevent endless loops of thorn damage.
	if isThorns {
		return
	}
	// Dead heroes and attackerless hits never reflect.
	if from == nil || gameWorld.GetHeroHP(c.Instance) <= 0 {
		return
	}
	thornsLevel := heroThornsLevel(c.Username)
	if thornsLevel < 1 {
		return
	}
	// 40% chance to activate thorns.
	if !lockedThornsRoll() {
		return
	}
	// Thorns damage is 10% per level of thorns enchantment, reflected via
	// attacker.hit(thornsDamage, player) — the full mob damage pipeline
	// (Points/death/loot + the attacker's own on-hit procs, TS-exact).
	thornsDamage := thornsReflect(dmg, thornsLevel)
	entity.HitMob(from,
		&entity.PlayerView{Instance: c.Instance, Username: c.Username},
		thornsDamage, gameWorld, time.Now(), func() bool {
			return mobAlive(from)
		})
}

// handleMobRespawn ports incoming.handleRespawn -> player.respawn: only when
// dead; teleport to spawn + Spawn broadcast + Respawn{x,y} + Points sync.
// The respawn tile is tracked via trackPos (persist parity: a disconnect
// right after respawn must relogin at spawn, not at the death tile).
func handleMobRespawn(c *playerConn) {
	if playerHP(c) > 0 {
		log.Printf("m9: invalid respawn request from %s", c.Username)
		return
	}
	// Home-point respawn: use the player's bound home when set, otherwise
	// fall back to the fixed spawn (entity.HeroSpawnX/Y).
	st := playerStateFor(c.Username)
	x, y := st.HomeX, st.HomeY
	if x == 0 && y == 0 {
		x, y = entity.HeroSpawnX, entity.HeroSpawnY
	}
	c.Sess.PlayerX, c.Sess.PlayerY = x, y
	if !entity.RespawnHero(gameWorld, c.Instance, x, y) {
		log.Printf("m9: invalid respawn request from %s", c.Username)
		return
	}
	deathFired.Delete(c.Instance) // next life dies loudly again
	trackPos(c)                   // tracks the respawn tile (plateauTrack rides along)
	plateauTrack(c)
	minigamePositionUpdate(c)  // respawn position can cross an area boundary
	areaPositionUpdate(c) // M10: area callbacks on the respawn tile too
	log.Printf("m9: %s respawned at %d,%d", c.Username, x, y)
}

// mobTargeting reports whether any live engine mob currently targets the
// instance (character.getAttackerCount/inCombat parity for the item-use
// combat gate). Lock order mobMu -> m.mu, leaf use only.
func mobTargeting(instance string) bool {
	if instance == "" {
		return false
	}
	mobMu.Lock()
	defer mobMu.Unlock()
	for _, m := range mobs {
		m.mu.Lock()
		target, dead := m.target, m.dead
		m.mu.Unlock()
		if !dead && target == instance {
			return true
		}
	}
	return false
}

// mobPlayerLeave drops per-player state on disconnect.
func mobPlayerLeave(c *playerConn) {
	playerHPs.Delete(c.Instance)
	deathFired.Delete(c.Instance)
	mobMu.Lock()
	for _, m := range mobs {
		m.mu.Lock()
		if m.target == c.Instance {
			m.target = ""
		}
		delete(m.attackers, c.Instance)
		m.mu.Unlock()
	}
	mobMu.Unlock()
}

// mobEngine boots the AI loop + adopts the demo mobs (main()).
func mobEngine() {
	loadMobTables()
	adoptExistingMobs()
	go func() {
		t := time.NewTicker(entity.RoamTick)
		defer t.Stop()
		for range t.C {
			mobTick()
		}
	}()
}

// adoptExistingMobs re-registers the demo mobs under the engine. m-rat-1 keeps
// its M3 slice-1 demo semantics (forced chase, no strikes, aggro 6, leash
// 10, respawn 10s). m1 (plain-mode rat) runs the full profile.
func adoptExistingMobs() {
	switch {
	case combatMode:
		spawnMob(combatRatInstance, "rat", combatRatX, combatRatY, mobOverrides{
			NoAttack: true, Chase: true, Aggro: combatRatAggro, Leash: combatRatLeash,
			Respawn: combatRatRespawnDelay,
		})
	case !testMode && !cleanMode:
		spawnMob("m1", "rat", 104, 104, mobOverrides{})
	}
}

// ratEntity returns the combat leash-demo rat's Spawn payload, or nil
// when it is dead/absent (combatSpawns() parity with the old ratData gate).
func ratEntity() *EntityData {
	m := mobFor(combatRatInstance)
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dead {
		return nil
	}
	d := m.data()
	return &d
}

// mobOnPlayerMoved is the position-update hook from the movement handler:
// Node runs detectAggro on every position change; the engine scans on the
// next tick, so this only fast-forwards the aggro scan for responsiveness.
func mobOnPlayerMoved(c *playerConn) {
	v := playerViewFor(c)
	mobMu.Lock()
	defer mobMu.Unlock()
	for _, m := range mobs {
		if m.dead {
			continue
		}
		m.mu.Lock()
		if m.target == "" && entity.CanAggro(m.prof, m.over, m.x, m.y, m.target, v) {
			m.target = c.Instance
			m.lastTgt = time.Now()
		}
		m.mu.Unlock()
	}
}

// handleMobTest is the TESTMAP-only debug dispatcher (m8test precedent):
// spawn/remove engine mobs and reposition the hero for deterministic e2e.
func handleMobTest(c *playerConn, data []byte) {
	if !testMode {
		return
	}
	// Admin-rank gate (see handleMinigameTest: TESTMAP default stays ON, the gate
	// closes the any-client warp/spawn hole).
	if !isAdmin(c) {
		return
	}
	var d struct {
		M9Test   string `json:"m9test"`
		Instance string `json:"instance"`
		Key      string `json:"key"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
		Aggro    int    `json:"aggro"`
		Leash    int    `json:"leash"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return
	}
	switch d.M9Test {
	case "spawn":
		spawnMob(d.Instance, d.Key, d.X, d.Y, mobOverrides{Aggro: d.Aggro, Leash: d.Leash})
	case "remove":
		if m := mobFor(d.Instance); m != nil {
			worldcore.Broadcast(pkt(PacketDespawn, despawnData{Instance: d.Instance}))
			removeMob(d.Instance)
		}
	case "tp": // reposition the hero server-side (seedPos precedent)
		if c != nil {
			c.Sess.PlayerX, c.Sess.PlayerY = d.X, d.Y
			worldcore.SetEntityPos(c.Instance, d.X, d.Y)
			// Every other server-side teleport (teleport/minigameTeleport,
			// worldApplyTeleport, walked movement) recomputes the
			// client's 9-region interest set — without it a debug tp
			// across regions leaves the client blind to region-scoped
			// frames (Spawn/Combat) at the landing tile.
			worldcore.UpdateRegion(c, d.X, d.Y)
			worldcore.Broadcast(pkt(PacketTeleport, teleportData{Instance: c.Instance, X: d.X, Y: d.Y}))
			trackPos(c) // persist parity: the test tile must survive a save
			plateauTrack(c)
			minigamePositionUpdate(c)
			mobOnPlayerMoved(c)     // position updates run the aggro scan
			areaPositionUpdate(c) // M10: camera/music/pvp/overlay area callbacks
		}
	case "mobhp": // debug echo: m9:mob=... hp=.../... x=... y=... tgt=...
		m := mobFor(d.Instance)
		if m == nil || c == nil {
			return
		}
		m.mu.Lock()
		echo := fmt.Sprintf("m9:mob=%s hp=%d/%d x=%d y=%d tgt=%s", d.Instance, m.hp, m.maxHP, m.x, m.y, m.target)
		m.mu.Unlock()
		notifyPlayer(c, echo)
	}
}
