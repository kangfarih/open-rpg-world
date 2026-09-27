package quest

import (
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"

	"rpg-world-server/internal/protocol"
)

// ---------------------------------------------------------------------------
// Fakes (Store/Bus/Abilities over memory; Conn over a static identity).
// ---------------------------------------------------------------------------

type fakeSlot struct {
	key   string
	count int
}

type fakeStore struct {
	slots   []fakeSlot
	skills  map[int]int
	dirty   map[string]int
	xpCalls [][3]int // (skill, amount) per AddXP call
	grants  [][2]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{skills: map[int]int{}, dirty: map[string]int{}}
}

func (f *fakeStore) InventoryLen(string) int { return len(f.slots) }

func (f *fakeStore) CountItem(_ string, itemKey string) int {
	n := 0
	for _, s := range f.slots {
		if s.key == itemKey {
			n += s.count
		}
	}
	return n
}

func (f *fakeStore) AddItem(_ string, itemKey string, count int) int {
	f.slots = append(f.slots, fakeSlot{key: itemKey, count: count})
	return len(f.slots) - 1
}

func (f *fakeStore) RemoveItem(_ string, itemKey string, count int) {
	for i := 0; i < len(f.slots) && count > 0; i++ {
		if f.slots[i].key != itemKey {
			continue
		}
		take := f.slots[i].count
		if take > count {
			take = count
		}
		f.slots[i].count -= take
		count -= take
	}
	out := f.slots[:0]
	for _, s := range f.slots {
		if s.count > 0 {
			out = append(out, s)
		}
	}
	f.slots = out
}

func (f *fakeStore) SkillLevel(_ string, skill int) (int, bool) {
	lv, found := f.skills[skill]
	return lv, found
}

func (f *fakeStore) AddXP(_ Conn, _ string, skill, amount int) int {
	f.xpCalls = append(f.xpCalls, [3]int{skill, amount, 0})
	return 1
}

func (f *fakeStore) MarkDirty(username string) { f.dirty[username]++ }

type fakeBus struct {
	sent    map[string][][]any
	notices map[string][]string
}

func newFakeBus() *fakeBus {
	return &fakeBus{sent: map[string][][]any{}, notices: map[string][]string{}}
}

func (b *fakeBus) SendTo(instance string, frames ...[]any) {
	b.sent[instance] = append(b.sent[instance], frames...)
}

func (b *fakeBus) Notify(instance string, message string) {
	b.notices[instance] = append(b.notices[instance], message)
}

type fakeAbilities struct {
	grants []string
}

func (f *fakeAbilities) GrantAbility(_ Conn, username, key string, level int) {
	f.grants = append(f.grants, username+"/"+key)
}

type fakeConn struct {
	instance string
	username string
}

func (c *fakeConn) InstanceID() string { return c.instance }
func (c *fakeConn) PlayerName() string { return c.username }

// ---------------------------------------------------------------------------
// Registry fixture (injected defs; globals restored after the test).
// ---------------------------------------------------------------------------

func withFixture(t *testing.T) (Deps, *fakeStore, *fakeBus, *fakeAbilities) {
	t.Helper()
	savedQ, savedQO := Quests, QuestOrder
	savedA, savedAO := Achs, AchOrder
	savedOK := ok
	savedStates := states
	Quests = map[string]*Quest{}
	QuestOrder = nil
	Achs = map[string]*AchDef{}
	AchOrder = nil
	states = map[string]*PlayerState{}

	// Prompted quest: stage 0 talk, stage 1 kill (2 rats).
	Quests["promptq"] = &Quest{
		Key: "promptq", StageCount: 2, NPCs: map[string]bool{"bob": true},
		Raw: Raw{
			Name: "Prompt Quest",
			Stages: map[string]StageData{
				"0": {Task: "talk", NPC: "bob", Text: []string{"hi"}},
				"1": {Task: "kill", Mob: []string{"rat"}, MobCountRequirement: 2},
			},
		},
	}
	// No-prompt quest: straight to the kill stage.
	Quests["killq"] = &Quest{
		Key: "killq", StageCount: 2, NoPrompts: true, NPCs: map[string]bool{"bob": true},
		Raw: Raw{
			Name: "Kill Quest",
			Stages: map[string]StageData{
				"0": {Task: "talk", NPC: "bob", Text: []string{"go"}},
				"1": {Task: "kill", Mob: []string{"rat"}, MobCountRequirement: 2},
			},
		},
	}
	// Gate quest: 3 stages for DropGated semantics.
	Quests["gateq"] = &Quest{
		Key: "gateq", StageCount: 3,
		Raw: Raw{Name: "Gate Quest", Stages: map[string]StageData{
			"0": {}, "1": {}, "2": {},
		}},
	}
	QuestOrder = []string{"gateq", "killq", "promptq"}
	Achs["gateach"] = &AchDef{Key: "gateach", StageCount: 2}
	AchOrder = []string{"gateach"}
	ok = true

	t.Cleanup(func() {
		Quests, QuestOrder = savedQ, savedQO
		Achs, AchOrder = savedA, savedAO
		ok = savedOK
		states = savedStates
	})

	store, bus, ab := newFakeStore(), newFakeBus(), &fakeAbilities{}
	return Deps{Store: store, Bus: bus, Abilities: ab, DB: nil}, store, bus, ab
}

