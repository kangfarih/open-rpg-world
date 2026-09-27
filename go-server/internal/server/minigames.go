package server

// Minigames — coursing + teamwar (the minigames slice of the clone).
//
// Ports game/minigames/minigame.ts (base: lobby areas, add/remove, stop,
// disconnect, tick), impl/coursing.ts (hunter/prey split + distance-from-
// centre scoring) and impl/teamwar.ts (red/blue kills). Areas come from
// world.json areas.minigame (mObjectType lobby/hunterspawn/preyspawn/centre/
// redteamspawn/blueteamspawn), linked by the `minigame` key exactly like
// minigames.ts linkAreas. S→C frames are Minigame(46,[opcode,{action,...}])
// (common/network/impl/minigame.ts) and Pointer(36,[opcode,{instance}]).
//
// Documented divergences (stub world): players never die in the stub
// (no PVP deaths), so teamwar kill points are driven through the M8TEST
// debug frame; the lobby countdown packets arrive on their real 1s cadence
// but the harness only samples them.
//
// State machine lives in internal/minigame (Manager + Game/Member); this
// file is a thin transport adapter (Effects impl + session-mirror sync +
// tick goroutines + M8TEST dispatcher). Packet shapes and cadences frozen.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"rpg-world-server/internal/minigame"
	gnet "rpg-world-server/internal/net"
	worldcore "rpg-world-server/internal/world"
)

// ---------------------------------------------------------------------------
// Constants (Opcodes.Minigame, MinigameState/Actions, Modules.MinigameConstants).
// Re-exported from internal/minigame (pure domain logic); values frozen.
// ---------------------------------------------------------------------------

const (
	MinigameTeamWar  = minigame.MinigameTeamWar  // Opcodes.Minigame.TeamWar
	MinigameCoursing = minigame.MinigameCoursing // Opcodes.Minigame.Coursing
)

const (
	MinigameActionScore = minigame.MinigameActionScore // MinigameActions.Score
	MinigameActionEnd   = minigame.MinigameActionEnd   // MinigameActions.End
	MinigameActionLobby = minigame.MinigameActionLobby // MinigameActions.Lobby
	MinigameActionExit  = minigame.MinigameActionExit  // MinigameActions.Exit
)

// MinigameState (opcodes.ts:184) — Lobby0 End1 Exit2. The base minigame.ts
// uses THESE for addPlayer/removePlayer (a Node inconsistency with the
// MinigameActions values used by the ticks; cloned verbatim).
const (
	minigameStateLobby = minigame.MinigameStateLobby
	minigameStateEnd   = minigame.MinigameStateEnd
	minigameStateExit  = minigame.MinigameStateExit
)

const (
	PointerLocation = minigame.PointerLocation // Opcodes.Pointer.Location
	PointerEntity   = minigame.PointerEntity   // Opcodes.Pointer.Entity
	PointerRemove   = minigame.PointerRemove   // Opcodes.Pointer.Remove
)

const (
	teamWarCountdown     = minigame.TeamWarCountdown     // TEAM_WAR_COUNTDOWN (lobby + in-game seconds)
	teamWarMinPlayers    = minigame.TeamWarMinPlayers    // TEAM_WAR_MIN_PLAYERS
	coursingCountdown    = minigame.CoursingCountdown    // COURSING_COUNTDOWN
	coursingMinPlayers   = minigame.CoursingMinPlayers   // COURSING_MIN_PLAYERS
	coursingScoreDivisor = minigame.CoursingScoreDivisor // COURSING_SCORE_DIVISOR
	coursingTeamPrey     = minigame.CoursingTeamPrey     // Team.Prey (api/minigame.ts)
	coursingTeamHunter   = minigame.CoursingTeamHunter   // Team.Hunter
	teamWarTeamRed       = minigame.TeamWarTeamRed       // Team.Red
	teamWarTeamBlue      = minigame.TeamWarTeamBlue      // Team.Blue
	coursingDeadPenalty  = minigame.CoursingDeadPenalty  // coursing.ts dead-player per-tick score
	coursingScoreTick    = minigame.CoursingScoreTick    // score update every 4 ticks
	coursingPointerDelay = minigame.CoursingPointerDelay // ms before sendPointers (coursing.ts)
	minigameTickInterval = minigame.MinigameTickInterval // minigame.ts tickInterval (1s)
	killPointsPerKill    = minigame.KillPointsPerKill    // teamwar.kill increments by one per kill
)

// ---------------------------------------------------------------------------
// Area model (areas/area.ts + areas/impl/minigame.ts).
// Pure logic lives in internal/minigame; mgArea is a direct alias so the
// world.json shape and all call sites below are unchanged.
// ---------------------------------------------------------------------------

// mgArea is one world.json areas.minigame entry (internal/minigame.Area).
type mgArea = minigame.Area

// ---------------------------------------------------------------------------
// Thin adapter: Manager owns the state machine; this file owns transport +
// session mirrors. m8Mu/mgGames moved into minigame.Manager (mutex + games
// map, instance-ID-keyed members, no playerConn pointers).
// ---------------------------------------------------------------------------

