package chat

import (
	"testing"
	"time"
)

// TestSanitize mirrors the m7 e2e leg: HTML-ish text is entity-escaped and
// NULs are dropped (sanitizer.escape/sanitize parity).
func TestSanitize(t *testing.T) {
	cases := map[string]string{
		`<b>&bold</b>`:  `&lt;b&gt;&amp;bold&lt;/b&gt;`,
		`"quoted"`:      `&quot;quoted&quot;`,
		"it's":          `it&#x27;s`,
		"a\x00b":        `ab`,
		"plain hello":   `plain hello`,
		"":              ``,
		"\x00":          ``,
		`<script>alert`: `&lt;script&gt;alert`,
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Fatalf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFormatName ports Utils.formatName: capitalize every word.
func TestFormatName(t *testing.T) {
	cases := map[string]string{
		"m7tester":  "M7tester",
		"bob smith": "Bob Smith",
		"ALICE":     "Alice",
		"":          "",
		"a":         "A",
		"x1 y2":     "X1 Y2",
	}
	for in, want := range cases {
		if got := FormatName(in); got != want {
			t.Fatalf("FormatName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBucketExhaustRefill pins the token-bucket behavior: burst of 3, then
// silent reject, then refill of one message per 2 seconds with clamping.
func TestBucketExhaustRefill(t *testing.T) {
	base := time.Now()
	var tokens float64
	var refill time.Time

	// First message seeds a full bucket.
	ok, tokens, refill := AllowBucket(tokens, refill, base)
	if !ok || tokens != 2 {
		t.Fatalf("seeded Allow = %v tokens = %v, want true/2", ok, tokens)
	}
	ok, tokens, refill = AllowBucket(tokens, refill, base)
	if !ok || tokens != 1 {
		t.Fatalf("second Allow = %v tokens = %v, want true/1", ok, tokens)
	}
	ok, tokens, refill = AllowBucket(tokens, refill, base)
	if !ok || tokens != 0 {
		t.Fatalf("third Allow = %v tokens = %v, want true/0", ok, tokens)
	}
	// Burst exhausted: rejected with no time passing.
	if ok, _, _ := AllowBucket(tokens, refill, base); ok {
		t.Fatal("exhausted bucket must reject")
	}
	// After 2s exactly one token refills.
	ok, tokens, refill = AllowBucket(tokens, refill, base.Add(2*time.Second))
	if !ok || tokens != 0 {
		t.Fatalf("refilled Allow = %v tokens = %v, want true/0", ok, tokens)
	}
	// Long idle clamps back to a full bucket, never above.
	ok, tokens, refill = AllowBucket(tokens, refill, base.Add(time.Hour))
	if !ok || tokens != BucketSize-1 {
		t.Fatalf("idle Allow = %v tokens = %v, want true/%v", ok, tokens, BucketSize-1)
	}
}

// TestBucketMethod covers the Bucket.Allow stateful wrapper over the same math.
func TestBucketMethod(t *testing.T) {
	b := &Bucket{}
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !b.Allow(now) {
			t.Fatalf("burst message %d must pass", i+1)
		}
	}
	if b.Allow(now) {
		t.Fatal("fourth burst message must be rejected")
	}
	if !b.Allow(now.Add(2 * time.Second)) {
		t.Fatal("message after 2s refill must pass")
	}
}

// TestGlobalCooldown pins the rank-based global cooldowns (player.ts
// getGlobalChatCooldown parity): 60min default, 5s for mods/admins,
// 15-55min patron tiers, duration floored at 1 minute.
func TestGlobalCooldown(t *testing.T) {
	if CooldownFor(RankNone) != 60*60_000 {
		t.Fatalf("default cooldown = %d, want 3600000", CooldownFor(RankNone))
	}
	if CooldownFor(RankModerator) != 5000 || CooldownFor(RankAdmin) != 5000 || CooldownFor(RankHollowAdmin) != 5000 {
		t.Fatal("mod/admin cooldown must be 5000")
	}
	for rank, want := range map[int]int64{
		RankTierOne: 55 * 60_000, RankTierTwo: 50 * 60_000,
		RankTierThree: 45 * 60_000, RankTierFour: 40 * 60_000,
		RankTierFive: 35 * 60_000, RankTierSix: 30 * 60_000,
		RankTierSeven: 15 * 60_000,
	} {
		if got := CooldownFor(rank); got != want {
			t.Fatalf("tier rank %d cooldown = %d, want %d", rank, got, want)
		}
	}
	// Unranked variants stay on the 60min default.
	for _, rank := range []int{RankVeteran, RankPatron, RankArtist, RankCheater, RankBooster} {
		if got := CooldownFor(rank); got != 60*60_000 {
			t.Fatalf("rank %d cooldown = %d, want 3600000", rank, got)
		}
	}
	now := int64(10_000_000)
	if !GlobalReady(RankNone, now-60*60_000-1, now) {
		t.Fatal("default rank must be ready after 60min")
	}
	if GlobalReady(RankNone, now-60*60_000, now) {
		t.Fatal("default rank must not be ready at exactly 60min (strict >)")
	}
	if !GlobalReady(RankAdmin, now-5001, now) {
		t.Fatal("admin must be ready after 5s")
	}
	if !GlobalReady(RankTierSeven, now-15*60_000-1, now) {
		t.Fatal("tier seven must be ready after 15min")
	}
	if got := GlobalDuration(RankNone, now-1000, now); got != 59 {
		t.Fatalf("duration = %d, want 59", got)
	}
	if got := GlobalDuration(RankNone, now-1000, now-1000+30_000); got != 59 {
		t.Fatalf("near-expiry duration = %d, want 59", got)
	}
	if got := GlobalDuration(RankModerator, now-1000, now); got != 1 {
		t.Fatalf("mod near-expiry duration floors at 1, got %d", got)
	}
}

// TestSplitCommand pins Commands.parse parity: single-prefix strip, space
// split, empty command rejected.
func TestSplitCommand(t *testing.T) {
	cmd, args, ok := SplitCommand("/players")
	if !ok || cmd != "players" || len(args) != 0 {
		t.Fatalf("SplitCommand(/players) = %q %v %v", cmd, args, ok)
	}
	cmd, args, ok = SplitCommand(";global hello world")
	if !ok || cmd != "global" || len(args) != 2 || args[1] != "world" {
		t.Fatalf("SplitCommand(;global hello world) = %q %v %v", cmd, args, ok)
	}
	if _, _, ok := SplitCommand("/"); ok {
		t.Fatal("bare / must not parse")
	}
	if _, _, ok := SplitCommand(""); ok {
		t.Fatal("empty text must not parse")
	}
	// Single-strip parity (TS slice(1)): a double prefix keeps the second
	// char as part of the command word.
	if cmd, _, ok := SplitCommand("/;cmd"); !ok || cmd != ";cmd" {
		t.Fatalf("SplitCommand(/;cmd) = %q %v, want %q true", cmd, ok, ";cmd")
	}
}

// TestParsePrivateMessage pins the /pm resolution: the username is the text
// between the `*` markers (lowercased) and the delivered message is every
// block after the username's blocks (no `*wrapper*` kept).
func TestParsePrivateMessage(t *testing.T) {
	user, msg, ok := ParsePrivateMessage([]string{"*bob*", "hello"})
	if !ok || user != "bob" || msg != "hello" {
		t.Fatalf("pm = %q %q %v", user, msg, ok)
	}
	user, msg, ok = ParsePrivateMessage([]string{"*bob", "smith*", "hi"})
	if !ok || user != "bob smith" || msg != "hi" {
		t.Fatalf("multiword pm = %q %q %v", user, msg, ok)
	}
	if _, _, ok := ParsePrivateMessage([]string{"hello"}); ok {
		t.Fatal("pm without markers must fail")
	}
	if _, _, ok := ParsePrivateMessage([]string{"*"}); ok {
		t.Fatal("pm with empty username must fail")
	}
}

// TestParseTeleportArgs pins /teleport x y integer parsing.
func TestParseTeleportArgs(t *testing.T) {
	x, y, ok := ParseTeleportArgs([]string{"100", "96"})
	if !ok || x != 100 || y != 96 {
		t.Fatalf("teleport = %d %d %v", x, y, ok)
	}
	if _, _, ok := ParseTeleportArgs([]string{"100"}); ok {
		t.Fatal("short teleport must fail")
	}
	if _, _, ok := ParseTeleportArgs([]string{"a", "b"}); ok {
		t.Fatal("non-numeric teleport must fail")
	}
}

// TestCommandTables pins the rank/command tables: every handled word, the
// g/gc/global and pm/msg aliases, and the moderator rank gate.
func TestCommandTables(t *testing.T) {
	for word, want := range map[string]PlayerCommand{
		"players": CmdPlayers, "coords": CmdCoords, "ping": CmdPing,
		"g": CmdGlobal, "gc": CmdGlobal, "global": CmdGlobal,
		"pm": CmdPM, "msg": CmdPM,
	} {
		got, ok := ClassifyPlayer(word)
		if !ok || got != want {
			t.Fatalf("ClassifyPlayer(%q) = %v %v, want %v true", word, got, ok, want)
		}
	}
	for _, word := range []string{"", "teleport", "guild", "mute", "dance"} {
		if _, ok := ClassifyPlayer(word); ok {
			t.Fatalf("ClassifyPlayer(%q) must not hit the m7 player table", word)
		}
	}
	if got, ok := ClassifyMod("teleport"); !ok || got != ModTeleport {
		t.Fatalf("ClassifyMod(teleport) = %v %v", got, ok)
	}
	if _, ok := ClassifyMod("players"); ok {
		t.Fatal("ClassifyMod(players) must miss")
	}
	if ModAllowed(RankNone) || !ModAllowed(RankModerator) || !ModAllowed(RankAdmin) {
		t.Fatal("moderator gate must pass at rank >= Moderator only")
	}
}

// TestOutcomeTexts pins the exact user-visible strings and source lines.
func TestOutcomeTexts(t *testing.T) {
	if got := PlayersSummary(1); got != "There is currently 1 person online." {
		t.Fatalf("players singular = %q", got)
	}
	if got := PlayersSummary(3); got != "There are currently 3 people online." {
		t.Fatalf("players plural = %q", got)
	}
	if got := CoordsText(100, 96); got != "x: 100 y: 96" {
		t.Fatalf("coords = %q", got)
	}
	if got := GlobalCooldownNotice(2); got != "misc:CANNOT_GLOBAL_CHAT_MINUTES;duration=2" {
		t.Fatalf("cooldown notice = %q", got)
	}
	if got := PMOffline("bob"); got != "misc:NOT_ONLINE;username=bob" {
		t.Fatalf("offline = %q", got)
	}
	if got := MutedText(); got != "You have been muted." {
		t.Fatalf("muted = %q", got)
	}
	from, to := PMSources("Alice", "Bob")
	if from != "[From Alice]" || to != "[To Bob]" {
		t.Fatalf("pm sources = %q %q", from, to)
	}
	if got := GlobalSource("Alice"); got != "[Global] Alice" {
		t.Fatalf("global source = %q", got)
	}
	if got := DisplayName("bob", RankAdmin); got != "[Admin] Bob" {
		t.Fatalf("ranked display = %q", got)
	}
	if got := DisplayName("bob", RankNone); got != "Bob" {
		t.Fatalf("plain display = %q", got)
	}
	if got := ResolveColour(RankAdmin, ""); got != GlobalColour {
		t.Fatalf("ranked colour = %q", got)
	}
	if got := ResolveColour(RankNone, ""); got != "" {
		t.Fatalf("plain colour must stay empty, got %q", got)
	}
	if !HasVisibleText("hi") || HasVisibleText("   ") {
		t.Fatal("visible-text check inverted")
	}
	if !IsCommand("/x") || !IsCommand(";x") || IsCommand("hi") {
		t.Fatal("command-prefix check inverted")
	}
}
