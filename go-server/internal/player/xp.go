// XP and skill awards, extracted behavior-frozen from the root m5.go
// adapter (task D2b item 1).
//
// TS sources (see the root adapter for the full tour):
// player.handleExperience (2 XP per damage, Health 1/4 + school share),
// resourceskill (table experience on exhaust), the RuneScape LevelExp port
// (loader.ts loadLevels) for thresholds, and the level-up broadcast (Sync +
// Experience Skill, plus the Healing FX as the heal anim — no Heal packet).
//
// The in-memory player state (m5State/pstates/pstateMu) stays with the root
// adapter: this package never touches it directly. All state mutation flows
// through Deps.ApplyAward (which the root implements over m5StateFor +
// pstateMu with identical locking), and all transport through Deps.Send /
// Deps.Broadcast. XP math (ExpToLevel/NextExp/Percentage) rides
// internal/meta with the same ModulesMaxLevel table. Frames are built with
// internal/protocol (the same constructors the root pkt/pktOp shims wrap),
// so wire bytes are identical. The "m5:" log prefix is kept verbatim.
package player

import (
	"fmt"
	"log"

	"rpg-world-server/internal/meta"
	"rpg-world-server/internal/protocol"
)

func xpIntp(v int) *int           { return &v }
func xpBoolp(v bool) *bool        { return &v }
func xpFloatp(v float64) *float64 { return &v }

// Skill ids mirror Modules.Skills order (modules.ts:215-235 enum order:
// Lumberjacking0 Accuracy1 Archery2 Health3 Magic4 Mining5 Strength6
// Defense7 Fishing8 Cooking9 Smithing10 Crafting11 Chiseling12 Fletching13
// Smelting14 Foraging15 Eating16 Loitering17 Alchemy18).
const (
	SkillLumberjacking = 0
	SkillAccuracy      = 1
	SkillArchery       = 2
	SkillHealth        = 3
	SkillMagic         = 4
	SkillMining        = 5
	SkillStrength      = 6
	SkillDefense       = 7
	SkillFishing       = 8
	SkillCooking       = 9
	SkillSmithing      = 10
	SkillCrafting      = 11
	SkillChiseling     = 12
	SkillFletching     = 13
	SkillSmelting      = 14
	SkillForaging      = 15
	SkillEating        = 16
	SkillLoitering     = 17
	SkillAlchemy       = 18
)

// Attack style ids mirror Modules.AttackStyle (modules.ts:158-179).
// Only the melee styles route XP; the archery/magic styles (Accurate,
// Fast, Focused, LongRange) never reach the style switch live because
// archer/magic weapons early-return via the class flags first (exactly
// as in TS) — should one ever arrive, it falls to the default branch.
const (
	StyleNone      = 0
	StyleStab      = 1
	StyleSlash     = 2
	StyleDefensive = 3
	StyleCrush     = 4
	StyleShared    = 5
	StyleHack      = 6
	StyleChop      = 7
	StyleAccurate  = 8
	StyleFast      = 9
	StyleFocused   = 10
	StyleLongRange = 11
)

// SkillName names a skill id for logs (m5SkillName verbatim).
func SkillName(id int) string {
	switch id {
	case SkillLumberjacking:
		return "Lumberjacking"
	case SkillAccuracy:
		return "Accuracy"
	case SkillArchery:
		return "Archery"
	case SkillHealth:
		return "Health"
	case SkillMagic:
		return "Magic"
	case SkillMining:
		return "Mining"
	case SkillStrength:
		return "Strength"
	case SkillDefense:
		return "Defense"
	case SkillFishing:
		return "Fishing"
	case SkillCooking:
		return "Cooking"
	case SkillSmithing:
		return "Smithing"
	case SkillCrafting:
		return "Crafting"
	case SkillChiseling:
		return "Chiseling"
	case SkillFletching:
		return "Fletching"
	case SkillSmelting:
		return "Smelting"
	case SkillForaging:
		return "Foraging"
	case SkillEating:
		return "Eating"
	case SkillLoitering:
		return "Loitering"
	case SkillAlchemy:
		return "Alchemy"
	}
	return fmt.Sprintf("Skill%d", id)
}

