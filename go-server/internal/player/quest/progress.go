package quest

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"

	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// Quest progression core (quest.ts setStage/progress).
// ---------------------------------------------------------------------------

// SetStage applies the new stage and emits Progress/pointer/popup side
// effects. progress=false mirrors setStage(..., false) for DB loads.
func SetStage(c Conn, d Deps, st *PlayerState, key string, stage, subStage int, progress bool) {
	def := Quests[key]
	if def == nil {
		return
	}
	q := st.Quest(key)
	isProgress := q.Stage != stage

	// Popup of the stage we are LEAVING fires before the new stage is set.
	// Colour rides the popup data (PopupData parity) with the legacy green
	// default when the JSON omits it (all shipped quests pin #33cc33).
	if progress && isProgress {
		if p := StageDef(def, q.Stage).Popup; p != nil {
			colour := p.Colour
			if colour == "" {
				colour = "#33cc33"
			}
			SendPopup(c, d, p.Title, p.Text, colour)
		}
		// Clear the pointer preemptively (quest.ts setStage).
		SendPointer(c, d, nil)
	}

	q.Stage, q.SubStage = stage, subStage
	if isProgress {
		// Stage change clears the substage set (quest.ts setStage parity).
		q.ClearCompleted()
	}
	if !progress {
		return
	}
	// Dirty on ANY progress-flavored mutation — stage moves AND substage
	// ticks (the old `!isProgress → return` dropped substage persistence,
	// stranding royalpet-style progress in memory only).
	d.Store.MarkDirty(st.Username)
	if !isProgress {
		// Substage-only: state + dirty advance, no Progress/pointer frames
		// (quest.ts setStage only fires progressCallback on isProgress).
		return
	}
	if c != nil {
		SendQuestProgress(c, d, key, q)
	}

	// Completion: Node sends no extra frame here (client colours the list);
	// drop the position-tracking state for parity hooks that read it.
	if q.Stage >= def.StageCount {
		log.Printf("m11: %s finished quest %s", st.Username, key)
		return
	}

	// Entering a new stage: fire its pointer (quest.ts setStage).
	if c != nil {
		if p := StageDef(def, q.Stage).Pointer; p != nil {
			SendPointer(c, d, p)
		}
	}
}

// GiveRewards grants stage itemRewards (givePlayerRewards): PLEASE_MAKE_ROOM
// notify when the inventory cannot fit every entry, else add each item and
// emit Container Add.
// VERIFY (NO_SPACE vs PLEASE_MAKE_ROOM): TS splits the strings —
// handleItemRequirement space check uses misc:NO_SPACE (quest.ts:261) while
// givePlayerRewards uses misc:PLEASE_MAKE_ROOM_REWARD (quest.ts:333). The old
// NO_SPACE here was the wrong leg; fixed.
func GiveRewards(c Conn, d Deps, st *PlayerState, rewards []Item) bool {
	if len(rewards) == 0 {
		return false
	}
	free := protocol.ModulesInventorySize - d.Store.InventoryLen(st.Username)
	if free < len(rewards) {
		if c != nil {
			d.Bus.Notify(c.InstanceID(), "misc:PLEASE_MAKE_ROOM_REWARD")
		}
		return false
	}
	for _, it := range rewards {
		idx := d.Store.AddItem(st.Username, it.Key, it.Count)
		if c != nil {
			d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketContainer, protocol.ContainerAdd, protocol.ContainerData{
				Type: protocol.ContainerTypeInventory,
				Slot: &protocol.SlotData{Index: idx, Key: it.Key, Count: it.Count, Enchantments: map[string]any{}},
			}))
		}
		d.Store.MarkDirty(st.Username)
	}
	return true
}

