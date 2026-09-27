package entity

import (
	"sync"
	"testing"
)

// fakeWorld is a transport-free World double: it records side-effect calls
// so follow/teleport/mirror orchestration can be asserted without globals,
// frames, m5 state or the combat pipeline.
type fakeWorld struct {
	mu         sync.Mutex
	pos        map[string][2]int
	mobs       map[string]bool
	dummy      string
	dummyDead  bool
	spawned    []Record
	moved      []Record
	teleported []Record
	despawned  []string
	mobHits    []mobHit
	dummyHits  []dummyHit
}

type mobHit struct {
	pet, owner, target string
	dmg                int
}

type dummyHit struct {
	pet, owner string
	dmg        int
}

func newFakeWorld() *fakeWorld {
	return &fakeWorld{
		pos:   map[string][2]int{},
		mobs:  map[string]bool{},
		dummy: "m-dummy",
	}
}

func (f *fakeWorld) OwnerPos(owner string) (int, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pos[owner]
	if !ok {
		return 0, 0, false
	}
	return p[0], p[1], true
}

func (f *fakeWorld) SpawnPet(rec Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawned = append(f.spawned, rec)
}

func (f *fakeWorld) MovePet(rec Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moved = append(f.moved, rec)
}

func (f *fakeWorld) TeleportPet(rec Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teleported = append(f.teleported, rec)
}

func (f *fakeWorld) DespawnPet(instance string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.despawned = append(f.despawned, instance)
}

func (f *fakeWorld) IsMob(target string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mobs[target]
}

func (f *fakeWorld) HitMob(petInstance, ownerInstance, target string, dmg int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mobHits = append(f.mobHits, mobHit{petInstance, ownerInstance, target, dmg})
}

func (f *fakeWorld) HitDummy(petInstance, ownerInstance string, dmg int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dummyDead {
		return false
	}
	f.dummyHits = append(f.dummyHits, dummyHit{petInstance, ownerInstance, dmg})
	return true
}

func (f *fakeWorld) DummyTarget() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dummy
}

// Follow: owner a few tiles away (within teleport range) advances the pet by
// exactly one FollowStep tile and emits a single Move.
func TestTickFollowStepsOneTile(t *testing.T) {
	reg := NewRegistry()
	rec, already := reg.Grant("p1", 0, 0, "rat", "ratpet", 1000)
	if already || rec == nil {
		t.Fatalf("grant failed: rec=%v already=%v", rec, already)
	}
	w := newFakeWorld()
	w.pos["p1"] = [2]int{5, 0} // distance 5: follow, not teleport

	reg.Tick(w)

	if len(w.moved) != 1 {
		t.Fatalf("expected 1 move, got %d", len(w.moved))
	}
	if len(w.teleported) != 0 {
		t.Fatalf("expected 0 teleports, got %d", len(w.teleported))
	}
	got := w.moved[0]
	// FollowStep(5,0 <- 0,0) runs along X: (1,0).
	if got.X != 1 || got.Y != 0 {
		t.Fatalf("expected move to 1,0, got %d,%d", got.X, got.Y)
	}
	if got.Instance != rec.Instance || got.Owner != "p1" {
		t.Fatalf("move identity mismatch: %+v", got)
	}
	live, ok := reg.ByOwner("p1")
	if !ok {
		t.Fatalf("pet missing after follow")
	}
	if live.X != 1 || live.Y != 0 {
		t.Fatalf("registry not advanced: %d,%d", live.X, live.Y)
	}
}

// Teleport: owner beyond pets.TeleportDistance relocates the pet onto the
// owner tile and emits a single Teleport (no Move).
func TestTickTeleportToOwner(t *testing.T) {
	reg := NewRegistry()
	rec, _ := reg.Grant("p1", 0, 0, "rat", "ratpet", 1000)
	w := newFakeWorld()
	w.pos["p1"] = [2]int{20, 0} // distance 20 > 10: teleport

	reg.Tick(w)

	if len(w.teleported) != 1 {
		t.Fatalf("expected 1 teleport, got %d", len(w.teleported))
	}
	if len(w.moved) != 0 {
		t.Fatalf("expected 0 moves, got %d", len(w.moved))
	}
	got := w.teleported[0]
	if got.X != 20 || got.Y != 0 {
		t.Fatalf("expected teleport to 20,0, got %d,%d", got.X, got.Y)
	}
	if got.Instance != rec.Instance {
		t.Fatalf("teleport changed identity: %q vs %q", got.Instance, rec.Instance)
	}
	live, _ := reg.ByOwner("p1")
	if live.X != 20 || live.Y != 0 {
		t.Fatalf("registry not teleported: %d,%d", live.X, live.Y)
	}
}

