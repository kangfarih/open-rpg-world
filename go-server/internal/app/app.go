// Package app is the E9b boot/env seam for the root server: environment +
// flag parsing (Config), the frozen boot order, and the thin-runner entry
// point shared by the canonical root binary (package main, `go run .`) and
// the cmd/server shim.
//
// The full boot sequence itself stays in package main for E9b: the boot
// calls m5Init/m6StartStoreTicker/m11/m13/ab/soc/world/m8/m10/m9 entry
// points that live in behavior-frozen root files, and this package cannot
// import them (import cycle). main.go therefore resolves its Config from
// here (addr + TESTMAP/CLEAN/COMBAT + DUMMY_* parsing) while executing the
// unchanged boot order documented in BootOrder.
package app

import (
	"log"
	"strconv"
	"strings"
	"time"

	"rpg-world-server/internal/world"
)

// Defaults frozen from main.go.
const (
	DefaultAddr   = "127.0.0.1:9001"
	DefaultDummy  = 5000
	DefaultDBPath = "" // m5: DB_PATH or embedded default
)

// Config is the server boot configuration resolved from env + CLI flags.
type Config struct {
	// Modes selects the map/entity overlay set (TESTMAP/CLEAN/COMBAT).
	Modes world.Modes
	// Role selects the instance role (ROLE env / --role flag): all-in-one
	// (default, today's behavior), router (hub server-list + login
	// routing, no sim), shard (game + hub Client registration).
	Role string
	// Port is the raw PORT env ("" = client-server default 9001).
	Port string
	// Addr is the resolved listen address (ListenAddr(Port)).
	Addr string
	// DBPath is DB_PATH ("" = embedded default in m5).
	DBPath string
	// ConsoleOff disables the stdin console (CONSOLE=0/false/off/no).
	ConsoleOff bool
	// DummyHP is the BossDummy max HP (DUMMY_HP, default 5000).
	DummyHP int
	// DummyRespawn is the boss respawn delay (DUMMY_RESPAWN seconds,
	// default 15s).
	DummyRespawn time.Duration
}

// ListenAddr resolves the listen address: PORT env or the client-server
// default 9001 (main.go addr verbatim). The result is always loopback-bound
// (127.0.0.1:<port>); invalid ports (non-numeric, out of 1-65535 range,
// or already containing a colon/host) fall back to DefaultAddr so boot
// never listens on an unintended interface or fails on a typo.
func ListenAddr(port string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return DefaultAddr
	}
	// Explicit addrs (already host:port) are rejected here: PORT is a bare
	// port knob only; use HUB_LISTEN/API_PORT for explicit binds.
	if strings.Contains(port, ":") {
		return DefaultAddr
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return DefaultAddr
	}
	return "127.0.0.1:" + strconv.Itoa(n)
}

// ParseDummyHP parses DUMMY_HP (BossDummy HP override; default 5000).
// Non-numeric or non-positive values fall back to the default.
func ParseDummyHP(raw string) int {
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return DefaultDummy
}

// ParseDummyRespawn parses DUMMY_RESPAWN seconds (default 15s).
func ParseDummyRespawn(raw string) time.Duration {
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return world.DummyRespawnAt
}

// ConsoleDisabled mirrors the ops_wire.go console gate: CONSOLE=0 or
// piped stdin disables the TTY console.
func ConsoleDisabled(raw string) bool {
	return raw == "0" || raw == "false" || raw == "off" || raw == "no"
}

// FromEnv resolves the boot Config from env + CLI args (main.go boot-env
// reads verbatim; lazy per-swing M4_* reads stay at their call sites).
func FromEnv(getenv func(string) string, args []string) Config {
	port := getenv("PORT")
	return Config{
		Modes:        world.ParseModes(getenv, args),
		Role:         ParseRole(getenv, args),
		Port:         port,
		Addr:         ListenAddr(port),
		DBPath:       getenv("DB_PATH"),
		ConsoleOff:   ConsoleDisabled(getenv("CONSOLE")),
		DummyHP:      ParseDummyHP(getenv("DUMMY_HP")),
		DummyRespawn: ParseDummyRespawn(getenv("DUMMY_RESPAWN")),
	}
}

// BootOrder documents the frozen boot sequence executed by main() — every
// step keeps its relative order and cadence (E9b: identical).
func BootOrder() []string {
	return []string{
		"m5Init (persist open + 10s dirty flush)",
		"m6StartStoreTicker (stores registry + 20s stock refresh)",
		"m11EnsureTables + m13EnsureTables + abEnsureTables + socEnsureTables",
		"socLoadGuilds + worldBoot (warps/globals/event scheduler)",
		"m8LoadGames (minigame areas + 1s tick engines)",
		"m10LoadAreas + m10InjectTestAreas",
		"m9Engine (mob AI, 500ms tick)",
		"startTickLoop (central 20Hz flush + subsystem ticks)",
		"initEntities (static registry seed)",
		"startShowcase (5s anim loop; skipped in CLEAN/COMBAT)",
		"startCombat (party brain; COMBAT only)",
		"opsAccept HTTP handler + opsStartAPI + opsStartConsole + ListenAndServe",
	}
}

// LogConfig logs the effective boot config (one line, boot only).
func LogConfig(cfg Config) {
	log.Printf("app: addr=%s db=%q consoleOff=%v test=%v clean=%v combat=%v dummyHP=%d dummyRespawn=%v",
		cfg.Addr, cfg.DBPath, cfg.ConsoleOff,
		cfg.Modes.Test, cfg.Modes.Clean, cfg.Modes.Combat,
		cfg.DummyHP, cfg.DummyRespawn)
}
