// Doors (M10b) for package entity: the map.ts loadDoors port (tile-indexed
// door table with linked destinations + level/skill/quest/achievement gates).
//
// loadDoors parity: the world.json `areas.doors` list (278 entries) is cloned
// and each entry carrying a `destination` id is linked to the clone with that
// id; the table key is the ENTRY tile index (y*width+x) and the value carries
// the DESTINATION coordinates plus the entry's own gate fields. Entries
// without a destination (6) and destinations with no match (none in the
// shipped map) are skipped, exactly like Node.
//
// The trigger point is player.ts handleMovementStop (TS player.ts:1280): the
// door fires when the player STOPS on a door tile, after setPosition —
// mirrored by the server's MovementStep authoritative-position hook, not by
// teleports (door teleport landings must not re-trigger, the pairs link both
// ways).
//
// Gate order mirrors handler.ts handleDoor: talkIndex reset, level/skill
// gate (NO_COMBAT_DOOR / NO_SKILL_DOOR), quest doorCallback (stage gate),
// achievement finish, reqAchievement, reqQuest, reqItem (+DOOR_KEY_CRUMBLES),
// then teleport to the linked destination. The evaluation itself lives with
// the server (it owns levels/inventory/quest state); this file owns the
// parsed + linked table only.
package entity

import (
	"encoding/json"
	"strconv"
)

// Door is one linked world.json door entry (ProcessedDoor parity for the
// fields the gate flow reads). X/Y is the entry tile; DestX/DestY is the
// linked destination tile. Stage accepts both JSON numbers and strings
// (the shipped map mixes 4 and "2").
type Door struct {
	ID             int
	X, Y           int
	DestX, DestY   int
	Orientation    string
	Quest          string
	Achievement    string
	ReqAchievement string
	ReqQuest       string
	ReqItem        string
	ReqItemCount   int
	Stage          int
	Skill          string
	Level          int
	NPC            string
}

// rawDoor is the on-disk shape of one areas.doors entry (file shape frozen).
type rawDoor struct {
	ID             int             `json:"id"`
	X              int             `json:"x"`
	Y              int             `json:"y"`
	Destination    int             `json:"destination"`
	Orientation    string          `json:"orientation"`
	Quest          string          `json:"quest"`
	Achievement    string          `json:"achievement"`
	ReqAchievement string          `json:"reqAchievement"`
	ReqQuest       string          `json:"reqQuest"`
	ReqItem        string          `json:"reqItem"`
	ReqItemCount   int             `json:"reqItemCount"`
	Stage          json.RawMessage `json:"stage"`
	Skill          string          `json:"skill"`
	Level          int             `json:"level"`
	NPC            string          `json:"npc"`
}

// doorStage parses the mixed number/string stage field (0 when absent).
func doorStage(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return 0
}

// loadDoors links raw door entries like map.ts loadDoors. width is the map
// width for coordToIndex. Returns the entry-index -> linked-door table.
// A non-positive width yields an empty table (no valid tile indexes).
func loadDoors(raw []rawDoor, width int) map[int]*Door {
	out := make(map[int]*Door)
	if width <= 0 {
		return out
	}
	clone := make([]rawDoor, len(raw))
	copy(clone, raw)
	byID := make(map[int]*rawDoor, len(clone))
	for i := range clone {
		byID[clone[i].ID] = &clone[i]
	}
	for _, d := range clone {
		if d.Destination == 0 {
			continue // no destination (6 entries in the shipped map)
		}
		dest, ok := byID[d.Destination]
		if !ok {
			continue
		}
		orient := dest.Orientation
		if orient == "" {
			orient = "d"
		}
		out[d.Y*width+d.X] = &Door{
			ID: d.ID, X: d.X, Y: d.Y,
			DestX: dest.X, DestY: dest.Y,
			Orientation:    orient,
			Quest:          d.Quest,
			Achievement:    d.Achievement,
			ReqAchievement: d.ReqAchievement,
			ReqQuest:       d.ReqQuest,
			ReqItem:        d.ReqItem,
			ReqItemCount:   d.ReqItemCount,
			Stage:          doorStage(d.Stage),
			Skill:          d.Skill,
			Level:          d.Level,
			NPC:            d.NPC,
		}
	}
	return out
}

// DoorAt reports the linked door whose ENTRY tile is (x,y), or nil.
func DoorAt(x, y int) *Door {
	areasMu.Lock()
	defer areasMu.Unlock()
	if areasWidth <= 0 {
		return nil
	}
	return doors[doorIndex(x, y)]
}

// IsDoor reports whether (x,y) is a door entry tile (map.ts isDoor parity).
func IsDoor(x, y int) bool {
	return DoorAt(x, y) != nil
}

// DoorCount reports the linked table size (introspection/tests).
func DoorCount() int {
	areasMu.Lock()
	defer areasMu.Unlock()
	return len(doors)
}

func doorIndex(x, y int) int {
	return y*areasWidth + x
}