var (
	mgMgr                  = minigame.NewManager()
	mgFx  minigame.Effects = &minigameEffects{}
)

// minigameEffects implements minigame.Effects with root transport.
type minigameEffects struct{}

func minigameConnFor(instance string) *playerConn {
	c, _ := worldcore.Find[*playerConn](instance)
	return c
}

func (minigameEffects) Notify(instance, msg string) {
	if c := minigameConnFor(instance); c != nil {
		notifyPlayer(c, msg)
	}
}

func (minigameEffects) Teleport(instance string, x, y int) {
	if c := minigameConnFor(instance); c != nil {
		minigameTeleport(c, x, y)
	}
}

func (minigameEffects) Broadcast(frames ...[]any) {
	worldcore.Broadcast(frames...)
}

func (minigameEffects) SendMinigame(instance string, opcode int, data map[string]any) {
	if c := minigameConnFor(instance); c != nil {
		_ = gnet.Send(c.Conn, pktOp(PacketMinigame, opcode, data))
	}
}

func (minigameEffects) SendPointer(instance string, target string) {
	if c := minigameConnFor(instance); c != nil {
		// player.pointer: Remove-then-Entity (pointer.ts default remove).
		// Remove carries no data → 2-element frame like Node's serialize.
		_ = gnet.Send(c.Conn, []any{PacketPointer, PointerRemove})
		_ = gnet.Send(c.Conn, pktOp(PacketPointer, PointerEntity, map[string]any{
			"instance": target, "type": PointerEntity,
		}))
	}
}

func (minigameEffects) EntityPos(instance string) (int, int, bool) {
	return worldcore.EntityPos(instance)
}

// syncMinigameStarted mirrors started assignments onto playerConn fields.
func syncMinigameStarted(gameKey string, assignments map[string]minigame.Member) {
	if len(assignments) == 0 {
		return
	}
	keys := make([]string, 0, len(assignments))
	for k := range assignments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		mem := assignments[k]
		if c := minigameConnFor(k); c != nil {
			c.mgGame = gameKey
			c.mgTeam = mem.Team
			c.mgScore = mem.Score
			c.mgTarget = mem.Target
		}
	}
}

// syncMinigameScored mirrors coursing score updates onto playerConn fields.
func syncMinigameScored(scored map[string]int) {
	if len(scored) == 0 {
		return
	}
	keys := make([]string, 0, len(scored))
	for k := range scored {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if c := minigameConnFor(k); c != nil {
			c.mgScore = scored[k]
		}
	}
}

// clearMinigameInstances resets session mirrors for stopped/disconnected players.
func clearMinigameInstances(instances []string) {
	for _, k := range instances {
		if c := minigameConnFor(k); c != nil {
			c.mgGame = ""
			c.mgTeam = 0
			c.mgScore = 0
			c.mgTarget = ""
		}
	}
}

// syncMinigameTick applies a TickResult to session mirrors.
func syncMinigameTick(gameKey string, res minigame.TickResult) {
	clearMinigameInstances(res.Stopped)
	syncMinigameStarted(gameKey, res.Started)
	syncMinigameScored(res.Scored)
}

// minigameTeleport ports character.teleport for minigame moves (position update +
// Teleport broadcast; the region recompute happens inside the broadcast's
// interest resolution and updateClientRegion here). The landing tile is
// tracked via trackPos (persist parity with walked movement).
func minigameTeleport(c *playerConn, x, y int) {
	if c == nil || c.Conn == nil {
		return
	}
	withTeleportBypass(c, func() {
		c.Sess.PlayerX = x
		c.Sess.PlayerY = y
		c.regionsLoaded = nil // region streaming: force re-stream on landing
		worldcore.SetEntityPos(c.Instance, x, y)
		worldcore.UpdateRegion(c, x, y)
		worldcore.Broadcast(pkt(PacketTeleport, teleportData{Instance: c.Instance, X: x, Y: y}))
		c.markTeleported()
		trackPos(c)
	})
}

// ---------------------------------------------------------------------------
// Area linking + lifecycle (minigames.ts constructor + linkAreas).
// ---------------------------------------------------------------------------

// loadMinigames parses world.json areas.minigame, links each area to its
// minigame (linkAreas), and starts the 1s tick goroutine per game.
func loadMinigames() {
	if mgMgr.HasGames() {
		return
	}
	loadWorld()
	if world == nil {
		return
	}
	raw, err := os.ReadFile(worldPath())
	if err != nil {
		return
	}
	areas, err := minigame.ParseAreasDoc(raw)
	if err != nil {
		log.Printf("m8: parse areas: %v", err)
		return
	}

	mgMgr.EnsureGame("coursing", MinigameCoursing, "Coursing",
		minigame.EnvCountdown("M8_COURSING_COUNTDOWN", coursingCountdown))
	mgMgr.EnsureGame("teamwar", MinigameTeamWar, "TeamWar",
		minigame.EnvCountdown("M8_TEAMWAR_COUNTDOWN", teamWarCountdown))

	for _, a := range areas {
		mgMgr.AddArea(a)
	}

	for _, key := range mgMgr.GameKeys() {
		k := key
		go func() {
			t := time.NewTicker(minigameTickInterval * time.Millisecond)
			defer t.Stop()
			for range t.C {
				res := mgMgr.Tick(k, mgFx)
				syncMinigameTick(k, res)
			}
		}()
	}
	log.Printf("m8: minigames loaded (coursing lobby=%v teamwar lobby=%v)",
		mgMgr.HasLobby("coursing"), mgMgr.HasLobby("teamwar"))
}

