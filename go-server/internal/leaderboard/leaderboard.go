// Package leaderboard is the PURE leaderboard aggregation domain, ported
// from packages/hub/src/controllers/cache.ts + api.ts (GET /leaderboards).
//
// TS contract mirrored here:
//   - 4 leaderboard categories: total XP, per-skill XP, mob kills, PvP kills.
//   - Each category returns at most 150 entries, sorted descending.
//   - Results are cached for 60 seconds (AggregateThreshold).
//   - Total XP sums all skills per player from the skills table.
//   - Per-skill XP queries a single skill from the skills table.
//   - Mob kills and PvP kills are aggregated from the statistics JSON blob.
//   - The default (total XP) response includes an availableMobs dictionary.
//
// Transport, persistence, and HTTP stay with the callers: this package
// only computes rankings from the data sources it is given.
package leaderboard

import (
	"database/sql"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// MaxEntries is the per-query result cap (TS $limit: 150).
const MaxEntries = 150

// AggregateThreshold is the cache TTL (TS AGGREGATE_THRESHOLD=60000).
const AggregateThreshold = 60 * time.Second

// ---------------------------------------------------------------------------
// Response shapes (TS leaderboards.d.ts parity).
// ---------------------------------------------------------------------------

// TotalExperience is one player's total XP across all skills.
type TotalExperience struct {
	Username        string `json:"username"`
	TotalExperience int    `json:"totalExperience"`
}

// SkillExperience is one player's XP in a single skill.
type SkillExperience struct {
	Username   string `json:"username"`
	Experience int    `json:"experience"`
}

// MobAggregate is one player's kill count for a specific mob.
type MobAggregate struct {
	Username string `json:"username"`
	Kills    int    `json:"kills"`
}

// PvpAggregate is one player's PvP kill count.
type PvpAggregate struct {
	Username string `json:"username"`
	PvPKills int    `json:"pvpKills"`
}

// ---------------------------------------------------------------------------
// Data source interfaces (caller supplies; package aggregates).
// ---------------------------------------------------------------------------

// SkillSource provides skill XP data from the persist layer.
type SkillSource interface {
	// TotalXP returns all players' total XP (sum across skills), sorted
	// descending, capped at MaxEntries.
	TotalXP() ([]TotalExperience, error)
	// SkillXP returns all players' XP for one skill, sorted descending,
	// capped at MaxEntries.
	SkillXP(skillID int) ([]SkillExperience, error)
}

// StatsSource provides statistics data (mob kills, PvP kills).
type StatsSource interface {
	// AllStats returns all players' statistics snapshots.
	AllStats() ([]StatsRow, error)
}

// StatsRow is one player's statistics blob (persist StatsBlob shape).
type StatsRow struct {
	Username string
	MobKills map[string]int
	PvPKills int
}

// ---------------------------------------------------------------------------
// Cache.
// ---------------------------------------------------------------------------

// Cache aggregates and caches leaderboard data.
type Cache struct {
	mu sync.Mutex

	skills SkillSource
	stats  StatsSource
	mobs   map[string]string // mob key -> display name (from mobs.json)

	lastTotal  time.Time
	lastSkill  map[int]time.Time
	lastMob    map[string]time.Time
	lastPvp    time.Time

	cachedTotal []TotalExperience
	cachedSkill map[int][]SkillExperience
	cachedMob    map[string][]MobAggregate
	cachedPvp   []PvpAggregate
}

// New creates a Cache backed by the given data sources. mobs is the
// available-mobs dictionary (loaded once from mobs.json at startup).
func New(skills SkillSource, stats StatsSource, mobs map[string]string) *Cache {
	return &Cache{
		skills:      skills,
		stats:       stats,
		mobs:        mobs,
		cachedSkill: make(map[int][]SkillExperience),
		cachedMob:   make(map[string][]MobAggregate),
	}
}

// AvailableMobs returns the mob key -> display name dictionary.
func (c *Cache) AvailableMobs() map[string]string {
	if c == nil || c.mobs == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(c.mobs))
	for k, v := range c.mobs {
		out[k] = v
	}
	return out
}

// GetTotalExperience returns the total XP leaderboard (cached 60s).
func (c *Cache) GetTotalExperience() ([]TotalExperience, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.lastTotal) < AggregateThreshold && c.cachedTotal != nil {
		return c.cachedTotal, nil
	}
	data, err := c.skills.TotalXP()
	if err != nil {
		return nil, err
	}
	c.cachedTotal = data
	c.lastTotal = time.Now()
	return data, nil
}

