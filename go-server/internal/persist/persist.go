// Package persist owns the M5 SQLite persistence core: the single-writer
// Store (one *sql.DB, one dirty set, one mutex), the players/inventory/bank/
// equipment/skills schema plus the additive meta(k,v) table (D3 world_hash
// boot gate), and the write/load/flush operations. Moved
// verbatim out of the root m5.go persist section (E9a) WITHOUT behavior
// change: identical DDL, identical WAL+NORMAL pragmas, identical SQL
// sequences and log text.
//
// What moved here (persistence only):
//   - DB open + pragmas + schema + M12 enchantments migration (Open,
//     EnsureSchema) from m5Init/dbPath.
//   - Dirty-set tracking (MarkDirty/MarkClean/DirtyList) from markDirty and
//     the dirty map.
//   - Row writes (WritePlayer) from writePlayer.
//   - Row reads (LoadPlayer) from the DB half of m5Load.
//   - Dirty flush + final close (FlushDirty/Close).
//   - State snapshot deep copy (Snapshot) from the copy half of m5Snapshot.
//
// What stayed in the root m5.go (NOT moved):
//   - Drops/loot rolls (m5RollEntry/m5GetDrops/m5SpawnLoot + loot timers).
//   - XP/skill awards (m5AddXP/m5AwardCombatXP/m5GatherXP + level curves).
//   - The in-memory pstates map + m5Snapshot (needs pstateMu) + the
//     m5State/m5Slot/m5Skill types (M12 Enchantments type is root-owned).
//   - m5Init/markDirty/flushDirty/m5SaveSync/m5Load/m5LoginWelcome keep
//     their signatures and delegate to Store (converting m5State <->
//     persist.State); dbConn/dbMu stay declared in the root so the
//     m11/m13/social/abilities/ops direct-table seams compile untouched.
//   - The 10s dirty-flush ticker + SIGTERM/SIGINT final-flush goroutines
//     (ownership stays in root m5Init; same cadence, same semantics).
//
// Locking: Store serializes its dirty set and every write/read sequence on
// its internal mutex (single writer, one connection via SetMaxOpenConns(1)).
// The root additionally holds its dbMu across the per-key write+clean and
// across m5Load, exactly as before, so the legacy direct-DB seams
// (m13 flags, social guilds/friends, abilities, ops console) keep the same
// mutual exclusion with the M5 write path. Lock order is always
// dbMu -> Store.mu -> pstateMu and never the reverse: snapshots are taken
// before either DB mutex is held, so the root "never hold dbMu and pstateMu
// at the same time" discipline is preserved.
//
// Errors: Open/EnsureSchema/WritePlayer wrap failures with the same text
// the root log.Fatalf/log.Printf lines used ("open db: ...",
// "pragma ...: ...", "ddl: ...", "m5: save ..."), so the root m5Init
// log.Fatalf("m5: %v", err) and the write-path logs read byte-identical.
package persist

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	_ "modernc.org/sqlite"
)

// Slot is one inventory/bank/equipment entry. Ench carries the raw
// enchantments JSON for the inventory.enchantments and
// equipment.enchantments columns ("{}" when none); bank rows carry ""
// (their table has no such column, matching the root writePlayer which
// only persists enchantments for inventory and equipment).
type Slot struct {
	Key   string
	Count int
	Ench  string
}

// Skill is one skill row (level + XP).
type Skill struct {
	Level int
	XP    int
}

// StatsBlob is one player's gameplay statistics snapshot (counters ported
// from statistics.ts: mobKills/mobExamines/resources/drops plus PvP counters
// and session/lifecycle fields). Stored as a JSON blob in the additive
// `statistics` table (one row per player): a blob keeps the counters
// schemaless like the TS StatisticsData object, so future counter additions
// need no DDL.
type StatsBlob struct {
	MobKills    map[string]int `json:"mobKills,omitempty"`
	MobExamines []string       `json:"mobExamines,omitempty"`
	Resources   map[string]int `json:"resources,omitempty"`
	Drops       map[string]int `json:"drops,omitempty"`
	// PvP counters (handler.ts handleKill/handleDeath when isPlayer).
	PvPKills  int `json:"pvpKills,omitempty"`
	PvPDeaths int `json:"pvpDeaths,omitempty"`
	// Session/lifecycle tracking (statistics.ts construction + serialize).
	CreationTime    int64 `json:"creationTime,omitempty"`
	TotalTimePlayed int64 `json:"totalTimePlayed,omitempty"`
	LastLogin       int64 `json:"lastLogin,omitempty"`
	LoginCount      int   `json:"loginCount,omitempty"`
}