// CombatSkill reports whether a skill id contributes to combat level
// (m5CombatSkill verbatim).
func CombatSkill(id int) bool {
	switch id {
	case SkillAccuracy, SkillArchery, SkillHealth, SkillMagic, SkillStrength, SkillDefense:
		return true
	}
	return false
}

// CombatLevel computes the combat level from a skill snapshot
// (m5CombatLevelLocked verbatim: 1 + sum of (level-1) over combat skills
// above 1, floored at 1). levels maps skill id -> level.
// VERIFY (level<1 dead code): the floor is unreachable in practice (level
// starts at 1 and only grows) but kept as a defensive guard — TS
// getCombatLevel has no floor only because skill levels can never drop below
// 1 there either. No behavior change.
func CombatLevel(levels map[int]int) int {
	level := 1
	for id, lv := range levels {
		if CombatSkill(id) && lv > 1 {
			level += lv - 1
		}
	}
	if level < 1 {
		level = 1
	}
	return level
}

// ---------------------------------------------------------------------------
// XP formula (formulas.ts LevelExp + loader.ts loadLevels, RuneScape curve).
// ---------------------------------------------------------------------------

// ExpToLevel maps XP to a level (m5 expToLevel verbatim via internal/meta).
func ExpToLevel(xp int) int {
	return meta.ExpToLevel(meta.BuildLevelExp(meta.MaxLevel), meta.MaxLevel, xp)
}

// NextExp returns the next threshold strictly above xp (m5 nextExp verbatim).
func NextExp(xp int) int {
	return meta.NextExp(meta.BuildLevelExp(meta.MaxLevel), xp)
}

// Percentage ports the m5 XP-bar fraction: (xp-px)/(nx-px) between the
// surrounding thresholds, 1 when maxed or degenerate, clamped at 0 below.
func Percentage(xp int) float64 {
	nx := NextExp(xp)
	if nx < 0 {
		return 1
	}
	tbl := meta.BuildLevelExp(meta.MaxLevel)
	px := 0
	for i := meta.MaxLevel - 1; i > 0; i-- {
		if i < len(tbl) && xp >= tbl[i] {
			px = tbl[i]
			break
		}
	}
	if nx <= px {
		return 1
	}
	p := float64(xp-px) / float64(nx-px)
	if p < 0 {
		return 0
	}
	return p
}

// ---------------------------------------------------------------------------
// Award orchestration (m5AddXP / m5AwardCombatXP / m5GatherXP verbatim,
// behind the Deps seam).
// ---------------------------------------------------------------------------

// Conn is the minimal per-award connection view (nil = no frames, e.g. the
// offline path — m5AddXP's `if c != nil` gates verbatim). Send delivers
// frames to this conn (gnet.Send parity, bound by the root adapter so
// delivery is direct, never re-resolved).
type Conn struct {
	Instance string
	Username string
	Send     func(frames ...[]any)
}

// AwardResult is the applied award (ApplyAward output).
type AwardResult struct {
	Prev        int // level before the award
	Level       int // level after
	XP          int // xp after
	CombatLevel int // total combat level after
	X, Y        int // authoritative tile after
}

