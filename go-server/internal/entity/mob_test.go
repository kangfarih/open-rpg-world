package entity

import (
	"testing"
	"time"
)

func mustProfiles(t *testing.T) (map[string]*MobProfile, map[string]*SpawnOverride) {
	t.Helper()
	mobs := `{"rat":{"name":"Rat","level":2,"hitPoints":30,"aggroRange":6,"attackRange":1,"attackRate":1000,"movementSpeed":220,"respawnDelay":4000,"roamDistance":7,"aggressive":true},"ghost":{"name":"Ghost","hitPoints":0}}`
	spawns := `{"126-96":{"level":5,"hitPoints":140}}`
	profs, ovs, err := LoadProfiles([]byte(mobs), []byte(spawns))
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	return profs, ovs
}

func TestProfileMergeAndDefaults(t *testing.T) {
	profs, ovs := mustProfiles(t)

	p := ProfileFor(profs, ovs, "rat", 126, 96)
	if p == nil {
		t.Fatal("ProfileFor rat@126-96 = nil")
	}
	if p.Level != 5 || p.HitPoints != 140 {
		t.Fatalf("spawn override not merged: %+v", p)
	}
	if p.AggroRange != 6 || !p.Aggressive {
		t.Fatalf("base fields lost: %+v", p)
	}

	q := ProfileFor(profs, ovs, "rat", 1, 1)
	if q == nil || q.Level != 2 || q.HitPoints != 30 {
		t.Fatalf("base profile wrong: %+v", q)
	}

	if got := ProfileFor(profs, ovs, "nope", 1, 1); got != nil {
		t.Fatalf("unknown key = %+v, want nil", got)
	}
	if got := ProfileFor(profs, ovs, "ghost", 1, 1); got != nil {
		t.Fatalf("zero-HP profile = %+v, want nil", got)
	}

	bare := &MobProfile{HitPoints: 10}
	ApplyDefaults(bare)
	if bare.Level != 1 || bare.AggroRange != AggroRange || bare.AttackRange != 1 ||
		bare.AttackRate != DefaultAttackRate || bare.MovementSpeed != DefaultMoveSpeed ||
		bare.RespawnDelay != int(RespawnDelay/time.Millisecond) || bare.RoamDistance != RoamDistance ||
		bare.Roaming == nil || !*bare.Roaming {
		t.Fatalf("defaults not filled: %+v", bare)
	}

	if _, _, err := LoadProfiles([]byte("{bad"), []byte("{}")); err == nil {
		t.Fatal("LoadProfiles bad mobs JSON: want error")
	}
}

func TestRespawnDelayPrecedence(t *testing.T) {
	p := ratProfile()
	if got := RespawnDelayFor(MobOverrides{}, p); got != 4000*time.Millisecond {
		t.Fatalf("profile delay = %v", got)
	}
	if got := RespawnDelayFor(MobOverrides{Respawn: 10 * time.Second}, p); got != 10*time.Second {
		t.Fatalf("override delay = %v", got)
	}
}

// stepRoam drives StepMob until the mob moves (roam draws are random) or
// the budget runs out. now advances past the roam interval each pass.
func stepRoam(t *testing.T, m *testMob, w *simFake, now time.Time, budget int) (time.Time, bool) {
	t.Helper()
	for i := 0; i < budget; i++ {
		now = now.Add(RoamInterval() + time.Millisecond)
		StepMob(m, w, now)
		w.mu.Lock()
		moved := len(w.moves) > 0
		w.mu.Unlock()
		if moved {
			return now, true
		}
	}
	return now, false
}