// Adjacent pets hold still: no World calls.
func TestTickAdjacentNoMove(t *testing.T) {
	reg := NewRegistry()
	if _, already := reg.Grant("p1", 100, 96, "rat", "ratpet", 1000); already {
		t.Fatalf("grant reported already-have")
	}
	w := newFakeWorld()
	w.pos["p1"] = [2]int{101, 96} // distance 1: neither follow nor teleport

	reg.Tick(w)

	if len(w.moved) != 0 || len(w.teleported) != 0 {
		t.Fatalf("expected silence, got moves=%d teleports=%d", len(w.moved), len(w.teleported))
	}
}

// Mirror: owner swing at a killable mob routes a fixed-damage HitMob
// credited to the owner; dummy target routes HitDummy instead.
func TestMirrorMobAndDummy(t *testing.T) {
	reg := NewRegistry()
	if _, already := reg.Grant("p1", 0, 0, "rat", "ratpet", 1000); already {
		t.Fatalf("grant reported already-have")
	}
	w := newFakeWorld()
	w.mobs["m-1"] = true

	reg.Mirror(w, "p1", "m-1")
	if len(w.mobHits) != 1 {
		t.Fatalf("expected 1 mob hit, got %d", len(w.mobHits))
	}
	hit := w.mobHits[0]
	if hit.target != "m-1" || hit.owner != "p1" || hit.dmg != MirrorDamage {
		t.Fatalf("mob hit mismatch: %+v", hit)
	}
	if len(w.dummyHits) != 0 {
		t.Fatalf("unexpected dummy hits: %d", len(w.dummyHits))
	}

	reg.Mirror(w, "p1", "m-dummy")
	if len(w.dummyHits) != 1 {
		t.Fatalf("expected 1 dummy hit, got %d", len(w.dummyHits))
	}
	if w.dummyHits[0].dmg != MirrorDamage {
		t.Fatalf("dummy dmg mismatch: %+v", w.dummyHits[0])
	}

	// Unknown target: silent no-op.
	reg.Mirror(w, "p1", "nope")
	if len(w.mobHits) != 1 || len(w.dummyHits) != 1 {
		t.Fatalf("unknown target must be a no-op")
	}

	// Owner without a pet: silent no-op.
	reg.Mirror(w, "ghost", "m-1")
	if len(w.mobHits) != 1 {
		t.Fatalf("petless owner must be a no-op")
	}
}

// Grant guard: second grant for the same owner reports already-have and
// keeps the original record; ResolveKey/LookupItem keep the items.json table.
func TestGrantAlreadyHaveAndKeys(t *testing.T) {
	reg := NewRegistry()
	first, already := reg.Grant("p1", 3, 4, "rat", "", 1000)
	if already || first == nil {
		t.Fatalf("first grant failed")
	}
	if first.ItemKey != "ratpet" {
		t.Fatalf("item default mismatch: %q", first.ItemKey)
	}
	if _, already := reg.Grant("p1", 9, 9, "cat", "catpet", 2000); !already {
		t.Fatalf("second grant must report already-have")
	}
	live, _ := reg.ByOwner("p1")
	if live.X != 3 || live.Y != 4 || live.MobKey != "rat" {
		t.Fatalf("original record must win: %+v", live)
	}

	if mob, item := ResolveKey("ratpet"); mob != "rat" || item != "ratpet" {
		t.Fatalf("resolve item key: %q %q", mob, item)
	}
	if mob, item := ResolveKey("cat"); mob != "cat" || item != "catpet" {
		t.Fatalf("resolve mob key: %q %q", mob, item)
	}
	if mob, ok := LookupItem("rathatpet"); !ok || mob != "rathat" {
		t.Fatalf("lookup item: %q %v", mob, ok)
	}
	if _, ok := LookupItem("sword"); ok {
		t.Fatalf("unknown item must miss")
	}
}
