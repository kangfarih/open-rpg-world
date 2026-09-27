// R3 deploy backup (GO-PLAN §12 R3): timestamped SQLite snapshots.
//
// Snapshot copies the live DB file to a timestamped archive with
// `VACUUM INTO '<archive>'` issued through database/sql on the owner
// connection (no sqlite3 CLI). VACUUM INTO takes a consistent snapshot
// even in WAL mode and refuses to overwrite an existing file, so every
// archive name embeds a UTC timestamp (plus the source base name).
//
// Env contract (all default-off, so the default boot is unchanged):
//
//	BACKUP_DIR     archive directory (default ./backups, created on demand).
//	BACKUP_ON_BOOT=1 snapshots the DB via the owner connection inside Open,
//	               before EnsureSchema migrates (default off: no snapshot).
package persist

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// EnvBackupDir selects the snapshot archive directory.
const EnvBackupDir = "BACKUP_DIR"

// EnvBackupOnBoot arms the pre-migrate snapshot inside Open.
const EnvBackupOnBoot = "BACKUP_ON_BOOT"

// DefaultBackupDir is the archive directory when BACKUP_DIR is unset.
const DefaultBackupDir = "./backups"

// BackupDir returns the archive directory (BACKUP_DIR, else ./backups).
func BackupDir() string { return BackupDirFromEnv(os.Getenv) }

// BackupDirFromEnv is BackupDir over an injected env lookup (tests).
func BackupDirFromEnv(getenv func(string) string) string {
	if getenv != nil {
		if v := strings.TrimSpace(getenv(EnvBackupDir)); v != "" {
			return v
		}
	}
	return DefaultBackupDir
}

// BackupOnBoot reports whether Open must snapshot before EnsureSchema
// migrates. Only the exact value "1" arms it; anything else (including
// unset) keeps today's behavior (no snapshot).
func BackupOnBoot() bool { return BackupOnBootFromEnv(os.Getenv) }

// BackupOnBootFromEnv is BackupOnBoot over an injected env lookup (tests).
func BackupOnBootFromEnv(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	return strings.TrimSpace(getenv(EnvBackupOnBoot)) == "1"
}

// archivePath renders <dir>/<UTC timestamp>-<base>.db. The timestamp has
// second granularity; uniqueness across rapid successive snapshots comes
// from snapshotOwner's suffixed retry loop below (VACUUM INTO never
// overwrites, so a clash surfaces as an error that the loop retries with
// a fresh suffix, never as silent data loss).
func archivePath(dir, srcPath string, now time.Time) string {
	base := filepath.Base(srcPath)
	if base == "" || base == "." || base == "/" {
		base = "data.db"
	}
	stamp := now.UTC().Format("20060102-150405")
	return filepath.Join(dir, stamp+"-"+base)
}

// snapshotInto copies db into a fresh archive file with VACUUM INTO.
// dest must not exist (VACUUM INTO errors otherwise — archives are never
// silently overwritten). Single quotes in the path are escaped per SQL.
func snapshotInto(db *sql.DB, dest string) error {
	if db == nil {
		return fmt.Errorf("backup: nil db")
	}
	if dest == "" {
		return fmt.Errorf("backup: empty archive path")
	}
	q := fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(dest, "'", "''"))
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("backup: vacuum into %s: %w", dest, err)
	}
	return nil
}

// snapshotOwner snapshots the already-open owner connection into a fresh
// timestamped archive under dir, creating dir on demand (0700: archives
// may contain player rows). It reports the archive path. A timestamp
// collision (VACUUM INTO refusing to overwrite) retries with incrementing
// numeric suffixes (-1, -2, ...) until a fresh name lands.
func snapshotOwner(db *sql.DB, srcPath, dir string, now time.Time) (string, error) {
	if db == nil {
		return "", fmt.Errorf("backup: nil db")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("backup: mkdir %s: %w", dir, err)
	}
	base := archivePath(dir, srcPath, now)
	trimmed := strings.TrimSuffix(base, ".db")
	for i := 0; i < 100; i++ {
		dest := base
		if i > 0 {
			dest = fmt.Sprintf("%s-%d.db", trimmed, i)
		}
		if err := snapshotInto(db, dest); err != nil {
			if !strings.Contains(err.Error(), "already exists") && !strings.Contains(strings.ToLower(err.Error()), "exists") {
				return "", err
			}
			continue
		}
		return dest, nil
	}
	return "", fmt.Errorf("backup: could not allocate archive name under %s", dir)
}

// SnapshotDB copies this Store's DB to a fresh timestamped archive under dir
// (empty dir = BACKUP_DIR env, else ./backups) via VACUUM INTO on the owner
// connection, and reports the archive path. The Store mutex is held across
// the copy so no write interleaves with the snapshot. This is the only
// supported live-DB snapshot route: there is no free (Store-less) Snapshot
// function by design, since a second handle cannot serialize against the
// owner's write transaction (torn-copy risk).
//
// (Named SnapshotDB — not Snapshot — because (*Store).Snapshot already
// deep-copies the in-memory player State.)
func (s *Store) SnapshotDB(dir string) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("backup: nil store")
	}
	if strings.TrimSpace(dir) == "" {
		dir = BackupDir()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshotOwner(s.db, s.path, dir, time.Now())
}

// maybePreMigrateBackup runs the BACKUP_ON_BOOT pre-deploy hook: when the
// env arms it, snapshot the just-opened owner connection before EnsureSchema
// migrates. A backup failure only warns (the deploy should not proceed on a
// broken disk, but boot refusal here would strand the old build; the
// operator gates on the log line). Default off: no snapshot, no log line,
// no behavior change.
func (s *Store) maybePreMigrateBackup() {
	if s == nil || s.db == nil || !BackupOnBoot() {
		return
	}
	dest, err := snapshotOwner(s.db, s.path, BackupDir(), time.Now())
	if err != nil {
		log.Printf("backup: WARN pre-migrate snapshot of %s failed: %v", s.path, err)
		return
	}
	log.Printf("backup: pre-migrate snapshot %s -> %s", s.path, dest)
}
