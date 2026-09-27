package entity

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// --- loot: TakeLoot atomic (single-Item duplication gap) ---

func TestTakeLootAtomicSingleClaim(t *testing.T) {
	inst := SpawnLootAt("hero", "logs", 1, 200, 200)
	if inst == "" {
		t.Fatal("spawn failed")
	}
	// First claim wins.
	got, ok := TakeLoot(inst)
	if !ok || got.Items[0].Key != "logs" {
		t.Fatalf("first TakeLoot = %+v,%v", got, ok)
	}
	if IsLoot(inst) {
		t.Fatal("taken loot still live")
	}
	// Second claim loses (no duplication).
	if _, ok := TakeLoot(inst); ok {
		t.Fatal("second TakeLoot must fail")
	}
	if _, ok := TakeLoot("nope"); ok {
		t.Fatal("unknown TakeLoot must fail")
	}
}

func TestTakeLootConcurrentSingleWinner(t *testing.T) {
	inst := SpawnLootAt("hero", "logs", 1, 201, 201)
	if inst == "" {
		t.Fatal("spawn failed")
	}
	var wg sync.WaitGroup
	wins := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok := TakeLoot(inst)
			wins <- ok
		}()
	}
	wg.Wait()
	close(wins)
	n := 0
	for ok := range wins {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("concurrent TakeLoot winners = %d, want 1", n)
	}
}

func TestBlinkLootClearsOwner(t *testing.T) {
	inst := SpawnLootAt("hero", "logs", 1, 202, 202)
	t.Cleanup(func() { DestroyLoot(inst, "test") })
	BlinkLoot(inst)
	l, ok := FindLoot(inst)
	if !ok {
		t.Fatal("loot gone after blink")
	}
	if l.Owner != "" {
		t.Fatalf("blink owner = %q, want empty", l.Owner)
	}
	BlinkLoot("nope") // no-op, no panic
}

func TestDoubleDropsEmptyNoPanic(t *testing.T) {
	prev := lootDeps
	ConfigureLoot(LootDeps{DoubleDrops: func([]Drop) []Drop { return nil }})
	defer ConfigureLoot(prev)
	if got := SpawnLoot("rat", 200, 200, "hero"); got != "" {
		t.Fatalf("empty DoubleDrops spawn = %q, want empty", got)
		DestroyLoot(got, "test")
	}
}

func TestRollEntryLevelClampNoPanic(t *testing.T) {
	entries := []DropJSON{{Key: "gold", Chance: 100000, Count: 1}, {Key: "arrow", Chance: 100000, Count: 1}}
	for _, lvl := range []int{0, -5} {
		for i := 0; i < 50; i++ {
			_, _, _ = RollEntry(entries, lvl)
		}
	}
}

func TestRollEntryGatedEmptyUserDeny(t *testing.T) {
	prev := lootDeps
	ConfigureLoot(LootDeps{Gate: func(u, q, a, s string) bool { return true }})
	defer ConfigureLoot(prev)
	gated := []DropJSON{{Key: "sword", Chance: 100000, Count: 1, Quest: "q1"}}
	// Empty user must deny gated entries (no survivors -> RollEntry false).
	if _, _, ok := RollEntryGated("", gated, 5); ok {
		t.Fatal("empty user must deny gated entries")
	}
	// Non-empty user with passing gate rolls.
	if _, _, ok := RollEntryGated("hero", gated, 5); !ok {
		// Chance 100000 always passes the probability roll, so must succeed.
		t.Fatal("gated entry with passing gate must roll")
	}
	// Nil gate denies gated even for non-empty users.
	ConfigureLoot(LootDeps{})
	if _, _, ok := RollEntryGated("hero", gated, 5); ok {
		t.Fatal("nil gate must deny gated entries")
	}
}

func TestFindLootAtOldestPick(t *testing.T) {
	a := SpawnLootAt("hero", "logs", 1, 210, 210)
	b := SpawnLootAt("hero", "logs", 1, 210, 210)
	if a == "" || b == "" {
		t.Fatal("spawn failed")
	}
	t.Cleanup(func() { DestroyLoot(a, "test"); DestroyLoot(b, "test") })
	got, ok := FindLootAt(210, 210)
	if !ok {
		t.Fatal("FindLootAt missed")
	}
	if got != a {
		t.Fatalf("FindLootAt = %q, want oldest %q", got, a)
	}
}