// minigameFor returns the named game (nil when missing).
func minigameFor(key string) *minigame.Game {
	return mgMgr.GameFor(key)
}

// ---------------------------------------------------------------------------
// Player-side hooks (called from the movement/disconnect paths).
// ---------------------------------------------------------------------------

// minigamePositionUpdate mirrors areas onEnter/onExit: on each confirmed position
// change, move the player between lobby membership and nothing (enter lobby
// -> addPlayer, leave lobby area -> removePlayer). In-game players use the
// exit hook only to detect leaving via the lobby (Node tracks exits through
// the area callbacks; the stub keeps membership until stop()).
func minigamePositionUpdate(c *playerConn) {
	mgMgr.OnPositionUpdate(c.Instance, c.Sess.PlayerX, c.Sess.PlayerY, c.mgGame, mgFx)
}

// minigameDisconnect ports Minigame.disconnect: teleport to a random lobby
// position, drop from the game, stop when fewer than 2 remain.
func minigameDisconnect(c *playerConn) {
	if c.mgGame == "" {
		return
	}
	key := c.mgGame
	x, y, remaining := mgMgr.Disconnect(key, c.Instance)

	c.mgGame = ""
	c.mgTeam = 0
	c.mgScore = 0
	c.mgTarget = ""

	// The conn is dying; update the session position so the disconnect
	// persist path saves the lobby tile and the relogin lands back in the
	// lobby area (disconnect() setPosition parity). Persist goes through
	// trackPos (never hold pstateMu outside it).
	c.Sess.PlayerX, c.Sess.PlayerY = x, y
	trackPos(c)

	// minigame.ts disconnect: stop when fewer than 2 players remain.
	if minigame.ShouldStop(remaining) {
		stopped := mgMgr.Stop(key, mgFx)
		clearMinigameInstances(stopped)
	}
}

// ---------------------------------------------------------------------------
// M8TEST debug frame (TESTMAP-only): deterministic kill/score injection so
// the e2e can exercise the kill path without real PVP deaths (the stub has
// none). Shape: C->S [46,{"m8test":"kill"|"score"}].
// ---------------------------------------------------------------------------

func handleMinigameTest(c *playerConn, frame clientFrame) {
	if !testMode || cleanMode || combatMode || len(frame) < 2 {
		return
	}
	// Admin-rank gate (debug dispatchers have no rank checks upstream — any
	// client could warp/spawn/setstage. TESTMAP=1 default stays ON for the
	// dev workflow; the gate, not a mode flip, closes the hole).
	if !isAdmin(c) {
		return
	}
	var data struct {
		M8Test string `json:"m8test"`
	}
	if err := json.Unmarshal(frame[1], &data); err != nil {
		return
	}
	switch data.M8Test {
	case "kill":
		if c.mgGame == "" {
			return
		}
		if minigameFor(c.mgGame) != nil {
			mgMgr.RecordKillTeam(c.mgGame, c.mgTeam)
		}
	case "kills":
		// Echo the teamwar kill counters (game state introspection).
		g := minigameFor("teamwar")
		if g == nil {
			return
		}
		r, b := mgMgr.KillCounts("teamwar")
		notifyPlayer(c, fmt.Sprintf("m8:kills red=%d blue=%d", r, b))
	case "pointers":
		// Re-fire coursing sendPointers (idempotent, TESTMAP-only) so the
		// harness gets a deterministic window for the Pointer frames.
		g := minigameFor("coursing")
		if g == nil {
			return
		}
		mgMgr.SendPointers("coursing", mgFx)
	case "state":
		// Full session introspection for the harness (positions, team,
		// score, target, game key).
		notifyPlayer(c, fmt.Sprintf("m8:state game=%s team=%d score=%d target=%s x=%d y=%d inst=%s",
			c.mgGame, c.mgTeam, c.mgScore, c.mgTarget, c.Sess.PlayerX, c.Sess.PlayerY, c.Instance))
	case "score":
		if c.mgGame != "coursing" {
			return
		}
		// Dead-prey per-tick penalty (coursing.ts incrementCoursingScore(-10)
		// with the clamp at 0). Echo the clamped score for the e2e.
		c.mgScore = minigame.ApplyDeadPenalty(c.mgScore)
		notifyPlayer(c, fmt.Sprintf("m8:score=%d", c.mgScore))
	}
}
