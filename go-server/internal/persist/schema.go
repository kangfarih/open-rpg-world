// R3 expand-only migrations (GO-PLAN §11 + §12 R3): schema_version gate.
//
// Migration rule (expand-only): every schema change must be backward
// compatible with the previous release's binary — additive tables, new
// columns that are NULLABLE or carry a DEFAULT, never renames or drops in
// the same release that stops writing them. Drops/renames land at least
// two releases after the code stops depending on the old shape, so a
// rollback to the previous binary always boots against a migrated DB.
//
// Enforcement: EnsureSchema stamps meta.schema_version with
// CurrentSchemaVersion on fresh DBs, re-stamps forward on older DBs (the
// additive DDL above already converged them), and REFUSES boot with an
// explicit error when the DB's version is newer than this binary knows —
// the caller (m5Init) turns that error into log.Fatalf, so a stale binary
// never serves a schema it cannot understand. Rollback across a migration
// is always safe (old binary + old-or-same version); roll-forward of the
// binary is the only supported direction for newer schemas.
package persist

import (
	"fmt"
	"strconv"
	"strings"
)

// SchemaVersionKey is the meta row carrying the schema version.
const SchemaVersionKey = "schema_version"

// CurrentSchemaVersion is the schema this binary understands. Bump it in
// the same commit that adds the expand-only DDL to EnsureSchema.
//
// v2 adds the `statistics` table (player statistics JSON blob, see
// persist.go StatsBlob). Expand-only: a new table no older query touches,
// so v1 binaries keep booting against a v2 DB for every table they know
// (they only refuse via the version gate below, which is the intended
// roll-forward-only direction).
//
// v3 adds `players.rank` (Modules.Ranks value, DEFAULT 0 = None;
// database.setRank parity for offline /setrank). Expand-only: a new column
// with a DEFAULT that no older query names, so v2 binaries keep reading
// and writing every column they know.
//
// v4 adds `equipment.enchantments` (raw enchantments JSON, DEFAULT '{}' =
// none; M12 inventory.enchantments parity so enchanted gear survives the
// DB round-trip). Expand-only: a new column with a DEFAULT that no older
// query names, so v3 binaries keep reading and writing every column they
// know.
//
// v5 folds the subsystem tables into the same version gate (abilities,
// guilds/guild_members, friends, quests/achievements — DDL identical to
// the owner packages' EnsureTables, CREATE IF NOT EXISTS so the owners'
// own EnsureTables stay idempotent). Previously those tables were created
// outside the gate: a stale binary could boot a DB whose subsystem tables
// it did not understand. Now the single schema_version covers them.
//
// v6 adds guild metadata columns (expand-only): `guilds.invite_only`,
// `guilds.creation_date`, `guilds.decoration` (JSON), and
// `guild_members.join_date`. Older binaries never name these columns, so
// they keep reading and writing every column they know.
//
// v7 adds home-point columns (expand-only): `players.homex` and
// `players.homey` (DEFAULT spawn coords 100,96). The respawn path reads
// these instead of the hardcoded HeroSpawn constants, so /sethome+/home
// survive restarts. Older binaries never name these columns.
//
// v8 adds daily/weekly reset timestamps (expand-only):
// `players.last_daily_reset` and `players.last_weekly_reset` (DEFAULT 0
// = never reset). The background ticker and login path call resets.MaybeReset
// to detect boundary crossings and bump the timestamps. Older binaries
// never name these columns.
const CurrentSchemaVersion = 8

// checkSchemaVersion stamps or gates meta.schema_version. Fresh DBs (no
// row) are stamped with the current version; older versions are re-stamped
// forward once the additive DDL has converged them; newer versions refuse
// boot with an explicit error naming both versions and the recovery path.
func (s *Store) checkSchemaVersion() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("ddl: nil store")
	}
	stored, ok := s.GetMeta(SchemaVersionKey)
	switch {
	case !ok || strings.TrimSpace(stored) == "":
		if err := s.SetMeta(SchemaVersionKey, strconv.Itoa(CurrentSchemaVersion)); err != nil {
			return fmt.Errorf("ddl: stamp schema_version: %w", err)
		}
		return nil
	}
	v, err := strconv.Atoi(strings.TrimSpace(stored))
	if err != nil {
		return fmt.Errorf("persist: schema_version %q unreadable (want integer): refusing boot; "+
			"restore a backup from %s or reset the DB", stored, DefaultBackupDir)
	}
	if v > CurrentSchemaVersion {
		return fmt.Errorf("persist: schema_version %d newer than binary supports (%d): refusing boot; "+
			"deploy the matching (or newer) binary, or restore a backup from %s",
			v, CurrentSchemaVersion, DefaultBackupDir)
	}
	if v < CurrentSchemaVersion {
		if err := s.SetMeta(SchemaVersionKey, strconv.Itoa(CurrentSchemaVersion)); err != nil {
			return fmt.Errorf("ddl: stamp schema_version: %w", err)
		}
	}
	return nil
}
