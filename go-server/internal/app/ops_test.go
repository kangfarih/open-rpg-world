package app

import (
	"testing"

	"rpg-world-server/internal/console"
)

// Console trio (console.ts:117-147 parity): rank strips resolve online and
// report 'Player is not logged in.' offline; unban clears without drops and
// logs the exact TS text (which reads 'banned' even for unban).
//
// NOTE (UnbanIP wording): the "has been banned" text on the unban path is
// verbatim upstream console.ts (ipban/unbanip share one log line) —
// asserted exactly here so a future "fix" of the wording shows up as a
// test failure, not silent drift.
func TestConsoleTrioStripAndUnban(t *testing.T) {
	old := opsDeps
	t.Cleanup(func() { opsDeps = old })

	var stripped []string
	var unbanned []string
	ConfigureOps(OpsDeps{
		StripPlayerRank: func(username string) (string, bool) {
			if username == "ghost" {
				return "", false
			}
			stripped = append(stripped, username)
			return username, true
		},
		UnbanIP: func(ip string) { unbanned = append(unbanned, ip) },
	})
	h := &opsConsole{}

	for _, cmd := range []string{"/removeadmin bob", "/removemod bob"} {
		if got := console.Exec(h, cmd); got == "" {
			t.Fatalf("Exec(%q) = empty, want strip confirmation", cmd)
		}
	}
	if len(stripped) != 2 || stripped[0] != "bob" || stripped[1] != "bob" {
		t.Fatalf("stripped = %v, want [bob bob]", stripped)
	}
	if got := console.Exec(h, "/removeadmin ghost"); got != "Player is not logged in." {
		t.Fatalf("offline strip = %q, want 'Player is not logged in.'", got)
	}
	if got := console.Exec(h, "/removemod ghost"); got != "Player is not logged in." {
		t.Fatalf("offline strip = %q, want 'Player is not logged in.'", got)
	}

	if got := console.Exec(h, "/unbanip 1.2.3.4"); got != "IP 1.2.3.4 has been banned." {
		t.Fatalf("unban = %q, want exact TS 'IP 1.2.3.4 has been banned.'", got)
	}
	if len(unbanned) != 1 || unbanned[0] != "1.2.3.4" {
		t.Fatalf("unbanned = %v, want [1.2.3.4]", unbanned)
	}
	if got := console.Exec(h, "/unbanip"); got != "Malformed command, expected /unbanip <ip>" {
		t.Fatalf("bare unban = %q, want malformed text", got)
	}
}
