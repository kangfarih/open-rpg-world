package quest

import (
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"

	"rpg-world-server/internal/protocol"
)

// TestAchProgressFinishedGuard pins the isFinished guard: extra triggers
// after finish change nothing (no stage bump, no frames, no re-rewards).
func TestAchProgressFinishedGuard(t *testing.T) {
	d, store, bus, _ := withFixture(t)
	Achs["guard1"] = &AchDef{Key: "guard1", StageCount: 2, Raw: AchievementRaw{Name: "G"}}
	c := &fakeConn{instance: "i-g", username: "u-guard"}
	st := StateFor("u-guard")
	st.Achs["guard1"] = 2 // already finished

	dirtyBefore := store.dirty["u-guard"]
	framesBefore := len(bus.sent["i-g"])
	AchProgress(c, d, st, "guard1")
	if st.Achs["guard1"] != 2 {
		t.Fatalf("finished ach stage = %d, want 2 (no bump)", st.Achs["guard1"])
	}
	if store.dirty["u-guard"] != dirtyBefore {
		t.Fatal("finished AchProgress must not mark dirty")
	}
	if len(bus.sent["i-g"]) != framesBefore {
		t.Fatal("finished AchProgress must send nothing")
	}
	if len(store.xpCalls) != 0 {
		t.Fatal("finished AchProgress must not grant rewards")
	}
}

// TestAchProgressSinglePopup pins the else-if: a single-stage achievement
// emits exactly one popup (Completed), never Discovered+Completed.
func TestAchProgressSinglePopup(t *testing.T) {
	d, _, bus, _ := withFixture(t)
	Achs["single1"] = &AchDef{Key: "single1", StageCount: 1, Raw: AchievementRaw{Name: "S"}}
	c := &fakeConn{instance: "i-s", username: "u-single"}
	st := StateFor("u-single")

	AchProgress(c, d, st, "single1")
	if st.Achs["single1"] != 1 {
		t.Fatalf("stage = %d, want 1", st.Achs["single1"])
	}
	prog, popups := 0, 0
	for _, f := range bus.sent["i-s"] {
		id, op := frameOp(f)
		switch {
		case id == protocol.PacketAchievement && op == AchievementProgress:
			prog++
		case id == protocol.PacketNotification && op == NotificationPopup:
			popups++
			raw, _ := json.Marshal(f[2])
			var nd protocol.NotificationPacketData
			if err := json.Unmarshal(raw, &nd); err != nil {
				t.Fatalf("popup payload: %v", err)
			}
			if nd.Title == nil || *nd.Title != "Achievement Completed!" {
				t.Fatalf("single-stage popup title = %v, want Completed", nd.Title)
			}
		}
	}
	if prog != 1 {
		t.Fatalf("progress frames = %d, want 1", prog)
	}
	if popups != 1 {
		t.Fatalf("popups = %d, want exactly 1 (Completed, no Discovered)", popups)
	}
}

// TestAchProgressMultiPopupOrder pins the two-stage order: stage 1 emits
// Discovered, stage 2 emits Completed.
func TestAchProgressMultiPopupOrder(t *testing.T) {
	d, _, bus, _ := withFixture(t)
	Achs["multi2"] = &AchDef{Key: "multi2", StageCount: 2, Raw: AchievementRaw{Name: "M"}}
	c := &fakeConn{instance: "i-m", username: "u-multi"}
	st := StateFor("u-multi")

	AchProgress(c, d, st, "multi2")
	var firstTitle string
	for _, f := range bus.sent["i-m"] {
		if id, op := frameOp(f); id == protocol.PacketNotification && op == NotificationPopup {
			raw, _ := json.Marshal(f[2])
			var nd protocol.NotificationPacketData
			_ = json.Unmarshal(raw, &nd)
			if nd.Title != nil {
				firstTitle = *nd.Title
			}
		}
	}
	if firstTitle != "Achievement Discovered" {
		t.Fatalf("stage-1 popup = %q, want Discovered", firstTitle)
	}
}