// Deps bundles the XP seams for one call (implemented by the root adapter).
type Deps struct {
	// ApplyAward atomically awards amount XP to skill for key (creating the
	// skill at 1 when missing, recomputing combat level for combat skills)
	// and returns the applied result. Zero-amount awards are rejected by the
	// caller before this runs (m5AddXP early return verbatim); negative
	// amounts subtract with XP clamped at 0 (TS addexp subtraction parity).
	ApplyAward func(key string, skill, amount int) AwardResult
	// Broadcast fans frames out (worldcore.Broadcast parity).
	Broadcast func(frames ...[]any)
	// MarkDirty flags the player row for the 10s persist flush.
	MarkDirty func(key string)
	// SyncFrame builds the level-up Sync broadcast payload for the
	// instance at (x, y) with the combat level (welcomePlayer parity).
	SyncFrame func(instance string, x, y, combatLevel int) []any
	// Lookup resolves an attacker instance to its conn view, Send bound
	// (m5GatherXP's worldcore.Find parity). ok=false = unknown attacker,
	// award skipped.
	Lookup func(instance string) (Conn, bool)
	// XPBoost reports the 1.5x experience event (worldXPBoost parity).
	XPBoost func() bool
	// Style reports the attacker's CURRENT attack style (the attack-style
	// store: last explicit switch, else the equipped weapon's first
	// style — player.handleExperience reads weapon.attackStyle live).
	// Nil = StyleNone, which takes the default (Strength) branch and
	// preserves the pre-parity melee behavior.
	Style func() int
	// HasMana reports hasManaForAttack (player.ts:1700 — current mana >=
	// the equipped weapon's manaCost, 0 for non-magic weapons so always
	// true). False halves the award. Nil = true (no halving, legacy).
	HasMana func() bool
}

// AddXP awards skill XP, emitting Experience Skill + Skill Update, and on
// level-up a Sync broadcast + Healing FX heal anim. Returns new level
// (m5AddXP verbatim; amount == 0 returns 1 with no state touched).
// Negative amounts mirror the TS addexp subtraction (commands.ts addexp
// passes any non-zero x to skill.addExperience, which does
// setExperience(experience + x)): XP is reduced, clamped at 0 (TS
// expToLevel goes to -1 below 0; the Go state floors level at 1), with the
// same frames as the positive path (a level change fans out Sync either
// way). Only /addexp can supply negatives — combat/gather/store/quest
// awards are guarded positive at their call sites.
func AddXP(d Deps, c *Conn, key string, skill, amount int) int {
	if amount == 0 {
		return 1
	}
	res := d.ApplyAward(key, skill, amount)
	// Clamp foreign-seam negative XP for frame building (the prod ApplyAward
	// floors stored XP at 0 per the Deps contract; a test/custom seam that
	// returns negative XP would otherwise emit malformed SkillUpdate frames
	// with negative experience/percentage/nextExperience).
	frameXP := res.XP
	if frameXP < 0 {
		frameXP = 0
	}
	if c != nil && c.Send != nil {
		c.Send(protocol.PktOp(protocol.PacketExperience, protocol.ExperienceSkill, protocol.ExperienceData{
			Instance: c.Instance, Amount: xpIntp(amount), Skill: xpIntp(skill),
		}))
		c.Send(protocol.PktOp(protocol.PacketSkill, protocol.SkillUpdate, protocol.SkillData{
			Type: skill, Experience: frameXP, Level: xpIntp(res.Level),
			Percentage: xpFloatp(Percentage(frameXP)), NextExperience: xpIntp(NextExp(frameXP)),
			Combat: xpBoolp(CombatSkill(skill)),
		}))
	}
	if res.Level != res.Prev {
		log.Printf("m5: %s %s leveled %d -> %d (xp=%d)", key, SkillName(skill), res.Prev, res.Level, res.XP)
		if c != nil {
			d.Broadcast(d.SyncFrame(c.Instance, res.X, res.Y, res.CombatLevel))
			d.Broadcast(protocol.PktOp(protocol.PacketEffect, protocol.EffectAdd, protocol.EffectData{Instance: c.Instance, Effect: protocol.EffectHealing}))
			log.Printf("m5: %s level-up heal anim (Healing FX only, no Heal packet)", key)
		}
		d.MarkDirty(key)
	}
	return res.Level
}

