// World event rotation orchestration over the pure internal/events
// scheduler.
//
// Mirrors packages/server/src/controllers/events.ts (weekend-only rotation
// over ['double drops','1.5x experience','lumberjacking','mining'], hourly
// check, world.globalMessage start/end announces, Utils flags +
// doubleDropProbability/experiencePerHit multipliers). This type owns the
// scheduler plus the active-set/fired counters; the root adapter owns
// transport (global chat fan-out) and the m5 multiplier call sites.
//
// Transport-free: no send/broadcast, no timers. The caller supplies nowMs
// clocks so tests stay deterministic. WORLD_EVENT_MS overrides the
// per-event cadence (test hook); default keeps the TS hourly cadence.
package controller

import (
	"os"
	"strconv"
	"sync"
	"time"

	"rpg-world-server/internal/events"
)

// EventController owns the rotation scheduler and the active event set.
type EventController struct {
	mu     sync.Mutex
	sched  *events.Scheduler
	active map[string]bool
	fired  int
	every  int64
}

// NewEventController returns an empty controller (Boot starts it).
func NewEventController() *EventController {
	return &EventController{active: map[string]bool{}}
}

// EventIntervalMs resolves the per-event cadence: WORLD_EVENT_MS when set
// to a positive integer, else the TS hourly CheckIntervalMs.
func EventIntervalMs() int64 {
	if v := os.Getenv("WORLD_EVENT_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return events.CheckIntervalMs
}

// Boot starts the rotation scheduler with DefaultEvents on the resolved
// cadence. It returns the event count and cadence for the caller's log
// line.
func (c *EventController) Boot() (int, int64) {
	list := events.DefaultEvents()
	every := EventIntervalMs()
	if every != events.CheckIntervalMs {
		for i := range list {
			list[i].IntervalMs = every
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.every = every
	if c.active == nil {
		c.active = map[string]bool{}
	}
	c.sched = events.NewScheduler(list)
	// Production weekend gate (TS events.ts getDay()%6 rotation): enabled
	// exactly when the cadence is NOT overridden. The WORLD_EVENT_MS test
	// hook implies test timing, where e2e/world requires events to fire
	// any day of the week — so the override keeps the flat cadence.
	if os.Getenv("WORLD_EVENT_MS") == "" {
		c.sched.SetNow(time.Now)
	}
	c.sched.Start()
	return len(list), every
}

// Due fans the scheduler at nowMs and activates a single event: the
// first due one in rotation order. TS keeps one activeEvent at a time
// (events.ts picks events[weekNumber % len] and holds it until disable),
// so consuming only the head of the due list is the rotation parity —
// firing the whole list would stack double-drops with 1.5x experience
// every cadence. (The scheduler package itself still reports all due
// events; single-pick lives here per its documented caller guidance.)
func (c *EventController) Due(nowMs int64) []events.Event {
	c.mu.Lock()
	sched := c.sched
	c.mu.Unlock()
	if sched == nil {
		return nil
	}
	due := sched.Due(nowMs)
	if len(due) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	head := due[0]
	c.active[head.Key] = true
	c.fired++
	return []events.Event{head}
}

// IsActive reports whether the event key has fired (Utils flag parity:
// doubleLumberjacking/doubleMining, doubleDropProbability,
// experiencePerHit).
func (c *EventController) IsActive(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[key]
}

// ActiveKeys returns the active event keys (unsorted; caller sorts for
// display).
func (c *EventController) ActiveKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.active))
	for k := range c.active {
		out = append(out, k)
	}
	return out
}

// Fired reports how many event firings have occurred.
func (c *EventController) Fired() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

// Interval reports the per-event cadence in milliseconds.
func (c *EventController) Interval() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.every
}
