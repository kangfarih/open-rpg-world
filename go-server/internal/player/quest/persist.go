package quest

import (
	"database/sql"
	"encoding/json"
	"log"
)

// ---------------------------------------------------------------------------
// Persistence (SQLite quests + achievements tables, m5 dirty-flush style).
// ---------------------------------------------------------------------------

// DB abstracts the quest/achievement tables. *sql.DB satisfies it; a nil DB
// disables persistence (dbConn == nil parity). The root adapter passes its
// dbConn through, converting a nil *sql.DB to a nil interface.
type DB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

// EnsureTables creates the quest/achievement tables (M5 DDL order). The
// quests table carries a completed JSON column for the substage NPC set
// (royalpet parity); legacy DBs without it are migrated via ADD COLUMN.
func EnsureTables(d Deps) {
	if d.DB == nil {
		return
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS quests(player TEXT, quest TEXT, stage INT, substage INT, completed TEXT DEFAULT '', PRIMARY KEY(player, quest))`,
		`CREATE TABLE IF NOT EXISTS achievements(player TEXT, ach TEXT, stage INT, PRIMARY KEY(player, ach))`,
	} {
		if _, err := d.DB.Exec(ddl); err != nil {
			log.Printf("m11: ddl: %v", err)
		}
	}
	// Migrate pre-completed-column DBs (fresh CREATEs no-op below).
	_, _ = d.DB.Exec(`ALTER TABLE quests ADD COLUMN completed TEXT DEFAULT ''`)
}

// questRow is the snapshot written without holding mu (snapshot-then-write:
// holding the registry lock across SQLite I/O stalled every conn goroutine
// on slow flushes).
type questRow struct {
	key       string
	stage     int
	sub       int
	completed string
}

// PersistQuests writes the quest rows for one player (called from the
// disconnect/flush path).
func PersistQuests(d Deps, username string) {
	if d.DB == nil || username == "" {
		return
	}
	mu.Lock()
	st, found := states[username]
	var quests []questRow
	var achs map[string]int
	if found {
		for key, q := range st.Quests {
			completed := ""
			if len(q.Completed) > 0 {
				if raw, err := json.Marshal(q.Completed); err == nil {
					completed = string(raw)
				}
			}
			quests = append(quests, questRow{key: key, stage: q.Stage, sub: q.SubStage, completed: completed})
		}
		achs = make(map[string]int, len(st.Achs))
		for key, stage := range st.Achs {
			achs[key] = stage
		}
	}
	mu.Unlock()
	if !found {
		return
	}
	if _, err := d.DB.Exec(`DELETE FROM quests WHERE player=?`, username); err != nil {
		return
	}
	for _, r := range quests {
		if _, err := d.DB.Exec(`INSERT INTO quests(player,quest,stage,substage,completed) VALUES(?,?,?,?,?)`,
			username, r.key, r.stage, r.sub, r.completed); err != nil {
			log.Printf("m11: save quest %s: %v", r.key, err)
		}
	}
	if _, err := d.DB.Exec(`DELETE FROM achievements WHERE player=?`, username); err != nil {
		return
	}
	for key, stage := range achs {
		if stage == 0 {
			continue
		}
		if _, err := d.DB.Exec(`INSERT INTO achievements(player,ach,stage) VALUES(?,?,?)`,
			username, key, stage); err != nil {
			log.Printf("m11: save achievement %s: %v", key, err)
		}
	}
}

// LoadQuests restores quest/achievement rows into the in-memory state
// (called before the login batches are built). Unknown keys are skipped
// (quests.ts load `if (quest)` parity); negative cursors are clamped to 0
// (corrupt-row guard — TS never writes negatives, valid rows are untouched,
// overflow stages still count as finished via the `>=` rule).
func LoadQuests(d Deps, username string) {
	Load()
	if !ok || d.DB == nil || username == "" {
		return
	}
	st := StateFor(username)
	if rows, err := d.DB.Query(`SELECT quest,stage,substage,completed FROM quests WHERE player=?`, username); err == nil {
		for rows.Next() {
			var key string
			var stage, sub int
			var completed sql.NullString
			if rows.Scan(&key, &stage, &sub, &completed) != nil {
				continue
			}
			if Quests[key] == nil {
				continue
			}
			if stage < 0 {
				stage = 0
			}
			if sub < 0 {
				sub = 0
			}
			q := st.Quest(key)
			q.Stage, q.SubStage = stage, sub
			q.ClearCompleted()
			if completed.Valid && completed.String != "" {
				var done []string
				if json.Unmarshal([]byte(completed.String), &done) == nil {
					for _, npc := range done {
						q.AddCompleted(npc)
					}
				}
			}
		}
		rows.Close()
	} else {
		loadQuestsLegacy(d, st)
	}
	if rows, err := d.DB.Query(`SELECT ach,stage FROM achievements WHERE player=?`, username); err == nil {
		for rows.Next() {
			var key string
			var stage int
			if rows.Scan(&key, &stage) == nil {
				if Achs[key] == nil {
					continue
				}
				if stage < 0 {
					stage = 0
				}
				st.Achs[key] = stage
			}
		}
		rows.Close()
	}
}

// loadQuestsLegacy restores the pre-completed-column 3-column shape.
func loadQuestsLegacy(d Deps, st *PlayerState) {
	rows, err := d.DB.Query(`SELECT quest,stage,substage FROM quests WHERE player=?`, st.Username)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var stage, sub int
		if rows.Scan(&key, &stage, &sub) == nil {
			if Quests[key] == nil {
				continue
			}
			if stage < 0 {
				stage = 0
			}
			if sub < 0 {
				sub = 0
			}
			q := st.Quest(key)
			q.Stage, q.SubStage = stage, sub
			q.ClearCompleted()
		}
	}
}