// skillByName maps quest skill names to skill ids (givePlayerExperience +
// hasRequirements parity). Full Modules.Skills parity via a case-insensitive
// enum-name match (utils.getSkill: Capitalize(key) → Modules.Skills[key]) —
// the old 10-entry subset locked codersglitch (accuracy/strength/defense),
// codersglitch2 and codersfallacy (alchemy/smithing) behind an
// always-false requirements gate.
func skillByName(name string) (int, bool) {
	id, found := map[string]int{
		"lumberjacking": SkillLumberjacking, "accuracy": SkillAccuracy,
		"archery": SkillArchery, "health": SkillHealth, "magic": SkillMagic,
		"mining": SkillMining, "strength": SkillStrength,
		"defense": SkillDefense, "fishing": SkillFishing,
		"cooking": SkillCooking, "smithing": SkillSmithing,
		"crafting": SkillCrafting, "chiseling": SkillChiseling,
		"fletching": SkillFletching, "smelting": SkillSmelting,
		"foraging": SkillForaging, "eating": SkillEating,
		"loitering": SkillLoitering, "alchemy": SkillAlchemy,
	}[strings.ToLower(name)]
	return id, found
}

// GrantExperience ports givePlayerExperience: skillRewards by name.
func GrantExperience(c Conn, d Deps, st *PlayerState, rewards []SkillReward) {
	for _, r := range rewards {
		id, found := skillByName(r.Key)
		if !found {
			log.Printf("m11: unknown skill reward %q (quest stage)", r.Key)
			continue
		}
		if c != nil {
			d.Store.AddXP(c, st.Username, id, r.Experience)
		}
	}
}

// HasAllItems checks inventory counts (hasAllItems).
func HasAllItems(d Deps, username string, items []Item) bool {
	for _, it := range items {
		if d.Store.CountItem(username, it.Key) < it.Count {
			return false
		}
	}
	return true
}

// TakeItems removes required items from the inventory (removeItem loop).
func TakeItems(d Deps, st *PlayerState, items []Item) {
	for _, it := range items {
		d.Store.RemoveItem(st.Username, it.Key, it.Count)
	}
	d.Store.MarkDirty(st.Username)
}

// Progress advances one stage (quest.ts progress).
func Progress(c Conn, d Deps, st *PlayerState, key string) {
	def := Quests[key]
	if def == nil {
		return
	}
	q := st.Quest(key)
	// pendingStart: the first progression surfaces the Start interface
	// instead of moving (quest.ts setStage startCallback).
	if !def.NoPrompts && !st.IsStarted(key) && q.Stage == 0 && !st.PendingStart[key] {
		st.PendingStart[key] = true
		if c != nil {
			d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketQuest, QuestStart, QuestData{Key: key}))
		}
		log.Printf("m11: %s prompted to start %s", st.Username, key)
		return
	}
	st.PendingStart[key] = false
	SetStage(c, d, st, key, q.Stage+1, 0, true)
}

// ProgressSub advances the substage (quest.ts progress(true)).
func ProgressSub(c Conn, d Deps, st *PlayerState, key string) {
	q := st.Quest(key)
	SetStage(c, d, st, key, q.Stage, q.SubStage+1, true)
}

// ---------------------------------------------------------------------------
// Talk trigger (quest.ts handleTalk + getNPCDialogue).
// ---------------------------------------------------------------------------

// Talk routes an NPC interaction through quests then achievements
// (handler.handleTalkToNPC order). Returns true when the quest/achievement
// consumed the interaction (caller skips the default dialogue).
func Talk(c Conn, d Deps, instance, npcKey string) bool {
	Load()
	if !ok || c == nil {
		return false
	}
	st := StateFor(c.PlayerName())

	// Quests first: the unfinished quest whose NPC set contains the key and
	// whose requirements hold (quests.ts getQuestFromNPC — first match wins).
	for _, key := range QuestOrder {
		def := Quests[key]
		if st.IsFinished(key) || !def.NPCs[npcKey] || !RequirementsOK(d, st, def) {
			continue
		}
		if HandleQuestTalk(c, d, st, key, instance, npcKey) {
			return true
		}
	}
	// Achievements: unfinished + npc match (getAchievementFromEntity).
	// VERIFY (dead clause): the `&& !(... && false)` tail was always false —
	// simplified to the plain npc-key match with identical behavior.
	for _, key := range AchOrder {
		def := Achs[key]
		if def.Raw.NPC != npcKey {
			continue
		}
		if st.Achs[key] >= def.StageCount {
			continue
		}
		if HandleAchTalk(c, d, st, key, instance) {
			return true
		}
	}
	return false
}

