// C->S Examine (packet 51) parity with incoming.ts handleExamine.
//
// Stock client (controllers/input.ts:391): socket.send(Packets.Examine,
// [instance]) -> [51,["<instance>"]] on the wire. TS looks the entity up,
// requires mob-or-item, records statistics.addMobExamine(key), then notifies
// the description or misc:NO_IDEA when it has none.
package server

import (
	"encoding/json"
	"log"

	"rpg-world-server/internal/entity"
)

// examineResolve maps an instance to its examine key + description without
// side effects (unit-test seam; the conn wrapper below adds stats+notify).
// Mob descriptions come from the mobs.json profile (eye's string[] picks
// uniformly, mob.getDescription parity); item descriptions from items.json
// `description`. Bags, unknown instances and description-less-but-known
// entities resolve per the ok/desc contract: ok=false (silent return, TS
// `if (!entity) return` / not-mob-or-item) vs ok=true with desc="" (notify
// misc:NO_IDEA).
func examineResolve(instance string) (key, desc string, ok bool) {
	if m := mobFor(instance); m != nil {
		key = m.key
		if d, has := entity.Profile(key); has && d != nil {
			if text, pick := d.Description.Pick(); pick {
				return key, text, true
			}
		}
		return key, "", true
	}
	if l, found := entity.FindLoot(instance); found {
		if l.Bag {
			return "", "", false // lootbags are neither mob nor item (TS guards)
		}
		if len(l.Items) == 0 || l.Items[0].Key == "" {
			return "", "", false
		}
		key = l.Items[0].Key
		if info := itemInfoFor(key); info != nil && info.Description != "" {
			return key, info.Description, true
		}
		return key, "", true
	}
	return "", "", false
}

// handleExamineReq routes one C Examine frame: [51,["<instance>"]] (stock
// client shape; a bare [51,"<instance>"] is accepted too).
func handleExamineReq(c *playerConn, frame clientFrame) {
	if c == nil || len(frame) < 2 {
		return
	}
	var instance string
	var arr []string
	if err := json.Unmarshal(frame[1], &arr); err == nil && len(arr) > 0 {
		instance = arr[0]
	} else if err := json.Unmarshal(frame[1], &instance); err != nil || instance == "" {
		return
	}
	key, desc, ok := examineResolve(instance)
	if !ok {
		return
	}
	// TS records the examine BEFORE the description check (even when the
	// entity has no description).
	statsAddMobExamine(c, key)
	if desc == "" {
		notifyPlayer(c, "misc:NO_IDEA")
		return
	}
	notifyPlayer(c, desc)
	log.Printf("examine: %s examined %s (%s)", c.Instance, instance, key)
}