// TestRoyalpetSubstageFlow pins the multi-NPC completion set: dedup, stage
// hold until all three turn in, then stage advance with set cleared.
func TestRoyalpetSubstageFlow(t *testing.T) {
	d, store, _, _ := withFixture(t)
	Quests["royal"] = &Quest{
		Key: "royal", StageCount: 3, NoPrompts: true,
		NPCs: map[string]bool{"king": true, "a": true, "b": true, "c": true},
		Raw: Raw{Name: "Royal", Stages: map[string]StageData{
			"0": {Task: "talk", NPC: "king", Text: []string{"hi"}},
			"1": {Task: "talk", SubStages: []StageData{
				{Task: "talk", NPC: "a", Text: []string{"a"}, HasItemText: []string{"ta"}, CompletedText: []string{"da"}, ItemRequirements: []Item{{Key: "book", Count: 1}}},
				{Task: "talk", NPC: "b", Text: []string{"b"}, HasItemText: []string{"tb"}, CompletedText: []string{"db"}, ItemRequirements: []Item{{Key: "book", Count: 1}}},
				{Task: "talk", NPC: "c", Text: []string{"c"}, HasItemText: []string{"tc"}, CompletedText: []string{"dc"}, ItemRequirements: []Item{{Key: "book", Count: 1}}},
			}},
			"2": {Task: "talk", NPC: "king", Text: []string{"done"}},
		}},
	}
	c := &fakeConn{instance: "i-r", username: "u-royal"}
	st := StateFor("u-royal")
	store.slots = append(store.slots, fakeSlot{key: "book", count: 3})
	SetStage(c, d, st, "royal", 1, 0, true)

	if !HandleQuestTalk(c, d, st, "royal", "i-r", "a") {
		t.Fatal("substage talk must consume")
	}
	q := st.Quest("royal")
	if q.Stage != 1 || q.SubStage != 1 {
		t.Fatalf("after a: stage/sub = %d/%d, want 1/1", q.Stage, q.SubStage)
	}
	if !q.HasCompleted("a") || len(q.Completed) != 1 {
		t.Fatalf("completed = %v, want [a]", q.Completed)
	}
	if store.dirty["u-royal"] == 0 {
		t.Fatal("substage turn-in must mark dirty")
	}

	// Duplicate turn-in replays completedText with no progress.
	subBefore := q.SubStage
	HandleQuestTalk(c, d, st, "royal", "i-r", "a")
	if q.SubStage != subBefore || len(q.Completed) != 1 {
		t.Fatalf("dup a: sub=%d completed=%v, want no change", q.SubStage, q.Completed)
	}

	HandleQuestTalk(c, d, st, "royal", "i-r", "b")
	if len(st.Quest("royal").Completed) != 2 || st.Quest("royal").Stage != 1 {
		t.Fatalf("after b: stage=%d completed=%v, want stage 1 len 2",
			st.Quest("royal").Stage, st.Quest("royal").Completed)
	}
	HandleQuestTalk(c, d, st, "royal", "i-r", "c")
	if got := st.Quest("royal").Stage; got != 2 {
		t.Fatalf("after all three: stage = %d, want 2", got)
	}
	if len(st.Quest("royal").Completed) != 0 {
		t.Fatalf("stage change must clear completed, got %v", st.Quest("royal").Completed)
	}
}

// TestRequirementsOKFullSkills pins codersglitch/codersfallacy parity:
// accuracy/strength/defense/alchemy/smithing gates evaluate instead of
// auto-failing; unknown names stay locked.
func TestRequirementsOKFullSkills(t *testing.T) {
	d, store, _, _ := withFixture(t)
	def := &Quest{Key: "g", Raw: Raw{SkillRequirements: map[string]int{
		"accuracy": 15, "strength": 20, "defense": 15,
	}}}
	st := StateFor("u-req")
	store.skills[SkillAccuracy] = 15
	store.skills[SkillStrength] = 20
	store.skills[SkillDefense] = 14
	if RequirementsOK(d, st, def) {
		t.Fatal("defense 14 must fail the 15 gate")
	}
	store.skills[SkillDefense] = 15
	if !RequirementsOK(d, st, def) {
		t.Fatal("met accuracy/strength/defense gates must pass")
	}
	def2 := &Quest{Key: "f", Raw: Raw{SkillRequirements: map[string]int{
		"alchemy": 35, "smithing": 45,
	}}}
	store.skills[SkillAlchemy] = 35
	store.skills[SkillSmithing] = 45
	if !RequirementsOK(d, StateFor("u-req"), def2) {
		t.Fatal("met alchemy/smithing gates must pass")
	}
	def3 := &Quest{Key: "x", Raw: Raw{SkillRequirements: map[string]int{"nosuch": 1}}}
	if RequirementsOK(d, st, def3) {
		t.Fatal("unknown skill must stay locked")
	}
}