func TestOpenBagDestroyedFails(t *testing.T) {
	inst := SpawnLootBag("hero", 220, 220, []Drop{{Key: "a", Count: 1}, {Key: "b", Count: 1}})
	if inst == "" {
		t.Fatal("spawn failed")
	}
	if !OpenBag("p-open-1", inst) {
		t.Fatal("open live bag must succeed")
	}
	DestroyLoot(inst, "test")
	ClearBagOpener("p-open-1")
	if OpenBag("p-open-1", inst) {
		t.Fatal("open destroyed bag must fail")
	}
	if OpenBag("p-open-1", "nope") {
		t.Fatal("open unknown must fail")
	}
}

// --- areas: rgb panic, chest seq, double-open, idx guard, freeze ---

func TestOverlayColourShortRGBNoPanic(t *testing.T) {
	for _, rgb := range []string{"1,2", "255", "255,0", "  ", "255, 0, 0"} {
		a := &Area{RGB: rgb, Darkness: 0.5}
		_ = a.OverlayColour() // must not panic
	}
	a := &Area{RGB: "1,2", Darkness: 0.5}
	if c := a.OverlayColour(); c != "rgba(0, 0, 0, 0.5)" {
		t.Fatalf("2-elem rgb colour = %q, want default", c)
	}
	a = &Area{RGB: "255, 0, 0", Darkness: 0.5}
	if c := a.OverlayColour(); c != "rgba(255, 0, 0, 0.5)" {
		t.Fatalf("spaced rgb colour = %q", c)
	}
}

func TestChestSeqDistinct(t *testing.T) {
	resetAreas()
	LoadAreas([]byte(`{"width":100,"areas":{"chests":[{"id":1,"x":10,"y":10,"width":2,"height":2,"items":"bronzesword","spawnX":10,"spawnY":10}]}}`))
	w := newSimFake()
	area := ChestAreas()[0]
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		AddChestMob(area, string(rune('a'+i))+`-seq`, 0, w)
		RemoveChestMob(area, string(rune('a'+i))+`-seq`, "", w)
		ch := area.LiveChest()
		if ch == nil {
			t.Fatal("no chest after clear")
		}
		if seen[ch.Instance] {
			t.Fatalf("duplicate chest instance %q", ch.Instance)
		}
		seen[ch.Instance] = true
		// Re-arm for next iteration: repopulate then clear needs delay guard;
		// reset lastSpawn by adopting a fresh area via reset.
		area.mobMu.Lock()
		area.chest = nil
		area.lastSpawn = 0
		area.mobs = nil
		area.mobMu.Unlock()
	}
}

func TestOpenChestDoubleOpen(t *testing.T) {
	resetAreas()
	LoadAreas([]byte(`{"width":100,"areas":{"chests":[{"id":6,"x":10,"y":10,"width":2,"height":2,"items":"bronzesword","spawnX":10,"spawnY":10}]}}`))
	w := newSimFake()
	area := ChestAreas()[0]
	AddChestMob(area, "m-dbl", 0, w)
	RemoveChestMob(area, "m-dbl", "", w)
	ch := area.LiveChest()
	if ch == nil {
		t.Fatal("no chest")
	}
	if !OpenChest(ch, "hero-1", "hero", w) {
		t.Fatal("first open must succeed")
	}
	nLoot := len(w.regLoot)
	if OpenChest(ch, "hero-1", "hero", w) {
		t.Fatal("second open must fail (double-open guard)")
	}
	if len(w.regLoot) != nLoot {
		t.Fatal("double open spawned a second loot")
	}
	if OpenChest(nil, "h", "u", w) {
		t.Fatal("nil chest must fail")
	}
}

func TestRemoveChestMobUnknownNoSpawn(t *testing.T) {
	resetAreas()
	LoadAreas([]byte(`{"width":100,"areas":{"chests":[{"id":6,"x":10,"y":10,"width":2,"height":2,"items":"bronzesword","spawnX":10,"spawnY":10}]}}`))
	w := newSimFake()
	area := ChestAreas()[0]
	AddChestMob(area, "m-known", 0, w)
	RemoveChestMob(area, "m-ghost", "", w) // unknown: no-op
	if area.LiveChest() != nil {
		t.Fatal("unknown removal spawned a chest")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.chests) != 0 {
		t.Fatal("unknown removal emitted chest frame")
	}
}

