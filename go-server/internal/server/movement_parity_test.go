package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"rpg-world-server/internal/abilities"
	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/entity"
	"rpg-world-server/internal/protocol"
)

// Movement-path parity (player.ts handleMovementRequest:1117-1141 +
// handleMovementStep:1191-1227): stun/teleport halt, container/bag/
// crafting/talk clears, in TS order.

// moveTestConn registers a fabricated live conn and drops every piece of
// per-user state the movement path touches.
func moveTestConn(t *testing.T, instance, username string) *playerConn {
	t.Helper()
	c, _ := deathConn(t, instance, username)
	t.Cleanup(func() {
		abilities.ClearStatus(instance)
		entity.ClearBagOpener(instance)
		controller.ForgetSession(username)
		c.sessMu.Lock()
		c.teleporting = false
		c.sessMu.Unlock()
		pstateMu.Lock()
		delete(pstates, username)
		pstateMu.Unlock()
	})
	return c
}

func moveRequest(x, y int) clientMovement {
	return clientMovement{Opcode: intp(MovementRequest), RequestX: intp(x), RequestY: intp(y)}
}

func hasFrame(frames [][]any, id int, op *int) bool {
	for _, f := range frames {
		if len(f) < 1 {
			continue
		}
		fid, ok := f[0].(int)
		if !ok || fid != id {
			continue
		}
		if op == nil {
			return true
		}
		if len(f) < 2 {
			continue
		}
		if o, ok := f[1].(int); ok && o == *op {
			return true
		}
	}
	return false
}

func framesContain(frames [][]any, s string) bool {
	for _, f := range frames {
		raw, _ := json.Marshal(f)
		if strings.Contains(string(raw), s) {
			return true
		}
	}
	return false
}

// Stun (weapon proc FxStun=4 and /toggle CmdEffectStun=4 share the ID)
// halts a Request: Stop + teleport-back + Positions, no disconnect, no
// position change, and — TS order — the clears do NOT run on the block.
func TestMovementStunBlocksRequest(t *testing.T) {
	const user, inst = "mv-stun-user", "mv-stun-inst"
	c := moveTestConn(t, inst, user)

	abilities.ApplyStatusEffect(inst, fxStun, stunDurationMs)
	if !abilities.HasStatusEffect(inst, fxStun) {
		t.Fatal("stun must be live in the status tracker")
	}
	c.SetTalkKey("20-21")
	c.SetTalkIndex(3)
	drainOutbox(c)

	if disconnect := handleMovement(c, moveRequest(101, 96)); disconnect {
		t.Fatal("stun block must not disconnect")
	}
	if c.Sess.PlayerX != 100 || c.Sess.PlayerY != 96 {
		t.Fatalf("pos = %d,%d, want unchanged 100,96", c.Sess.PlayerX, c.Sess.PlayerY)
	}
	frames := drainOutbox(c)
	if !hasFrame(frames, PacketMovement, intp(MovementStop)) {
		t.Fatal("stun block must emit Movement Stop (stopMovement parity)")
	}
	if !hasFrame(frames, PacketTeleport, nil) {
		t.Fatal("stun block must emit the teleport-back correction")
	}
	if !hasFrame(frames, PacketList, intp(ListPositions)) {
		t.Fatal("stun block must emit the List Positions correction")
	}
	// TS order: the halt returns before the clears (player.ts:1122-1127).
	if c.TalkKey() != "20-21" || c.TalkIndex() != 3 {
		t.Fatalf("blocked Request must skip clears, talk=%q idx=%d", c.TalkKey(), c.TalkIndex())
	}
}

// Server teleports arm the 500ms grace window (character.teleport
// setTimeout parity): Requests inside it halt exactly like a stun, and the
// window clears on its own.
func TestMovementTeleportBlocksRequest(t *testing.T) {
	const user, inst = "mv-tp-user", "mv-tp-inst"
	c := moveTestConn(t, inst, user)

	teleport(c, 102, 96) // funnel arms the flag + tracks the landing tile
	if !c.isTeleporting() {
		t.Fatal("teleport must arm the teleporting flag")
	}
	drainOutbox(c)

	if disconnect := handleMovement(c, moveRequest(103, 96)); disconnect {
		t.Fatal("teleport block must not disconnect")
	}
	if c.Sess.PlayerX != 102 || c.Sess.PlayerY != 96 {
		t.Fatalf("pos = %d,%d, want unchanged 102,96", c.Sess.PlayerX, c.Sess.PlayerY)
	}
	if frames := drainOutbox(c); !hasFrame(frames, PacketMovement, intp(MovementStop)) {
		t.Fatal("teleport block must emit Movement Stop")
	}

	time.Sleep(600 * time.Millisecond)
	if c.isTeleporting() {
		t.Fatal("teleporting flag must clear ~500ms after the teleport")
	}
	if movementBlocked(c) {
		t.Fatal("movement must be allowed once the window expires")
	}
}

