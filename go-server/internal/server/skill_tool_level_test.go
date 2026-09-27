// Package server tests for real skill/tool level lookups (replacing stubs).
package server

import (
	"os"
	"testing"

	"github.com/gorilla/websocket"

	"rpg-world-server/internal/controller"
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/player"
	worldcore "rpg-world-server/internal/world"
)

// TestToolTierFromItemsJSON verifies that ToolTier reads the gathering tool
// tiers from items.json (lumberjacking/mining/fishing fields).
func TestToolTierFromItemsJSON(t *testing.T) {
	// Bronze axe: lumberjacking 1 (items.json line ~793)
	if got := controller.ToolTier("bronzeaxe", "lumberjacking"); got != 1 {
		t.Fatalf("bronzeaxe lumberjacking = %d, want 1", got)
	}
	// Bronze pickaxe: mining 1 (items.json line ~572)
	if got := controller.ToolTier("bronzepickaxe", "mining"); got != 1 {
		t.Fatalf("bronzepickaxe mining = %d, want 1", got)
	}
	// Fishing pole: fishing 1 (items.json line ~2053)
	if got := controller.ToolTier("fishingpole", "fishing"); got != 1 {
		t.Fatalf("fishingpole fishing = %d, want 1", got)
	}
	// Cobalt axe: lumberjacking 2 (items.json)
	if got := controller.ToolTier("cobaltaxe", "lumberjacking"); got != 2 {
		t.Fatalf("cobaltaxe lumberjacking = %d, want 2", got)
	}
	// Sword: not a gathering tool (no lumberjacking/mining/fishing field)
	if got := controller.ToolTier("ironsword", "lumberjacking"); got != 0 {
		t.Fatalf("ironsword lumberjacking = %d, want 0 (not a tool)", got)
	}
	// Unknown item
	if got := controller.ToolTier("nonexistent", "mining"); got != 0 {
		t.Fatalf("nonexistent mining = %d, want 0", got)
	}
	// Unknown skill
	if got := controller.ToolTier("bronzeaxe", "foraging"); got != 0 {
		t.Fatalf("bronzeaxe foraging = %d, want 0 (foraging needs no tool)", got)
	}
}

// makeTestPlayer creates a playerConn with the given instance and username,
// registers it in worldcore, and returns the conn and its registry key.
func makeTestPlayer(t *testing.T, inst, username string) (*playerConn, *websocket.Conn) {
	t.Helper()
	c := &playerConn{Conn: gnet.NewConn(nil, inst)}
	c.Username = username
	key := &websocket.Conn{}
	worldcore.AddPlayer(key, c)
	t.Cleanup(func() { worldcore.Default.RemoveWS(key) })
	return c, key
}

// TestPlayerSkillLevelReal verifies that playerSkillLevel reads from the
// player's actual skill state, not a stub.
func TestPlayerSkillLevelReal(t *testing.T) {
	username := "test-skill-hero"
	inst := "test-skill-inst"

	// Create player state with skills.
	st := playerStateFor(username)
	pstateMu.Lock()
	st.Skills[player.SkillLumberjacking] = &skillDef{Level: 15, XP: 2500}
	st.Skills[player.SkillMining] = &skillDef{Level: 8, XP: 800}
	// Fishing untrained (no entry)
	pstateMu.Unlock()

	// Register a mock player connection.
	makeTestPlayer(t, inst, username)

	// Test: lumberjacking level 15
	if got := playerSkillLevel(inst, "lumberjacking"); got != 15 {
		t.Fatalf("lumberjacking level = %d, want 15", got)
	}
	// Test: mining level 8
	if got := playerSkillLevel(inst, "mining"); got != 8 {
		t.Fatalf("mining level = %d, want 8", got)
	}
	// Test: fishing untrained -> default 1
	if got := playerSkillLevel(inst, "fishing"); got != 1 {
		t.Fatalf("fishing level = %d, want 1 (untrained)", got)
	}
	// Test: foraging untrained -> default 1
	if got := playerSkillLevel(inst, "foraging"); got != 1 {
		t.Fatalf("foraging level = %d, want 1 (untrained)", got)
	}
	// Test: unknown skill -> default 1
	if got := playerSkillLevel(inst, "unknown"); got != 1 {
		t.Fatalf("unknown skill level = %d, want 1", got)
	}
	// Test: offline player -> default 1
	if got := playerSkillLevel("offline-inst", "lumberjacking"); got != 1 {
		t.Fatalf("offline player level = %d, want 1", got)
	}
}

// TestPlayerSkillLevelEnvOverride verifies that env overrides still work.
func TestPlayerSkillLevelEnvOverride(t *testing.T) {
	// Set env override.
	os.Setenv("M4_SKILL_MINING", "42")
	defer os.Unsetenv("M4_SKILL_MINING")

	// Env should win over real player state.
	if got := playerSkillLevel("any-inst", "mining"); got != 42 {
		t.Fatalf("mining level with env = %d, want 42", got)
	}
}

// TestPlayerToolLevelReal verifies that playerToolLevel reads the equipped
// weapon's tool tier from items.json.
func TestPlayerToolLevelReal(t *testing.T) {
	username := "test-tool-hero"
	inst := "test-tool-inst"

	// Create player state with a bronze axe equipped.
	st := playerStateFor(username)
	pstateMu.Lock()
	// Ensure Equip slice is long enough.
	for len(st.Equip) <= EquipmentWeapon {
		st.Equip = append(st.Equip, slotDef{})
	}
	st.Equip[EquipmentWeapon] = slotDef{Key: "bronzeaxe", Count: 1}
	pstateMu.Unlock()

	// Register a mock player connection.
	makeTestPlayer(t, inst, username)

	// Test: bronze axe -> lumberjacking 1
	if got := playerToolLevel(inst, "lumberjacking"); got != 1 {
		t.Fatalf("bronzeaxe lumberjacking tier = %d, want 1", got)
	}
	// Test: bronze axe is not a mining tool -> 0
	if got := playerToolLevel(inst, "mining"); got != 0 {
		t.Fatalf("bronzeaxe mining tier = %d, want 0", got)
	}
}

// TestPlayerToolLevelNoWeapon verifies that missing weapon returns 0.
func TestPlayerToolLevelNoWeapon(t *testing.T) {
	username := "test-noweapon-hero"
	inst := "test-noweapon-inst"

	// Create player state with no weapon equipped.
	st := playerStateFor(username)
	pstateMu.Lock()
	for len(st.Equip) <= EquipmentWeapon {
		st.Equip = append(st.Equip, slotDef{})
	}
	st.Equip[EquipmentWeapon] = slotDef{} // empty slot
	pstateMu.Unlock()

	makeTestPlayer(t, inst, username)

	// No weapon -> tool level 0 (denied with INVALID_WEAPON)
	if got := playerToolLevel(inst, "lumberjacking"); got != 0 {
		t.Fatalf("no weapon lumberjacking tier = %d, want 0", got)
	}
}

// TestPlayerToolLevelEnvOverride verifies that env overrides still work.
func TestPlayerToolLevelEnvOverride(t *testing.T) {
	os.Setenv("M4_TOOL_MINING", "99")
	defer os.Unsetenv("M4_TOOL_MINING")

	if got := playerToolLevel("any-inst", "mining"); got != 99 {
		t.Fatalf("mining tool level with env = %d, want 99", got)
	}
}
