package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/persist"
	worldcore "rpg-world-server/internal/world"
)

// openDeathDB installs a real sqlite persist store in a temp dir (the exact
// savePlayerSync production path, no stub) and restores the globals afterwards.
func openDeathDB(t *testing.T) {
	t.Helper()
	st, err := persist.Open(filepath.Join(t.TempDir(), "death.db"))
	if err != nil {
		t.Fatalf("persist.Open: %v", err)
	}
	oldStore, oldDB := persistStore, dbConn
	persistStore, dbConn = st, st.DB()
	t.Cleanup(func() {
		persistStore, dbConn = oldStore, oldDB
		_ = st.DB().Close()
	})
}

// deathConn registers a fabricated live conn (fresh outbox, nil socket) in
// the world registry and returns it with its registry key.
func deathConn(t *testing.T, instance, username string) (*playerConn, *websocket.Conn) {
	t.Helper()
	c := &playerConn{Conn: gnet.NewConn(nil, instance)}
	c.Username = username
	c.Sess.PlayerX, c.Sess.PlayerY = 100, 96
	key := &websocket.Conn{}
	worldcore.AddPlayer(key, c)
	t.Cleanup(func() { worldcore.Default.RemoveWS(key) })
	return c, key
}

// drainOutbox non-blockingly collects every queued frame for a conn.
func drainOutbox(c *playerConn) [][]any {
	var out [][]any
	for {
		select {
		case f := <-c.Conn.Outbox:
			out = append(out, f)
		default:
			return out
		}
	}
}

// frameInstance extracts the instance payload of a Death/Despawn frame:
// Death is [29, instance], Despawn is [13, {instance}].
func frameInstance(f []any) (id int, instance string, ok bool) {
	if len(f) != 2 {
		return 0, "", false
	}
	id, ok = f[0].(int)
	if !ok {
		return 0, "", false
	}
	switch id {
	case PacketDeath:
		inst, ok := f[1].(string)
		return id, inst, ok
	case PacketDespawn:
		d, ok := f[1].(despawnData)
		if !ok {
			return 0, "", false
		}
		return id, d.Instance, true
	}
	return 0, "", false
}

// HeroDied runs the full player handleDeath path: pet despawned (disconnect
// removePet parity, same Despawn frame), Death unicast to the victim only
// (TS sends Death to self), Despawn broadcast to regions, and a synchronous
// persist flush of the victim's row (disconnect savePlayerSync path, reused).
func TestHeroDiedRoutesAndPersists(t *testing.T) {
	openDeathDB(t)
	petConfigure() // production companion wiring (disconnect parity)

	user, victimInst := "death-hero", "death-victim-inst"
	st := playerStateFor(user)
	pstateMu.Lock()
	st.Skills[SkillDefense] = &skillDef{Level: 5, XP: 1234}
	pstateMu.Unlock()
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
	})

	victim, _ := deathConn(t, victimInst, user)
	observer, _ := deathConn(t, "death-observer-inst", "death-observer-user")

	rec := petGrant(victim, "rat", "")
	if rec == nil {
		t.Fatal("pet grant failed")
	}
	petInst := rec.Instance
	abApplyPoison(victimInst) // live DoT the death path must clear
	drainOutbox(victim)
	drainOutbox(observer)

	gameWorld.HeroDied(victimInst, user, "m9test")

	if petHasOwner(victimInst) {
		t.Fatal("owner pet survived death")
	}

	victimFrames := drainOutbox(victim)
	observerFrames := drainOutbox(observer)

	count := func(frames [][]any, id int, instance string) int {
		n := 0
		for _, f := range frames {
			if fid, inst, ok := frameInstance(f); ok && fid == id && inst == instance {
				n++
			}
		}
		return n
	}

	if got := count(victimFrames, PacketDeath, victimInst); got != 1 {
		t.Fatalf("victim Death frames = %d, want exactly 1", got)
	}
	if got := count(observerFrames, PacketDeath, victimInst); got != 0 {
		t.Fatalf("observer saw %d Death frames, want 0 (Death is victim-unicast)", got)
	}
	if got := count(victimFrames, PacketDespawn, victimInst); got < 1 {
		t.Fatal("victim missing Despawn broadcast for its own instance")
	}
	if got := count(observerFrames, PacketDespawn, victimInst); got < 1 {
		t.Fatal("observer missing Despawn broadcast for the victim")
	}
	if got := count(victimFrames, PacketDespawn, petInst); got < 1 {
		t.Fatal("missing pet Despawn frame on death")
	}
	if got := count(observerFrames, PacketDespawn, petInst); got < 1 {
		t.Fatal("observer missing pet Despawn frame on death")
	}

	// Synchronous flush: the pre-death XP row is readable immediately,
	// without waiting for the 10s dirty flush.
	ps, ok := persistStore.LoadPlayer(user)
	if !ok {
		t.Fatal("victim row missing after death save")
	}
	sk, ok := ps.Skills[SkillDefense]
	if !ok || sk.XP != 1234 || sk.Level != 5 {
		t.Fatalf("persisted defense skill = %+v, want level 5 / 1234 XP", sk)
	}
}