// Every unblocked Request clears container access, the open bag, the
// crafting interface and talk (player.ts:1125-1127 + Step:1224 resetTalk),
// before verify — so Take and Craft both fail afterwards.
func TestMovementRequestClearsState(t *testing.T) {
	const user, inst = "mv-clear-user", "mv-clear-inst"
	c := moveTestConn(t, inst, user)
	t.Cleanup(func() { cleanupLootUser(user) })

	c.SetCanAccess(true)
	controller.HandleTest(c, []byte(`{"m12test":"craftif","iface":5}`), tradeDeps())
	bag := entity.SpawnLootBag(user, 100, 96, []entity.Drop{{Key: "gold", Count: 5}, {Key: "logs", Count: 2}})
	if bag == "" {
		t.Fatal("SpawnLootBag returned empty instance")
	}
	t.Cleanup(func() { entity.DestroyLoot(bag, "test") })
	if !entity.OpenBag(inst, bag) {
		t.Fatal("OpenBag must record the opener")
	}
	c.SetTalkKey("20-21")
	c.SetTalkIndex(3)
	drainOutbox(c)

	if disconnect := handleMovement(c, moveRequest(101, 96)); disconnect {
		t.Fatal("unblocked Request must not disconnect")
	}
	if c.CanAccess() {
		t.Fatal("move must revoke container access")
	}
	if c.TalkKey() != "" || c.TalkIndex() != 0 {
		t.Fatalf("move must reset talk, talk=%q idx=%d", c.TalkKey(), c.TalkIndex())
	}
	if _, ok := entity.ActiveBag(inst); ok {
		t.Fatal("move must clear the lootbag opener")
	}
	// Crafting with a cleared interface is rejected with CANNOT_DO_THAT
	// (incoming.handleCrafting activeCraftingInterface===-1 parity).
	raw, _ := json.Marshal(map[string]any{"opcode": protocol.CraftingSelect, "key": "sword"})
	controller.HandleCrafting(c, raw, tradeDeps())
	if frames := drainOutbox(c); !framesContain(frames, "misc:CANNOT_DO_THAT") {
		t.Fatal("craft after move must be rejected with misc:CANNOT_DO_THAT")
	}
	// Take with a cleared opener transfers nothing.
	handleLootBagReq(c, lootTakeFrame(0))
	if invCount(user, "gold") != 0 {
		t.Fatal("take after move must transfer nothing")
	}
}

// Step still applies the position update and resets talk (TS Step:1223-1224
// setPosition + resetTalk), and a stunned Step halts without returning.
func TestMovementStepClearsTalk(t *testing.T) {
	const user, inst = "mv-step-user", "mv-step-inst"
	c := moveTestConn(t, inst, user)

	c.SetTalkKey("20-21")
	c.SetTalkIndex(3)
	step := clientMovement{Opcode: intp(MovementStep), PlayerX: intp(101), PlayerY: intp(96)}
	if disconnect := handleMovement(c, step); disconnect {
		t.Fatal("Step must not disconnect")
	}
	if c.Sess.PlayerX != 101 || c.Sess.PlayerY != 96 {
		t.Fatalf("pos = %d,%d, want 101,96", c.Sess.PlayerX, c.Sess.PlayerY)
	}
	if c.TalkKey() != "" || c.TalkIndex() != 0 {
		t.Fatalf("Step must reset talk, talk=%q idx=%d", c.TalkKey(), c.TalkIndex())
	}

	// Stunned Step: halt emitted, processing falls through (no TS return).
	abilities.ApplyStatusEffect(inst, fxStun, stunDurationMs)
	drainOutbox(c)
	step2 := clientMovement{Opcode: intp(MovementStep), PlayerX: intp(102), PlayerY: intp(96)}
	if disconnect := handleMovement(c, step2); disconnect {
		t.Fatal("stunned Step must not disconnect")
	}
	if frames := drainOutbox(c); !hasFrame(frames, PacketMovement, intp(MovementStop)) {
		t.Fatal("stunned Step must emit Movement Stop")
	}
}