// AwardCombatXP ports player.handleExperience (player.ts:990-1107)
// TS-exact: damage < 1 no-op; experience = damage * EXPERIENCE_PER_HIT
// (2, via getExperiencePerHit which already carries the 1.5x event);
// low-mana halving (Math.floor(experience/2) when !hasManaForAttack);
// Health ceil(xp/4); then class routing (archer -> Archery, mage ->
// Magic, both ceil(xp*0.75)) ahead of the weapon attackStyle switch
// (Stab -> Accuracy, Slash -> Strength, Defensive -> Defense, all
// ceil(xp*0.75); Crush -> Accuracy+Strength ceil(xp*0.375) each;
// Shared -> Accuracy+Strength+Defense ceil(xp*0.25) each; Hack ->
// Strength+Defense ceil(xp*0.375) each; Chop -> Accuracy+Defense
// floor(xp*0.375) each — note floor, not ceil; default/unarmed ->
// Strength ceil(xp*0.75)). The 0.375/0.75/0.25 factors are exactly
// representable in binary, so the integer forms ((3*xp+7)/8,
// (3*xp+3)/4, (xp+3)/4, (3*xp)/8) match Math.ceil/floor bit-for-bit.
//
// The archer/mage params carry the weapon class (weapon.isArcher /
// isMagic — wired by the server adapter from the equipped weapon) and
// keep precedence over Style exactly as in TS.
//
// The `false` withInfo arg TS passes to every addExperience call is
// intentionally NOT mirrored: withInfo=false only suppresses the
// Experience Skill popup packet (skills.ts:180 — the Skill Update +
// level-up popup/Sync still send), while Go AddXP has no such param
// and always emits Experience Skill + Skill Update. Suppressing the
// popup here would change live packet flow (the combat e2e counts
// Experience Skill frames), so the divergence is documented, not
// ported — no packet-shape changes.
func AwardCombatXP(d Deps, c *Conn, key string, damage int, archer, mage bool) {
	if damage < 1 {
		return
	}
	xp := damage * 2 // Modules.Constants.EXPERIENCE_PER_HIT.
	if d.XPBoost != nil && d.XPBoost() {
		xp = xp * 3 / 2 // world: 1.5x experience event (experiencePerHit parity)
	}
	if d.HasMana != nil && !d.HasMana() {
		xp /= 2 // Math.floor(experience / 2) — integer division floors.
	}
	AddXP(d, c, key, SkillHealth, (xp+3)/4) // Math.ceil(experience / 4).
	switch {
	case archer:
		AddXP(d, c, key, SkillArchery, (xp*3+3)/4)
	case mage:
		AddXP(d, c, key, SkillMagic, (xp*3+3)/4)
	default:
		style := StyleNone
		if d.Style != nil {
			style = d.Style()
		}
		switch style {
		case StyleStab:
			AddXP(d, c, key, SkillAccuracy, (xp*3+3)/4)
		case StyleSlash:
			AddXP(d, c, key, SkillStrength, (xp*3+3)/4)
		case StyleDefensive:
			AddXP(d, c, key, SkillDefense, (xp*3+3)/4)
		case StyleCrush:
			AddXP(d, c, key, SkillAccuracy, (xp*3+7)/8)
			AddXP(d, c, key, SkillStrength, (xp*3+7)/8)
		case StyleShared:
			AddXP(d, c, key, SkillAccuracy, (xp+3)/4)
			AddXP(d, c, key, SkillStrength, (xp+3)/4)
			AddXP(d, c, key, SkillDefense, (xp+3)/4)
		case StyleHack:
			AddXP(d, c, key, SkillStrength, (xp*3+7)/8)
			AddXP(d, c, key, SkillDefense, (xp*3+7)/8)
		case StyleChop:
			AddXP(d, c, key, SkillAccuracy, (xp*3)/8)
			AddXP(d, c, key, SkillDefense, (xp*3)/8)
		default:
			AddXP(d, c, key, SkillStrength, (xp*3+3)/4)
		}
	}
}

// GatherXP is the M4-hook successor: table experience on exhaust. Unknown
// attackers are skipped (m5GatherXP verbatim). Unknown skill names return
// early instead of defaulting to Lumberjacking(0) — the old zero-value map
// hit silently credited the wrong skill.
func GatherXP(d Deps, attackerInstance, skill string, xp int) {
	c, ok := d.Lookup(attackerInstance)
	if !ok {
		return
	}
	id, found := map[string]int{
		"lumberjacking": SkillLumberjacking, "mining": SkillMining,
		"fishing": SkillFishing, "foraging": SkillForaging,
	}[skill]
	if !found {
		return
	}
	cc := c
	AddXP(d, &cc, c.Username, id, xp)
}