// TestSkillByNameFull pins all 19 Modules.Skills names (case-insensitive).
func TestSkillByNameFull(t *testing.T) {
	cases := map[string]int{
		"lumberjacking": 0, "accuracy": 1, "archery": 2, "health": 3,
		"magic": 4, "mining": 5, "strength": 6, "defense": 7,
		"fishing": 8, "cooking": 9, "smithing": 10, "crafting": 11,
		"chiseling": 12, "fletching": 13, "smelting": 14, "foraging": 15,
		"eating": 16, "loitering": 17, "alchemy": 18,
		"Alchemy": 18, "SMITHING": 10, "Cooking": 9,
	}
	for name, want := range cases {
		got, found := skillByName(name)
		if !found || got != want {
			t.Fatalf("skillByName(%q) = (%d,%v), want (%d,true)", name, got, found, want)
		}
	}
	if _, found := skillByName("nosuch"); found {
		t.Fatal("unknown skill must not resolve")
	}
}

// TestHandleDoor pins door-task progression and its no-op legs.
func TestHandleDoor(t *testing.T) {
	d, _, bus, _ := withFixture(t)
	Quests["doorq"] = &Quest{
		Key: "doorq", StageCount: 2, NoPrompts: true,
		Raw: Raw{Name: "D", Stages: map[string]StageData{
			"0": {Task: "talk", NPC: "bob", Text: []string{"hi"}},
			"1": {Task: "door"},
		}},
	}
	c := &fakeConn{instance: "i-d", username: "u-door"}
	st := StateFor("u-door")
	st.Quest("doorq").Stage = 1

	HandleDoor(c, d, "u-door", "doorq")
	if got := StateFor("u-door").Quest("doorq").Stage; got != 2 {
		t.Fatalf("door-task stage = %d, want 2", got)
	}
	foundProgress := false
	for _, f := range bus.sent["i-d"] {
		if id, op := frameOp(f); id == protocol.PacketQuest && op == QuestProgress {
			foundProgress = true
		}
	}
	if !foundProgress {
		t.Fatal("door progress must emit a Quest Progress frame")
	}

	// Non-door task: no-op.
	st.Quest("doorq").Stage = 0
	before := len(bus.sent["i-d"])
	HandleDoor(c, d, "u-door", "doorq")
	if got := st.Quest("doorq").Stage; got != 0 {
		t.Fatalf("non-door HandleDoor moved to %d, want 0", got)
	}
	if len(bus.sent["i-d"]) != before {
		t.Fatal("non-door HandleDoor must send nothing")
	}

	// Finished + unknown: no-op, no panic.
	st.Quest("doorq").Stage = 2
	HandleDoor(c, d, "u-door", "doorq")
	HandleDoor(c, d, "u-door", "nope")
	HandleDoor(nil, d, "", "doorq")
}

// TestSecretProgressRedact pins the secret leak fix: unfinished secret
// progress omits name/description behind the secret flag; finished restores.
func TestSecretProgressRedact(t *testing.T) {
	d, _, bus, _ := withFixture(t)
	Achs["sec1"] = &AchDef{Key: "sec1", StageCount: 2,
		Raw: AchievementRaw{Name: "Hidden Truth", Description: "Spoilers", Secret: true}}
	c := &fakeConn{instance: "i-sec", username: "u-sec"}
	st := StateFor("u-sec")

	AchProgress(c, d, st, "sec1")
	var first AchievementData
	for _, f := range bus.sent["i-sec"] {
		if id, op := frameOp(f); id == protocol.PacketAchievement && op == AchievementProgress {
			raw, _ := json.Marshal(f[2])
			_ = json.Unmarshal(raw, &first)
		}
	}
	if first.Name != nil || first.Description != nil {
		t.Fatalf("secret unfinished leaks name/desc: %+v", first)
	}
	if first.Secret == nil || !*first.Secret {
		t.Fatalf("secret unfinished must carry the secret flag: %+v", first)
	}

	Finish(c, d, st, "sec1")
	var last AchievementData
	for _, f := range bus.sent["i-sec"] {
		if id, op := frameOp(f); id == protocol.PacketAchievement && op == AchievementProgress {
			raw, _ := json.Marshal(f[2])
			_ = json.Unmarshal(raw, &last)
		}
	}
	if last.Name == nil || *last.Name != "Hidden Truth" {
		t.Fatalf("secret finished must restore name: %+v", last)
	}
}

