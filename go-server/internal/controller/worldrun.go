// World-run orchestration (warps + events remainder), extracted
// behavior-frozen from the root world_wire.go adapter (task D2b item 2:
// world_wire.go -> internal/controller + internal/worldmap; warp/event
// orchestration was already split in E1c — this moves the remainder:
// the controller singletons, the menu-driven warp runner with its
// Teleport side effects, and the event rotation fan-out + multiplier
// probes).
//
// TS sources (see the root adapter for the full tour):
// controllers/warps.ts warp() (gates, random landing, Teleport delivery,
// `warps:*` notifies), incoming.ts handleWarp [39,{id}], modules.ts Warps
// enum, controllers/events.ts rotation (hourly check, globalMessage
// announces, doubleDropProbability/experiencePerHit multipliers).
//
// Everything transport/world/state related stays with the root adapter and
// is reached only through WarpConn (per-warp caller) and Deps. Landing math
// and gate computation ride the existing WarpController/EventController.
// Frames are built by the caller's seams; log strings are kept verbatim.
package controller

import (
	"encoding/json"
	"log"
	"math/rand"
	"time"

	"rpg-world-server/internal/events"
)

// Warps is the menu-driven warp registry + gating table + cooldown clocks
// (world_wire.go worldWarpCtl parity, owned here after E1c).
var Warps = NewWarpController()

// Events owns the rotation scheduler plus the active set/fired counters
// (world_wire.go worldEventCtl parity, owned here after E1c).
var Events = NewEventController()

// WarpConn is the minimal per-warp caller view. Admin is resolved by the
// root adapter at call time (chatStateFor rank parity).
type WarpConn struct {
	Instance string
	Username string
	Admin    bool
}

// WarpDeps bundles the warp-runner seams (implemented by the root adapter;
// never by this package).
type WarpDeps struct {
	// Gate reads (worldWarpStore parity, username-keyed except InCombat
	// which resolves the caller's instance).
	IsJailed         func(username string) bool
	TutorialFinished func(username string) bool
	InCombat         func(instance string) bool
	PlayerLevel      func(username string) int
	QuestFinished    func(username, quest string) bool
	AchievementDone  func(username, ach string) bool
	FormatName       func(name string) string
	// Notify sends a client text notification (m6Notify parity).
	Notify func(instance, message string)
	// ApplyTeleport applies the landing: authoritative pos + region +
	// Teleport frame + m10/m5/push side effects (worldDoWarp parity).
	ApplyTeleport func(instance string, x, y int)
}

var warpDeps WarpDeps

// ConfigureWarps installs the warp-runner seams (called once from the root
// boot, before warps load or Warp frames route).
func ConfigureWarps(d WarpDeps) { warpDeps = d }

// warpStore adapts WarpDeps + the caller to the WarpStore gate interface.
type warpStore struct {
	admin    bool
	instance string
}

func (s warpStore) IsJailed(username string) bool {
	if warpDeps.IsJailed == nil {
		return false
	}
	return warpDeps.IsJailed(username)
}

func (s warpStore) IsAdmin(username string) bool { return s.admin }

// TutorialFinished mirrors quests.ts isTutorialFinished (warps.ts warp()
// tutorial gate). An unwired seam defaults to finished so the gate stays
// open, preserving the pre-gate behaviour for non-server callers.
func (s warpStore) TutorialFinished(username string) bool {
	if warpDeps.TutorialFinished == nil {
		return true
	}
	return warpDeps.TutorialFinished(username)
}

// InCombat mirrors character.ts inCombat (warps.ts warp() combat gate) via
// the caller's instance. An unwired seam defaults to peace so the gate
// stays open, preserving the pre-gate behaviour for non-server callers.
func (s warpStore) InCombat(username string) bool {
	if warpDeps.InCombat == nil {
		return false
	}
	return warpDeps.InCombat(s.instance)
}

func (s warpStore) PlayerLevel(username string) int {
	if warpDeps.PlayerLevel == nil {
		return 0
	}
	return warpDeps.PlayerLevel(username)
}

func (s warpStore) QuestFinished(username, quest string) bool {
	if warpDeps.QuestFinished == nil {
		return false
	}
	return warpDeps.QuestFinished(username, quest)
}

func (s warpStore) AchievementDone(username, ach string) bool {
	if warpDeps.AchievementDone == nil {
		return false
	}
	return warpDeps.AchievementDone(username, ach)
}

func (s warpStore) FormatName(name string) string {
	if warpDeps.FormatName == nil {
		return name
	}
	return warpDeps.FormatName(name)
}