// State is the persist snapshot for one player: position/level/vitals plus
// the inventory, bank and equipment slot lists and the skills map.
// Equipment is dense by type index (holes are zero Slots); LoadPlayer sizes
// it to maxType+1 (nil when no rows) and the root pads/truncates it to the
// fixed ModulesEquipmentCount array exactly like the old m5Load.
// Rank is the Modules.Ranks value (database.setRank parity: offline /setrank
// persists here, the login path restores it onto the session).
type State struct {
	X      int
	Y      int
	Level  int
	HP     int
	Rank   int
	HomeX  int // home-point x (respawn target; DEFAULT spawn)
	HomeY  int // home-point y (respawn target; DEFAULT spawn)
	LastDailyReset  int64 // epoch ms of last daily reset (0 = never)
	LastWeeklyReset int64 // epoch ms of last weekly reset (0 = never)
	Inv    []Slot
	Bank   []Slot
	Equip  []Slot
	Skills map[int]Skill
	Stats  StatsBlob
}

// Snapshot deep-copies a State (nil-safe for the Skills map and the
// Stats blob maps/slices — statistics must survive every flush).
func (s *Store) Snapshot(st State) State {
	cp := State{
		X: st.X, Y: st.Y, Level: st.Level, HP: st.HP, Rank: st.Rank,
		HomeX: st.HomeX, HomeY: st.HomeY,
		LastDailyReset: st.LastDailyReset, LastWeeklyReset: st.LastWeeklyReset,
		Skills: make(map[int]Skill, len(st.Skills)),
	}
	cp.Inv = append(cp.Inv, st.Inv...)
	cp.Bank = append(cp.Bank, st.Bank...)
	cp.Equip = append(cp.Equip, st.Equip...)
	for id, sk := range st.Skills {
		cp.Skills[id] = sk
	}
	if st.Stats.MobKills != nil {
		cp.Stats.MobKills = make(map[string]int, len(st.Stats.MobKills))
		for k, v := range st.Stats.MobKills {
			cp.Stats.MobKills[k] = v
		}
	}
	if st.Stats.MobExamines != nil {
		cp.Stats.MobExamines = append([]string(nil), st.Stats.MobExamines...)
	}
	if st.Stats.Resources != nil {
		cp.Stats.Resources = make(map[string]int, len(st.Stats.Resources))
		for k, v := range st.Stats.Resources {
			cp.Stats.Resources[k] = v
		}
	}
	if st.Stats.Drops != nil {
		cp.Stats.Drops = make(map[string]int, len(st.Stats.Drops))
		for k, v := range st.Stats.Drops {
			cp.Stats.Drops[k] = v
		}
	}
	// Scalar stats: value types, safe to copy directly (no aliasing).
	cp.Stats.PvPKills = st.Stats.PvPKills
	cp.Stats.PvPDeaths = st.Stats.PvPDeaths
	cp.Stats.CreationTime = st.Stats.CreationTime
	cp.Stats.TotalTimePlayed = st.Stats.TotalTimePlayed
	cp.Stats.LastLogin = st.Stats.LastLogin
	cp.Stats.LoginCount = st.Stats.LoginCount
	return cp
}

// Store is the single-writer SQLite handle: one *sql.DB (single pooled
// connection), the dirty-player set, and the mutex serializing both.
type Store struct {
	db    *sql.DB
	mu    sync.Mutex
	dirty map[string]bool
	path  string
}

// Open opens the SQLite DB at path with the shipped pragmas and schema
// (WAL + NORMAL, single connection, players/inventory/bank/equipment/skills
// tables, M12 inventory.enchantments migration). Error text matches the old
// root m5Init fatal lines so the root log.Fatalf("m5: %v", err) reads
// identical.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dirty: map[string]bool{}, path: path}
	for _, pr := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL"} {
		if _, err := db.Exec(pr); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pr, err)
		}
	}
	// R3 pre-deploy hook (default off): BACKUP_ON_BOOT=1 snapshots the
	// owner connection before EnsureSchema migrates (see backup.go).
	s.maybePreMigrateBackup()
	if err := s.EnsureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	// M12 migration: pre-existing DBs lack the inventory enchantments column
	// (CREATE IF NOT EXISTS is a no-op there). Ignore failure = column exists.
	_, _ = db.Exec(`ALTER TABLE inventory ADD COLUMN enchantments TEXT`)
	return s, nil
}

