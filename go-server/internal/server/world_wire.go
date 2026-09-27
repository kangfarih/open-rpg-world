// World wiring (warps + events + lights/signs globals) — thin root adapter
// over internal/controller (warp runner + event fan-out) and
// internal/worldmap (glow serving), reusing the pure internal/warps,
// internal/events and internal/globals packages.
//
// Canonical owners: internal/controller (worldrun.go: warp singleton +
// menu-driven DoWarp/HandleWarp, event singleton + EventTick + multiplier
// probes) and internal/worldmap (glowserve.go: shared glow singleton,
// boot, push wrappers, forget, debug accessors). This file only wires the
// package seams to the root globals (m13/m5/m11/m7 reads, playerConn
// transport, TESTMAP/clean/combat mode flags, worldPath resolution) and
// keeps the entry points main.go/m5.go call — with UNCHANGED signatures —
// delegating to the packages. Packet shapes, tick cadence, TESTMAP debug
// strings and log lines are identical (owned by the packages, except the
// cross-domain worldtest dispatcher below which fans legs out to them).
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"rpg-world-server/internal/controller"
	"rpg-world-server/internal/globals"
	gnet "rpg-world-server/internal/net"
	worldcore "rpg-world-server/internal/world"
	"rpg-world-server/internal/worldmap"
)

// worldWarpExt aliases the controller gating entry so call sites keep
// their signatures.
type worldWarpExt = controller.WarpEntry

// worldWarpNames mirrors Modules.Warps order (modules.ts:206-213): the C->S
// Warp {id} indexes this enum, controllers/warps.ts getWarp lowercases the
// name to find the world.json entry.
var worldWarpNames = controller.WarpNames

// ---------------------------------------------------------------------------
// Warps.
// ---------------------------------------------------------------------------

// worldWarpCooldownMs is the TS warpTimeout (warps.ts: 300s between warps).
// WORLD_WARP_COOLDOWN_MS overrides it (0 disables, for the e2e).
func worldWarpCooldownMs() int64 {
	return controller.CooldownMs()
}

// worldWarpConn converts a root conn to the package view (admin resolved
// here: worldWarpStore IsAdmin parity via chatStateFor rank).
func worldWarpConn(c *playerConn) controller.WarpConn {
	if c == nil {
		return controller.WarpConn{}
	}
	return controller.WarpConn{
		Instance: c.Instance,
		Username: c.Username,
		Admin:    chatStateFor(c).rank >= RankAdmin,
	}
}

// worldConfigureWarps wires the warp-runner seams (called once from initPlayerState,
// before warps load or Warp frames route).
func worldConfigureWarps() {
	controller.ConfigureWarps(controller.WarpDeps{
		IsJailed:         isJailed,
		TutorialFinished: func(username string) bool { return loiterTutorialFinished(username) },
		InCombat: func(instance string) bool {
			// character.ts inCombat parity (warps.ts warp() combat gate) via
			// the live-target indicators the stub already owns: the hero's
			// live ability target (abTarget liveness through
			// abilities.LiveTarget) or any live engine mob targeting the
			// hero (mobTargeting). econVitals.InCombat owns exactly this
			// composition (item-use combat gate precedent); reusing it keeps
			// the warp gate and the item gate in agreement on "in combat".
			// Combat-timestamp recency is deliberately NOT consulted: the
			// stub tracks no per-hero combat clock, and the two live-target
			// signals already cover both directions (hero attacking, hero
			// attacked).
			return econVitals{}.InCombat(instance)
		},
		PlayerLevel:   func(username string) int { return playerStateFor(username).Level },
		QuestFinished: func(username, quest string) bool { return questStateFor(username).isFinished(quest) },
		AchievementDone: func(username, ach string) bool {
			def := achDefs[ach]
			st := questStateFor(username)
			return def != nil && st.Achs[ach] >= def.StageCount
		},
		FormatName: formatName,
		Notify: func(instance, message string) {
			c, _ := worldcore.Find[*playerConn](instance)
			if c != nil {
				notifyPlayer(c, message)
			}
		},
		ApplyTeleport: worldApplyTeleport,
	})
}