// frameOp extracts the packet id + opcode of a sent frame.
func frameOp(f []any) (int, int) {
	id, _ := f[0].(int)
	op, _ := f[1].(int)
	return id, op
}

// TestPromptAcceptLifecycle pins the pendingStart flow: the first Progress
// on a prompted quest surfaces Start (no stage move); accept advances.
func TestPromptAcceptLifecycle(t *testing.T) {
	d, _, bus, _ := withFixture(t)
	c := &fakeConn{instance: "i1", username: "u-prompt"}
	st := StateFor("u-prompt")

	Progress(c, d, st, "promptq")
	if !st.PendingStart["promptq"] {
		t.Fatal("first Progress must raise PendingStart")
	}
	if got := st.Quest("promptq").Stage; got != 0 {
		t.Fatalf("stage during prompt = %d, want 0", got)
	}
	sent := bus.sent["i1"]
	if len(sent) != 1 {
		t.Fatalf("frames during prompt = %d, want 1 Start", len(sent))
	}
	if id, op := frameOp(sent[0]); id != protocol.PacketQuest || op != QuestStart {
		t.Fatalf("prompt frame = [%d,%d], want [%d,%d]", id, op, protocol.PacketQuest, QuestStart)
	}

	HandleAccept(c, d, []byte(`{"key":"promptq"}`))
	if st.PendingStart["promptq"] {
		t.Fatal("accept must clear PendingStart")
	}
	if got := st.Quest("promptq").Stage; got != 1 {
		t.Fatalf("stage after accept = %d, want 1", got)
	}
	sent = bus.sent["i1"]
	if len(sent) != 3 {
		t.Fatalf("frames after accept = %d, want 3 (Start + PointerRemove + Progress)", len(sent))
	}
	if id, op := frameOp(sent[2]); id != protocol.PacketQuest || op != QuestProgress {
		t.Fatalf("accept frame = [%d,%d], want Quest Progress", id, op)
	}
	var qd QuestData
	raw, _ := json.Marshal(sent[2][2])
	if err := json.Unmarshal(raw, &qd); err != nil || qd.Key != "promptq" || qd.Stage != 1 {
		t.Fatalf("progress payload = %+v err=%v, want promptq stage 1", qd, err)
	}
}

// TestKillSubstageLifecycle pins the kill flow: subStage per kill, stage at
// mobCountRequirement, and the started/finished gate flip.
func TestKillSubstageLifecycle(t *testing.T) {
	d, store, _, _ := withFixture(t)
	c := &fakeConn{instance: "i2", username: "u-kill"}
	st := StateFor("u-kill")

	// Enter the kill stage directly (TESTMAP setstage parity: no prompt).
	SetStage(c, d, st, "killq", 1, 0, true)
	if store.dirty["u-kill"] == 0 {
		t.Fatal("SetStage with progress must mark dirty")
	}
	if !DropGated("u-kill", "killq", "", "started") {
		t.Fatal("gate must report started at stage 1")
	}

	Kill(c, d, "rat")
	if got := st.Quest("killq").SubStage; got != 1 {
		t.Fatalf("subStage after 1 kill = %d, want 1", got)
	}
	if DropGated("u-kill", "killq", "", "") {
		t.Fatal("gate must not report finished mid-stage")
	}
	Kill(c, d, "other-mob") // unrelated mob: no credit
	if got := st.Quest("killq").SubStage; got != 1 {
		t.Fatalf("subStage after unrelated kill = %d, want 1", got)
	}
	Kill(c, d, "rat")
	if got := st.Quest("killq").Stage; got != 2 {
		t.Fatalf("stage after requirement met = %d, want 2 (finished)", got)
	}
	if !DropGated("u-kill", "killq", "", "") {
		t.Fatal("gate must report finished at stageCount")
	}
	if DropGated("u-kill", "killq", "", "started") {
		t.Fatal("gate must not report started once finished")
	}
}