// RequirementsOK mirrors hasRequirements: skill levels + finished quests.
// Skill lookup rides skillByName (full Modules.Skills parity) so
// accuracy/strength/defense/alchemy/smithing gates evaluate instead of
// auto-failing; unknown names stay locked (return false).
func RequirementsOK(d Deps, st *PlayerState, def *Quest) bool {
	for skill, level := range def.Raw.SkillRequirements {
		id, found := skillByName(skill)
		if !found {
			return false
		}
		if lv, trained := d.Store.SkillLevel(st.Username, id); !trained || lv < level {
			return false
		}
	}
	for _, req := range def.Raw.QuestRequirements {
		if !st.IsFinished(req) {
			return false
		}
	}
	return true
}

// HandleQuestTalk ports handleTalk + getNPCDialogue: dialogue selection
// (stage text / hasItemText / completedText by search order, including
// substage NPCs), progression on dialogue end, item requirement consumption
// and reward grants.
func HandleQuestTalk(c Conn, d Deps, st *PlayerState, key, instance, npcKey string) bool {
	def := Quests[key]
	q := st.Quest(key)

	// Dialogue resolution (getNPCDialogue): iterate stages backwards from the
	// current one, first stage that references the NPC wins. Substage NPCs
	// resolve against the CURRENT stage's subStages (getSubStageByNPC parity):
	// completed → completedText, hasItem → hasItemText when stocked.
	var dialogue []string
	seen := false
	for i := q.Stage; i >= 0; i-- {
		sd := StageDef(def, i)
		// Current-stage substage reference (royalpet pattern).
		if i == q.Stage {
			for _, ss := range sd.SubStages {
				if NpcOf(ss) != npcKey {
					continue
				}
				seen = true
				if q.HasCompleted(npcKey) {
					dialogue = ss.CompletedText
					break
				}
				if len(ss.ItemRequirements) > 0 {
					if HasAllItems(d, st.Username, ss.ItemRequirements) {
						dialogue = ss.HasItemText
						break
					}
					// Items missing: TS continues the outer scan (which finds
					// no older reference) then falls back to [''] — mirror
					// by leaving dialogue empty for the [''] fallback below.
					dialogue = nil
					break
				}
				dialogue = ss.Text
				break
			}
			if seen {
				break
			}
		}
		if NpcOf(sd) != npcKey {
			continue
		}
		seen = true
		if i < q.Stage {
			dialogue = sd.CompletedText // past reference: post-stage text
			break
		}
		// Current stage: hasItemText when requirements are met, else text.
		if len(sd.ItemRequirements) > 0 {
			if HasAllItems(d, st.Username, sd.ItemRequirements) {
				dialogue = sd.HasItemText
				break
			}
			continue // items missing: try older references
		}
		dialogue = sd.Text
		break
	}
	if !seen {
		// NPC is involved in a *later* stage: Node falls back to [''] (empty
		// bubble) and swallows the default text. Mirroring that makes early
		// quest NPCs stay silent — keep it but log.
		dialogue = []string{""}
	}
	if len(dialogue) == 0 {
		dialogue = []string{""}
	}

	// Dialogue advancement uses the player's talkIndex (npc.talk).
	if st.TalkNPC != npcKey {
		st.TalkNPC = npcKey
		st.TalkIndex = 0
	}
	text := dialogue[min(st.TalkIndex, len(dialogue)-1)]
	endOfDialogue := st.TalkIndex == len(dialogue)-1
	st.TalkIndex++
	instPtr := &instance
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketNPC, protocol.NPCTalk, protocol.NpcPacketData{Instance: instPtr, Text: &text}))

	// quest.ts handleTalk: progression fires when the final line shows.
	if !endOfDialogue {
		return true
	}

	sd := StageDef(def, q.Stage)
	isStageNPC := NpcOf(sd) == npcKey

	// Substage NPCs (royalpet): each distinct NPC turns in once
	// (completedSubStages parity). When the last pending substage completes,
	// the STAGE advances; otherwise only the substage ticks. Re-talking a
	// completed substage NPC replays completedText with no further progress.
	for i := range sd.SubStages {
		ss := &sd.SubStages[i]
		if NpcOf(*ss) != npcKey {
			continue
		}
		if q.HasCompleted(npcKey) {
			return true // already turned in: dialogue shown, hold position
		}
		if len(ss.ItemRequirements) > 0 {
			if !HasAllItems(d, st.Username, ss.ItemRequirements) {
				return true // dialogue shown, items missing
			}
			TakeItems(d, st, ss.ItemRequirements)
			GiveRewards(c, d, st, ss.ItemRewards)
			GrantExperience(c, d, st, ss.SkillRewards)
			if ss.Ability != "" {
				lvl := ss.AbilityLevel
				if lvl < 1 {
					lvl = 1 // abilityLevel || 1 parity
				}
				d.Abilities.GrantAbility(c, st.Username, ss.Ability, lvl)
			}
		} else {
			if len(ss.ItemRewards) > 0 {
				if !GiveRewards(c, d, st, ss.ItemRewards) {
					return true // PLEASE_MAKE_ROOM: hold until room is made
				}
			}
			GrantExperience(c, d, st, ss.SkillRewards)
			if ss.Ability != "" {
				lvl := ss.AbilityLevel
				if lvl < 1 {
					lvl = 1
				}
				d.Abilities.GrantAbility(c, st.Username, ss.Ability, lvl)
			}
		}
		q.AddCompleted(npcKey)
		if len(q.Completed) < len(sd.SubStages) {
			ProgressSub(c, d, st, key)
		} else {
			Progress(c, d, st, key)
		}
		return true
	}

	if !isStageNPC {
		return true // involved NPC, but the current stage points elsewhere
	}

	// Current stage talk: item requirement → consumption path; item rewards
	// → grant + progress; plain → progress (handleTalk else-chain).
	if len(sd.ItemRequirements) > 0 {
		if !HasAllItems(d, st.Username, sd.ItemRequirements) {
			return true // hasItemText path already rejected above; stay put
		}
		if sd.ItemRewards != nil {
			free := protocol.ModulesInventorySize - d.Store.InventoryLen(st.Username)
			if free < len(sd.ItemRewards) {
				if c != nil {
					d.Bus.Notify(c.InstanceID(), "misc:NO_SPACE")
				}
				return true
			}
		}
		TakeItems(d, st, sd.ItemRequirements)
		GiveRewards(c, d, st, sd.ItemRewards)
		GrantExperience(c, d, st, sd.SkillRewards)
		Progress(c, d, st, key)
		return true
	}
	if len(sd.ItemRewards) > 0 {
		// Rewards granted WITHOUT consumption (guard stage 2 pattern): Node
		// grants then progresses via givePlayerRewards(progress=true).
		if !GiveRewards(c, d, st, sd.ItemRewards) {
			return true // PLEASE_MAKE_ROOM: quest holds until room is made
		}
		GrantExperience(c, d, st, sd.SkillRewards)
		Progress(c, d, st, key)
		return true
	}
	if sd.Ability != "" {
		// Quest stage ability reward (quest.ts givePlayerAbility):
		// abilities.add(ability, abilityLevel || 1).
		lvl := sd.AbilityLevel
		if lvl < 1 {
			lvl = 1
		}
		d.Abilities.GrantAbility(c, st.Username, sd.Ability, lvl)
	}
	GrantExperience(c, d, st, sd.SkillRewards)
	Progress(c, d, st, key)
	return true
}

