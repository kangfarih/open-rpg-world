package server

import (
	"testing"

	"rpg-world-server/internal/controller"
)

// Batch hardening: nil guards, admin gates, shared-rand lock, mobOf nil.

// isAdmin: nil safe, rank-gated.
func TestM7IsAdminGate(t *testing.T) {
	if isAdmin(nil) {
		t.Fatal("nil conn must not be admin")
	}
	c := lootTestConn("p-admin-gate", "u-admin-gate", 100, 96)
	t.Cleanup(func() { cleanupLootUser("u-admin-gate") })
	// Fresh conn defaults to RankNone.
	if isAdmin(c) {
		t.Fatal("fresh conn must not be admin")
	}
	chatStateFor(c).rank = RankModerator
	if isAdmin(c) {
		t.Fatal("moderator must not pass the admin gate")
	}
	chatStateFor(c).rank = RankAdmin
	if !isAdmin(c) {
		t.Fatal("admin must pass the gate")
	}
}

// mobOf nil: every MobAdmin accessor must be nil-safe.
func TestMobOfNilSafe(t *testing.T) {
	d := cmdDeps()
	if got := d.Mobs.MobInstance(nil); got != "" {
		t.Fatalf("MobInstance(nil) = %q, want empty", got)
	}
	if got := d.Mobs.MobKey(nil); got != "" {
		t.Fatalf("MobKey(nil) = %q, want empty", got)
	}
	if got := d.Mobs.MobHP(nil); got != 0 {
		t.Fatalf("MobHP(nil) = %d, want 0", got)
	}
	if x, y := d.Mobs.MobPos(nil); x != 0 || y != 0 {
		t.Fatalf("MobPos(nil) = %d,%d, want 0,0", x, y)
	}
	// Mutators must not panic on nil.
	d.Mobs.SetMobPos(nil, 1, 1)
	d.Mobs.Attack(nil, nil)
	d.Mobs.AttackTarget(nil, "p-x")
	d.Mobs.ClearTarget(nil)
	d.Mobs.HitMob(nil, 10)
	// Wrapper entry points must not panic on nil mobs.
	setMobPos(nil, 1, 1)
	mobAttack(nil, nil)
	mobAttackTarget(nil, "p-x")
	mobRoam(nil)
	var _ controller.MobHandle
}

// Nil-receiver Conn methods must not panic.
func TestNilReceiverConnMethods(t *testing.T) {
	var c *playerConn
	if c.InstanceID() != "" {
		t.Fatal("nil InstanceID must be empty")
	}
	if c.PlayerName() != "" {
		t.Fatal("nil PlayerName must be empty")
	}
	if c.TileX() != 0 || c.TileY() != 0 {
		t.Fatal("nil Tile must be 0,0")
	}
	if c.Rank() != 0 {
		t.Fatal("nil Rank must be 0")
	}
	if c.MovementSpeed() != 0 {
		t.Fatal("nil MovementSpeed must be 0")
	}
	c.SetMovementSpeed(99) // must not panic
	c.GrantContainerAccess()
	if c.StoreOpen() != "" || c.CanAccess() || c.TalkKey() != "" || c.TalkIndex() != 0 {
		t.Fatal("nil store/talk accessors must be zero")
	}
}

// Network entry nil guards must not panic.
func TestNilGuardsNoPanic(t *testing.T) {
	pickup(nil, "loot-x")
	pickupAtTile(nil, 100, 96)
	trackPos(nil)
	teleport(nil, 1, 1)
	minigameTeleport(nil, 1, 1)
	openChest(nil, nil)
	takeLootBagItem(nil, 0)
	if worldSignTalk(nil, "1-1") {
		t.Fatal("nil sign talk must be false")
	}
	handleWho(nil, nil)
	handleSyncReq(nil, nil)
	handleList(nil)
	handleTarget(nil, nil)
	openLootBagFor(nil, "x")
	sendLootBagOpen(nil, "x")
	if lootBagOwnerDenied(nil, "someone") {
		t.Fatal("nil owner check must not deny")
	}
	if playerByName("") != nil {
		// Empty lookup must not panic; result is nil when offline.
		t.Log("empty lookup returned non-nil (online user with empty name?)")
	}
}

// Shared-rand locked helpers must be callable (race-guarded).
func TestLockedProcRolls(t *testing.T) {
	for i := 0; i < 50; i++ {
		_, _ = lockedHeroDamageType("u-proc-batch")
		_ = lockedBloodsuckRoll()
		_ = lockedThornsRoll()
	}
	cleanupLootUser("u-proc-batch")
}
