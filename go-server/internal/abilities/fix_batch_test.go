package abilities

import (
	"encoding/json"
	"testing"
)

func TestHandleAbilityNilConnNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("HandleAbility(nil, ...) panicked: %v", r)
		}
	}()
	HandleAbility(nil, []byte(`{"opcode":4,"key":"run","index":1}`))
	HandleAbility(nil, []byte(`{"opcode":3,"key":"run"}`))
}

func TestQuickSlotStoresSlot(t *testing.T) {
	old := sdeps
	t.Cleanup(func() { sdeps = old })
	path := realData(t)
	ConfigureSessions(SessionDeps{
		DataPath: func(name string) string {
			if name == "abilities" {
				return path
			}
			return name
		},
		Broadcast: func(frames ...[]any) {},
	})
	if loadRegistry() == nil {
		t.Skip("abilities.json not present")
	}
	c := &Conn{
		Instance: "inst-fix",
		Username: "fixuser",
		Send:     func(...[]any) {},
		Notify:   func(string) {},
	}
	t.Cleanup(func() { ResetAbilities(c.Username) })
	GrantAbility(c, c.Username, "run", 1)
	raw, _ := json.Marshal(map[string]any{"opcode": AbilityQuickSlot, "key": "run", "index": 3})
	HandleAbility(c, raw)
	abMu.Lock()
	got := abQuick[c.Username]["run"]
	abMu.Unlock()
	if got != 3 {
		t.Fatalf("quickSlot = %d, want 3", got)
	}
	// Nil index is a no-op, not a zero write.
	raw, _ = json.Marshal(map[string]any{"opcode": AbilityQuickSlot, "key": "run"})
	HandleAbility(c, raw)
	abMu.Lock()
	got = abQuick[c.Username]["run"]
	abMu.Unlock()
	if got != 3 {
		t.Fatalf("quickSlot after nil index = %d, want still 3", got)
	}
}

func TestTestHandlerManaClamp(t *testing.T) {
	ConfigureSessions(SessionDeps{Test: true})
	c := &Conn{
		Instance: "inst-mana",
		Username: "manauser",
		Send:     func(...[]any) {},
		Notify:   func(string) {},
	}
	raw, _ := json.Marshal(map[string]any{"abtest": "mana", "value": 9999})
	TestHandler(c, raw)
	expectedMax := manaMaxForInstance(c.Instance)
	if got := ManaFor(c.Instance); got != expectedMax {
		t.Fatalf("mana after 9999 = %d, want clamp %d", got, expectedMax)
	}
	raw, _ = json.Marshal(map[string]any{"abtest": "mana", "value": -5})
	TestHandler(c, raw)
	if got := ManaFor(c.Instance); got != 0 {
		t.Fatalf("mana after -5 = %d, want clamp 0", got)
	}
}
