package server

import (
	"encoding/json"
	"testing"

	"rpg-world-server/internal/entity"
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/player/stats"
)

func lootTestConn(instance, username string, x, y int) *playerConn {
	c := &playerConn{Conn: gnet.NewConn(nil, instance)}
	c.Username = username
	c.Sess.PlayerX, c.Sess.PlayerY = x, y
	return c
}

func lootTakeFrame(index int) clientFrame {
	rawID, _ := json.Marshal(PacketLootBag)
	rawData, _ := json.Marshal(map[string]any{"opcode": LootBagTake, "index": index})
	return clientFrame{rawID, rawData}
}

func cleanupLootUser(username string) {
	pstateMu.Lock()
	delete(pstates, username)
	pstateMu.Unlock()
	stats.Forget(username)
}

// Take validation matrix: no-open deny, single-stack transfer with stable
// indices, hole + out-of-range deny, distance deny, owner deny, full
// inventory deny, emptying destroys.
func TestLootBagTakeValidation(t *testing.T) {
	const user = "u-lbtake"
	t.Cleanup(func() { cleanupLootUser(user) })
	c := lootTestConn("p-lbtake", user, 100, 96)
	t.Cleanup(func() { entity.ClearBagOpener("p-lbtake") })

	bag := entity.SpawnLootBag(user, 100, 96, []entity.Drop{
		{Key: "gold", Count: 5}, {Key: "logs", Count: 2}, {Key: "arrow", Count: 9},
	})
	if bag == "" {
		t.Fatal("SpawnLootBag returned empty instance")
	}
	t.Cleanup(func() { entity.DestroyLoot(bag, "test") })

	// Take with no open bag: denied, loot untouched.
	handleLootBagReq(c, lootTakeFrame(0))
	if invCount(user, "gold") != 0 {
		t.Fatal("take without an open bag must transfer nothing")
	}

	// Open, then take the middle stack.
	if !sendLootBagOpen(c, bag) {
		t.Fatal("sendLootBagOpen must succeed on a live bag")
	}
	handleLootBagReq(c, lootTakeFrame(1))
	if invCount(user, "logs") != 2 {
		t.Fatalf("logs in inventory = %d, want 2", invCount(user, "logs"))
	}
	if slots, _ := entity.BagSlots(bag); len(slots) != 2 {
		t.Fatalf("bag slots after take = %d, want 2", len(slots))
	}

	// Hole + out-of-range takes: denied.
	handleLootBagReq(c, lootTakeFrame(1))
	handleLootBagReq(c, lootTakeFrame(9))
	if invCount(user, "logs") != 2 || invCount(user, "gold") != 0 {
		t.Fatal("hole/oob takes must transfer nothing")
	}

	// Distance: too far denies, adjacent takes.
	c.Sess.PlayerX, c.Sess.PlayerY = 110, 110
	handleLootBagReq(c, lootTakeFrame(0))
	if invCount(user, "gold") != 0 {
		t.Fatal("far take must transfer nothing")
	}
	c.Sess.PlayerX, c.Sess.PlayerY = 100, 96
	handleLootBagReq(c, lootTakeFrame(0))
	if invCount(user, "gold") != 5 {
		t.Fatalf("gold in inventory = %d, want 5", invCount(user, "gold"))
	}

	// Last stack empties and destroys the bag (Close + Despawn path).
	handleLootBagReq(c, lootTakeFrame(2))
	if entity.IsLoot(bag) {
		t.Fatal("emptied bag must be destroyed")
	}
	if _, ok := entity.ActiveBag("p-lbtake"); ok {
		t.Fatal("destroy must clear the opener")
	}
	if invCount(user, "arrow") != 9 {
		t.Fatalf("arrow in inventory = %d, want 9", invCount(user, "arrow"))
	}
}

// Foreign-owned bags deny takes (item:CANNOT_ACCESS_LOOTBAG parity);
// a full inventory denies with misc:NO_SPACE parity.
func TestLootBagTakeDenies(t *testing.T) {
	const user = "u-lbdeny"
	t.Cleanup(func() { cleanupLootUser(user) })
	c := lootTestConn("p-lbdeny", user, 100, 96)
	t.Cleanup(func() { entity.ClearBagOpener("p-lbdeny") })

	foreign := entity.SpawnLootBag("someone-else", 100, 96, []entity.Drop{
		{Key: "gold", Count: 3}, {Key: "logs", Count: 1},
	})
	if foreign == "" {
		t.Fatal("SpawnLootBag returned empty instance")
	}
	t.Cleanup(func() { entity.DestroyLoot(foreign, "test") })
	if !sendLootBagOpen(c, foreign) {
		t.Fatal("open records the opener even for foreign bags (take gates)")
	}
	handleLootBagReq(c, lootTakeFrame(0))
	if invCount(user, "gold") != 0 {
		t.Fatal("foreign bag take must transfer nothing")
	}
	if slots, _ := entity.BagSlots(foreign); len(slots) != 2 {
		t.Fatal("denied take must leave the bag intact")
	}

	full := entity.SpawnLootBag(user, 100, 96, []entity.Drop{
		{Key: "gold", Count: 3}, {Key: "logs", Count: 1},
	})
	if full == "" {
		t.Fatal("SpawnLootBag returned empty instance")
	}
	t.Cleanup(func() { entity.DestroyLoot(full, "test") })
	for i := 0; i < ModulesInventorySize; i++ {
		addItem(user, "fillitem", 1)
	}
	if !sendLootBagOpen(c, full) {
		t.Fatal("open must succeed on the fresh bag")
	}
	handleLootBagReq(c, lootTakeFrame(0))
	if invCount(user, "gold") != 0 {
		t.Fatal("full-inventory take must transfer nothing")
	}
	if slots, _ := entity.BagSlots(full); len(slots) != 2 {
		t.Fatal("denied take must leave the bag intact")
	}
}