// HandleAchTalk ports achievement.handleTalk: hidden/started dialogue,
// progress on dialogue end (discover stage), item requirements consumed.
func HandleAchTalk(c Conn, d Deps, st *PlayerState, key, instance string) bool {
	def := Achs[key]
	dialogue := def.Raw.DialogueHidden
	if st.Achs[key] > 0 {
		dialogue = def.Raw.DialogueStarted
	}
	if len(dialogue) == 0 {
		return false // no dialogue: fall through to default NPC talk
	}
	if st.TalkNPC != key {
		st.TalkNPC = key
		st.TalkIndex = 0
	}
	text := dialogue[min(st.TalkIndex, len(dialogue)-1)]
	endOfDialogue := st.TalkIndex == len(dialogue)-1
	st.TalkIndex++
	instPtr := &instance
	d.Bus.SendTo(c.InstanceID(), protocol.PktOp(protocol.PacketNPC, protocol.NPCTalk, protocol.NpcPacketData{Instance: instPtr, Text: &text}))
	if !endOfDialogue {
		return true
	}
	// Item requirement achievements resolve at the NPC (handleItemRequirement).
	if def.Raw.Item != "" {
		if d.Store.CountItem(st.Username, def.Raw.Item) < def.Raw.ItemCount {
			return true
		}
		d.Store.RemoveItem(st.Username, def.Raw.Item, def.Raw.ItemCount)
	}
	AchProgress(c, d, st, key)
	return true
}