// worldApplyTeleport applies the warp landing: authoritative pos + region +
// Teleport delivery + area/pos/lamp side effects (worldDoWarp parity).
func worldApplyTeleport(instance string, lx, ly int) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	withTeleportBypass(c, func() {
		c.Sess.PlayerX, c.Sess.PlayerY = lx, ly
		c.regionsLoaded = nil // region streaming: force re-stream on landing
		worldcore.SetEntityPos(c.Instance, lx, ly)
		worldcore.UpdateRegion(c, lx, ly)
		worldcore.Broadcast(pkt(PacketTeleport, teleportData{Instance: c.Instance, X: lx, Y: ly}))
		c.markTeleported()
		areaPositionUpdate(c)
		trackPos(c)
		worldPushLights(c)
	})
}

// worldBootWarps loads the warp registry + the menu-gating table from the
// same world.json path the map loader uses.
func worldBootWarps() {
	controller.BootWarps(worldPath())
}

// worldFindWarp resolves a warp by Modules.Warps enum id (getWarp parity).
func worldFindWarp(id int) *worldWarpExt {
	return controller.FindWarp(id)
}

// worldFindWarpByName resolves a warp by (case-insensitive) name.
func worldFindWarpByName(name string) *worldWarpExt {
	return controller.FindWarpByName(name)
}

// worldHandleWarp routes C->S Warp frames [39,{id}] (incoming.ts handleWarp).
func worldHandleWarp(c *playerConn, data []byte) {
	controller.HandleWarp(worldWarpConn(c), data)
}

// worldDoWarp ports controllers/warps.ts warp() via the package runner.
func worldDoWarp(c *playerConn, w *worldWarpExt) bool {
	return controller.DoWarp(worldWarpConn(c), w)
}

// ---------------------------------------------------------------------------
// Events.
// ---------------------------------------------------------------------------

// worldConfigureEvents wires the event fan-out seam (called once from
// initPlayerState, before the scheduler starts).
func worldConfigureEvents() {
	controller.ConfigureEvents(controller.EventDeps{
		Announce: func(eventName string) {
			socRouteGlobal(pkt(PacketChat, chatPacketData{
				Source:  "[Global] WORLD",
				Message: "The " + eventName + " event has started.",
			}))
		},
	})
}

// worldBootEvents starts the rotation scheduler. WORLD_EVENT_MS overrides the
// per-event cadence (test hook for fast event-notice legs); default keeps the
// TS hourly cadence from DefaultEvents.
func worldBootEvents() {
	controller.BootEvents()
}

// worldEventTick fans due rotation events out as global notices. Called from
// the central 20Hz tick loop (additive: no events due most ticks).
func worldEventTick() {
	controller.EventTick()
}

// worldDoubleDrops duplicates a mob roll while the double-drops event is
// active (events.ts doubleDropProbability parity, expressed as a repeated
// roll). Dormant otherwise — callers pass the roll through unchanged.
func worldDoubleDrops(drops []dropDef) []dropDef {
	return controller.DoubleDrops(drops)
}

// worldXPBoost reports the 1.5x experience event (experiencePerHit parity).
func worldXPBoost() bool {
	return controller.XPBoost()
}

// worldHarvestDouble reports the lumberjacking/mining double-yield events
// (Utils.doubleLumberjacking/doubleMining parity) for a gathering skill.
func worldHarvestDouble(skill string) bool {
	return controller.HarvestDouble(skill)
}

// ---------------------------------------------------------------------------
// Lights + signs globals.
// ---------------------------------------------------------------------------

// Overlay Lamp opcodes (Opcodes.Overlay: Set0 Remove1 Lamp2 RemoveLamps3;
// m10.go only names Set/Remove, so Lamp lives here).
const OverlayLamp = worldmap.OverlayLamp

// Bubble opcodes (Opcodes.Bubble: Entity0 Position1).
const BubblePosition = worldmap.BubblePosition

// worldLightData mirrors SerializedLight (overlay.ts) for Lamp frames.
type worldLightData struct {
	Instance         string  `json:"instance"`
	X                int     `json:"x"`
	Y                int     `json:"y"`
	Colour           string  `json:"colour"`
	Diffuse          float64 `json:"diffuse"`
	Distance         int     `json:"distance"`
	FlickerSpeed     int     `json:"flickerSpeed"`
	FlickerIntensity int     `json:"flickerIntensity"`
}

