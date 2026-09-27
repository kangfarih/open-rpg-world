package guilds

import (
	"testing"
)

func TestMemberKeyCaseNormalization(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Create("Alice", "Knights"); err != nil {
		t.Fatalf("Create(Alice) = %v", err)
	}
	// Same member in different case must hit the one-guild rule, not
	// create a shadow membership.
	if err := r.Invite("ALICE", "BOB"); err != nil {
		t.Fatalf("Invite(ALICE,BOB) = %v, want nil (case-insensitive)", err)
	}
	if err := r.AcceptInvite("bob", "KNIGHTS"); err != nil {
		t.Fatalf("AcceptInvite(bob,KNIGHTS) = %v, want nil", err)
	}
	if _, err := r.GuildOf("BOB"); err != nil {
		t.Fatalf("GuildOf(BOB) = %v, want guild (case-insensitive)", err)
	}
	if err := r.Kick("alice", "BoB"); err != nil {
		t.Fatalf("Kick(alice,BoB) = %v, want nil", err)
	}
	if _, err := r.GuildOf("bob"); err == nil {
		t.Fatal("GuildOf(bob) after kick = found, want ErrNotMember")
	}
}

func TestCreateCaseNormalization(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Create("Alice", "Knights"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create("alice", "Mages"); err != ErrAlreadyInGuild {
		t.Fatalf("Create(alice) second = %v, want ErrAlreadyInGuild (owner normalized)", err)
	}
	if _, err := r.Create("bob", "KNIGHTS"); err != ErrExists {
		t.Fatalf("Create duplicate name case = %v, want ErrExists", err)
	}
}
