// Drain driver (GO-PLAN §12 R1 / REWRITE-V2 V2-M1): per-instance
// RUNNING -> DRAINING -> SHUTDOWN for the game roles (all-in-one, shard).
//
// SIGTERM/SIGINT enters DRAINING: the accept gate closes (new WS conns get
// 503, login routing moves to the newest healthy RUNNING shard via the hub
// server-list) while the sim keeps running for existing conns. When the
// instance is empty or DRAIN_TIMEOUT elapses, stragglers get a notice +
// disconnect (forced evacuation is never a Teleport), the pre-shutdown
// flush barrier runs (persist + quests + guilds/friends rows), and the
// process exits.
//
// Started from initPlayerState (same frozen boot step; BootOrder unchanged). The old
// initPlayerState "signal -> final flush -> exit" handler is replaced by this
// driver: with zero players connected the observable behavior is the same
// (flush + exit), preceded by the DRAINING state flip.
package server

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	gnet "rpg-world-server/internal/net"
	worldcore "rpg-world-server/internal/world"

	"rpg-world-server/internal/app"
	"rpg-world-server/internal/hub"
	"rpg-world-server/internal/social"
	"rpg-world-server/internal/version"
)

// shardHubClient is the ROLE=shard hub registration (nil in all-in-one and
// router roles). DRAINING flips its heartbeat state so the router stops
// sending NEW sessions while existing ones play on.
var shardHubClient *hub.Client

// startDrainDriver owns SIGTERM/SIGINT for the game roles.
func startDrainDriver() {
	go func() {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		beginDrain()
		// Empty-or-timeout ends the drain; a second signal skips the wait
		// and flushes immediately.
		emptied := make(chan bool, 1)
		go func() {
			emptied <- app.Default.WaitEmptyOrTimeout(drainEmpty, version.DrainTimeout())
		}()
		select {
		case ok := <-emptied:
			if !ok {
				evacuateStragglers()
			}
		case <-ch:
			log.Printf("drain: second signal -> flushing immediately")
		}
		preShutdownFlush()
		app.Default.MarkShutdown()
		log.Printf("drain: SHUTDOWN (flush barrier complete)")
		os.Exit(0)
	}()
}

// beginDrain flips RUNNING -> DRAINING: stop accepting new conns, keep the
// sim for existing ones, tell the hub (shard role) to route NEW sessions
// elsewhere.
func beginDrain() {
	if !app.Default.BeginDraining() {
		return
	}
	gnet.SetAccepting(false)
	if shardHubClient != nil {
		shardHubClient.SetState(version.StateDraining)
	}
	log.Printf("drain: shutdown signal -> DRAINING (no new conns, sim continues for %d players)",
		worldcore.PlayerCount())
}

// drainEmpty reports whether the instance has no live conns left.
func drainEmpty() bool { return worldcore.PlayerCount() == 0 }

// evacuateStragglers disconnects players still online at the drain timeout:
// a notice on an existing opcode (Notification Text, no wire change —
// never a Teleport: cross-build moves are always disconnect+reconnect via
// the hub) followed by RemoveClient, which runs the per-user persist hooks
// (m5/m11/social) before the barrier sweep.
func evacuateStragglers() {
	conns := worldcore.AllWS()
	if len(conns) == 0 {
		return
	}
	log.Printf("drain: timeout with %d players online -> evacuating", len(conns))
	for _, ws := range conns {
		_ = gnet.SendDirect(ws, pktOp(PacketNotification, NotificationText, notificationPacketData{
			Message: "server shutting down: please reconnect (your progress is saved).",
		}))
		worldcore.RemoveClient(ws)
	}
}

// preShutdownFlush is the DRAINING -> SHUTDOWN barrier: per-user persist
// rows (m5 player rows sync, m11 quest rows, abilities unlocks, social
// friends rows) for any still-online players — the disconnect hooks already
// covered the gone ones — then the final dirty sweep. Guild rows write
// through on every mutation, so no guild queue exists to flush.
func preShutdownFlush() {
	for _, n := range playerUsernames() {
		savePlayerSync(n)
		persistQuests(n)
		social.FlushUser(n)
	}
	abFlushShutdown()
	flushDirty()
}

// startShardClient registers ROLE=shard with the hub (register + heartbeat
// with build/gVer/state/load stamps for the router server-list). All-in-one
// (default) starts no client: the default path stays exactly as today. A
// shard without HUB_ADDR serves standalone (log + continue).
//
// R2 wiring: the client reports the explicit VERSION tag + SHARD_REGIONS
// scope, delivers hub-relayed frames to local conns (direct-chat parity for
// guild/global fanout), answers handoff requests (pre-load + ack), checks
// local presence through the game registry, and sweeps the refresh banner on
// preferred-version changes.
func startShardClient() {
	if app.ParseRole(os.Getenv, os.Args[1:]) != app.RoleShard {
		return
	}
	addr := os.Getenv(hub.EnvHubAddr)
	if addr == "" {
		log.Printf("shard: ROLE=shard without HUB_ADDR (standalone; no hub registration)")
		return
	}
	c := hub.NewClient(addr, hub.SharedToken(), hub.ShardName(), hub.NewRouter(), nil, deliverRelayToLocal)
	c.SetBuild(version.BuildID, version.GVer, app.ListenAddr(os.Getenv("PORT")))
	c.SetPlayersProvider(playerUsernames)
	c.SetRegions(hub.ShardRegions())
	c.SetLocalCheck(func(username string) bool { return playerByName(username) != nil })
	c.SetHandoffHandler(applyHandoffRequest)
	c.SetOnPreferred(func(string) { bannerOnPreferredAll() })
	shardHubClient = c
	go c.Start(context.Background())
	log.Printf("shard: hub client -> %s as %q (buildID=%s gVer=%s version=%s)",
		addr, hub.ShardName(), version.BuildID, version.GVer, ownVersion())
}