// worldBubbleData mirrors BubblePacketData for Position bubbles (bubble.ts).
type worldBubbleData struct {
	Instance string `json:"instance"`
	Text     string `json:"text"`
	X        *int   `json:"x,omitempty"`
	Y        *int   `json:"y,omitempty"`
}

// worldGlowWorld implements worldmap.GlowWorld over the root transport:
// region math via regionOf/surroundingRegions, sends via send() unicast to
// the conn holding the instance. Packet shapes are unchanged.
type worldGlowWorld struct{}

func (worldGlowWorld) RegionOf(x, y int) int { return worldcore.TileRegion(x, y) }

func (worldGlowWorld) SurroundingRegions(rid int) []int { return surroundingRegions(rid) }

func (worldGlowWorld) SendLamp(instance string, l globals.Light) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	inst := fmt.Sprintf("light-%d", l.ID)
	if l.ID == 0 {
		inst = fmt.Sprintf("light-%d-%d", l.X, l.Y)
	}
	_ = gnet.Send(c.Conn, pktOp(PacketOverlay, OverlayLamp, map[string]any{
		"light": worldLightData{
			Instance:         inst,
			X:                l.X, Y: l.Y, Colour: l.Colour,
			Diffuse:          l.Diffuse,
			Distance:         l.Radius,
			FlickerSpeed:     l.FlickerSpeed,
			FlickerIntensity: l.FlickerIntensity,
		},
	}))
}

func (worldGlowWorld) SendBubble(instance, bubbleInstance, text string, x, y int) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	_ = gnet.Send(c.Conn, pktOp(PacketBubble, BubblePosition, worldBubbleData{
		Instance: bubbleInstance, Text: text, X: intp(x), Y: intp(y),
	}))
}

// worldBootGlobals loads lights/signs from world.json (TESTMAP synthetic
// lamp included; see worldmap.BootGlobals).
func worldBootGlobals() {
	worldmap.BootGlobals(worldPath(), testMode, cleanMode, combatMode, worldcore.TileRegion)
}

// worldPushLights unicasts Overlay Lamp frames for every not-yet-loaded light
// in the surrounding regions (handler.ts handleLights parity). Silent when
// nothing is new.
func worldPushLights(c *playerConn) {
	if c == nil {
		return
	}
	worldmap.PushLights(worldGlowWorld{}, c.Instance, c.Username, c.Sess.PlayerX, c.Sess.PlayerY)
}

// worldPushLightsForce clears the per-conn loaded set then pushes (debug
// re-send for the worldtest lights leg).
func worldPushLightsForce(c *playerConn) int {
	return worldmap.PushLightsForce(worldGlowWorld{}, c.Instance, c.Sess.PlayerX, c.Sess.PlayerY)
}

// worldForgetPlayer drops per-conn lamp state on disconnect.
func worldForgetPlayer(c *playerConn) {
	if c == nil {
		return
	}
	worldmap.ForgetGlow(c.Instance)
}

// worldSignTalk handles Target Talk on a sign position ("x-y" instance,
// player.ts handleObjectInteraction parity): Bubble Position with talkIndex
// paging over the comma-split text. Reports whether a sign matched.
func worldSignTalk(c *playerConn, instance string) bool {
	if c == nil || c.Conn == nil || instance == "" {
		return false
	}
	var msg string
	var ok bool
	// The talk cursor is session-locked (race fix): TalkWith
	// read-modify-writes it synchronously, so hold the lock across the call.
	c.withTalk(func(npc *string, idx *int) {
		msg, ok = worldmap.Shared().TalkWith(worldGlowWorld{}, c.Instance, instance, npc, idx)
	})
	if !ok {
		return false
	}
	log.Printf("world: %s read sign %s (%q)", c.Username, instance, msg)
	return true
}

// worldBoot loads all three subsystems (called once from main).
func worldBoot() {
	worldBootWarps()
	worldBootEvents()
	worldBootGlobals()
}

