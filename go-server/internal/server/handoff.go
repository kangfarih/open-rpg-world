// Cross-shard handoff, refresh-banner and relay-delivery wiring (GO-PLAN
// §12 R2 / REWRITE-V2 V2-M2). All of it is hub-gated: with no HUB_ADDR the
// shard client never starts (shardHubClient stays nil) and every entry point
// below is a nil-safe no-op, so the default all-in-one path is byte-identical.
//
// Mechanism (documented choice: hub-relayed RPC, reusing the shard<->hub
// sockets; no new packet opcode, relay-envelope reuse for chat only):
//
//	send: the TESTMAP handofftest op (PacketMinigame [46,
//	  {"handofftest":{"target":...}}], existing debug-op surface) flushes the
//	  sender's persist rows (players/inventory via savePlayerSync, quests via
//	  persistQuests), packs the snapshot + quest rows + region/pos into a
//	  hub HandoffRequest, and waits for the ack. The hub version-gates first
//	  (mismatch => immediate reject ack; a cross-build move must go
//	  disconnect+reconnect via login/hub with a client reload, never a bare
//	  Teleport across builds) and otherwise forwards to the target's socket.
//	receive: applyHandoffRequest double-gates the exact build/gVer, rejects
//	  when the player is already online here (dup protection), installs the
//	  persist snapshot into pstates + the player tables, writes the quest
//	  rows, and records a pending transfer. On ack-ok the sender notifies
//	  its client (existing Notification-25: "handoff: reconnect to <addr>")
//	  and RemoveClient runs the normal disconnect path (persist hooks,
//	  Despawn broadcast, close).
//	seamless same-build move = client socket re-point (no page reload): the
//	  client closes its game WS and dials the target addr from the notify,
//	  then logs in normally — the transferred rows are already in the target
//	  DB, so Welcome/Map/Spawn restore the session on live state. No asset
//	  refetch happens (same build). The only Teleport ever emitted lives
//	  inside the target shard's own socket flow (the seeded-pos echo after
//	  login); a bare Teleport across builds is never used.
//
// Region->build lookup lives on the hub (regions reported in register/
// heartbeat via SHARD_REGIONS; LookupRegion); the test op takes an explicit
// target and falls back to the player's current region lookup when the hub
// client knows no better (it cannot query the hub table, so region-only
// requests resolve locally and are rejected when unresolvable — the live
// harness always passes an explicit target).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/persist"
	"rpg-world-server/internal/version"
	worldcore "rpg-world-server/internal/world"

	"rpg-world-server/internal/app"
	"rpg-world-server/internal/hub"
)

// handoffPendingTTL bounds how long a received transfer waits for the
// re-pointed client login (observability only — the rows are already in the
// DB, so an expiry never loses state).
const handoffPendingTTL = 60 * time.Second

// handoffQuestRow mirrors one quests-table row for the transfer payload.
type handoffQuestRow struct {
	Quest    string `json:"quest"`
	Stage    int    `json:"stage"`
	SubStage int    `json:"substage"`
}

// handoffAchRow mirrors one achievements-table row for the transfer payload.
type handoffAchRow struct {
	Ach   string `json:"ach"`
	Stage int    `json:"stage"`
}

// handoffQuests is the opaque Quests payload: raw quest/achievement rows.
type handoffQuests struct {
	Quests       []handoffQuestRow `json:"quests"`
	Achievements []handoffAchRow   `json:"achievements"`
}

var (
	handoffMu      sync.Mutex
	handoffPending = map[string]time.Time{} // username -> transfer arrival (re-point window)
)

// deliverRelayToLocal delivers one hub-relayed game frame to its local
// player (hub.Client RelayHandler parity: the inner frame passes through to
// the player conn untouched, queued on the outbox like steady-state
// traffic). Unknown targets are dropped with a log (handleRelay parity).
func deliverRelayToLocal(to string, inner json.RawMessage) {
	t := playerByName(to)
	if t == nil {
		log.Printf("hub: relay for offline %q dropped", to)
		return
	}
	var frame []any
	if err := json.Unmarshal(inner, &frame); err != nil {
		log.Printf("hub: relay for %q dropped (bad frame: %v)", to, err)
		return
	}
	_ = gnet.Send(t.Conn, frame)
}

