// Package controller owns the M13 progression slice — the 16 remaining
// in-game admin commands from commands.ts (behavior-frozen port, no change
// to the existing player/mod/admin tables in commands.go/mod.go/admin.go).
//
// Ports faithfully (handleAdminCommands gate: rank >= Admin):
//   - addexp/addexperience (self XP, silent on bad/unknown skill),
//   - setlevel (target skill level up/down, exact malformed/unknown strings),
//   - resetskills (zero self skills + sync), max (huge award per skill),
//   - attackrange (notify the admin's attack range, TS does log.info),
//   - resetregions (resend region frames to the admin),
//   - debug (CommandPacket {command:'debug'} frame),
//   - poison (toggle on an instance or self, exact TS strings),
//   - poisonarea (poison every mob+player in the admin's region),
//   - addability/setability/setquickslot/resetabilities (ability registry),
//   - openbank (grant access + target's bank Batch for the admin),
//   - setrank (rank set incl. HollowAdmin silence + offline persist path),
//   - setpet (pet grant), ipban (IP ban + same-IP drop over a username).
//
// Transport and shared state stay with the root server: skills, abilities,
// rank, pets, poison, bank frames, regions and IP bans are only touched
// through the SkillAdmin/AbilityAdmin/RankAdmin/PetAdmin/PoisonAdmin/
// MiscAdmin seams below plus the CommandConn/CommandBus/CommandPeers base
// seams, which the root adapter (m13.go) implements over its globals.
// Packet shapes are unchanged — frames are built with internal/protocol,
// the same constructors the root uses.
package controller

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"rpg-world-server/internal/protocol"
)

// CmdRankHollowAdmin is Modules.Ranks.HollowAdmin (modules.ts:327 enum
// order: None0 Moderator1 Admin2 Veteran3 Patron4 Artist5 Cheater6
// TierOne7 TierTwo8 TierThree9 TierFour10 TierFive11 TierSix12 TierSeven13
// HollowAdmin14 Booster15). Hollow admins silently skip /setrank.
const CmdRankHollowAdmin = 14

// SkillNameToID maps the capitalized skill names (commands.ts addexp/
// setlevel capitalize the first letter, then index Modules.Skills) to the
// Modules.Skills ids (modules.ts:215). Only the skills present in the TS
// Skills dictionary (skills.ts) resolve — Chiseling(12) and Smelting(14)
// are absent there, so they report invalid exactly like TS (addexp silent
// no-op, setlevel 'Invalid skill.').
var SkillNameToID = map[string]int{
	"Lumberjacking": 0,
	"Accuracy":      1,
	"Archery":       2,
	"Health":        3,
	"Magic":         4,
	"Mining":        5,
	"Strength":      6,
	"Defense":       7,
	"Fishing":       8,
	"Cooking":       9,
	"Smithing":      10,
	"Crafting":      11,
	"Fletching":     13,
	"Foraging":      15,
	"Eating":        16,
	"Loitering":     17,
	"Alchemy":       18,
}

// ProgressionSkillIDs lists every skill id the TS forEachSkill legs
// (resetskills/max) iterate, in Modules.Skills order.
var ProgressionSkillIDs = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 16, 17, 18}

// RankNameToID maps Modules.Ranks member names (modules.ts:327) to their
// values (/setrank rankText parity — case-sensitive like the TS enum
// index; unknown names are NaN there, !ok here).
var RankNameToID = map[string]int{
	"None": 0, "Moderator": 1, "Admin": 2, "Veteran": 3,
	"Patron": 4, "Artist": 5, "Cheater": 6, "TierOne": 7,
	"TierTwo": 8, "TierThree": 9, "TierFour": 10, "TierFive": 11,
	"TierSix": 12, "TierSeven": 13, "HollowAdmin": 14, "Booster": 15,
}

// MaxAwardXP is the /max per-skill award (commands.ts 'max': 696_420_969).
const MaxAwardXP = 696_420_969

// PoisonExpiry mirrors the status engine Venom default
// (PoisonDurationDefaultMs): re-poison replaces (no stacking) and natural
// expiry clears the toggle bookkeeping below.
const PoisonExpiry = 30 * time.Second

// capitalizeSkill mirrors `key.charAt(0).toUpperCase() + key.slice(1)`
// (commands.ts addexp/setlevel).
func capitalizeSkill(key string) string {
	if key == "" {
		return ""
	}
	return strings.ToUpper(key[:1]) + key[1:]
}