// GetSkillExperience returns the per-skill XP leaderboard (cached 60s).
func (c *Cache) GetSkillExperience(skillID int) ([]SkillExperience, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.lastSkill[skillID]; ok && time.Since(t) < AggregateThreshold {
		if data, ok := c.cachedSkill[skillID]; ok {
			return data, nil
		}
	}
	data, err := c.skills.SkillXP(skillID)
	if err != nil {
		return nil, err
	}
	c.cachedSkill[skillID] = data
	c.lastSkill[skillID] = time.Now()
	return data, nil
}

// GetMobKills returns the mob kills leaderboard for one mob key (cached 60s).
func (c *Cache) GetMobKills(mobKey string) ([]MobAggregate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.lastMob[mobKey]; ok && time.Since(t) < AggregateThreshold {
		if data, ok := c.cachedMob[mobKey]; ok {
			return data, nil
		}
	}
	rows, err := c.stats.AllStats()
	if err != nil {
		return nil, err
	}
	agg := aggregateMobKills(rows, mobKey)
	c.cachedMob[mobKey] = agg
	c.lastMob[mobKey] = time.Now()
	return agg, nil
}

// GetPvPData returns the PvP kills leaderboard (cached 60s).
func (c *Cache) GetPvPData() ([]PvpAggregate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.lastPvp) < AggregateThreshold && c.cachedPvp != nil {
		return c.cachedPvp, nil
	}
	rows, err := c.stats.AllStats()
	if err != nil {
		return nil, err
	}
	agg := aggregatePvPKills(rows)
	c.cachedPvp = agg
	c.lastPvp = time.Now()
	return agg, nil
}

// ---------------------------------------------------------------------------
// Aggregation helpers.
// ---------------------------------------------------------------------------

func aggregateMobKills(rows []StatsRow, mobKey string) []MobAggregate {
	var out []MobAggregate
	for _, r := range rows {
		kills := r.MobKills[mobKey]
		if kills > 0 {
			out = append(out, MobAggregate{Username: r.Username, Kills: kills})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kills != out[j].Kills {
			return out[i].Kills > out[j].Kills
		}
		return out[i].Username < out[j].Username
	})
	if len(out) > MaxEntries {
		out = out[:MaxEntries]
	}
	return out
}

func aggregatePvPKills(rows []StatsRow) []PvpAggregate {
	var out []PvpAggregate
	for _, r := range rows {
		if r.PvPKills > 0 {
			out = append(out, PvpAggregate{Username: r.Username, PvPKills: r.PvPKills})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PvPKills != out[j].PvPKills {
			return out[i].PvPKills > out[j].PvPKills
		}
		return out[i].Username < out[j].Username
	})
	if len(out) > MaxEntries {
		out = out[:MaxEntries]
	}
	return out
}

// ---------------------------------------------------------------------------
// SQL-backed SkillSource.
// ---------------------------------------------------------------------------

// DBSkillSource implements SkillSource using the SQLite skills table.
type DBSkillSource struct {
	DB *sql.DB
}

func (d *DBSkillSource) TotalXP() ([]TotalExperience, error) {
	// Sum XP across all skills per player, sorted descending, capped.
	rows, err := d.DB.Query(`SELECT player, SUM(xp) as total FROM skills GROUP BY player ORDER BY total DESC LIMIT ?`, MaxEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TotalExperience
	for rows.Next() {
		var t TotalExperience
		if err := rows.Scan(&t.Username, &t.TotalExperience); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if out == nil {
		out = []TotalExperience{}
	}
	return out, rows.Err()
}

func (d *DBSkillSource) SkillXP(skillID int) ([]SkillExperience, error) {
	rows, err := d.DB.Query(`SELECT player, xp FROM skills WHERE skill=? ORDER BY xp DESC LIMIT ?`, skillID, MaxEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillExperience
	for rows.Next() {
		var s SkillExperience
		if err := rows.Scan(&s.Username, &s.Experience); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if out == nil {
		out = []SkillExperience{}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// StatsSource from persist statistics table.
// ---------------------------------------------------------------------------

// DBStatsSource implements StatsSource using the SQLite statistics table.
type DBStatsSource struct {
	DB *sql.DB
}

func (d *DBStatsSource) AllStats() ([]StatsRow, error) {
	rows, err := d.DB.Query(`SELECT player, data FROM statistics`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatsRow
	for rows.Next() {
		var r StatsRow
		var data string
		if err := rows.Scan(&r.Username, &data); err != nil {
			return nil, err
		}
		var blob statsBlob
		if err := json.Unmarshal([]byte(data), &blob); err != nil {
			continue // skip corrupt rows
		}
		r.MobKills = blob.MobKills
		r.PvPKills = blob.PvPKills
		out = append(out, r)
	}
	return out, rows.Err()
}

// statsBlob is the JSON shape of the statistics data column (persist.StatsBlob
// subset — only the fields the leaderboard needs).
type statsBlob struct {
	MobKills map[string]int `json:"mobKills,omitempty"`
	PvPKills int            `json:"pvpKills,omitempty"`
}
