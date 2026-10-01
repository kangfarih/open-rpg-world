// Quests wire — quests + achievements engine.
//
// Thin adapter over internal/player/quest (behavior-frozen move, task E7):
// all quest/achievement logic lives in the quest package operating on the
// Conn/Store/Bus/Abilities/DB seams below. This file only wires those seams
// to the root globals (players map, send, pstates, player-state/economy
// helpers, ability grants, dbConn) and keeps the entry points main.go and
// other wire files call — with UNCHANGED signatures — delegating to the
// quest package. Quest data, gates, frames and the SQLite schema are
// identical.
//
// Stayed in root (cannot move cleanly): invCount/removeItem (economy-owned
// inventory helpers over pstates/pstateMu, also called by other wire files
// directly), playerConn transport (send/connByInstance), player-state XP/item
// helpers, economy notify, abGrantAbility, dbConn, and the testMode gate.
package server

import (
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/player/quest"
	worldcore "rpg-world-server/internal/world"
)

// Type aliases so existing names keep resolving to the moved types.
type (
	questDef          = quest.Quest
	achievementRaw = quest.AchievementRaw
	achDef         = quest.AchDef
	questRaw          = quest.Raw
	questStageData    = quest.StageData
	questItem         = quest.Item
	questPointer      = quest.Pointer
	questPopup        = quest.Popup
	questSkillReward  = quest.SkillReward
	questData         = quest.QuestData
	achievementData   = quest.AchievementData
)

// Opcode aliases (quest/achievement frame values, unchanged).
const (
	QuestBatch          = quest.QuestBatch
	QuestProgress       = quest.QuestProgress
	QuestFinish         = quest.QuestFinish
	QuestStart          = quest.QuestStart
	AchievementBatch    = quest.AchievementBatch
	AchievementProgress = quest.AchievementProgress
	NotificationPopup   = quest.NotificationPopup
)

// Registry mirrors for direct map readers (m13/world_wire parity). The
// authoritative maps live in the quest package; these reference the same
// underlying maps so external call sites compile untouched.
var (
	questDefs = quest.Quests
	achDefs = quest.Achs
)

// playerQuestState wraps the canonical quest.PlayerState so the lowercase
// accessors used by m13.go/world_wire.go (st.quest, st.isFinished,
// st.isStarted) keep compiling; the embedded pointer promotes the shared
// fields (Username/Quests/Achs/TalkNPC/TalkIndex/PendingStart).
type playerQuestState struct {
	*quest.PlayerState
}

// questRunState wraps the canonical quest.QuestState; Stage/SubStage
// promote through the embedded pointer.
type questRunState struct {
	*quest.QuestState
}

func (st *playerQuestState) quest(key string) *questRunState {
	return &questRunState{st.PlayerState.Quest(key)}
}

func (st *playerQuestState) isFinished(key string) bool {
	return st.PlayerState.IsFinished(key)
}

func (st *playerQuestState) isStarted(key string) bool {
	return st.PlayerState.IsStarted(key)
}

// ---------------------------------------------------------------------------
// quest.Conn seam (*playerConn satisfies it, so identity is preserved).
// ---------------------------------------------------------------------------

// questWrapConn boxes a root conn as a quest.Conn, preserving nil (a nil *playerConn
// becomes a nil interface so the quest package's c == nil guards fire).
func questWrapConn(c *playerConn) quest.Conn {
	if c == nil {
		return nil
	}
	return c
}

// questUnwrapConn unwraps a quest.Conn back to the root conn (XP/ability side
// effects need it); falls back to an instance lookup for foreign impls.
func questUnwrapConn(c quest.Conn) *playerConn {
	if pc, ok := c.(*playerConn); ok {
		return pc
	}
	if c == nil {
		return nil
	}
	pc, _ := worldcore.Find[*playerConn](c.InstanceID())
	return pc
}

// ---------------------------------------------------------------------------
// quest.Store / quest.Bus / quest.Abilities seams.
// ---------------------------------------------------------------------------

type questStore struct{}

func (questStore) InventoryLen(username string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	return len(st.Inv)
}

func (questStore) CountItem(username, itemKey string) int {
	return invCount(username, itemKey)
}

func (questStore) AddItem(username, itemKey string, count int) int {
	return addItem(username, itemKey, count)
}