// ---------------------------------------------------------------------------
// Kill trigger (quest.ts handleKill + achievement.handleKill).
// ---------------------------------------------------------------------------

// Kill fires on mob death credited to the killer.
// VERIFY (requirements on Kill path): TS getQuestFromMob checks only
// isFinished/isStarted/task/mob — NOT hasRequirements (requirements gate
// only getQuestFromNPC talk routing). Mirroring that here is parity, not a
// gap: no requirements check added.
func Kill(c Conn, d Deps, mobKey string) {
	if c == nil {
		return
	}
	Load()
	if !ok {
		return
	}
	st := StateFor(c.PlayerName())

	// Achievement first (handler.handleDeath order — achievements.ts kill
	// routing rides the same mob-death signal; first unfinished match wins).
	for _, key := range AchOrder {
		def := Achs[key]
		if st.Achs[key] >= def.StageCount || !AchHasMob(def, mobKey) || st.Achs[key] == 0 {
			continue
		}
		AchProgress(c, d, st, key)
	}

	// Quest kill stages: the started, unfinished quest whose current stage is
	// a kill task listing the mob (quests.ts getQuestFromMob — single match).
	for _, key := range QuestOrder {
		def := Quests[key]
		if st.IsFinished(key) || !st.IsStarted(key) {
			continue
		}
		sd := StageDef(def, st.Quest(key).Stage)
		if sd.Task != "kill" || len(sd.Mob) == 0 || !containsStr(sd.Mob, mobKey) {
			continue
		}
		// subStage per kill, stage at mobCountRequirement (handleKill).
		ProgressSub(c, d, st, key)
		if st.Quest(key).SubStage >= sd.MobCountRequirement {
			Progress(c, d, st, key)
		}
		break
	}
}

// AchHasMob reports whether the achievement tracks the mob.
func AchHasMob(def *AchDef, mobKey string) bool {
	return containsStr(def.Raw.Mob, mobKey)
}

// ---------------------------------------------------------------------------
// Resource trigger (quest.ts handleResource).
// ---------------------------------------------------------------------------

// Resource fires when a gather exhausts. skill is the Go skill name
// (lumberjacking/mining/fishing), resourceKey the resource def key
// (quest.ts handleResource: match stage.<tree|fish|rock> then count down the
// substage; no count = single-stage progress).
// VERIFY (requirements on Resource path): TS handleResource checks only
// isFinished/resource-key — never hasRequirements. No gate added (parity).
// Foraging/cooking route nowhere in TS either (getResourceType maps only
// lumberjacking→tree, fishing→fish, mining→rock; everything else defaults to
// 'tree' and then fails the `if (!resource) return` unless the stage has a
// tree field). The Go early-return for unmapped skills preserves that
// no-progress outcome without crediting lumberjacking by accident.
func Resource(c Conn, d Deps, skill, resourceKey string) {
	if c == nil {
		return
	}
	Load()
	if !ok {
		return
	}
	st := StateFor(c.PlayerName())
	field := map[string]string{
		"lumberjacking": "tree", "fishing": "fish", "mining": "rock",
	}[skill]
	if field == "" {
		return
	}
	for _, key := range QuestOrder {
		def := Quests[key]
		if st.IsFinished(key) || !st.IsStarted(key) {
			continue
		}
		q := st.Quest(key)
		sd := StageDef(def, q.Stage)
		if sd.Task != field {
			continue
		}
		var resKey string
		var resCount int
		switch field {
		case "tree":
			resKey, resCount = sd.Tree, sd.TreeCount
		case "fish":
			resKey, resCount = sd.Fish, sd.FishCount
		case "rock":
			resKey, resCount = sd.Rock, sd.RockCount
		}
		if resKey == "" || resKey != resourceKey {
			continue
		}
		if resCount == 0 {
			Progress(c, d, st, key)
			break
		}
		ProgressSub(c, d, st, key)
		if st.Quest(key).SubStage >= resCount {
			Progress(c, d, st, key)
		}
		break
	}
}

