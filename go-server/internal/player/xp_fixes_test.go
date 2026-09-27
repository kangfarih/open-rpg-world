package player

import (
	"encoding/json"
	"testing"

	"rpg-world-server/internal/protocol"
)

// TestGatherXPUnknownSkill pins the early return: unknown gather skills must
// not default to Lumberjacking(0).
func TestGatherXPUnknownSkill(t *testing.T) {
	st := newXPState()
	d := testXPDeps(st)
	called := false
	d.Lookup = func(instance string) (Conn, bool) {
		return Conn{Instance: "i1", Username: "hero"}, true
	}
	origApply := d.ApplyAward
	d.ApplyAward = func(key string, skill, amount int) AwardResult {
		called = true
		return origApply(key, skill, amount)
	}
	GatherXP(d, "i1", "nosuchskill", 100)
	if called {
		t.Fatal("unknown gather skill must not award XP")
	}
	if len(st.xp) != 0 {
		t.Fatalf("unknown gather skill touched %v, want none", st.xp)
	}
	// Known skills still award.
	GatherXP(d, "i1", "mining", 100)
	if st.xp[SkillMining] != 100 {
		t.Fatalf("mining xp = %v, want 100", st.xp)
	}
}

// TestAddXPNegativeClamp pins frame sanitizing: a seam returning negative XP
// must still emit non-negative SkillUpdate experience/percentage.
func TestAddXPNegativeClamp(t *testing.T) {
	st := newXPState()
	d := testXPDeps(st)
	d.ApplyAward = func(key string, skill, amount int) AwardResult {
		return AwardResult{Prev: 1, Level: 1, XP: -50, X: 100, Y: 96}
	}
	c := &Conn{Instance: "i1", Username: "hero", Send: func(frames ...[]any) {
		st.sent = append(st.sent, frames...)
	}}
	AddXP(d, c, "hero", SkillFishing, -10)
	if len(st.sent) != 2 {
		t.Fatalf("sent frames = %d, want 2", len(st.sent))
	}
	raw, _ := json.Marshal(st.sent[1][2])
	var sd struct {
		Experience     int      `json:"experience"`
		Percentage     *float64 `json:"percentage"`
		NextExperience *int     `json:"nextExperience"`
	}
	if err := json.Unmarshal(raw, &sd); err != nil {
		t.Fatalf("skill payload: %v", err)
	}
	if sd.Experience < 0 {
		t.Fatalf("skill experience = %d, want clamped >= 0", sd.Experience)
	}
	if sd.Percentage != nil && *sd.Percentage < 0 {
		t.Fatalf("percentage = %v, want >= 0", *sd.Percentage)
	}
	_ = protocol.SkillUpdate // pin import
}