func (questStore) RemoveItem(username, itemKey string, count int) {
	invRemoveItem(username, itemKey, count)
}

func (questStore) SkillLevel(username string, skill int) (int, bool) {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	if s := st.Skills[skill]; s != nil {
		return s.Level, true
	}
	return 0, false
}

func (questStore) AddXP(c quest.Conn, username string, skill, amount int) int {
	return addXP(questUnwrapConn(c), username, skill, amount)
}

func (questStore) MarkDirty(username string) { markDirty(username) }

type questBus struct{}

func (questBus) SendTo(instance string, frames ...[]any) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	_ = gnet.Send(c.Conn, frames...)
}

func (questBus) Notify(instance string, message string) {
	c, _ := worldcore.Find[*playerConn](instance)
	if c == nil {
		return
	}
	notifyPlayer(c, message)
}

type questAbilities struct{}

func (questAbilities) GrantAbility(c quest.Conn, username, key string, level int) {
	abGrantAbility(questUnwrapConn(c), username, key, level)
}

// questDeps wires the quest seams to the root globals (a nil dbConn becomes a
// nil quest.DB so persistence no-ops, dbConn == nil parity).
func questDeps() quest.Deps {
	var db quest.DB
	if dbConn != nil {
		db = dbConn
	}
	return quest.Deps{Store: questStore{}, Bus: questBus{}, Abilities: questAbilities{}, DB: db}
}

// ---------------------------------------------------------------------------
// Entry points (signatures UNCHANGED; main.go/m5.go/m6.go/m9.go/m13.go and
// world_wire.go call sites compile as-is). The testMode gate stays here —
// it reads a root global.
// ---------------------------------------------------------------------------

func questStateFor(username string) *playerQuestState {
	return &playerQuestState{quest.StateFor(username)}
}

// forgetQuestSession drops in-memory quest state (TESTMAP harness isolation).
func forgetQuestSession(username string) {
	quest.ForgetSession(username)
}

// sendQuestProgress emits Quest Progress1 (quests.ts handleProgress).
func sendQuestProgress(c *playerConn, key string, q *questRunState) {
	quest.SendQuestProgress(questWrapConn(c), questDeps(), key, q.QuestState)
}

// sendAchievementProgress emits Achievement Progress1 (name/description
// ride along for client Task creation — setAchievement contract).
func sendAchievementProgress(c *playerConn, key string, stage int) {
	quest.SendAchievementProgress(questWrapConn(c), questDeps(), key, stage)
}

// sendQuestPointer mirrors player.pointer: Remove first, then Location.
func sendQuestPointer(c *playerConn, p *questPointer) {
	quest.SendPointer(questWrapConn(c), questDeps(), p)
}

// sendQuestPopup mirrors player.popup: Notification Popup3 {title,message,colour}.
func sendQuestPopup(c *playerConn, title, message, colour string) {
	quest.SendPopup(questWrapConn(c), questDeps(), title, message, colour)
}

// questLoginBatches builds the Quest Batch + Achievement Batch frames queued
// after the Welcome extras (handler.handleQuests/handleAchievements on
// quests.onLoaded). Batched questData carries the definition fields.
func questLoginBatches(username string) [][]any {
	return quest.LoginBatches(username)
}

// questLoginPointer sends the current stage's quest pointer after Ready
// (Tutorial.loaded → setStage(0,0,false) → pointerCallback; for other quests
// Node only re-points on stage changes, we mirror that by pointing only when
// the tutorial is unfinished).
func questLoginPointer(c *playerConn) {
	quest.LoginPointer(questWrapConn(c), questDeps())
}

func questStageDef(q *questDef, stage int) questStageData {
	return quest.StageDef(q, stage)
}

// questNpcOf returns the stage's npc key honoring the `noc` typo.
func questNpcOf(st questStageData) string {
	return quest.NpcOf(st)
}

// questSetStage applies the new stage and emits Progress/pointer/popup side
// effects. progress=false mirrors setStage(..., false) for DB loads.
func questSetStage(c *playerConn, st *playerQuestState, key string, stage, subStage int, progress bool) {
	quest.SetStage(questWrapConn(c), questDeps(), st.PlayerState, key, stage, subStage, progress)
	if progress {
		// Dynmap: quests.ts handleProgress re-sends the region on finish
		// (player.updateRegion); resets can revert a remap the same way.
		// maybePush is signature-gated, so non-gating stages send nothing.
		maybePushDynamicMap(c)
	}
}