// HeroDied is exactly-once per life (TS character.hit dead-guard parity):
// a second funnel call for the same corpse is silent (no Points-0
// rebroadcasts, no duplicate Death/Despawn/save). Other mobs release the
// corpse on their next tick via the existing gone-target path (corpses
// leave the Players scan set), so no synchronous mob surgery happens here.
func TestHeroDiedIdempotent(t *testing.T) {
	openDeathDB(t)
	victim, _ := deathConn(t, "idempotent-victim-inst", "idempotent-victim-user")
	t.Cleanup(func() { deathFired.Delete(victim.Conn.Instance) })
	drainOutbox(victim)

	gameWorld.HeroDied(victim.Conn.Instance, victim.Username, "m9test")
	gameWorld.HeroDied(victim.Conn.Instance, victim.Username, "m9test")

	frames := drainOutbox(victim)
	deaths, despawns := 0, 0
	for _, f := range frames {
		if fid, inst, ok := frameInstance(f); ok && inst == victim.Conn.Instance {
			switch fid {
			case PacketDeath:
				deaths++
			case PacketDespawn:
				despawns++
			}
		}
	}
	if deaths != 1 || despawns != 1 {
		t.Fatalf("duplicate funnel: deaths=%d despawns=%d, want 1/1", deaths, despawns)
	}

	// Disconnect clears the flag: the next life dies loudly again.
	mobPlayerLeave(victim)
	t.Cleanup(func() { deathFired.Delete(victim.Conn.Instance) })
	gameWorld.HeroDied(victim.Conn.Instance, victim.Username, "m9test")
	frames = drainOutbox(victim)
	deaths = 0
	for _, f := range frames {
		if fid, inst, ok := frameInstance(f); ok && fid == PacketDeath && inst == victim.Conn.Instance {
			deaths++
		}
	}
	if deaths != 1 {
		t.Fatalf("post-leave funnel deaths=%d, want 1", deaths)
	}
}

// Corpses are excluded from the aggro-scan player set (TS: dead players
// drop out of combat), so the next tick cannot re-acquire and re-strike
// them (no Points-0 rebroadcasts, no duplicate Death).
func TestPlayersExcludesCorpses(t *testing.T) {
	victim, _ := deathConn(t, "corpse-inst", "corpse-user")
	t.Cleanup(func() {
		playerHPs.Delete(victim.Conn.Instance)
		pstateMu.Lock()
		delete(pstates, victim.Username)
		pstateMu.Unlock()
	})

	playerHPs.Store(victim.Conn.Instance, heroHPEntry{hp: 0, maxHP: 69})
	for _, v := range gameWorld.Players() {
		if v.Instance == victim.Conn.Instance {
			t.Fatal("corpse (HP 0) present in aggro-scan player set")
		}
	}
	playerHPs.Store(victim.Conn.Instance, heroHPEntry{hp: 50, maxHP: 69})
	found := false
	for _, v := range gameWorld.Players() {
		if v.Instance == victim.Conn.Instance {
			found = true
		}
	}
	if !found {
		t.Fatal("live hero missing from aggro-scan player set")
	}
}

// HeroDied never blocks under the engine tick's locks: the strike path
// calls it holding mobMu (mobTick) and the killer's m.mu, so taking either
// self-deadlocks the tick and freezes the whole mob engine.
func TestHeroDiedTickLockContext(t *testing.T) {
	openDeathDB(t)
	victim, _ := deathConn(t, "tickctx-victim-inst", "tickctx-victim-user")
	t.Cleanup(func() { deathFired.Delete(victim.Conn.Instance) })
	m := &mob{instance: "tickctx-mob"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		mobMu.Lock()
		defer mobMu.Unlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		gameWorld.HeroDied(victim.Conn.Instance, victim.Username, m.instance)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HeroDied blocked holding mobMu + killer lock (tick self-deadlock)")
	}
}

// HeroDied with no live conn and no pet is a safe no-op for transport
// (status clear + Despawn broadcast + save attempt run; nothing is sent).
func TestHeroDiedNoConnNoPet(t *testing.T) {
	openDeathDB(t)
	gameWorld.HeroDied("ghost-inst", "ghost-user", "m9test")
	if _, ok := persistStore.LoadPlayer("ghost-user"); ok {
		t.Fatal("save wrote a row for unknown state")
	}
}