// recordingAbilities captures grant levels (the shared fake drops them).
type recordingAbilities struct {
	levels map[string]int
}

func (f *recordingAbilities) GrantAbility(_ Conn, username, key string, level int) {
	if f.levels == nil {
		f.levels = map[string]int{}
	}
	f.levels[username+"/"+key] = level
}

// TestAbilityLevelDefault pins abilityLevel || 1 at both grant sites.
func TestAbilityLevelDefault(t *testing.T) {
	d, _, _, _ := withFixture(t)
	rec := &recordingAbilities{}
	d.Abilities = rec
	Quests["abq"] = &Quest{
		Key: "abq", StageCount: 1, NoPrompts: true, NPCs: map[string]bool{"bob": true},
		Raw: Raw{Name: "A", Stages: map[string]StageData{
			"0": {Task: "talk", NPC: "bob", Text: []string{"hi"}, Ability: "dash"},
		}},
	}
	c := &fakeConn{instance: "i-ab", username: "u-ab"}
	st := StateFor("u-ab")
	st.Quest("abq").Stage = 0
	HandleQuestTalk(c, d, st, "abq", "i-ab", "bob")
	if got := rec.levels["u-ab/dash"]; got != 1 {
		t.Fatalf("quest ability level = %d, want 1", got)
	}

	Achs["abach"] = &AchDef{Key: "abach", StageCount: 1,
		Raw: AchievementRaw{Name: "AA", RewardAbility: "blink"}}
	st2 := StateFor("u-ab2")
	c2 := &fakeConn{instance: "i-ab2", username: "u-ab2"}
	AchProgress(c2, d, st2, "abach")
	if got := rec.levels["u-ab2/blink"]; got != 1 {
		t.Fatalf("achievement ability level = %d, want 1", got)
	}
}

// TestMarkDirtySubstage pins dirty-on-substage (old code returned before
// MarkDirty when only subStage moved).
func TestMarkDirtySubstage(t *testing.T) {
	d, store, _, _ := withFixture(t)
	c := &fakeConn{instance: "i-sub", username: "u-sub"}
	st := StateFor("u-sub")
	SetStage(c, d, st, "killq", 1, 0, true)
	dirtyAfterStage := store.dirty["u-sub"]
	ProgressSub(c, d, st, "killq")
	if store.dirty["u-sub"] <= dirtyAfterStage {
		t.Fatal("ProgressSub must mark dirty")
	}
	if got := st.Quest("killq").SubStage; got != 1 {
		t.Fatalf("subStage = %d, want 1", got)
	}
}

// TestCompletedPersistRoundTrip pins the substage-set SQLite round-trip.
func TestCompletedPersistRoundTrip(t *testing.T) {
	d, _, _, _ := withFixture(t)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open memory sqlite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	d.DB = db
	EnsureTables(d)

	st := StateFor("u-csub")
	st.Quest("gateq").Stage, st.Quest("gateq").SubStage = 1, 2
	st.Quest("gateq").Completed = []string{"a", "b"}
	PersistQuests(d, "u-csub")

	ForgetSession("u-csub")
	LoadQuests(d, "u-csub")
	restored := StateFor("u-csub")
	q := restored.Quest("gateq")
	if q.Stage != 1 || q.SubStage != 2 {
		t.Fatalf("restored cursor = %d/%d, want 1/2", q.Stage, q.SubStage)
	}
	if !q.HasCompleted("a") || !q.HasCompleted("b") || len(q.Completed) != 2 {
		t.Fatalf("restored completed = %v, want [a b]", q.Completed)
	}
}

