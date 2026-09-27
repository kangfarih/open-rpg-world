package server

import (
	"testing"

	"rpg-world-server/internal/entity"
)

// Every server-side position set must flow through trackPos (the same
// helper walked movement uses), or saves persist the stale pre-teleport
// tile: death-save, disconnect-save and the 10s dirty flush all read
// pstates, never the session.

// Warp/door/mod/admin teleports funnel through teleport.
func TestM7TeleportTracksPosition(t *testing.T) {
	const user, inst = "tp-m7-user", "tp-m7-inst"
	c, _ := deathConn(t, inst, user)
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
	})
	c.Sess.PlayerX, c.Sess.PlayerY = 100, 96
	trackPos(c) // baseline, as a walked step would

	teleport(c, 150, 150)

	if c.Sess.PlayerX != 150 || c.Sess.PlayerY != 150 {
		t.Fatalf("session pos = %d,%d, want 150,150", c.Sess.PlayerX, c.Sess.PlayerY)
	}
	if st := playerSnapshot(user); st == nil || st.X != 150 || st.Y != 150 {
		t.Fatalf("tracked pos = %+v, want 150,150", st)
	}
}

// Minigame moves funnel through minigameTeleport.
func TestM8TeleportTracksPosition(t *testing.T) {
	const user, inst = "tp-m8-user", "tp-m8-inst"
	c, _ := deathConn(t, inst, user)
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
	})
	c.Sess.PlayerX, c.Sess.PlayerY = 100, 96
	trackPos(c)

	minigameTeleport(c, 151, 151)

	if st := playerSnapshot(user); st == nil || st.X != 151 || st.Y != 151 {
		t.Fatalf("tracked pos = %+v, want 151,151", st)
	}
}

// Respawn must track the spawn tile: a disconnect right after respawn
// relogins at spawn, not at the death tile.
func TestRespawnTracksSpawnTile(t *testing.T) {
	const user, inst = "tp-respawn-user", "tp-respawn-inst"
	c, _ := deathConn(t, inst, user)
	t.Cleanup(func() {
		pstateMu.Lock()
		delete(pstates, user)
		pstateMu.Unlock()
		playerHPs.Delete(inst)
		deathFired.Delete(inst)
	})
	// Die at 150,150 (tracked, as a walked arrival would).
	c.Sess.PlayerX, c.Sess.PlayerY = 150, 150
	trackPos(c)
	playerHPs.Store(inst, heroHPEntry{hp: 0, maxHP: 69})

	handleMobRespawn(c)

	if c.Sess.PlayerX != entity.HeroSpawnX || c.Sess.PlayerY != entity.HeroSpawnY {
		t.Fatalf("session pos = %d,%d, want spawn %d,%d",
			c.Sess.PlayerX, c.Sess.PlayerY, entity.HeroSpawnX, entity.HeroSpawnY)
	}
	if st := playerSnapshot(user); st == nil || st.X != entity.HeroSpawnX || st.Y != entity.HeroSpawnY {
		t.Fatalf("tracked pos = %+v, want spawn %d,%d", st, entity.HeroSpawnX, entity.HeroSpawnY)
	}
}