// TestDropGatedSemantics pins the notstarted/started/finished matrix plus
// the achievement gate.
func TestDropGatedSemantics(t *testing.T) {
	d, _, _, _ := withFixture(t)
	c := &fakeConn{instance: "i3", username: "u-gate"}
	st := StateFor("u-gate")

	if !DropGated("u-gate", "", "", "") {
		t.Fatal("empty gate must pass")
	}
	if !DropGated("u-gate", "gateq", "", "notstarted") {
		t.Fatal("fresh quest must be notstarted")
	}
	if DropGated("u-gate", "gateq", "", "started") {
		t.Fatal("fresh quest must not be started")
	}
	SetStage(c, d, st, "gateq", 1, 0, true)
	if !DropGated("u-gate", "gateq", "", "started") {
		t.Fatal("stage 1 must be started")
	}
	SetStage(c, d, st, "gateq", 3, 0, true)
	if !DropGated("u-gate", "gateq", "", "") {
		t.Fatal("stageCount must be finished")
	}
	if DropGated("u-gate", "nope", "", "") {
		t.Fatal("unknown quest must fail the gate")
	}

	if DropGated("u-gate", "", "gateach", "") {
		t.Fatal("unfinished achievement must fail the gate")
	}
	AchProgress(c, d, st, "gateach")
	AchProgress(c, d, st, "gateach")
	if !DropGated("u-gate", "", "gateach", "") {
		t.Fatal("finished achievement must pass the gate")
	}
}

// TestGiveRewardsSpace pins the NO_SPACE branch and the grant branch.
func TestGiveRewardsSpace(t *testing.T) {
	d, store, bus, _ := withFixture(t)
	c := &fakeConn{instance: "i4", username: "u-reward"}
	st := StateFor("u-reward")

	for i := 0; i < protocol.ModulesInventorySize; i++ {
		store.slots = append(store.slots, fakeSlot{key: "junk", count: 1})
	}
	if GiveRewards(c, d, st, []Item{{Key: "sword", Count: 1}}) {
		t.Fatal("full inventory must refuse rewards")
	}
	if n := len(bus.notices["i4"]); n != 1 || bus.notices["i4"][0] != "misc:PLEASE_MAKE_ROOM_REWARD" {
		t.Fatalf("notices = %v, want one PLEASE_MAKE_ROOM_REWARD", bus.notices["i4"])
	}

	store.slots = nil
	if !GiveRewards(c, d, st, []Item{{Key: "sword", Count: 1}}) {
		t.Fatal("roomy inventory must grant rewards")
	}
	if got := store.CountItem("u-reward", "sword"); got != 1 {
		t.Fatalf("sword count = %d, want 1", got)
	}
	sent := bus.sent["i4"]
	if len(sent) != 1 {
		t.Fatalf("grant frames = %d, want 1 Container Add", len(sent))
	}
	if id, op := frameOp(sent[0]); id != protocol.PacketContainer || op != protocol.ContainerAdd {
		t.Fatalf("grant frame = [%d,%d], want Container Add", id, op)
	}
}

// TestLoginBatchesShape pins the Batch/Progress frame ids in the login
// batch and the quest definition fields.
func TestLoginBatchesShape(t *testing.T) {
	withFixture(t)
	frames := LoginBatches("u-batch")
	if len(frames) != 2 {
		t.Fatalf("login batches = %d frames, want 2", len(frames))
	}
	if id, op := frameOp(frames[0]); id != protocol.PacketQuest || op != QuestBatch {
		t.Fatalf("frame 0 = [%d,%d], want Quest Batch", id, op)
	}
	if id, op := frameOp(frames[1]); id != protocol.PacketAchievement || op != AchievementBatch {
		t.Fatalf("frame 1 = [%d,%d], want Achievement Batch", id, op)
	}
	raw, _ := json.Marshal(frames[0][2])
	var batch struct {
		Quests []QuestData `json:"quests"`
	}
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatalf("quest batch payload: %v", err)
	}
	if len(batch.Quests) != 3 || batch.Quests[0].Key != "gateq" {
		t.Fatalf("quest batch = %+v, want 3 sorted defs", batch.Quests)
	}
	if batch.Quests[1].Name == nil || *batch.Quests[1].Name != "Kill Quest" {
		t.Fatalf("quest batch def fields = %+v, want Kill Quest", batch.Quests[1])
	}
}

// TestPersistRoundTrip pins the SQLite schema path: persist, forget, load.
func TestPersistRoundTrip(t *testing.T) {
	d, _, _, _ := withFixture(t)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open memory sqlite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	d.DB = db

	EnsureTables(d)
	st := StateFor("u-persist")
	st.Quest("gateq").Stage, st.Quest("gateq").SubStage = 2, 1
	st.Achs["gateach"] = 1
	PersistQuests(d, "u-persist")

	ForgetSession("u-persist")
	if _, found := states["u-persist"]; found {
		t.Fatal("ForgetSession must drop the state")
	}
	LoadQuests(d, "u-persist")
	restored := StateFor("u-persist")
	if got := restored.Quest("gateq"); got.Stage != 2 || got.SubStage != 1 {
		t.Fatalf("restored quest = %+v, want 2/1", got)
	}
	if restored.Achs["gateach"] != 1 {
		t.Fatalf("restored ach = %d, want 1", restored.Achs["gateach"])
	}
}