// questGiveRewards grants stage itemRewards (givePlayerRewards): NO_SPACE
// notify when the inventory cannot fit every entry, else add each item and
// emit Container Add.
func questGiveRewards(c *playerConn, st *playerQuestState, rewards []questItem) bool {
	return quest.GiveRewards(questWrapConn(c), questDeps(), st.PlayerState, rewards)
}

// questGrantExperience ports givePlayerExperience: skillRewards by name.
func questGrantExperience(c *playerConn, st *playerQuestState, rewards []questSkillReward) {
	quest.GrantExperience(questWrapConn(c), questDeps(), st.PlayerState, rewards)
}

// questHasAllItems checks inventory counts (hasAllItems).
func questHasAllItems(username string, items []questItem) bool {
	return quest.HasAllItems(questDeps(), username, items)
}

// questTakeItems removes required items from the inventory (removeItem loop).
func questTakeItems(st *playerQuestState, items []questItem) {
	quest.TakeItems(questDeps(), st.PlayerState, items)
}

// questProgress advances one stage (quest.ts progress).
func questProgress(c *playerConn, st *playerQuestState, key string) {
	quest.Progress(questWrapConn(c), questDeps(), st.PlayerState, key)
}

// questProgressSub advances the substage (quest.ts progress(true)).
func questProgressSub(c *playerConn, st *playerQuestState, key string) {
	quest.ProgressSub(questWrapConn(c), questDeps(), st.PlayerState, key)
}

// questTalk routes an NPC interaction through quests then achievements
// (handler.handleTalkToNPC order). Returns true when the quest/achievement
// consumed the interaction (caller skips the default dialogue).
func questTalk(c *playerConn, instance, npcKey string) bool {
	out := quest.Talk(questWrapConn(c), questDeps(), instance, npcKey)
	// Dynmap: talk can advance achievement discovery stages internally
	// (HandleAchTalk bypasses achProgress); signature-gated no-op
	// when nothing changed.
	maybePushDynamicMap(c)
	return out
}

// questRequirementsOK mirrors hasRequirements: skill levels + finished quests.
func questRequirementsOK(st *playerQuestState, def *questDef) bool {
	return quest.RequirementsOK(questDeps(), st.PlayerState, def)
}

// handleQuestTalk ports handleTalk + getNPCDialogue: dialogue selection
// (stage text / hasItemText / completedText by search order), progression on
// dialogue end, item requirement consumption and reward grants.
func handleQuestTalk(c *playerConn, st *playerQuestState, key, instance, npcKey string) bool {
	return quest.HandleQuestTalk(questWrapConn(c), questDeps(), st.PlayerState, key, instance, npcKey)
}

// handleAchTalk ports achievement.handleTalk: hidden/started dialogue,
// progress on dialogue end (discover stage), item requirements consumed.
func handleAchTalk(c *playerConn, st *playerQuestState, key, instance string) bool {
	return quest.HandleAchTalk(questWrapConn(c), questDeps(), st.PlayerState, key, instance)
}

// questKill fires on mob death credited to the killer.
func questKill(c *playerConn, mobKey string) {
	quest.Kill(questWrapConn(c), questDeps(), mobKey)
	// Dynmap: kill achievements progress inside the quest package;
	// signature-gated (questTalk parity).
	maybePushDynamicMap(c)
}

func achHasMob(def *achDef, mobKey string) bool {
	return quest.AchHasMob(def, mobKey)
}

// questResource fires when a gather exhausts. skill is the Go skill name
// (lumberjacking/mining/fishing/foraging), resourceKey the resource def key
// (quest.ts handleResource: match stage.<tree|fish|rock> then count down the
// substage; no count = single-stage progress).
func questResource(c *playerConn, skill, resourceKey string) {
	quest.Resource(questWrapConn(c), questDeps(), skill, resourceKey)
	// Dynmap: resource quest stages progress inside the quest package;
	// signature-gated (questTalk parity).
	maybePushDynamicMap(c)
}

// heroDamageMult multiplies hero damage vs engine mobs when M11_HERODMG is
// set (debug accelerator, mirrors M9_MOBDMG) — keeps 140-HP e2e mobs in a
// few-swing kill range without touching XP accounting.
func heroDamageMult() float64 {
	return quest.HeroDamageMult()
}

