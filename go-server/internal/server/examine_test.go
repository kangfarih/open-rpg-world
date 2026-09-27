package server

import (
	"encoding/json"
	"strings"
	"testing"

	"rpg-world-server/internal/entity"
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/player/stats"
)

// Examine resolution matrix (incoming.ts handleExamine parity):
// mob with description / mob without / item with / bag + unknown silent.
func TestExamineResolve(t *testing.T) {
	mobMu.Lock()
	mobs["ex-rat"] = &mob{instance: "ex-rat", key: "rat"}
	mobs["ex-golem"] = &mob{instance: "ex-golem", key: "golem"}
	mobMu.Unlock()
	t.Cleanup(func() { removeMob("ex-rat"); removeMob("ex-golem") })

	key, desc, ok := examineResolve("ex-rat")
	if !ok || key != "rat" || desc == "" {
		t.Fatalf("rat = %q,%q,%v, want description path", key, desc, ok)
	}
	if !strings.Contains(desc, "rat") {
		t.Fatalf("rat description = %q, want the mobs.json text", desc)
	}

	key, desc, ok = examineResolve("ex-golem")
	if !ok || key != "golem" || desc != "" {
		t.Fatalf("golem = %q,%q,%v, want ok with empty description (NO_IDEA path)", key, desc, ok)
	}

	item := entity.SpawnLootAt("u-ex", "logs", 1, 100, 96)
	if item == "" {
		t.Fatal("SpawnLootAt returned empty instance")
	}
	t.Cleanup(func() { entity.DestroyLoot(item, "test") })
	key, desc, ok = examineResolve(item)
	if !ok || key != "logs" || desc == "" {
		t.Fatalf("logs = %q,%q,%v, want items.json description", key, desc, ok)
	}

	bag := entity.SpawnLootBag("u-ex", 101, 96, []entity.Drop{
		{Key: "gold", Count: 1}, {Key: "logs", Count: 1},
	})
	if bag == "" {
		t.Fatal("SpawnLootBag returned empty instance")
	}
	t.Cleanup(func() { entity.DestroyLoot(bag, "test") })
	if _, _, ok := examineResolve(bag); ok {
		t.Fatal("lootbag must not resolve (neither mob nor item)")
	}
	if _, _, ok := examineResolve("no-such-entity"); ok {
		t.Fatal("unknown instance must not resolve")
	}
}

// Examine frame parsing accepts the stock [51,["inst"]] shape and the bare
// [51,"inst"] variant; unknown instances stay silent (no stats, no panic).
// Notifies ride the world bus (unregistered in unit tests), so the
// observable contract here is stats growth per valid shape.
func TestHandleExamineFrameShapes(t *testing.T) {
	c := &playerConn{Conn: gnet.NewConn(nil, "p-ex")}
	c.Username = "u-exshape"
	c.Sess.PlayerX, c.Sess.PlayerY = 100, 96
	mobMu.Lock()
	mobs["ex-shape"] = &mob{instance: "ex-shape", key: "rat"}
	mobs["ex-shape-bare"] = &mob{instance: "ex-shape-bare", key: "skeleton"}
	mobMu.Unlock()
	t.Cleanup(func() {
		removeMob("ex-shape")
		removeMob("ex-shape-bare")
		stats.Forget("u-exshape")
	})

	mk := func(payload string) clientFrame {
		rawID, _ := json.Marshal(PacketExamine)
		return clientFrame{rawID, json.RawMessage(payload)}
	}
	examined := func() []string {
		return stats.CopyOf("u-exshape").MobExamines
	}

	handleExamineReq(c, mk(`["ex-shape"]`))
	if got := examined(); len(got) != 1 || got[0] != "rat" {
		t.Fatalf("array shape examined = %v, want [rat]", got)
	}
	handleExamineReq(c, mk(`"ex-shape-bare"`))
	if got := examined(); len(got) != 2 || got[1] != "skeleton" {
		t.Fatalf("bare shape examined = %v, want [rat skeleton]", got)
	}
	handleExamineReq(c, mk(`["no-such-entity"]`))
	if got := examined(); len(got) != 2 {
		t.Fatalf("unknown instance examined = %v, want silence", got)
	}
	handleExamineReq(nil, mk(`["ex-shape"]`))
}
