package server

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"rpg-world-server/internal/account"
	gnet "rpg-world-server/internal/net"
)

// testGuestConn builds an authenticated guest session record. Username lives on
// the embedded transport Conn, so the session needs one.
func testGuestConn(key string) *playerConn {
	conn := gnet.NewConn(nil, "p-test")
	conn.Username = key
	return &playerConn{Conn: conn, Guest: true, loggedIn: true}
}

// loginTestDB opens a fresh in-memory SQLite with the accounts table created.
func loginTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	account.EnsureTables(db)
	return db
}

// plainEnv is the credential-checking login env (SKIP_DATABASE off).
func plainEnv(db account.DB) loginEnv {
	return loginEnv{DB: db, DisableRegister: false}
}

// TestResolveLoginOpcodeParity pins incoming.ts handleLogin's decision tree.
func TestResolveLoginOpcodeParity(t *testing.T) {
	db := loginTestDB(t)
	if _, err := account.Create(db, "Taken", "secret", "taken@example.com"); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	cases := []struct {
		name    string
		req     loginRequest
		env     loginEnv
		user    string
		guest   bool
		reason  string
		wantErr bool
	}{
		{
			name: "guest with no username mints a guest name",
			req:  loginRequest{Opcode: LoginOpcodeGuest},
			env:  plainEnv(db), guest: true, user: "guest0",
		},
		{
			name: "guest ignores a supplied username",
			req:  loginRequest{Opcode: LoginOpcodeGuest, Username: "alice", Password: "secret"},
			env:  plainEnv(db), guest: true, user: "guest1",
		},
		{
			name: "login with a short password is refused before anything else",
			req:  loginRequest{Opcode: LoginOpcodeLogin, Username: "alice", Password: "ab"},
			env:  plainEnv(db), reason: gnet.ReasonInvalidPassword,
		},
		{
			name: "login for an unknown user is invalidlogin",
			req:  loginRequest{Opcode: LoginOpcodeLogin, Username: "alice", Password: "secret"},
			env:  plainEnv(db), reason: gnet.ReasonInvalidLogin,
		},
		{
			name: "login with a wrong password is invalidlogin (no enumeration)",
			req:  loginRequest{Opcode: LoginOpcodeLogin, Username: "taken", Password: "wrong"},
			env:  plainEnv(db), reason: gnet.ReasonInvalidLogin,
		},
		{
			name: "login with the right password is accepted",
			req:  loginRequest{Opcode: LoginOpcodeLogin, Username: "Taken", Password: "secret"},
			env:  plainEnv(db), user: "taken",
		},
		{
			name:   "an online username is rejected as loggedin before credentials",
			req:    loginRequest{Opcode: LoginOpcodeLogin, Username: "taken", Password: "secret"},
			env:    loginEnv{DB: db, Online: func(string) bool { return true }},
			reason: gnet.ReasonLoggedIn,
		},
		{
			name:   "register is refused when registration is disabled",
			req:    loginRequest{Opcode: LoginOpcodeRegister, Username: "newbie", Password: "secret"},
			env:    loginEnv{DB: db, DisableRegister: true},
			reason: gnet.ReasonDisabledRegister,
		},
		{
			name: "register of an existing username is userexists",
			req:  loginRequest{Opcode: LoginOpcodeRegister, Username: "taken", Password: "secret"},
			env:  plainEnv(db), reason: gnet.ReasonUserExists,
		},
		{
			name: "register reusing an email is emailexists",
			req: loginRequest{
				Opcode: LoginOpcodeRegister, Username: "newbie",
				Password: "secret", Email: "TAKEN@example.com",
			},
			env: plainEnv(db), reason: gnet.ReasonEmailExists,
		},
		{
			name: "register with an illegal username is invalidinput",
			req:  loginRequest{Opcode: LoginOpcodeRegister, Username: "bad-name!", Password: "secret"},
			env:  plainEnv(db), reason: gnet.ReasonInvalidInput,
		},
		{
			name: "register with a malformed email is invalidinput",
			req:  loginRequest{Opcode: LoginOpcodeRegister, Username: "newbie", Password: "secret", Email: "nope"},
			env:  plainEnv(db), reason: gnet.ReasonInvalidInput,
		},
		{
			name: "register without a username is invalidinput",
			req:  loginRequest{Opcode: LoginOpcodeRegister, Password: "secret"},
			env:  plainEnv(db), reason: gnet.ReasonInvalidInput,
		},
		{
			name: "register creates the account",
			req:  loginRequest{Opcode: LoginOpcodeRegister, Username: "Newbie", Password: "secret"},
			env:  plainEnv(db), user: "newbie",
		},
		{
			name: "skip-database accepts any opcode without touching the store",
			req:  loginRequest{Opcode: LoginOpcodeLogin, Username: "whoever", Password: "secret"},
			env:  loginEnv{DB: db, SkipDatabase: true}, user: "whoever",
		},
		{
			name: "skip-database still runs the duplicate-session check",
			req:  loginRequest{Opcode: LoginOpcodeLogin, Username: "whoever", Password: "secret"},
			env: loginEnv{
				DB: db, SkipDatabase: true,
				Online: func(string) bool { return true },
			},
			reason: gnet.ReasonLoggedIn,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveLogin(tc.req, tc.env)
			if got.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.reason)
			}
			if tc.reason != "" {
				return
			}
			if tc.user != "" && got.Username != tc.user {
				t.Fatalf("username = %q, want %q", got.Username, tc.user)
			}
			if got.Guest != tc.guest {
				t.Fatalf("guest = %v, want %v", got.Guest, tc.guest)
			}
		})
	}

	// Register actually persisted the account (and hashed it).
	if _, ok := account.Authenticate(db, "newbie", "secret"); !ok {
		t.Fatal("registered account does not authenticate")
	}
	if account.ValidPassword("") {
		t.Fatal("empty password must be invalid")
	}
}