// EnsureSchema creates the persist tables when missing (verbatim DDL
// from the old m5Init), then stamps/gates meta.schema_version (see
// schema.go: fresh DBs are stamped, older re-stamped forward, newer refuse
// boot). The DDL strings below are frozen; schema changes add new
// expand-only statements in the same commit that bumps
// CurrentSchemaVersion. v5 additionally folds the subsystem tables
// (abilities, guilds/guild_members, friends, quests/achievements — DDL
// identical to the owner packages) into the same gate so one version
// covers every table this binary reads or writes.
func (s *Store) EnsureSchema() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("ddl: nil store")
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS players(instance TEXT PRIMARY KEY, name TEXT, x INT, y INT, level INT, hp INT, data TEXT)`,
		`CREATE TABLE IF NOT EXISTS inventory(player TEXT, slot INT, item TEXT, count INT, enchantments TEXT, PRIMARY KEY(player, slot))`,
		`CREATE TABLE IF NOT EXISTS bank(player TEXT, slot INT, item TEXT, count INT, PRIMARY KEY(player, slot))`,
		`CREATE TABLE IF NOT EXISTS equipment(player TEXT, type INT, item TEXT, count INT, PRIMARY KEY(player, type))`,
		`CREATE TABLE IF NOT EXISTS skills(player TEXT, skill INT, level INT, xp INT, PRIMARY KEY(player, skill))`,
		`CREATE TABLE IF NOT EXISTS statistics(player TEXT PRIMARY KEY, data TEXT)`,
		`CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT)`,
		// v5 fold-in: subsystem tables gated by the same schema_version
		// (owner EnsureTables remain idempotent; DDL strings identical).
		`CREATE TABLE IF NOT EXISTS abilities(player TEXT, ability TEXT, level INT, PRIMARY KEY(player, ability))`,
		`CREATE TABLE IF NOT EXISTS guilds(id TEXT PRIMARY KEY, name TEXT, owner TEXT, xp INT, invite_only INT DEFAULT 0, creation_date INT DEFAULT 0, decoration TEXT DEFAULT '{}')`,
		`CREATE TABLE IF NOT EXISTS guild_members(guild TEXT, player TEXT, rank INT, join_date INT DEFAULT 0, PRIMARY KEY(guild, player))`,
		`CREATE TABLE IF NOT EXISTS friends(player TEXT, friend TEXT, PRIMARY KEY(player, friend))`,
		`CREATE TABLE IF NOT EXISTS quests(player TEXT, quest TEXT, stage INT, substage INT, PRIMARY KEY(player, quest))`,
		`CREATE TABLE IF NOT EXISTS achievements(player TEXT, ach TEXT, stage INT, PRIMARY KEY(player, ach))`,
		// v9 fold-in: identities (login name -> password derivation + reset
		// token). DDL identical to account.AccountsDDL so that package's own
		// EnsureTables stays idempotent.
		`CREATE TABLE IF NOT EXISTS accounts(id TEXT PRIMARY KEY, username TEXT UNIQUE, password_hash BLOB, salt BLOB, iterations INT DEFAULT 0, algo TEXT DEFAULT '', email TEXT DEFAULT '', email_norm TEXT DEFAULT '', created_at INT DEFAULT 0, reset_token_hash BLOB, reset_token_exp INT DEFAULT 0)`,
	} {
		if _, err := s.db.Exec(ddl); err != nil {
			return fmt.Errorf("ddl: %w", err)
		}
	}
	// v3 migration (expand-only): `players.rank` with a DEFAULT, so
	// pre-v3 rows read back 0 (None). Ignore failure = column exists
	// (M12 inventory.enchantments precedent in Open).
	_, _ = s.db.Exec(`ALTER TABLE players ADD COLUMN rank INT DEFAULT 0`)
	// v4 migration (expand-only): `equipment.enchantments` with a DEFAULT,
	// so pre-v4 rows read back '{}' (no enchantments). Ignore failure =
	// column exists (rank precedent above).
	_, _ = s.db.Exec(`ALTER TABLE equipment ADD COLUMN enchantments TEXT DEFAULT '{}'`)
	// v6 migration (expand-only): guild fields — invite_only, creation_date,
	// decoration on guilds; join_date on guild_members. Ignore failure =
	// column exists (precedent above).
	_, _ = s.db.Exec(`ALTER TABLE guilds ADD COLUMN invite_only INT DEFAULT 0`)
	_, _ = s.db.Exec(`ALTER TABLE guilds ADD COLUMN creation_date INT DEFAULT 0`)
	_, _ = s.db.Exec(`ALTER TABLE guilds ADD COLUMN decoration TEXT DEFAULT '{}'`)
	_, _ = s.db.Exec(`ALTER TABLE guild_members ADD COLUMN join_date INT DEFAULT 0`)
	// v7 migration (expand-only): home-point columns on players — DEFAULT
	// spawn coords so pre-v7 rows respawn at the same tile as before.
	_, _ = s.db.Exec(`ALTER TABLE players ADD COLUMN homex INT DEFAULT 100`)
	_, _ = s.db.Exec(`ALTER TABLE players ADD COLUMN homey INT DEFAULT 96`)
	// v8 migration (expand-only): daily/weekly reset timestamps on players —
	// DEFAULT 0 (never reset) so pre-v8 rows trigger a reset on first login.
	_, _ = s.db.Exec(`ALTER TABLE players ADD COLUMN last_daily_reset INT DEFAULT 0`)
	_, _ = s.db.Exec(`ALTER TABLE players ADD COLUMN last_weekly_reset INT DEFAULT 0`)
	// v9: account email lookup index (CREATE INDEX IF NOT EXISTS, so the
	// account package's own EnsureTables stays idempotent).
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS accounts_email ON accounts(email_norm)`)
	return s.checkSchemaVersion()
}