func TestRoamMovesWithinRadius(t *testing.T) {
	w := newSimFake()
	m := newTestMob("mob-roam", "rat", ratProfile(), 100, 100)
	now := time.Now()
	if _, ok := stepRoam(t, m, w, now, 25); !ok {
		t.Fatal("roaming mob never moved in 25 roam intervals")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, mv := range w.moves {
		if Manhattan(100, 100, mv.x, mv.y) > m.prof.RoamDistance {
			t.Fatalf("roam target %v outside roam distance", mv)
		}
	}
	if got := w.entityAt["mob-roam"]; Manhattan(100, 100, got[0], got[1]) > m.prof.RoamDistance {
		t.Fatalf("mob ended outside roam distance: %v", got)
	}
}

func TestRoamDisabledStays(t *testing.T) {
	w := newSimFake()
	p := ratProfile()
	f := false
	p.Roaming = &f
	m := newTestMob("mob-still", "rat", p, 100, 100)
	now := time.Now()
	for i := 0; i < 5; i++ {
		now = now.Add(RoamInterval() + time.Millisecond)
		StepMob(m, w, now)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.moves) != 0 {
		t.Fatalf("non-roaming mob moved: %v", w.moves)
	}
}

func TestRoamBlockedStays(t *testing.T) {
	w := newSimFake()
	w.blocked = func(x, y int) bool { return true }
	m := newTestMob("mob-wall", "rat", ratProfile(), 100, 100)
	now := time.Now()
	for i := 0; i < 5; i++ {
		now = now.Add(RoamInterval() + time.Millisecond)
		StepMob(m, w, now)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.moves) != 0 {
		t.Fatalf("fully-blocked mob moved: %v", w.moves)
	}
}

func TestAggroChaseAndStrike(t *testing.T) {
	w := newSimFake()
	w.withPlayer("hero-1", "hero", 103, 100, 1, 1)
	m := newTestMob("mob-aggro", "rat", ratProfile(), 100, 100)
	m.lastRoam = time.Now() // suppress roam: isolate the aggro leg
	now := time.Now()

	StepMob(m, w, now)
	m.mu.Lock()
	tgt := m.target
	m.mu.Unlock()
	if tgt != "hero-1" {
		t.Fatalf("target = %q, want hero-1", tgt)
	}

	// Out of strike range but inside the leash: the mob chases.
	w.mu.Lock()
	w.players[0].X, w.players[0].Y = 110, 100
	w.mu.Unlock()
	m.mu.Lock()
	m.lastMove = now.Add(-FollowThrottle - time.Millisecond)
	m.mu.Unlock()
	StepMob(m, w, now.Add(time.Millisecond))
	w.mu.Lock()
	nMoves := len(w.moves)
	w.mu.Unlock()
	if nMoves == 0 {
		t.Fatal("chasing mob did not move")
	}
	m.mu.Lock()
	mx, my := m.x, m.y
	m.mu.Unlock()
	if Manhattan(mx, my, 110, 100) >= Manhattan(100, 100, 110, 100) {
		t.Fatalf("chase did not close distance: at %d,%d", mx, my)
	}

	// Adjacent melee with a stale attack clock: hold position, strike.
	w.mu.Lock()
	w.players[0].X, w.players[0].Y = mx+1, my
	w.mu.Unlock()
	m.mu.Lock()
	m.lastAtk = now.Add(-2 * time.Second)
	m.lastMove = now
	m.mu.Unlock()
	movesBefore := len(w.moves)
	StepMob(m, w, now.Add(2*time.Millisecond))
	w.mu.Lock()
	if len(w.moves) != movesBefore {
		w.mu.Unlock()
		t.Fatal("adjacent melee mob moved instead of holding")
	}
	if len(w.strikes) != 1 {
		w.mu.Unlock()
		t.Fatalf("strikes = %d, want 1", len(w.strikes))
	}
	st := w.strikes[0]
	hp := w.heroHP["hero-1"]
	w.mu.Unlock()
	if st.attacker != "mob-aggro" || st.target != "hero-1" {
		t.Fatalf("strike = %+v", st)
	}
	if st.dmg < 0 || hp != 69-st.dmg {
		t.Fatalf("strike dmg=%d heroHP=%d", st.dmg, hp)
	}
}

func TestLeashReturnsToSpawn(t *testing.T) {
	w := newSimFake()
	w.withPlayer("hero-1", "hero", 101, 100, 1, 1)
	p := ratProfile()
	p.RoamDistance = 5
	m := newTestMob("mob-leash", "rat", p, 100, 100)
	now := time.Now()
	StepMob(m, w, now)
	m.mu.Lock()
	if m.target == "" {
		m.mu.Unlock()
		t.Fatal("mob did not aggro adjacent hero")
	}
	m.mu.Unlock()

	// Let the mob chase one step off its spawn first (leash return only
	// emits a move when the mob is actually away from home).
	w.mu.Lock()
	w.players[0].X, w.players[0].Y = 103, 100
	w.mu.Unlock()
	m.mu.Lock()
	m.lastMove = now.Add(-FollowThrottle - time.Millisecond)
	m.mu.Unlock()
	StepMob(m, w, now.Add(time.Millisecond))
	m.mu.Lock()
	mx, my := m.x, m.y
	m.lastRoam = time.Now()
	m.mu.Unlock()
	if mx == 100 && my == 100 {
		t.Fatal("mob did not chase off its spawn")
	}

	// Drag the hero past roamDistance*2 from the spawn: drop + return.
	w.mu.Lock()
	w.players[0].X, w.players[0].Y = 100+5*2+1, 100
	w.mu.Unlock()
	StepMob(m, w, now.Add(2*time.Millisecond))
	m.mu.Lock()
	tgt, mx, my := m.target, m.x, m.y
	m.mu.Unlock()
	if tgt != "" {
		t.Fatalf("leashed mob kept target %q", tgt)
	}
	if mx != 100 || my != 100 {
		t.Fatalf("leashed mob at %d,%d, want spawn 100,100", mx, my)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.moves) == 0 {
		t.Fatal("leash return emitted no move")
	}
	last := w.moves[len(w.moves)-1]
	if last.x != 100 || last.y != 100 {
		t.Fatalf("leash return move = %+v", last)
	}
}

func TestAggroGates(t *testing.T) {
	now := time.Now()

	// Passive mob never targets.
	w := newSimFake()
	w.withPlayer("hero-1", "hero", 101, 100, 1, 1)
	p := ratProfile()
	p.Aggressive = false
	m := newTestMob("mob-passive", "rat", p, 100, 100)
	m.lastRoam = now
	for i := 0; i < 3; i++ {
		StepMob(m, w, now.Add(time.Duration(i)*time.Millisecond))
	}
	m.mu.Lock()
	tgt := m.target
	m.mu.Unlock()
	if tgt != "" {
		t.Fatalf("passive mob targeted %q", tgt)
	}

	// level*3 gate: a level-10 hero ignores a level-2 mob...
	w2 := newSimFake()
	w2.withPlayer("hero-9", "big", 101, 100, 10, 1)
	m2 := newTestMob("mob-gate", "rat", ratProfile(), 100, 100)
	m2.lastRoam = now
	StepMob(m2, w2, now)
	m2.mu.Lock()
	tgt2 := m2.target
	m2.mu.Unlock()
	if tgt2 != "" {
		t.Fatalf("level-gated mob targeted %q", tgt2)
	}

	// ...unless alwaysAggressive.
	p3 := ratProfile()
	p3.AlwaysAggro = true
	m3 := newTestMob("mob-always", "rat", p3, 100, 100)
	m3.lastRoam = now
	StepMob(m3, w2, now)
	m3.mu.Lock()
	tgt3 := m3.target
	m3.mu.Unlock()
	if tgt3 != "hero-9" {
		t.Fatalf("always-aggressive target = %q", tgt3)
	}
}

func TestHitRetaliateKillRespawn(t *testing.T) {
	resetAreas()
	w := newSimFake()
	// Kill credit resolves against existing players (TS
	// world.entities.get + isPlayer parity), so the attacker must be
	// registered — an unregistered attacker is a stale entry (no loot).
	w.withPlayer("hero-1", "hero", 100, 101, 1, 1)
	atk := &PlayerView{Instance: "hero-1", Username: "hero"}
	m := newTestMob("mob-fight", "rat", ratProfile(), 100, 100)
	now := time.Now()
	alive := true

	// Non-lethal hit: Points + retaliate.
	HitMob(m, atk, 5, w, now, func() bool { return alive })
	m.mu.Lock()
	hp, tgt := m.hp, m.target
	m.mu.Unlock()
	if hp != 25 || tgt != "hero-1" {
		t.Fatalf("after hit hp=%d target=%q", hp, tgt)
	}
	w.mu.Lock()
	if len(w.mobPts) != 1 || w.mobPts[0].hp != 25 {
		w.mu.Unlock()
		t.Fatalf("points = %+v", w.mobPts)
	}
	w.mu.Unlock()

	// Lethal hit: Despawn + loot + quest + respawn timer.
	HitMob(m, atk, 999, w, now, func() bool { return alive })
	m.mu.Lock()
	dead := m.dead
	m.mu.Unlock()
	if !dead {
		t.Fatal("mob survived lethal hit")
	}
	w.mu.Lock()
	if len(w.despawns) != 1 || w.despawns[0] != "mob-fight" {
		t.Fatalf("despawns = %v", w.despawns)
	}
	if len(w.lootDrops) != 1 || w.lootDrops[0].mobKey != "rat" || w.lootDrops[0].owner != "hero" {
		t.Fatalf("loot = %+v", w.lootDrops)
	}
	if len(w.quests) != 1 || w.quests[0].killer != "hero-1" || w.quests[0].mobKey != "rat" {
		t.Fatalf("quests = %+v", w.quests)
	}
	if len(w.delays) != 1 || w.delays[0].d != 4000*time.Millisecond {
		t.Fatalf("delays = %+v", w.delays)
	}
	w.mu.Unlock()

	// Hitting a corpse is a no-op.
	HitMob(m, atk, 5, w, now, func() bool { return alive })
	w.mu.Lock()
	nPts := len(w.mobPts)
	w.mu.Unlock()
	if nPts != 2 {
		t.Fatalf("dead-mob hit emitted points (%d)", nPts)
	}

	// Fire the respawn timer: full HP at spawn + Spawn frame.
	w.fireDelays()
	m.mu.Lock()
	dead, hp, mx, my := m.dead, m.hp, m.x, m.y
	m.mu.Unlock()
	if dead || hp != 30 || mx != 100 || my != 100 {
		t.Fatalf("respawned dead=%v hp=%d at %d,%d", dead, hp, mx, my)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.spawns) != 1 || w.spawns[0].Instance != "mob-fight" || w.spawns[0].HP != 30 {
		t.Fatalf("respawn spawns = %+v", w.spawns)
	}
}

// Environmental mob death (DoT tick, admin command): no attacker exists,
// so no killer is invented — but the full KillMob path still runs: Despawn,
// unowned loot (owner ""), chest hooks, the quest hook with an empty killer
// instance (nil-safe no-op downstream: no quest or statistics credit for
// anyone), and the respawn timer.
func TestKillWithoutKillerDropsUnownedLoot(t *testing.T) {
	resetAreas()
	w := newSimFake()
	m := newTestMob("mob-dot", "rat", ratProfile(), 100, 100)
	HitMob(m, nil, 999, w, time.Now(), func() bool { return true })
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.despawns) != 1 {
		t.Fatalf("despawns = %v", w.despawns)
	}
	if len(w.lootDrops) != 1 || w.lootDrops[0].mobKey != "rat" || w.lootDrops[0].owner != "" {
		t.Fatalf("environmental kill must drop unowned loot: %+v", w.lootDrops)
	}
	if len(w.quests) != 1 || w.quests[0].killer != "" || w.quests[0].mobKey != "rat" {
		t.Fatalf("quest hook must fire killerless (no credit): %+v", w.quests)
	}
	if len(w.delays) != 1 {
		t.Fatalf("respawn timer missing: %+v", w.delays)
	}
}

