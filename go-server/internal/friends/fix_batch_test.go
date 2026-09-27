package friends

import (
	"testing"
)

func TestZeroListLazyInit(t *testing.T) {
	var l List
	if !l.Add("bob") {
		t.Fatal("zero List Add(bob) = false, want true (lazy map init)")
	}
	if !l.IsFriend("BOB") {
		t.Fatal("zero List IsFriend(BOB) = false, want true")
	}
	if !l.Block("carol") {
		t.Fatal("zero List Block(carol) = false, want true")
	}
	if !l.IsBlocked("CAROL") {
		t.Fatal("zero List IsBlocked = false, want true")
	}
	if !l.Unblock("carol") {
		t.Fatal("zero List Unblock = false, want true")
	}
	l.Load([]string{"dave"})
	if !l.IsFriend("dave") {
		t.Fatal("zero List Load(dave) missing, want present")
	}
	var nilL *List
	if nilL.IsFriend("x") || nilL.IsBlocked("x") || nilL.Add("x") || nilL.Block("x") || nilL.Unblock("x") || nilL.Remove("x") || nilL.SetStatus("x", true, 1) || nilL.ShouldNotify("x") {
		t.Fatal("nil List must report false, never panic")
	}
	if _, ok := nilL.Info("x"); ok {
		t.Fatal("nil Info = true, want false")
	}
	if _, ok := nilL.OnlineEvent("x", 1); ok {
		t.Fatal("nil OnlineEvent = true, want false")
	}
	if got := nilL.Members(); len(got) != 0 {
		t.Fatalf("nil Members = %v, want empty", got)
	}
}