// ---------------------------------------------------------------------------
// TESTMAP debug dispatcher ([46 {worldtest:...}], socialtest/pettest
// precedent). All legs echo through notifyPlayer so the e2e can grep them.
// The warp/event legs read the controller singletons; the lights/signs
// legs read the worldmap shared glow.
// ---------------------------------------------------------------------------

func worldTestHandler(c *playerConn, data []byte) {
	if !testMode || c == nil {
		return
	}
	// Admin-rank gate (see handleMinigameTest: TESTMAP default stays ON, the gate
	// closes the any-client warp hole).
	if !isAdmin(c) {
		return
	}
	var d struct {
		WorldTest string `json:"worldtest"`
		ID        *int   `json:"id"`
		Name      string `json:"name"`
		X         *int   `json:"x"`
		Y         *int   `json:"y"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.WorldTest == "" {
		return
	}
	switch d.WorldTest {
	case "echo":
		nw := controller.Warps.Count()
		nl, ns := 0, 0
		if nl2, ns2, ok := worldmap.GlowStats(); ok {
			nl, ns = nl2, ns2
		}
		notifyPlayer(c, fmt.Sprintf("world:ok warps=%d events=%d lights=%d signs=%d", nw, controller.DefaultEventCount(), nl, ns))
	case "warps":
		names := controller.Warps.Names()
		notifyPlayer(c, "world:warps ["+strings.Join(names, ",")+"]")
	case "warp":
		var w *worldWarpExt
		if d.Name != "" {
			w = worldFindWarpByName(d.Name)
		} else if d.ID != nil {
			w = worldFindWarp(*d.ID)
		}
		if w == nil {
			notifyPlayer(c, "world:warp unknown")
			return
		}
		if worldDoWarp(c, w) {
			notifyPlayer(c, fmt.Sprintf("world:warp %s x=%d y=%d", w.Name, c.Sess.PlayerX, c.Sess.PlayerY))
		}
	case "at":
		if d.X == nil || d.Y == nil || !controller.Warps.Loaded() {
			return
		}
		if w := controller.Warps.At(*d.X, *d.Y); w != nil {
			notifyPlayer(c, fmt.Sprintf("world:at %d,%d id=%d x=%d y=%d w=%d h=%d", *d.X, *d.Y, w.ID, w.X, w.Y, w.W, w.H))
		} else {
			notifyPlayer(c, fmt.Sprintf("world:at %d,%d none", *d.X, *d.Y))
		}
	case "events":
		active := controller.Events.ActiveKeys()
		n, every := controller.Events.Fired(), controller.Events.Interval()
		sort.Strings(active)
		notifyPlayer(c, fmt.Sprintf("world:events active=[%s] fired=%d intervalMs=%d", strings.Join(active, ","), n, every))
	case "lights":
		n := worldPushLightsForce(c)
		notifyPlayer(c, fmt.Sprintf("world:lights n=%d", n))
	case "signs":
		sx, sy, text, n, ok := worldmap.FirstSign()
		if !ok {
			notifyPlayer(c, "world:signs n=0")
			return
		}
		first := ""
		if n > 0 {
			if len(text) > 48 {
				text = text[:48] + "..."
			}
			first = fmt.Sprintf(" first=%d-%d:%s", sx, sy, text)
		}
		notifyPlayer(c, fmt.Sprintf("world:signs n=%d%s", n, first))
	case "sign":
		if _, _, _, _, ok := worldmap.FirstSign(); !ok {
			notifyPlayer(c, "world:sign none")
			return
		}
		var text string
		var ok bool
		var sx, sy int
		if d.X != nil && d.Y != nil {
			text, ok = worldmap.SignAt(*d.X, *d.Y)
			sx, sy = *d.X, *d.Y
			if !ok {
				notifyPlayer(c, fmt.Sprintf("world:sign %d-%d none", *d.X, *d.Y))
				return
			}
		} else {
			var n int
			sx, sy, text, n, ok = worldmap.FirstSign()
			if !ok || n == 0 {
				notifyPlayer(c, "world:sign none")
				return
			}
		}
		inst := strconv.Itoa(sx) + "-" + strconv.Itoa(sy)
		c.resetTalk()
		worldSignTalk(c, inst)
		notifyPlayer(c, fmt.Sprintf("world:sign %s text=%s", inst, text))
	}
}