func TestDamageHeroDeathHook(t *testing.T) {
	w := newSimFake()
	w.withPlayer("hero-1", "hero", 100, 100, 1, 1)
	m := newTestMob("mob-killer", "rat", ratProfile(), 100, 101)

	// Non-lethal: Points only.
	DamageHero(w, "hero-1", "hero", 10, m)
	w.mu.Lock()
	if w.heroHP["hero-1"] != 69-10 || len(w.heroPts) != 1 || len(w.deaths) != 0 {
		w.mu.Unlock()
		t.Fatalf("hp=%v pts=%v deaths=%v", w.heroHP, w.heroPts, w.deaths)
	}
	w.mu.Unlock()

	// Lethal with a mob: target cleared + Death frame.
	m.mu.Lock()
	m.target = "hero-1"
	m.mu.Unlock()
	DamageHero(w, "hero-1", "hero", 69, m)
	m.mu.Lock()
	tgt := m.target
	m.mu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	if tgt != "" {
		t.Fatalf("killer kept target %q", tgt)
	}
	if len(w.deaths) != 1 || w.deaths[0].mob != "mob-killer" {
		t.Fatalf("deaths = %+v", w.deaths)
	}

	// Lethal without a source (DoT tick, admin command): Points to zero and
	// the HeroDied funnel still runs as an environmental death (empty
	// mobInstance — no killer is invented).
	w2 := newSimFake()
	DamageHero(w2, "hero-1", "hero", 69, nil)
	w2.mu.Lock()
	defer w2.mu.Unlock()
	if w2.heroHP["hero-1"] != 0 || len(w2.heroPts) != 1 {
		t.Fatalf("sourceless kill hp=%v pts=%v", w2.heroHP, w2.heroPts)
	}
	if len(w2.deaths) != 1 || w2.deaths[0].player != "hero-1" || w2.deaths[0].username != "hero" || w2.deaths[0].mob != "" {
		t.Fatalf("environmental death must run the funnel killerless: %+v", w2.deaths)
	}
}