// handoffReadQuestRows snapshots one player's quest/achievement rows from
// the local DB (caller holds no locks; dbMu guards the queries).
func handoffReadQuestRows(username string) handoffQuests {
	var out handoffQuests
	if dbConn == nil || username == "" {
		return out
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if rows, err := dbConn.Query(`SELECT quest,stage,substage FROM quests WHERE player=?`, username); err == nil {
		for rows.Next() {
			var r handoffQuestRow
			if rows.Scan(&r.Quest, &r.Stage, &r.SubStage) == nil {
				out.Quests = append(out.Quests, r)
			}
		}
		rows.Close()
	}
	if rows, err := dbConn.Query(`SELECT ach,stage FROM achievements WHERE player=?`, username); err == nil {
		for rows.Next() {
			var r handoffAchRow
			if rows.Scan(&r.Ach, &r.Stage) == nil {
				out.Achievements = append(out.Achievements, r)
			}
		}
		rows.Close()
	}
	return out
}

// handoffWriteQuestRows installs transferred quest/achievement rows (DELETE
// + INSERT, quest persist parity). Caller holds no locks.
func handoffWriteQuestRows(username string, q handoffQuests) {
	if dbConn == nil || username == "" {
		return
	}
	dbMu.Lock()
	defer dbMu.Unlock()
	if _, err := dbConn.Exec(`DELETE FROM quests WHERE player=?`, username); err != nil {
		return
	}
	for _, r := range q.Quests {
		if _, err := dbConn.Exec(`INSERT INTO quests(player,quest,stage,substage) VALUES(?,?,?,?)`,
			username, r.Quest, r.Stage, r.SubStage); err != nil {
			log.Printf("handoff: save quest %s/%s: %v", username, r.Quest, err)
		}
	}
	if _, err := dbConn.Exec(`DELETE FROM achievements WHERE player=?`, username); err != nil {
		return
	}
	for _, r := range q.Achievements {
		if r.Stage == 0 {
			continue
		}
		if _, err := dbConn.Exec(`INSERT INTO achievements(player,ach,stage) VALUES(?,?,?)`,
			username, r.Ach, r.Stage); err != nil {
			log.Printf("handoff: save achievement %s/%s: %v", username, r.Ach, err)
		}
	}
}

// applyHandoffRequest receives one hub-relayed transfer (hub.Client
// HandoffHandler parity): exact build/gVer gate, dup protection, row
// install, pending record, ack. Transport-owning side effects (game socket
// writes) never happen here — the re-pointed client logs in normally.
func applyHandoffRequest(req hub.HandoffRequest) hub.HandoffAck {
	reject := func(reason string) hub.HandoffAck {
		log.Printf("handoff: reject %s -> %s: %s", req.From, req.Player, reason)
		return hub.HandoffAck{Ok: false, Reason: reason}
	}
	if req.Player == "" {
		return reject("unknown player")
	}
	// Exact build gate (the hub already version-gated on the display tag;
	// the receiver enforces the wire contract — cross-build transfers must
	// go disconnect+reconnect, never a bare Teleport across builds).
	if req.BuildID != version.BuildID || req.GVer != version.GVer {
		return reject(fmt.Sprintf("build mismatch (need %s/%s)", version.BuildID, version.GVer))
	}
	// World-version gate (R2 display tag: explicit VERSION or buildID+gVer
	// pair — a cross-version move must go disconnect+reconnect with a client
	// reload, never a bare transfer across versions).
	if req.Version != "" && req.Version != ownVersion() {
		return reject(fmt.Sprintf("version mismatch (need %s)", ownVersion()))
	}
	if playerByName(req.Player) != nil {
		return reject("player already online here")
	}
	var st persist.State
	if len(req.State) > 0 {
		if err := json.Unmarshal(req.State, &st); err != nil {
			return reject("bad player snapshot")
		}
	} else if dbConn != nil && persistStore != nil {
		// Empty snapshot with a local row: fall back to the local row so a
		// same-shard-file move still lands on state (defensive only).
		dbMu.Lock()
		loaded, ok := persistStore.LoadPlayer(req.Player)
		dbMu.Unlock()
		if !ok {
			return reject("no player snapshot")
		}
		st = loaded
	} else {
		return reject("no player snapshot")
	}
	var quests handoffQuests
	if len(req.Quests) > 0 {
		if err := json.Unmarshal(req.Quests, &quests); err != nil {
			return reject("bad quest snapshot")
		}
	}
	// Install: pstates (pstateMu) + player tables (dbMu, writePlayer
	// parity) + quest rows, matching the loadPlayerState + loadQuests outcome so
	// the re-pointed login restores the transferred session.
	pstateMu.Lock()
	pstates[req.Player] = persistToM5(st)
	pstateMu.Unlock()
	// Statistics counters install from the transferred snapshot (the
	// snapshot is authoritative — writePlayer below persists it).
	statsInstall(req.Player, st.Stats)
	dbMu.Lock()
	if inst := persistToM5(st); inst != nil {
		writePlayer(req.Player, inst)
	}
	persistStore.MarkClean(req.Player)
	dbMu.Unlock()
	handoffWriteQuestRows(req.Player, quests)

	handoffMu.Lock()
	handoffPending[req.Player] = time.Now().Add(handoffPendingTTL)
	handoffMu.Unlock()
	log.Printf("handoff: accepted %s from %s (pos %d,%d)", req.Player, req.From, st.X, st.Y)
	return hub.HandoffAck{Ok: true, Addr: app.ListenAddr(os.Getenv("PORT"))}
}

// handoffTestHandler triggers an outbound transfer (TESTMAP debug op
// [46, {"handofftest":{"target":...,"region":...}}], abtest/pettest
// precedent): flush, pack, request, and on ack-ok notify + disconnect so the
// client re-points. Hub-gated and testMode-gated (nil-safe no-op otherwise).
func handoffTestHandler(c *playerConn, data []byte) {
	if !testMode || c == nil || shardHubClient == nil || !shardHubClient.Connected() {
		return
	}
	// Admin-rank gate (see handleMinigameTest: TESTMAP default stays ON, the gate
	// closes the any-client handoff hole).
	if !isAdmin(c) {
		return
	}
	var d struct {
		HandoffTest *struct {
			Target string `json:"target"`
			Region *int   `json:"region"`
		} `json:"handofftest"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.HandoffTest == nil {
		return
	}
	target := d.HandoffTest.Target
	region := worldcore.TileRegion(c.Sess.PlayerX, c.Sess.PlayerY)
	if d.HandoffTest.Region != nil {
		region = *d.HandoffTest.Region
	}
	if target == "" {
		notifyPlayer(c, "handoff: no target (pass {\"target\":...})")
		return
	}
	go handoffSend(c, target, region)
}

// handoffSend flushes one player's rows, requests the transfer, and on
// ack-ok tells the client to re-point (existing Notification-25) before the
// normal disconnect path (persist hooks + Despawn broadcast + close). On
// reject the session stays put with a notify (cross-build: the client must
// disconnect+reconnect via login/hub with a reload).
func handoffSend(c *playerConn, target string, region int) {
	username := c.Username
	// Sync the live position into the persist state first (teleports and
	// mid-stride moves update the session/registry immediately and the
	// persist row only on track/flush — the transfer must carry the live
	// tile). Then flush sender rows (owner-to-owner RPC rule: the transfer
	// carries the post-flush snapshot; the two shards never dual-write one
	// file).
	trackPos(c)
	savePlayerSync(username)
	persistQuests(username)
	st := playerSnapshot(username)
	if st == nil {
		notifyPlayer(c, "handoff: no player state")
		return
	}
	// Statistics counters ride the transfer snapshot (writePlayer parity).
	ps := toPersist(st)
	snap := statsCopyOf(username)
	ps.Stats = persist.StatsBlob{
		MobKills: snap.MobKills, MobExamines: snap.MobExamines,
		Resources: snap.Resources, Drops: snap.Drops,
		PvPKills: snap.PvPKills, PvPDeaths: snap.PvPDeaths,
		CreationTime: snap.CreationTime, TotalTimePlayed: snap.TotalTimePlayed,
		LastLogin: snap.LastLogin, LoginCount: snap.LoginCount,
	}
	stateRaw, err := json.Marshal(ps)
	if err != nil {
		notifyPlayer(c, "handoff: snapshot failed")
		return
	}
	questsRaw, err := json.Marshal(handoffReadQuestRows(username))
	if err != nil {
		notifyPlayer(c, "handoff: quest snapshot failed")
		return
	}
	req := hub.HandoffRequest{
		Player: username, BuildID: version.BuildID, GVer: version.GVer,
		Version: ownVersion(), State: stateRaw, Quests: questsRaw,
		Region: region, X: st.X, Y: st.Y,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ack, err := shardHubClient.RequestHandoff(ctx, target, req)
	if err != nil {
		notifyPlayer(c, "handoff: "+err.Error())
		return
	}
	if !ack.Ok {
		notifyPlayer(c, "handoff rejected: "+ack.Reason)
		log.Printf("handoff: %s -> %s rejected: %s", username, target, ack.Reason)
		return
	}
	addr := ack.Addr
	if addr == "" {
		addr = target
	}
	log.Printf("handoff: %s -> %s (%s), re-pointing client", username, target, addr)
	// Immediate write (evacuation parity): the outbox tick may not flush
	// before RemoveClient closes the socket below.
	_ = gnet.SendDirect(c.WS, pktOp(PacketNotification, NotificationText, notificationPacketData{
		Message: "handoff: reconnect to " + addr,
	}))
	worldcore.RemoveClient(c.WS)
}