// TestLoadQuestsValidation pins unknown-key skip + negative clamp.
func TestLoadQuestsValidation(t *testing.T) {
	d, _, _, _ := withFixture(t)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open memory sqlite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	d.DB = db
	EnsureTables(d)

	if _, err := db.Exec(`INSERT INTO quests(player,quest,stage,substage,completed) VALUES(?,?,?,?,?)`,
		"u-val", "gateq", -3, -1, ""); err != nil {
		t.Fatalf("seed gateq: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO quests(player,quest,stage,substage,completed) VALUES(?,?,?,?,?)`,
		"u-val", "nope", 5, 0, ""); err != nil {
		t.Fatalf("seed unknown: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO achievements(player,ach,stage) VALUES(?,?,?)`,
		"u-val", "nopeach", 4); err != nil {
		t.Fatalf("seed unknown ach: %v", err)
	}
	LoadQuests(d, "u-val")
	st := StateFor("u-val")
	if got := st.Quest("gateq"); got.Stage != 0 || got.SubStage != 0 {
		t.Fatalf("negative cursor = %d/%d, want 0/0", got.Stage, got.SubStage)
	}
	if _, found := st.Quests["nope"]; found {
		t.Fatal("unknown quest key must be skipped on load")
	}
	if _, found := st.Achs["nopeach"]; found {
		t.Fatal("unknown achievement key must be skipped on load")
	}
}

// TestPointerForwarding pins Entity/Relative legs (old code dropped them).
func TestPointerForwarding(t *testing.T) {
	d, _, bus, _ := withFixture(t)
	c := &fakeConn{instance: "i-p", username: "u-p"}

	SendPointer(c, d, &Pointer{Type: PointerEntity, Instance: "1-2-3"})
	frames := bus.sent["i-p"]
	if len(frames) != 2 {
		t.Fatalf("entity frames = %d, want 2 (Remove + Entity)", len(frames))
	}
	if id, op := frameOp(frames[1]); id != protocol.PacketPointer || op != PointerEntity {
		t.Fatalf("entity frame = [%d,%d], want Pointer Entity", id, op)
	}
	raw, _ := json.Marshal(frames[1][2])
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	if payload["instance"] != "1-2-3" {
		t.Fatalf("entity payload = %v, want instance", payload)
	}

	bus.sent["i-p"] = nil
	SendPointer(c, d, &Pointer{Type: PointerRelative, X: 5, Y: 6})
	frames = bus.sent["i-p"]
	if len(frames) != 2 {
		t.Fatalf("relative frames = %d, want 2", len(frames))
	}
	if id, op := frameOp(frames[1]); id != protocol.PacketPointer || op != PointerRelative {
		t.Fatalf("relative frame = [%d,%d], want Pointer Relative", id, op)
	}

	bus.sent["i-p"] = nil
	SendPointer(c, d, &Pointer{Type: 99})
	if len(bus.sent["i-p"]) != 1 {
		t.Fatalf("unknown type frames = %d, want 1 (Remove only)", len(bus.sent["i-p"]))
	}
}

// TestPopupColour pins the popup colour passthrough (JSON colour wins,
// empty falls back to the legacy green).
func TestPopupColour(t *testing.T) {
	d, _, bus, _ := withFixture(t)
	Quests["popq"] = &Quest{
		Key: "popq", StageCount: 2, NoPrompts: true,
		Raw: Raw{Name: "P", Stages: map[string]StageData{
			"0": {Task: "talk", Popup: &Popup{Title: "T", Text: "x", Colour: "#ff0000"}},
			"1": {Task: "talk"},
		}},
	}
	c := &fakeConn{instance: "i-pop", username: "u-pop"}
	st := StateFor("u-pop")
	SetStage(c, d, st, "popq", 1, 0, true)
	found := false
	for _, f := range bus.sent["i-pop"] {
		if id, op := frameOp(f); id == protocol.PacketNotification && op == NotificationPopup {
			raw, _ := json.Marshal(f[2])
			var nd protocol.NotificationPacketData
			_ = json.Unmarshal(raw, &nd)
			if nd.Colour != nil && *nd.Colour == "#ff0000" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("leaving-stage popup must carry its JSON colour")
	}
}

// TestEchoAchUnknown pins the nil-deref guard (unknown key replies ?/?).
func TestEchoAchUnknown(t *testing.T) {
	d, _, bus, _ := withFixture(t)
	c := &fakeConn{instance: "i-e", username: "u-e"}
	HandleTest(c, d, []byte(`{"m11test":"echo","echo":"ach","key":"nope"}`))
	notices := bus.notices["i-e"]
	if len(notices) != 1 || notices[0] != "m11:ach:nope=?/?" {
		t.Fatalf("echo unknown ach = %v, want ?/? reply", notices)
	}
}