func TestRespawnHero(t *testing.T) {
	w := newSimFake()

	if RespawnHero(w, "hero-1", HeroSpawnX, HeroSpawnY) {
		t.Fatal("live hero respawned")
	}
	w.mu.Lock()
	if len(w.teleports) != 0 {
		w.mu.Unlock()
		t.Fatal("live hero emitted frames")
	}
	w.mu.Unlock()

	w.SetHeroHP("hero-1", 0)
	if !RespawnHero(w, "hero-1", HeroSpawnX, HeroSpawnY) {
		t.Fatal("dead hero did not respawn")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.heroHP["hero-1"] != 69 {
		t.Fatalf("respawn hp = %d", w.heroHP["hero-1"])
	}
	if len(w.teleports) != 1 || w.teleports[0].x != HeroSpawnX || w.teleports[0].y != HeroSpawnY {
		t.Fatalf("teleports = %+v", w.teleports)
	}
	if len(w.spawnHeros) != 1 || len(w.respawned) != 1 || len(w.heroPts) != 1 {
		t.Fatalf("spawn=%v respawned=%v pts=%v", w.spawnHeros, w.respawned, w.heroPts)
	}
	if got := w.entityAt["hero-1"]; got != [2]int{HeroSpawnX, HeroSpawnY} {
		t.Fatalf("hero registry pos = %v", got)
	}
}

func TestChaseStepUnit(t *testing.T) {
	open := func(x, y int) bool { return false }

	// Adjacent melee holds.
	if _, _, ok := ChaseStep(100, 100, 101, 100, 1, open); ok {
		t.Fatal("adjacent melee stepped")
	}
	// Ranged adjacent still closes.
	if nx, ny, ok := ChaseStep(100, 100, 101, 100, 3, open); !ok || nx != 101 || ny != 100 {
		t.Fatalf("ranged chase = %d,%d,%v", nx, ny, ok)
	}
	// Diagonal pursuit.
	if nx, ny, ok := ChaseStep(100, 100, 102, 103, 1, open); !ok || nx != 101 || ny != 101 {
		t.Fatalf("diagonal chase = %d,%d,%v", nx, ny, ok)
	}
	// Diagonal blocked: X fallback.
	blocked := func(x, y int) bool { return x == 101 && y == 101 }
	if nx, ny, ok := ChaseStep(100, 100, 102, 103, 1, blocked); !ok || nx != 101 || ny != 100 {
		t.Fatalf("fallback chase = %d,%d,%v", nx, ny, ok)
	}
	// Fully boxed: hold.
	boxed := func(x, y int) bool { return x != 100 || y != 100 }
	if _, _, ok := ChaseStep(100, 100, 105, 105, 1, boxed); ok {
		t.Fatal("boxed mob stepped")
	}
}

func TestRollMobDamageBounds(t *testing.T) {
	p := ratProfile()
	bonus, str := 4, 3
	p.Bonuses.Strength = bonus
	p.Skills.Strength = str
	maxDmg := int(float64(bonus+str)*1.25) + 1 // mult=1, floor(rand^acc*(max+1)) <= max
	for i := 0; i < 200; i++ {
		if d := RollMobDamage(p, 1, 1, [5]int{}); d < 0 || d > maxDmg {
			t.Fatalf("damage %d out of [0,%d]", d, maxDmg)
		}
	}
}