func TestOverlayFreezeExitToNonFreezing(t *testing.T) {
	resetAreas()
	LoadAreas([]byte(`{"width":100,"areas":{"overlay":[{"id":1,"x":50,"y":50,"width":4,"height":4,"type":"freezing"},{"id":2,"x":60,"y":60,"width":4,"height":4,"type":"dark","darkness":0.5}]}}`))
	w := newSimFake()
	areasMu.Lock()
	var freezing, dark *Area
	for _, a := range overlayAreas {
		if a.Type == "freezing" {
			freezing = a
		} else {
			dark = a
		}
	}
	areasMu.Unlock()
	if freezing == nil || dark == nil {
		t.Fatal("fixture missing")
	}
	UpdateOverlay("hero-1", "hero", freezing, w)
	w.mu.Lock()
	if len(w.freezes) != 1 || !w.freezes[0].on {
		w.mu.Unlock()
		t.Fatalf("freeze enter = %+v", w.freezes)
	}
	w.mu.Unlock()
	// Move freezing -> non-freezing overlay: must clear freezing.
	UpdateOverlay("hero-1", "hero", dark, w)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.freezes) != 2 || w.freezes[1].on {
		t.Fatalf("freeze exit to dark = %+v, want clear", w.freezes)
	}
}

// --- mob: roaming nil, negative clamp, prune, overrides, zombie ---

func TestRoamNilRoamingNoPanic(t *testing.T) {
	w := newSimFake()
	m := newTestMob("mob-nilroam", "rat", ratProfile(), 100, 100)
	m.mu.Lock()
	m.prof.Roaming = nil
	m.mu.Unlock()
	m.Lock()
	roamMob(m, w)
	m.Unlock()
	// No panic = pass; nil means roaming (may or may not move, both fine).
}

func TestHitMobNegativeNoHeal(t *testing.T) {
	resetAreas()
	w := newSimFake()
	w.withPlayer("hero-1", "hero", 100, 101, 1, 1)
	atk := &PlayerView{Instance: "hero-1", Username: "hero"}
	m := newTestMob("mob-neg", "rat", ratProfile(), 100, 100)
	HitMob(m, atk, -50, w, time.Now(), func() bool { return true })
	m.mu.Lock()
	hp := m.hp
	m.mu.Unlock()
	if hp != 30 {
		t.Fatalf("negative HitMob healed to %d, want 30", hp)
	}
}

func TestDamageHeroNegativeNoHeal(t *testing.T) {
	w := newSimFake()
	w.withPlayer("hero-1", "hero", 100, 100, 1, 1)
	w.SetHeroHP("hero-1", 50)
	DamageHero(w, "hero-1", "hero", -20, nil)
	if got := w.GetHeroHP("hero-1"); got != 50 {
		t.Fatalf("negative DamageHero hp=%d, want 50", got)
	}
	DamageHero(w, "hero-1", "hero", 9999, nil)
	if got := w.GetHeroHP("hero-1"); got != 0 {
		t.Fatalf("overkill hp=%d, want 0", got)
	}
}

func TestAttackerPruneTimeoutVsFar(t *testing.T) {
	p := ratProfile()
	p.RoamDistance = 5
	now := time.Now()
	// Target stays close (no leash): pruning is the only mutation.
	m2 := newTestMob("mob-prune2", "rat", p, 100, 100)
	m2.mu.Lock()
	m2.target = "hero-close"
	m2.attackers = map[string]time.Time{"hero-gone": now.Add(-time.Millisecond)}
	m2.mu.Unlock()
	w2 := newSimFake()
	w2.withPlayer("hero-close", "close", 102, 100, 1, 1)
	StepMob(m2, w2, now)
	m2.mu.Lock()
	_, kept := m2.attackers["hero-gone"]
	m2.mu.Unlock()
	if !kept {
		t.Fatal("recently-gone attacker must survive until AttackerTimeout")
	}
	m3 := newTestMob("mob-prune3", "rat", p, 100, 100)
	m3.mu.Lock()
	m3.target = "hero-close"
	m3.attackers = map[string]time.Time{"hero-gone": now.Add(-AttackerTimeout - time.Second)}
	m3.mu.Unlock()
	StepMob(m3, w2, now)
	m3.mu.Lock()
	_, kept3 := m3.attackers["hero-gone"]
	m3.mu.Unlock()
	if kept3 {
		t.Fatal("stale gone attacker must be pruned after timeout")
	}
	// Far live attacker drops immediately.
	m4 := newTestMob("mob-prune4", "rat", p, 100, 100)
	m4.mu.Lock()
	m4.target = "hero-close"
	m4.attackers = map[string]time.Time{"hero-far": now}
	m4.mu.Unlock()
	w2.withPlayer("hero-far", "far", 200, 200, 1, 1)
	StepMob(m4, w2, now)
	m4.mu.Lock()
	_, kept4 := m4.attackers["hero-far"]
	m4.mu.Unlock()
	if kept4 {
		t.Fatal("far live attacker must be pruned")
	}
}