// handleQuestAccept processes the C Quest {key} frame: the pending start
// interface accepted → progress past stage 0 (handlePrompt setStage+1).
func handleQuestAccept(c *playerConn, data []byte) {
	quest.HandleAccept(questWrapConn(c), questDeps(), data)
}

// questDropGated reports whether a drop entry's quest gate passes. status
// semantics (mob.fullfillsQuest): empty status = require finished;
// notstarted = require not started; started = started && not finished.
// achievements gate the same way on stage >= stageCount.
func questDropGated(username, questKey, achievementKey, status string) bool {
	return quest.DropGated(username, questKey, achievementKey, status)
}

// achProgress advances an achievement one stage and fires popups/rewards
// at the finish stage (achievement.setStage).
func achProgress(c *playerConn, st *playerQuestState, key string) {
	quest.AchProgress(questWrapConn(c), questDeps(), st.PlayerState, key)
	// Dynmap: achievements.ts finishCallback re-sends the region on finish.
	maybePushDynamicMap(c)
}

// finishAchievement finishes an achievement outright (achievement.finish
// parity: jump to the finish stage with a single progress callback, no
// discovery popup). Used by the statistics milestone paths (examiner +
// gather skills), which finish single-stage achievements directly like TS.
// Unknown keys and missing connections are ignored (TS
// `achievements.get(key)?.finish()` parity).
func finishAchievement(c *playerConn, key string) {
	if c == nil || c.Username == "" || achDefs[key] == nil {
		return
	}
	quest.Finish(questWrapConn(c), questDeps(), quest.StateFor(c.Username), key)
	// Dynmap: outright achievement finish re-skins tiles like a stage
	// finish (covers statistics milestones, doors, chest clears).
	maybePushDynamicMap(c)
}

// ensureQuestTables creates the quest/achievement tables (M5 DDL order).
func ensureQuestTables() {
	quest.EnsureTables(questDeps())
}

// persistQuests writes the quest rows for one player (called from the
// disconnect/flush path).
func persistQuests(username string) {
	quest.PersistQuests(questDeps(), username)
}

// loadQuests restores quest/achievement rows into the in-memory state
// (called before the login batches are built).
func loadQuests(username string) {
	quest.LoadQuests(questDeps(), username)
}

// handleQuestTest processes TESTMAP debug ops: stage/substage injection,
// achievement stage injection, and state echoes for the e2e harness.
func handleQuestTest(c *playerConn, data []byte) {
	if !testMode {
		return
	}
	// Admin-rank gate (see handleMinigameTest: TESTMAP default stays ON, the gate
	// closes the any-client setstage hole).
	if !isAdmin(c) {
		return
	}
	quest.HandleTest(questWrapConn(c), questDeps(), data)
	// Dynmap: setstage/setach inject finish states directly (bypassing the
	// wrappers above); signature-gated.
	maybePushDynamicMap(c)
}

// ---------------------------------------------------------------------------
// Small helpers shared with the m6 slice (m6-owned inventory helpers over
// pstates/pstateMu; also called by m12.go/m13.go directly — intentionally
// left in root).
// ---------------------------------------------------------------------------

// invCount counts inventory copies of a key (inventory.getIndex parity).
func invCount(username, itemKey string) int {
	st := playerStateFor(username)
	pstateMu.Lock()
	defer pstateMu.Unlock()
	n := 0
	for _, s := range st.Inv {
		if s.Key == itemKey {
			n += s.Count
		}
	}
	return n
}

// invRemoveItem removes count copies of a key across stacks (inventory.
// removeItem parity): decrement stacks, then compact zeros. Container Remove
// frames ride the harness ClientContainerSync drain instead — matches Node,
// where quest item consumption never sends container frames.
func invRemoveItem(username, itemKey string, count int) {
	st := playerStateFor(username)
	pstateMu.Lock()
	for i := 0; i < len(st.Inv) && count > 0; i++ {
		if st.Inv[i].Key != itemKey {
			continue
		}
		take := st.Inv[i].Count
		if take > count {
			take = count
		}
		st.Inv[i].Count -= take
		count -= take
	}
	out := st.Inv[:0]
	for _, s := range st.Inv {
		if s.Count > 0 {
			out = append(out, s)
		}
	}
	st.Inv = out
	pstateMu.Unlock()
}