// BootWarps loads the warp registry + the menu-gating table from the same
// world.json path the map loader uses.
func BootWarps(worldPath string) {
	if err := Warps.Load(worldPath); err != nil {
		log.Printf("world: warps: %v (warp engine disabled)", err)
		return
	}
	log.Printf("world: warps loaded=%d", Warps.Count())
}

// FindWarp resolves a warp by Modules.Warps enum id (getWarp parity).
func FindWarp(id int) *WarpEntry { return Warps.FindByID(id) }

// FindWarpByName resolves a warp by (case-insensitive) name.
func FindWarpByName(name string) *WarpEntry { return Warps.FindByName(name) }

// HandleWarp routes C->S Warp frames [39,{id}] (incoming.ts handleWarp).
func HandleWarp(c WarpConn, data []byte) {
	var d struct {
		ID *int `json:"id"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.ID == nil {
		return
	}
	w := FindWarp(*d.ID)
	if w == nil {
		log.Printf("world: Could not find warp with id %d.", *d.ID)
		return
	}
	DoWarp(c, w)
}

// DoWarp ports controllers/warps.ts warp(): jail/tutorial/combat/cooldown/
// requirement gates then a random landing tile + Teleport delivery.
func DoWarp(c WarpConn, w *WarpEntry) bool {
	if c.Instance == "" || c.Username == "" || w == nil {
		return false
	}
	if w.W <= 0 || w.H <= 0 {
		return false
	}
	nowMs := time.Now().UnixMilli()
	if deny := Warps.Authorize(c.Username, w, nowMs, warpStore{admin: c.Admin, instance: c.Instance}); deny != "" {
		// Notify is a seam and may be unwired in non-server callers;
		// never panic on the deny path (ApplyTeleport below is
		// already nil-guarded the same way).
		if warpDeps.Notify != nil {
			warpDeps.Notify(c.Instance, deny)
		}
		return false
	}
	lx, ly, ok := Landing(w, rand.Intn)
	if !ok {
		return false
	}
	if warpDeps.ApplyTeleport != nil {
		warpDeps.ApplyTeleport(c.Instance, lx, ly)
	}
	Warps.Record(c.Username, nowMs)
	name := w.Name
	if warpDeps.FormatName != nil {
		name = warpDeps.FormatName(w.Name)
	}
	if warpDeps.Notify != nil {
		warpDeps.Notify(c.Instance, "warps:WARPED_TO;name="+name)
	}
	log.Printf("world: %s warped to %s (%d,%d)", c.Username, w.Name, lx, ly)
	return true
}

// ---------------------------------------------------------------------------
// Events.
// ---------------------------------------------------------------------------

// EventDeps bundles the event fan-out seam (implemented by the root
// adapter; never by this package).
type EventDeps struct {
	// Announce fans one rotation notice as a global chat line
	// (world.globalMessage parity via the socRouteGlobal path).
	Announce func(eventName string)
}

var eventDeps EventDeps

// ConfigureEvents installs the event fan-out seam (called once from the
// root boot, before the scheduler starts).
func ConfigureEvents(d EventDeps) { eventDeps = d }

// BootEvents starts the rotation scheduler. WORLD_EVENT_MS overrides the
// per-event cadence (test hook for fast event-notice legs); default keeps the
// TS hourly cadence from DefaultEvents.
func BootEvents() {
	n, every := Events.Boot()
	log.Printf("world: events started=%d intervalMs=%d", n, every)
}

// EventTick fans due rotation events out as global notices. Called from
// the central 20Hz tick loop (additive: no events due most ticks).
func EventTick() {
	for _, e := range Events.Due(time.Now().UnixMilli()) {
		if eventDeps.Announce != nil {
			eventDeps.Announce(e.Name)
		}
		log.Printf("world: event started: %s", e.Name)
	}
}

// DoubleDrops duplicates a mob roll while the double-drops event is
// active (events.ts doubleDropProbability parity, expressed as a repeated
// roll). Dormant otherwise — callers pass the roll through unchanged.
func DoubleDrops(drops []Drop) []Drop {
	if !Events.IsActive("double-drops") || len(drops) == 0 {
		return drops
	}
	out := make([]Drop, 0, len(drops)*2)
	return append(append(out, drops...), drops...)
}

// XPBoost reports the 1.5x experience event (experiencePerHit parity).
func XPBoost() bool { return Events.IsActive("experience") }

// HarvestDouble reports the lumberjacking/mining double-yield events
// (Utils.doubleLumberjacking/doubleMining parity) for a gathering skill.
func HarvestDouble(skill string) bool {
	switch skill {
	case "lumberjacking":
		return Events.IsActive("lumberjacking")
	case "mining":
		return Events.IsActive("mining")
	}
	return false
}

// DefaultEventCount reports the rotation size (worldtest echo parity).
func DefaultEventCount() int { return len(events.DefaultEvents()) }
