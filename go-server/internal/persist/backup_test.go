package persist

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Snapshot round-trip: write rows, VACUUM INTO a timestamped archive (no
// sqlite3 CLI), reopen the archive, same rows back.
func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvBackupDir, dir)
	src := filepath.Join(t.TempDir(), "src.db")
	s, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := testState()
	if err := s.WritePlayer("hero", want); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	arch, err := s.SnapshotDB("")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !strings.HasPrefix(arch, dir) || !strings.HasSuffix(arch, ".db") {
		t.Fatalf("Snapshot path = %q, want timestamped .db under %q", arch, dir)
	}
	if err := s.Close(nil); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The archive is a full DB copy: reopen through the same stack.
	r, err := Open(arch)
	if err != nil {
		t.Fatalf("re-Open archive: %v", err)
	}
	defer r.Close(nil)
	got, ok := r.LoadPlayer("hero")
	if !ok {
		t.Fatalf("LoadPlayer(hero) on archive = false, want true")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("archive LoadPlayer mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// Live-DB snapshots route via Store.SnapshotDB only (there is no free
// Store-less Snapshot by design: a second handle cannot serialize against
// the owner's write transaction). This covers the owner-connection route
// twice in one process to exercise the suffixed-retry allocation.
func TestSnapshotPathFunc(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvBackupDir, dir)
	src := filepath.Join(t.TempDir(), "src.db")
	s, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(nil) }()
	if err := s.WritePlayer("hero", testState()); err != nil {
		t.Fatalf("WritePlayer: %v", err)
	}
	arch, err := s.SnapshotDB("")
	if err != nil {
		t.Fatalf("SnapshotDB: %v", err)
	}
	arch2, err := s.SnapshotDB("")
	if err != nil {
		t.Fatalf("SnapshotDB retry: %v", err)
	}
	if arch2 == arch {
		t.Fatalf("second snapshot = %q, want suffixed retry name", arch2)
	}
	r, err := Open(arch)
	if err != nil {
		t.Fatalf("re-Open archive: %v", err)
	}
	defer r.Close(nil)
	if _, ok := r.LoadPlayer("hero"); !ok {
		t.Fatalf("LoadPlayer(hero) on archive = false, want true")
	}
}

// BACKUP_ON_BOOT=1 snapshots before EnsureSchema migrates (archive appears
// in BACKUP_DIR); default (unset) snapshots nothing.
func TestBackupOnBootHook(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvBackupDir, dir)
	t.Setenv(EnvBackupOnBoot, "1")
	src := filepath.Join(t.TempDir(), "hooked.db")
	s, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s.Close(nil)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".db") {
		t.Fatalf("BACKUP_DIR entries = %v, want one timestamped .db", entries)
	}
}

func TestBackupOnBootDefaultOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvBackupDir, dir)
	src := filepath.Join(t.TempDir(), "plain.db")
	s, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s.Close(nil)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("default boot wrote backups = %v, want none", entries)
	}
}

func TestBackupEnvParsing(t *testing.T) {
	if got := BackupDirFromEnv(getenvOfBackup(nil)); got != DefaultBackupDir {
		t.Fatalf("default dir = %q, want %q", got, DefaultBackupDir)
	}
	g := func(k string) string {
		if k == EnvBackupDir {
			return "/tmp/archives"
		}
		return ""
	}
	if got := BackupDirFromEnv(g); got != "/tmp/archives" {
		t.Fatalf("dir = %q", got)
	}
	if BackupOnBootFromEnv(getenvOfBackup(nil)) {
		t.Fatal("unset BACKUP_ON_BOOT must be off")
	}
	on := func(k string) string {
		if k == EnvBackupOnBoot {
			return "1"
		}
		return ""
	}
	if !BackupOnBootFromEnv(on) {
		t.Fatal("BACKUP_ON_BOOT=1 must be on")
	}
}

func TestSnapshotDirModePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "archives")
	src := filepath.Join(t.TempDir(), "src.db")
	s, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(nil) }()
	if _, err := s.SnapshotDB(dir); err != nil {
		t.Fatalf("SnapshotDB: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("backup dir mode = %o, want 0700 (no group/other bits)", fi.Mode().Perm())
	}
}

func getenvOfBackup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}