// DB exposes the underlying handle for the legacy direct-table seams (m11
// quest rows, m13 flags, social guilds/friends, abilities, ops console),
// which keep issuing their own SQL under the root dbMu.
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// Path returns the DB path the Store was opened with.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// MarkDirty flags a player for the next flush ("" is ignored, as before).
func (s *Store) MarkDirty(key string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	s.dirty[key] = true
	s.mu.Unlock()
}

// MarkClean clears a player's dirty flag.
func (s *Store) MarkClean(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.dirty, key)
	s.mu.Unlock()
}

// DirtyList returns the currently dirty keys (order unspecified, as before).
func (s *Store) DirtyList() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	keys := make([]string, 0, len(s.dirty))
	for key := range s.dirty {
		keys = append(keys, key)
	}
	s.mu.Unlock()
	return keys
}

// WritePlayer writes one player's full row set (players upsert + inventory +
// bank + equipment + skills + statistics) inside a single BEGIN IMMEDIATE
// transaction, verbatim SQL and log text from the old root writePlayer. The
// transaction keeps the 7-table DELETE+INSERT sequence atomic: a mid-write
// crash or error can no longer leave torn rows (e.g. inventory cleared but
// skills not yet rewritten). The error return is for tests/callers;
// failures are already logged with the shipped "m5: ..." lines, so root
// callers ignore it.
func (s *Store) WritePlayer(key string, st State) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("m5: save %s: nil store", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("m5: save players %s: %v", key, err)
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	extra := map[string]any{"equip": []any{}}
	if len(st.Equip) > 0 {
		eqs := make([]any, 0, len(st.Equip))
		for t, e := range st.Equip {
			if e.Key == "" || e.Count < 1 {
				continue
			}
			eqs = append(eqs, map[string]any{"type": t, "key": e.Key, "count": e.Count})
		}
		extra["equip"] = eqs
	}
	extraRaw, _ := json.Marshal(extra)
	if _, err := tx.Exec(
		`INSERT INTO players(instance,name,x,y,level,hp,data,rank,homex,homey,last_daily_reset,last_weekly_reset) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) `+
			`ON CONFLICT(instance) DO UPDATE SET name=excluded.name,x=excluded.x,y=excluded.y,`+
			`level=excluded.level,hp=excluded.hp,data=excluded.data,rank=excluded.rank,`+
			`homex=excluded.homex,homey=excluded.homey,`+
			`last_daily_reset=excluded.last_daily_reset,last_weekly_reset=excluded.last_weekly_reset`,
		key, key, st.X, st.Y, st.Level, st.HP, string(extraRaw), st.Rank, st.HomeX, st.HomeY,
		st.LastDailyReset, st.LastWeeklyReset); err != nil {
		log.Printf("m5: save players %s: %v", key, err)
		return err
	}
	if _, err := tx.Exec(`DELETE FROM inventory WHERE player=?`, key); err != nil {
		log.Printf("m5: clear inventory %s: %v", key, err)
		return err
	}
	for i, sl := range st.Inv {
		if _, err := tx.Exec(
			`INSERT INTO inventory(player,slot,item,count,enchantments) VALUES(?,?,?,?,?)`, key, i, sl.Key, sl.Count, sl.Ench); err != nil {
			log.Printf("m5: save inventory %s: %v", key, err)
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM bank WHERE player=?`, key); err != nil {
		log.Printf("m5: clear bank %s: %v", key, err)
		return err
	}
	for i, sl := range st.Bank {
		if _, err := tx.Exec(
			`INSERT INTO bank(player,slot,item,count) VALUES(?,?,?,?)`, key, i, sl.Key, sl.Count); err != nil {
			log.Printf("m5: save bank %s: %v", key, err)
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM equipment WHERE player=?`, key); err != nil {
		log.Printf("m5: clear equipment %s: %v", key, err)
		return err
	}
	for t, e := range st.Equip {
		if e.Key == "" || e.Count < 1 {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO equipment(player,type,item,count,enchantments) VALUES(?,?,?,?,?)`, key, t, e.Key, e.Count, e.Ench); err != nil {
			log.Printf("m5: save equipment %s: %v", key, err)
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM skills WHERE player=?`, key); err != nil {
		log.Printf("m5: clear skills %s: %v", key, err)
		return err
	}
	for id, sk := range st.Skills {
		if _, err := tx.Exec(
			`INSERT INTO skills(player,skill,level,xp) VALUES(?,?,?,?)`, key, id, sk.Level, sk.XP); err != nil {
			log.Printf("m5: save skills %s: %v", key, err)
			return err
		}
	}
	if err := s.writeStatsTx(tx, key, st.Stats); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		log.Printf("m5: save players %s: %v", key, err)
		return err
	}
	committed = true
	log.Printf("m5: saved %s (pos %d,%d level %d inv %d skills %d)", key, st.X, st.Y, st.Level, len(st.Inv), len(st.Skills))
	return nil
}

// WriteStats upserts one player's statistics blob (JSON; empty blobs still
// write a row so load-vs-never-played stays unambiguous).
func (s *Store) WriteStats(key string, blob StatsBlob) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("m5: save stats %s: nil store", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeStatsLocked(key, blob)
}

// writeStatsLocked is WriteStats with the store lock already held
// (WritePlayer calls it mid-write; Go mutexes are not reentrant).
func (s *Store) writeStatsLocked(key string, blob StatsBlob) error {
	raw, _ := json.Marshal(blob)
	if _, err := s.db.Exec(
		`INSERT INTO statistics(player,data) VALUES(?,?) `+
			`ON CONFLICT(player) DO UPDATE SET data=excluded.data`,
		key, string(raw)); err != nil {
		log.Printf("m5: save stats %s: %v", key, err)
		return err
	}
	return nil
}

// writeStatsTx is writeStatsLocked over an open transaction (the WritePlayer
// atomic-write path).
func (s *Store) writeStatsTx(tx *sql.Tx, key string, blob StatsBlob) error {
	raw, _ := json.Marshal(blob)
	if _, err := tx.Exec(
		`INSERT INTO statistics(player,data) VALUES(?,?) `+
			`ON CONFLICT(player) DO UPDATE SET data=excluded.data`,
		key, string(raw)); err != nil {
		log.Printf("m5: save stats %s: %v", key, err)
		return err
	}
	return nil
}

// LoadStats reads one player's statistics blob (false when no row yet).
func (s *Store) LoadStats(key string) (StatsBlob, bool) {
	if s == nil || s.db == nil || key == "" {
		return StatsBlob{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadStatsLocked(key)
}

// loadStatsLocked is LoadStats with the store lock already held
// (LoadPlayer calls it mid-read).
func (s *Store) loadStatsLocked(key string) (StatsBlob, bool) {
	var raw string
	if err := s.db.QueryRow(`SELECT data FROM statistics WHERE player=?`, key).Scan(&raw); err != nil {
		return StatsBlob{}, false
	}
	var blob StatsBlob
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		return StatsBlob{}, false
	}
	return blob, true
}

// LoadPlayer reads one player's full row set back (players + inventory +
// bank + equipment + skills), verbatim SQL from the old root m5Load. The
// second return is false when the player row is missing. Equipment comes
// back dense by type index (nil when the table is missing or empty); the
// root pads it to the fixed equipment array. Like the old code, the
// players.data JSON column is read but intentionally ignored (equipment
// restores from the equipment table).
func (s *Store) LoadPlayer(key string) (State, bool) {
	if s == nil || s.db == nil || key == "" {
		return State{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var name string
	st := State{Skills: map[int]Skill{}}
	var data string
	err := s.db.QueryRow(
		`SELECT name,x,y,level,hp,data,rank,homex,homey,last_daily_reset,last_weekly_reset FROM players WHERE instance=?`, key,
	).Scan(&name, &st.X, &st.Y, &st.Level, &st.HP, &data, &st.Rank, &st.HomeX, &st.HomeY,
		&st.LastDailyReset, &st.LastWeeklyReset)
	if err != nil {
		return State{}, false
	}
	rows, err := s.db.Query(`SELECT item,count,enchantments FROM inventory WHERE player=? ORDER BY slot`, key)
	if err != nil {
		return State{}, false
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var c int
		var ench string
		if err := rows.Scan(&k, &c, &ench); err != nil {
			continue
		}
		st.Inv = append(st.Inv, Slot{Key: k, Count: c, Ench: ench})
	}
	rows.Close()
	brows, err := s.db.Query(`SELECT item,count FROM bank WHERE player=? ORDER BY slot`, key)
	if err != nil {
		return State{}, false
	}
	for brows.Next() {
		var k string
		var c int
		if err := brows.Scan(&k, &c); err != nil {
			continue
		}
		st.Bank = append(st.Bank, Slot{Key: k, Count: c})
	}
	brows.Close()
	if erows, err := s.db.Query(`SELECT type,item,count,enchantments FROM equipment WHERE player=?`, key); err == nil {
		byType := map[int]Slot{}
		maxT := -1
		for erows.Next() {
			var t int
			var k string
			var c int
			var ench string
			if err := erows.Scan(&t, &k, &c, &ench); err != nil {
				continue
			}
			if t < 0 {
				continue
			}
			byType[t] = Slot{Key: k, Count: c, Ench: ench}
			if t > maxT {
				maxT = t
			}
		}
		erows.Close()
		if maxT >= 0 {
			st.Equip = make([]Slot, maxT+1)
			for t, sl := range byType {
				st.Equip[t] = sl
			}
		}
	} // else: pre-equipment DB, Equip stays nil (root normalizes)
	srows, err := s.db.Query(`SELECT skill,level,xp FROM skills WHERE player=?`, key)
	if err != nil {
		return State{}, false
	}
	defer srows.Close()
	for srows.Next() {
		var id, lv, xp int
		if err := srows.Scan(&id, &lv, &xp); err != nil {
			continue
		}
		st.Skills[id] = Skill{Level: lv, XP: xp}
	}
	// Statistics blob (missing row = fresh counters, statistics table is
	// additive — pre-v2 DBs simply have no rows yet).
	if blob, ok := s.loadStatsLocked(key); ok {
		st.Stats = blob
	}
	return st, true
}

// GetMeta reads one meta key (e.g. "world_hash" for the D3 boot gate).
// The second return is false when the key is missing or the store is nil.
// Callers hold the root dbMu, matching the other direct-table seams.
func (s *Store) GetMeta(key string) (string, bool) {
	if s == nil || s.db == nil || key == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

// SetMeta upserts one meta key. Callers hold the root dbMu, matching the
// other direct-table seams.
func (s *Store) SetMeta(key, value string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("meta: set %s: nil store", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(
		`INSERT INTO meta(k,v) VALUES(?,?) `+
			`ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		key, value); err != nil {
		return fmt.Errorf("meta: set %s: %w", key, err)
	}
	return nil
}

// FlushDirty writes every dirty player using snap to supply the
// authoritative in-memory state, then clears its flag (even when snap
// reports the key missing, matching the old flushDirty delete semantics).
// A nil snap only clears flags. Snapshots are taken with no Store lock
// held, so snap may safely take the owner's state lock.
func (s *Store) FlushDirty(snap func(key string) (State, bool)) {
	if s == nil {
		return
	}
	for _, key := range s.DirtyList() {
		var st State
		var ok bool
		if snap != nil {
			st, ok = snap(key)
		}
		if ok {
			_ = s.WritePlayer(key, st)
		}
		s.MarkClean(key)
	}
}

// Close performs a final FlushDirty(snap) and closes the DB handle.
func (s *Store) Close(snap func(key string) (State, bool)) error {
	if s == nil {
		return nil
	}
	s.FlushDirty(snap)
	return s.db.Close()
}