// ---------------------------------------------------------------------------
// Door trigger (quest.ts handleDoor: stage gate + isDoorTask progress).
// ---------------------------------------------------------------------------

// HandleDoor ports Quest.handleDoor for door-task stages: the doors.go pass
// path calls this AFTER the stage gate passes (planDoor parity). When the
// player's current stage for key is a `task: door` stage, the walk-through
// progresses the quest; otherwise it is a pure teleport with no quest side
// effect (non-door tasks, finished quests and unknown keys are no-ops).
//
// Verified quest-data notes (do NOT add other triggers):
//   - tutorial 1/9/15 + ricksroll 2 + evilsanta 1 are `task: door` → covered
//     here (without this, the tutorial can NEVER advance past stage 1 in
//     prod — the blocking bug).
//   - ricksroll 0/1 are `task: cooking` with NPC dialogue: TS has NO cooking
//     hook (handleResource only maps tree/fish/rock; cooking falls to the
//     default-'tree' leg and returns). They progress via TALK
//     (handleTalk/handleItemRequirement on npc rick), already covered by
//     HandleQuestTalk — no cooking trigger added, matching TS.
//   - tutorial 4 is `task: npc`: TS has NO separate npc hook either —
//     handleTalk routes by npc key regardless of task, so HandleQuestTalk
//     already covers it. No new trigger.
//   - evilsanta's custom handleDoor override (WHY_GO_THERE/DONT_THINK_GO_IN +
//     its own reqItem leg) is a per-quest subclass: the shared doors.go
//     planDoor already enforces the stage/reqItem gates with the same
//     user-visible notifies, so the default semantics here are the correct
//     shared port.
func HandleDoor(c Conn, d Deps, username string, key string) {
	Load()
	if !ok || username == "" || key == "" {
		return
	}
	def := Quests[key]
	if def == nil {
		return
	}
	st := StateFor(username)
	q := st.Quest(key)
	if q.Stage >= def.StageCount {
		return
	}
	if StageDef(def, q.Stage).Task != "door" {
		return
	}
	Progress(c, d, st, key)
}

// ---------------------------------------------------------------------------
// C→S accept (incoming.handleQuest → quest.handlePrompt).
// ---------------------------------------------------------------------------

// HeroDamageMult multiplies hero damage vs engine mobs when M11_HERODMG is
// set (debug accelerator, mirrors M9_MOBDMG) — keeps 140-HP e2e mobs in a
// few-swing kill range without touching XP accounting.
// VERIFY (HeroDamageMult placement): the helper lives in the quest package so
// the root adapter (m11HeroDamageMult) stays a one-line delegate like every
// other M11 entry point — placement is intentional, not a layering breach.
func HeroDamageMult() float64 {
	if v := os.Getenv("M11_HERODMG"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return 1
}

// HandleAccept processes the C Quest {key} frame: the pending start
// interface accepted → progress past stage 0 (handlePrompt setStage+1).
// VERIFY (requirements on Accept path): TS handlePrompt checks only
// pendingStart/isStarted — never hasRequirements. No gate added (parity).
func HandleAccept(c Conn, d Deps, data []byte) {
	var pkt struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &pkt); err != nil || pkt.Key == "" {
		return
	}
	Load()
	if !ok || Quests[pkt.Key] == nil {
		return
	}
	st := StateFor(c.PlayerName())
	if !st.PendingStart[pkt.Key] {
		return
	}
	st.PendingStart[pkt.Key] = false
	// handlePrompt: setStage(stage+1, 0, true, skipPrompts=true) — NOT
	// progress(), whose unstarted guard would re-surface the interface.
	q := st.Quest(pkt.Key)
	SetStage(c, d, st, pkt.Key, q.Stage+1, 0, true)
}

// ---------------------------------------------------------------------------
// Quest-gated drops (mob.getDrops fullfillsQuest + getDropTable quest gates).
// ---------------------------------------------------------------------------