func TestEffectiveAggroLeashOverrides(t *testing.T) {
	p := ratProfile()
	p.AggroRange = 2
	p.RoamDistance = 5
	o := MobOverrides{Aggro: 10, Leash: 20}
	if got := EffectiveAggro(p, o); got != 10 {
		t.Fatalf("aggro override = %d", got)
	}
	if got := EffectiveRoam(p, o); got != 20 {
		t.Fatalf("leash override = %d", got)
	}
	if got := EffectiveAggro(p, MobOverrides{}); got != 2 {
		t.Fatalf("aggro default = %d", got)
	}
	// CanAggro honors the override: hero at distance 8 aggroes only with override.
	v := PlayerView{Instance: "h", X: 108, Y: 100, Level: 1}
	pp := ratProfile()
	pp.AggroRange = 2
	pp.Aggressive = true
	if CanAggro(pp, MobOverrides{}, 100, 100, "", v) {
		t.Fatal("profile range must not aggro at 8")
	}
	if !CanAggro(pp, MobOverrides{Aggro: 10}, 100, 100, "", v) {
		t.Fatal("override range must aggro at 8")
	}
}

func TestZombieRespawnNilAliveBlocked(t *testing.T) {
	resetAreas()
	w := newSimFake()
	m := newTestMob("mob-zombie", "rat", ratProfile(), 100, 100)
	KillMob(m, nil, w, nil)
	w.fireDelays()
	m.mu.Lock()
	dead := m.dead
	m.mu.Unlock()
	if !dead {
		t.Fatal("nil-alive zombie must stay dead (no respawn)")
	}
}

func TestDamageTableUsernameSticky(t *testing.T) {
	var dt DamageTable
	dt.Add("x", 10, "First")
	dt.Add("x", 5, "Second")
	r := dt.Rank()
	if len(r) != 1 || r[0].Username != "First" || r[0].Damage != 15 {
		t.Fatalf("username flap: %+v", r)
	}
	var dt2 DamageTable
	dt2.Add("y", 7, "")
	dt2.Add("y", 3, "Late")
	if r := dt2.Rank(); r[0].Username != "Late" {
		t.Fatalf("empty-first must fill late: %+v", r)
	}
}

func TestProfileForAppliesDefaults(t *testing.T) {
	profs := map[string]*MobProfile{"rat": {Name: "Rat", HitPoints: 10}}
	spawns := map[string]*SpawnOverride{}
	p := ProfileFor(profs, spawns, "rat", 0, 0)
	if p == nil {
		t.Fatal("nil profile")
	}
	if p.Roaming == nil || !*p.Roaming {
		t.Fatal("defaults must set Roaming true")
	}
	if p.Level != 1 || p.AggroRange != AggroRange {
		t.Fatalf("defaults missing: %+v", p)
	}
}

// --- pet: tick ghost + mirror ghost ---

type ghostWorld struct {
	fakeWorld
}

func TestPetTickGhostSkipped(t *testing.T) {
	reg := NewRegistry()
	if _, already := reg.Grant("ghost-owner", 0, 0, "rat", "ratpet", 1000); already {
		t.Fatal("grant failed")
	}
	w := newFakeWorld()
	w.pos["ghost-owner"] = [2]int{5, 0}
	// Remove before tick: ghost must not move/teleport.
	if _, ok := reg.RemoveByOwner("ghost-owner"); !ok {
		t.Fatal("remove failed")
	}
	reg.Tick(w)
	if len(w.moved) != 0 || len(w.teleported) != 0 {
		t.Fatalf("ghost tick emitted moves=%d teleports=%d", len(w.moved), len(w.teleported))
	}
}