// parseNum scans a decimal int the way the admin table does (Sscanf
// convention); ok=false mirrors a NaN parse.
func parseNum(raw string) (v int, ok bool) {
	if n, _ := fmt.Sscanf(raw, "%d", &v); n == 1 {
		return v, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Seams (implemented by the root adapter; never by this package).
// ---------------------------------------------------------------------------

// SkillAdmin abstracts the m5 skill state + XP frame paths.
type SkillAdmin interface {
	DirtyTracker
	// SkillOf reads XP/level; ok=false when the skill was never earned
	// (callers default to XP 0 / level 1 like a fresh TS skill).
	SkillOf(c CommandConn, skill int) (xp, level int, ok bool)
	// AddSkillXP awards XP over the m5AddXP frame path.
	AddSkillXP(c CommandConn, skill, amount int)
	// SetSkillXP sets XP directly + emits the Experience Skill / Skill
	// Update frames (setExperience+addExperience parity).
	SetSkillXP(c CommandConn, skill, xp int)
	// ResetSkills zeroes every skill + syncs (resetskills parity).
	ResetSkills(c CommandConn)
	// MaxSkills zeroes then hugely awards every skill (max parity).
	MaxSkills(c CommandConn)
	// SyncSkills sends the Skill Batch + Experience Sync (skills.sync).
	SyncSkills(c CommandConn)
	// LevelsToExperience is Formulas.levelsToExperience (LevelExp table).
	LevelsToExperience(fromLevel, toLevel int) int
}

// AbilityAdmin abstracts the abilities registry grant paths.
type AbilityAdmin interface {
	DirtyTracker
	// HasAbility reports an existing unlock (setLevel/setQuickSlot
	// silent-miss parity).
	HasAbility(username, key string) bool
	// GrantAbility grants at level over the abGrantAbility seam (new
	// unlock = Add frame, re-grant = Update frame).
	GrantAbility(c CommandConn, username, key string, level int) bool
	// QuickSlotAbility stores the quick slot over the same store the
	// C->S Ability QuickSlot opcode uses.
	QuickSlotAbility(c CommandConn, username, key string, slot int)
	// ResetAbilities reloads the client ability list (reset parity).
	ResetAbilities(c CommandConn)
}

// RankAdmin abstracts player rank sets.
type RankAdmin interface {
	DirtyTracker
	// SetRank sets the live rank + Rank frame + Sync (setRank/sync).
	SetRank(target CommandConn, rank int)
	// SetRankOffline persists a rank for an offline player
	// (database.setRank parity).
	SetRankOffline(username string, rank int)
}

// PetAdmin abstracts the pet grant path.
type PetAdmin interface {
	// GrantPet grants the pet for key (petGrant seam; the duplicate-pet
	// notify lives in the grant path like player.setPet).
	GrantPet(c CommandConn, key string)
}

// PoisonAdmin abstracts poison state + region character scans.
type PoisonAdmin interface {
	// PoisonHas reports tracked poison on the instance.
	PoisonHas(instance string) bool
	// PoisonApply poisons via the status Tracker pipeline.
	PoisonApply(instance string)
	// PoisonClear cures tracked poison.
	PoisonClear(instance string)
	// EntityKind classifies an instance: "mob", "player", "other"
	// (known non-character), or "" when unknown (entities.get miss).
	EntityKind(instance string) string
	// RegionCharInstances lists live mob+player instances in the
	// admin's region (region entity iteration parity).
	RegionCharInstances(c CommandConn) []string
}

// MiscAdmin abstracts attack-range reads, the debug frame, region
// resends and IP bans.
type MiscAdmin interface {
	// AttackRange is the admin's attack range value.
	AttackRange(c CommandConn) int
	// SendDebug sends the CommandPacket {command:'debug'} frame.
	SendDebug(c CommandConn)
	// ResendRegions resends the region frames (updateRegion parity).
	ResendRegions(c CommandConn)
	// PlayerIP resolves the live conn's remote IP for a username.
	PlayerIP(username string) (ip string, ok bool)
	// BanIP records the IP ban + drops matching conns (console /ipban
	// parity — one shared implementation in the adapter).
	BanIP(ip string)
}

// ---------------------------------------------------------------------------
// Poison toggle bookkeeping (controller-side Has/Clear over the seam Apply,
// which the status engine owns — see the adapter note on early cure).
// ---------------------------------------------------------------------------

var (
	poisonMu  sync.Mutex
	poisoned  = map[string]int64{}
	poisonGen int64
)

// PoisonMarked reports tracked poison (seam Has parity for tests).
func PoisonMarked(instance string) bool {
	poisonMu.Lock()
	defer poisonMu.Unlock()
	return poisoned[instance] != 0
}

// markPoison records poison + schedules expiry bookkeeping (Venom default).
func markPoison(instance string, d PoisonAdmin) {
	poisonMu.Lock()
	poisonGen++
	gen := poisonGen
	poisoned[instance] = gen
	poisonMu.Unlock()
	time.AfterFunc(PoisonExpiry, func() {
		poisonMu.Lock()
		defer poisonMu.Unlock()
		if poisoned[instance] == gen {
			delete(poisoned, instance)
		}
	})
}

// clearPoison drops the tracked poison.
func clearPoison(instance string) {
	poisonMu.Lock()
	delete(poisoned, instance)
	poisonMu.Unlock()
}

// ---------------------------------------------------------------------------
// Progression admin commands.
// ---------------------------------------------------------------------------

// AdminProgressionCommands ports the 16 missing handleAdminCommands cases.
// Gate: rank >= Admin (same as AdminCommands).
func AdminProgressionCommands(c CommandConn, command string, blocks []string, d CommandDeps) {
	if c.Rank() < CmdRankAdmin {
		return
	}

	switch command {
	case "addexp", "addexperience": // /addexp [skill] [xp] (self, silent miss)
		key, x := "", 0
		if len(blocks) > 0 {
			key = blocks[0]
		}
		if len(blocks) > 1 {
			fmt.Sscanf(blocks[1], "%d", &x)
		}
		if key == "" || x == 0 {
			return
		}
		id, ok := SkillNameToID[capitalizeSkill(key)]
		if !ok {
			return
		}
		d.Skills.AddSkillXP(c, id, x)

	case "setlevel": // /setlevel [skill] [level] [username]
		key, x, username := "", 0, ""
		if len(blocks) > 0 {
			key = blocks[0]
		}
		if len(blocks) > 1 {
			fmt.Sscanf(blocks[1], "%d", &x)
		}
		if len(blocks) > 2 {
			username = strings.Join(blocks[2:], " ")
		}
		if username == "" || key == "" || x == 0 {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /setlevel [skill] [level] [username]")
			return
		}
		target, ok := d.Peers.ByUsername(username)
		if !ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Player %s is not online.", username))
			return
		}
		id, ok := SkillNameToID[capitalizeSkill(key)]
		if !ok {
			d.Bus.Notify(c.InstanceID(), "Invalid skill.")
			return
		}
		_, level, _ := d.Skills.SkillOf(target, id)
		if level < 1 {
			level = 1
		}
		if x < level {
			d.Skills.SetSkillXP(target, id, 0)
		} else {
			d.Skills.AddSkillXP(target, id, d.Skills.LevelsToExperience(level, x))
		}

	case "resetskills": // zero every self skill + sync
		d.Skills.ResetSkills(c)

	case "max": // huge award per self skill
		d.Skills.MaxSkills(c)

	case "attackrange": // TS does log.info — mirror as a notify
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("%d", d.Misc.AttackRange(c)))

	case "resetregions": // clear loaded regions + resend
		log.Printf("m13: Resetting regions...")
		d.Misc.ResendRegions(c)

	case "debug": // CommandPacket {command:'debug'}
		d.Misc.SendDebug(c)

	case "poison": // toggle poison on an instance, or self
		PoisonCommand(c, firstBlock(blocks), d)

	case "poisonarea": // poison every mob+player in the admin's region
		d.Bus.Notify(c.InstanceID(), "All entities in the region will be nuked with poison.")
		for _, inst := range d.Poison.RegionCharInstances(c) {
			d.Poison.PoisonApply(inst)
			markPoison(inst, d.Poison)
		}

	case "addability": // /addability key (self, grant level 1)
		key := firstBlock(blocks)
		if key == "" {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /addability key")
			return
		}
		d.Abilities.GrantAbility(c, c.PlayerName(), key, 1)

	case "setability": // /setability key level (self, owned only)
		key, x := "", 0
		if len(blocks) > 0 {
			key = blocks[0]
		}
		if len(blocks) > 1 {
			fmt.Sscanf(blocks[1], "%d", &x)
		}
		if key == "" || x == 0 {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /setability key level")
			return
		}
		if !d.Abilities.HasAbility(c.PlayerName(), key) {
			return
		}
		d.Abilities.GrantAbility(c, c.PlayerName(), key, x)

	case "setquickslot": // /setquickslot key quickslot (self)
		key := ""
		var x int
		xOk := false
		if len(blocks) > 0 {
			key = blocks[0]
		}
		if len(blocks) > 1 {
			x, xOk = parseNum(blocks[1])
		}
		if key == "" || !xOk {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /setquickslot key quickslot")
			return
		}
		if x < 0 {
			x = -1
		}
		if x > 4 {
			x = 4
		}
		d.Abilities.QuickSlotAbility(c, c.PlayerName(), key, x)

	case "resetabilities": // reload the client ability list
		d.Abilities.ResetAbilities(c)

	case "openbank": // grant access + target's bank Batch for the admin
		username := firstBlock(blocks)
		if username == "" {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /openbank username")
			return
		}
		target, ok := d.Peers.ByUsername(username)
		if !ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Could not find player: %s", username))
			return
		}
		OpenBankOf(c, target.PlayerName(), d)

	case "setrank": // /setrank rank username... (rank token first, TS order)
		if c.Rank() == CmdRankHollowAdmin {
			return
		}
		rankText, username := "", ""
		if len(blocks) > 0 {
			rankText = blocks[0]
		}
		if len(blocks) > 1 {
			username = strings.Join(blocks[1:], " ")
		}
		if username == "" || rankText == "" {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /setrank username rank")
			return
		}
		// The rank token validates on both paths: an unknown name must
		// notify Invalid rank, never persist the zero value offline
		// (TS only guards the online path — the offline NaN write is
		// the bug being fixed here).
		rank, ok := RankNameToID[rankText]
		if !ok {
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Invalid rank: %s", rankText))
			return
		}
		target, ok := d.Peers.ByUsername(username)
		if !ok {
			d.Ranks.SetRankOffline(username, rank)
			return
		}
		d.Ranks.SetRank(target, rank)

	case "setpet": // /setpet key (self)
		key := firstBlock(blocks)
		if key == "" {
			d.Bus.Notify(c.InstanceID(), "Malformed command, expected /setpet key")
			return
		}
		d.Pets.GrantPet(c, key)

	case "ipban": // /ipban username -> ban their IP + drop same-IP conns
		username := strings.ToLower(strings.Join(blocks, " "))
		if username == "" {
			log.Printf("m13: Malformed command, expected /ipban <username>")
			return
		}
		target, ok := d.Peers.ByUsername(username)
		if !ok {
			log.Printf("m13: Could not find player by name: %s.", username)
			return
		}
		ip, ok := d.Misc.PlayerIP(target.PlayerName())
		if !ok || ip == "" {
			log.Printf("m13: Could not find player by name: %s.", username)
			return
		}
		d.Misc.BanIP(ip)
		d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Player %s has been IP banned", target.PlayerName()))
	}
}