// DropGated reports whether a drop entry's quest gate passes. status
// semantics (mob.fullfillsQuest): empty status = require finished;
// notstarted = require not started; started = started && not finished.
// achievements gate the same way on stage >= stageCount.
// VERIFY (DropGated achievement status): TS fullfillsQuest gates ONLY on
// drop.quest (+status) — drops never carry an achievement status variant.
// The achievementKey leg here is a Go extension that always requires
// finished (no notstarted/started variants exist in TS to mirror), so the
// status parameter intentionally applies to the quest leg only.
func DropGated(username, questKey, achievementKey, status string) bool {
	if questKey == "" && achievementKey == "" {
		return true
	}
	Load()
	if !ok {
		return false
	}
	st := StateFor(username)
	if achievementKey != "" {
		def := Achs[achievementKey]
		if def == nil || st.Achs[achievementKey] < def.StageCount {
			return false
		}
	}
	if questKey == "" {
		return true
	}
	def := Quests[questKey]
	if def == nil {
		return false
	}
	q := st.Quest(questKey)
	switch status {
	case "notstarted":
		return q.Stage == 0
	case "started":
		return q.Stage > 0 && q.Stage < def.StageCount
	default:
		return q.Stage >= def.StageCount
	}
}

// ---------------------------------------------------------------------------
// Achievement progression core.
// ---------------------------------------------------------------------------

// AchProgress advances an achievement one stage and fires popups/rewards
// at the finish stage (achievement.setStage). No-op when unknown or already
// finished (isFinished guard): without it every extra trigger re-granted
// rewards infinitely.
func AchProgress(c Conn, d Deps, st *PlayerState, key string) {
	def := Achs[key]
	if def == nil {
		return
	}
	if st.Achs[key] >= def.StageCount {
		return
	}
	stage := st.Achs[key] + 1
	st.Achs[key] = stage
	d.Store.MarkDirty(st.Username)
	if c != nil {
		SendAchievementProgress(c, d, key, stage)
		// else-if parity (achievement.setStage): discovery and finish never
		// double-fire — single-stage achievements emit Completed only.
		if stage >= def.StageCount {
			SendPopup(c, d, "Achievement Completed!",
				"@green@You have completed the achievement @crimson@"+def.Raw.Name+"@green@!", "#33cc33")
		} else if stage == 1 {
			SendPopup(c, d, "Achievement Discovered", def.Raw.Name+" has been discovered!", "#33cc33")
		}
	}
	if stage >= def.StageCount {
		log.Printf("m11: %s finished achievement %s", st.Username, key)
		grantAchRewards(c, d, st, def)
	}
}

// Finish ports achievement.finish(): jump straight to the finish stage
// (setStage(stageCount)) with a single progress callback + the finish popup
// + rewards. Discovered-stage popups are skipped when discovery and finish
// coincide (the setStage else-if), so milestone achievements (all
// single-stage) must finish through here rather than repeated AchProgress
// calls, which would emit a spurious "Discovered" popup. No-op when unknown
// or already finished (isFinished guard parity).
func Finish(c Conn, d Deps, st *PlayerState, key string) {
	def := Achs[key]
	if def == nil || st.Achs[key] >= def.StageCount {
		return
	}
	st.Achs[key] = def.StageCount
	d.Store.MarkDirty(st.Username)
	if c != nil {
		SendAchievementProgress(c, d, key, def.StageCount)
		SendPopup(c, d, "Achievement Completed!",
			"@green@You have completed the achievement @crimson@"+def.Raw.Name+"@green@!", "#33cc33")
	}
	log.Printf("m11: %s finished achievement %s", st.Username, key)
	grantAchRewards(c, d, st, def)
}

// grantAchRewards runs the finish-stage rewards shared by AchProgress and
// Finish (achievement.ts finishCallback: ability + skill XP; rewardItem
// grants are not modeled — no milestone/examiner achievement carries one,
// verified against achievements.json).
func grantAchRewards(c Conn, d Deps, st *PlayerState, def *AchDef) {
	// Achievement ability reward (achievement.ts finishCallback ->
	// abilities.add(rewardAbility, rewardAbilityLevel || 1)).
	if def.Raw.RewardAbility != "" {
		lvl := def.Raw.RewardAbilityLevel
		if lvl < 1 {
			lvl = 1
		}
		d.Abilities.GrantAbility(c, st.Username, def.Raw.RewardAbility, lvl)
	}
	if c != nil && def.Raw.RewardExperience > 0 {
		if id, found := skillByName(def.Raw.RewardSkill); found {
			d.Store.AddXP(c, st.Username, id, def.Raw.RewardExperience)
		}
	}
}