func TestPetMirrorGhostSkipped(t *testing.T) {
	reg := NewRegistry()
	if _, already := reg.Grant("m-owner", 0, 0, "rat", "ratpet", 1000); already {
		t.Fatal("grant failed")
	}
	w := newFakeWorld()
	w.mobs["m-1"] = true
	if _, ok := reg.RemoveByOwner("m-owner"); !ok {
		t.Fatal("remove failed")
	}
	reg.Mirror(w, "m-owner", "m-1")
	if len(w.mobHits) != 0 {
		t.Fatal("ghost mirror must not hit")
	}
}

// --- companion: double-pickup CAS + nil guards ---

func TestCompanionDoublePickupCAS(t *testing.T) {
	// Isolate the global registry with a unique owner.
	owner := "cas-owner-1"
	companions.RemoveByOwner(owner)
	prev := cdeps
	adds := 0
	ConfigureCompanions(CompanionDeps{
		InventoryCount: func(string) int { return 0 },
		AddItem:        func(u, k string, c int) int { adds++; return 0 },
		MarkDirty:      func(string) {},
	})
	defer ConfigureCompanions(prev)
	defer companions.RemoveByOwner(owner)

	c := &CompanionConn{Instance: owner, Username: "cas-user", Send: func(...[]any) {}, Notify: func(string) {}}
	rec, already := companions.Grant(owner, 0, 0, "rat", "ratpet", 1)
	if already || rec == nil {
		t.Fatal("grant failed")
	}
	frame := []json.RawMessage{json.RawMessage(`58`), json.RawMessage(`{"opcode":0}`)}
	HandleCompanionPacket(c, frame)
	// Second (duplicate) pickup must lose the CAS: no second AddItem.
	HandleCompanionPacket(c, frame)
	if adds != 1 {
		t.Fatalf("AddItem calls = %d, want 1 (double-pickup)", adds)
	}
}

func TestCompanionNilGuardsNoPanic(t *testing.T) {
	HandleCompanionPacket(nil, nil)
	HandleCompanionPacket(&CompanionConn{}, nil)
	if GrantCompanion(nil, "rat", "ratpet") != nil {
		t.Fatal("nil conn grant must be nil")
	}
	ForgetCompanion("")
	CompanionTestHandler(nil, nil)
	MirrorCompanionSwing("", "")
	// No panic = pass.
}

// --- doors + dynamic nits ---

func TestDoorsWidthGuard(t *testing.T) {
	if got := loadDoors([]rawDoor{{ID: 1, X: 1, Y: 1, Destination: 2}, {ID: 2, X: 2, Y: 2, Destination: 1}}, 0); len(got) != 0 {
		t.Fatalf("width 0 doors = %d", len(got))
	}
	resetAreas()
	LoadAreas([]byte(`{"width":0,"areas":{"doors":[]}}`))
	if DoorAt(1, 1) != nil {
		t.Fatal("DoorAt with width 0 must be nil")
	}
}

func TestDynamicSignedOffset(t *testing.T) {
	resetAreas()
	LoadAreas([]byte(`{"width":100,"areas":{"dynamic":[{"id":7,"x":1,"y":1,"width":2,"height":2,"mapping":8,"quest":"q"},{"id":8,"x":5,"y":5,"width":2,"height":2}]}}`))
	done := stubProg{quests: map[string]bool{"q": true}}
	if mx, my, ok := DynamicRemap(1, 1, done); !ok || mx != 5 || my != 5 {
		t.Fatalf("remap(1,1)=%d,%d,%v", mx, my, ok)
	}
	if mx, my, ok := DynamicRemap(2, 2, done); !ok || mx != 6 || my != 6 {
		t.Fatalf("remap(2,2)=%d,%d,%v", mx, my, ok)
	}
}

func TestCombatLoopNoOrphanState(t *testing.T) {
	resetPlugins()
	w := newPluginFake()
	m := newTestMob("fd-orphan", "forestdragon", bossProfile(5000), 100, 100)
	// No target: tick must not create plugin state.
	PluginTick(m, w, time.Now())
	if _, ok := getState("fd-orphan"); ok {
		t.Fatal("idle combatLoop created orphan state")
	}
}

func TestInPolygonFloatNoPanic(t *testing.T) {
	a := &Area{ID: 1}
	a.Polygon = []struct {
		X int `json:"x"`
		Y int `json:"y"`
	}{{0, 0}, {10, 0}, {10, 10}, {0, 10}}
	if !a.Inside(5, 5) || a.Inside(20, 20) {
		t.Fatal("polygon wrong")
	}
}