// PoisonCommand ports the 'poison' toggle: with an instance it toggles
// that entity (unknown instance notifies + returns; known non-character
// notifies but still toggles — the TS missing-return parity), without one
// it toggles the admin.
func PoisonCommand(c CommandConn, instance string, d CommandDeps) {
	if instance != "" {
		log.Printf("m13: Poisoning entity...")
		switch d.Poison.EntityKind(instance) {
		case "":
			d.Bus.Notify(c.InstanceID(), fmt.Sprintf("Could not find entity with instance: %s", instance))
			return
		case "mob", "player":
		default:
			d.Bus.Notify(c.InstanceID(), "That entity cannot be poisoned.")
		}
		if d.Poison.PoisonHas(instance) {
			d.Poison.PoisonClear(instance)
			clearPoison(instance)
			d.Bus.Notify(c.InstanceID(), "Entity has been cured of poison.")
		} else {
			d.Poison.PoisonApply(instance)
			markPoison(instance, d.Poison)
			d.Bus.Notify(c.InstanceID(), "Entity has been poisoned.")
		}
		return
	}
	log.Printf("m13: Poisoning player.")
	if d.Poison.PoisonHas(c.InstanceID()) {
		d.Poison.PoisonClear(c.InstanceID())
		clearPoison(c.InstanceID())
		d.Bus.Notify(c.InstanceID(), "Your poison has been cured!")
	} else {
		d.Poison.PoisonApply(c.InstanceID())
		markPoison(c.InstanceID(), d.Poison)
		d.Bus.Notify(c.InstanceID(), "You have been poisoned!")
	}
}

// OpenBankOf grants the admin container access and sends the target's bank
// as an NPC Bank frame (commands.ts /openbank: `new NPCPacket(
// Opcodes.NPC.Bank, player.bank.serialize())` — {slots:[...]}, no NPC
// instance context). The stock client renders this without a banker NPC:
// connection.ts handleNPC Bank is just `menu.getBank().show(info.slots!)`
// with no entity lookup, so no Container Batch fallback is needed. The slot
// shape matches bank.serialize() (clientInfo=false): index/key/count/
// enchantments only.
func OpenBankOf(c CommandConn, username string, d CommandDeps) {
	c.GrantContainerAccess()
	slots := d.Inv.BankSlots(username)
	out := make([]protocol.SlotData, 0, len(slots))
	for i, s := range slots {
		out = append(out, protocol.SlotData{
			Index: i, Key: s.Key, Count: s.Count, Enchantments: map[string]any{},
		})
	}
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketNPC, protocol.NPCBank,
		protocol.NpcPacketData{Slots: out}))
}