// TestGuestNamesAreUnique pins Utils.counter++ (guest0, guest1, ...).
func TestGuestNamesAreUnique(t *testing.T) {
	a := resolveLogin(loginRequest{Opcode: LoginOpcodeGuest}, loginEnv{})
	b := resolveLogin(loginRequest{Opcode: LoginOpcodeGuest}, loginEnv{})
	if a.Username == b.Username {
		t.Fatalf("two guest logins shared the name %q", a.Username)
	}
	if !a.Guest || !b.Guest {
		t.Fatal("guest sessions must be flagged")
	}
}

// TestUpgradeGuestCarriesLiveState is the phase-7 contract: a guest becomes a
// real account mid-session without losing position, inventory or progress, and
// the new identity persists from then on.
func TestUpgradeGuestCarriesLiveState(t *testing.T) {
	oldDB := dbConn
	db := loginTestDB(t)
	dbConn = db
	t.Cleanup(func() { dbConn = oldDB })

	const guestKey = "guestX"
	guestKeys.Store(guestKey, struct{}{})
	pstateMu.Lock()
	st := &playerState{Level: 7}
	pstates[guestKey] = st
	pstateMu.Unlock()
	t.Cleanup(func() {
		guestKeys.Delete(guestKey)
		guestKeys.Delete("upgraded")
		pstateMu.Lock()
		delete(pstates, guestKey)
		delete(pstates, "upgraded")
		pstateMu.Unlock()
	})

	c := testGuestConn(guestKey)
	name, reason := upgradeGuest(c, loginRequest{Username: "Upgraded", Password: "secret"})
	if reason != "" {
		t.Fatalf("upgrade refused: %s", reason)
	}
	if name != "upgraded" || c.Username != "upgraded" || c.Guest {
		t.Fatalf("upgrade left c = {user:%q guest:%v}, name %q", c.Username, c.Guest, name)
	}
	if !account.Exists(db, "upgraded") {
		t.Fatal("upgraded identity has no account row")
	}
	// Live state moved with the session, and it persists now.
	pstateMu.Lock()
	moved, hadOld := pstates["upgraded"], pstates[guestKey]
	pstateMu.Unlock()
	if moved != st || hadOld != nil {
		t.Fatalf("state not re-keyed (moved=%p hadOld=%p)", moved, hadOld)
	}
	if isGuestKey("upgraded") {
		t.Fatal("upgraded identity must persist")
	}

	// A second upgrade on an account session is refused like a second Login.
	if _, r := upgradeGuest(c, loginRequest{Username: "other", Password: "secret"}); r != gnet.ReasonLoggedIn {
		t.Fatalf("re-upgrade reason = %q, want %q", r, gnet.ReasonLoggedIn)
	}
	// A guest socket that never logged in cannot upgrade.
	if _, r := upgradeGuest(&playerConn{Guest: true}, loginRequest{Username: "other", Password: "secret"}); r != gnet.ReasonInvalidLogin {
		t.Fatalf("pre-login upgrade reason = %q, want %q", r, gnet.ReasonInvalidLogin)
	}
}

// TestUpgradeGuestRefusesTakenName pins that the upgrade reuses the Register
// validation instead of inventing a second rulebook.
func TestUpgradeGuestRefusesTakenName(t *testing.T) {
	oldDB := dbConn
	db := loginTestDB(t)
	dbConn = db
	t.Cleanup(func() { dbConn = oldDB })

	if _, err := account.Create(db, "taken", "secret", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := testGuestConn("guestY")
	if _, r := upgradeGuest(c, loginRequest{Username: "Taken", Password: "secret"}); r != gnet.ReasonUserExists {
		t.Fatalf("reason = %q, want %q", r, gnet.ReasonUserExists)
	}
	if c.Username != "guestY" || !c.Guest {
		t.Fatal("a refused upgrade must leave the session untouched")
	}
}
