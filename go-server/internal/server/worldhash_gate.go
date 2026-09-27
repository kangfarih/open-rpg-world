// D3 world_hash boot gate (additive, behavior-frozen when data unchanged).
//
// At boot, after the persist store opens (still inside initPlayerState, the first
// frozen boot step — boot order unchanged), the gate compares the sha256 of
// the world.json bytes actually resolved for this boot
// (internal/data.WorldHash: WORLD_JSON override, then checkout filesystem,
// then the embedded copy) against the meta.world_hash row in SQLite:
//
//   - first boot (no row): store the hash, log it, continue;
//   - match: log ok, continue;
//   - mismatch: the map bytes changed since the DB was stamped (or the DB is
//     stale/copied from another checkout). Log an explicit error every time.
//     Refuse boot (log.Fatalf) ONLY when WORLD_HASH_STRICT=1; otherwise warn
//     and continue.
//
// Strict-off is the default because dev iteration constantly edits
// world.json and the TESTMAP/CLEAN/COMBAT e2e loops must boot through map
// drift — the gate is a drift detector, not a dev blocker. Prod deploys set
// WORLD_HASH_STRICT=1 so a stale map (or a DB copied across checkouts, where
// persisted player positions no longer line up with collision geometry)
// fails loudly instead of serving a desynced world. Recovery is explicit in
// the fatal text: restore the matching world.json, or delete/reset data.db
// to re-stamp (losing persisted players).
//
// No packet shapes, no other schema, no loader paths change: every data
// loader keeps its own filesystem resolution (byte-identical while files are
// unchanged), and the gate only reads/adds the meta row.
package server

import (
	"log"
	"os"

	"rpg-world-server/internal/data"
)

// worldHashKey is the meta row stamped by the gate.
const worldHashKey = "world_hash"

// worldHashStrict reports whether a world_hash mismatch must refuse boot.
// Only the exact value "1" arms it; anything else (including unset) warns.
func worldHashStrict() bool {
	return os.Getenv("WORLD_HASH_STRICT") == "1"
}

// checkWorldHashGate runs the D3 gate. Callers hold no locks; dbMu is taken
// here around the meta row access, matching the other direct-table seams
// (lock order dbMu -> Store.mu, never the reverse).
func checkWorldHashGate() {
	hash := data.WorldHash()
	if hash == "" {
		// world.json unreadable: the map loader fails boot with its own
		// error below; the gate must not mask it with a second fatal.
		log.Printf("worldhash: WARN cannot hash world.json (map loader will report; gate skipped)")
		return
	}
	dbMu.Lock()
	stored, ok := persistStore.GetMeta(worldHashKey)
	dbMu.Unlock()
	switch {
	case !ok || stored == "":
		dbMu.Lock()
		err := persistStore.SetMeta(worldHashKey, hash)
		dbMu.Unlock()
		if err != nil {
			log.Printf("worldhash: WARN first boot could not store world_hash=%s: %v", hash, err)
			return
		}
		log.Printf("worldhash: stored world_hash=%s (first boot)", hash)
	case stored == hash:
		log.Printf("worldhash: ok world_hash=%s", hash)
	default:
		if worldHashStrict() {
			log.Fatalf("worldhash: MISMATCH data=%s stored=%s: refusing boot (WORLD_HASH_STRICT=1). "+
				"Restore the matching world.json, or reset data.db to re-stamp (loses persisted players).",
				hash, stored)
		}
		log.Printf("worldhash: WARN MISMATCH data=%s stored=%s (continuing; set WORLD_HASH_STRICT=1 to refuse boot on drift)", hash, stored)
	}
}
