package account

import (
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

// testDB opens a fresh in-memory SQLite with the account table created.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	EnsureTables(db)
	EnsureTables(db) // idempotent: second call must not fail
	return db
}

// TestValidationParity pins the TS Utils rules the login handler gates on.
func TestValidationParity(t *testing.T) {
	usernames := map[string]bool{
		"alice":       true,
		"Alice 99":    true,
		"under_score": true,
		"bob!":        false, // '!' is outside [\w ]
		"":            false,
		"a-b":         false, // '-' is outside [\w ]
		"jóse":        false, // non-latin
	}
	for name, want := range usernames {
		if got := ValidUsername(name); got != want {
			t.Fatalf("ValidUsername(%q) = %v, want %v", name, got, want)
		}
	}

	if !ValidPassword("abc") || !ValidPassword(string(make([]byte, 64))) {
		t.Fatal("ValidPassword rejected a boundary-length password")
	}
	if ValidPassword("ab") || ValidPassword(string(make([]byte, 65))) {
		t.Fatal("ValidPassword accepted an out-of-range password")
	}

	if !ValidEmail("user@example.com") {
		t.Fatal("ValidEmail rejected user@example.com")
	}
	if ValidEmail("nope") || ValidEmail("") || ValidEmail("@example.com") {
		t.Fatal("ValidEmail accepted an invalid address")
	}
}

// TestFormatNameParity pins Utils.formatName (player.ts display name).
func TestFormatNameParity(t *testing.T) {
	cases := map[string]string{
		"guest1":      "Guest1",
		"alice":       "Alice",
		"the king":    "The King",
		"ALL CAPS":    "All Caps",
		"under_score": "Under_score",
		"":            "",
	}
	for in, want := range cases {
		if got := FormatName(in); got != want {
			t.Fatalf("FormatName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHashVerify: a correct password verifies, a wrong one does not, two hashes
// of the same password differ (fresh salt), and an unknown algorithm is refused
// rather than silently compared.
func TestHashVerify(t *testing.T) {
	hash, salt, iterations, algo, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if algo != AlgoPBKDF2 || iterations != DefaultIterations || len(salt) != SaltLen {
		t.Fatalf("hash params = %q/%d/%d, want %q/%d/%d", algo, iterations, len(salt), AlgoPBKDF2, DefaultIterations, SaltLen)
	}
	if !VerifyPassword("hunter2", hash, salt, iterations, algo) {
		t.Fatal("VerifyPassword rejected the correct password")
	}
	if VerifyPassword("hunter3", hash, salt, iterations, algo) {
		t.Fatal("VerifyPassword accepted a wrong password")
	}
	if VerifyPassword("hunter2", hash, salt, iterations, "bcrypt") {
		t.Fatal("VerifyPassword accepted an unknown algorithm tag")
	}

	hash2, salt2, _, _, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword (2): %v", err)
	}
	if string(hash) == string(hash2) {
		t.Fatal("two hashes of the same password are identical (salt not fresh)")
	}
	if string(salt) == string(salt2) {
		t.Fatal("salt reused across hashes")
	}
}

// TestCreateDuplicateAndAuthenticate covers the register/login split the
// handler maps onto 'userexists' / 'emailexists' / 'invalidlogin'.
func TestCreateDuplicateAndAuthenticate(t *testing.T) {
	db := testDB(t)

	a, err := Create(db, "Alice", "hunter2", "alice@example.com")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Username != "alice" {
		t.Fatalf("stored username = %q, want lowercased %q", a.Username, "alice")
	}
	if a.ID == "" {
		t.Fatal("Create returned an empty id")
	}

	if _, err := Create(db, "alice", "hunter2", "other@example.com"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate username err = %v, want ErrExists", err)
	}
	if _, err := Create(db, "bob", "hunter2", "ALICE@example.com"); !errors.Is(err, ErrEmailExists) {
		t.Fatalf("duplicate email err = %v, want ErrEmailExists", err)
	}

	if _, ok := Authenticate(db, "ALICE", "hunter2"); !ok {
		t.Fatal("Authenticate rejected case-insensitive username")
	}
	if _, ok := Authenticate(db, "alice", "wrong"); ok {
		t.Fatal("Authenticate accepted a wrong password")
	}
	if _, ok := Authenticate(db, "nobody", "hunter2"); ok {
		t.Fatal("Authenticate accepted an unknown user")
	}

	if !Exists(db, "alice") || Exists(db, "nobody") {
		t.Fatal("Exists disagrees with the stored row")
	}
}

// TestResetTokenLifecycle: unknown emails get no token, known ones do, the
// token is single-use for its window, and the new password takes effect.
func TestResetTokenLifecycle(t *testing.T) {
	db := testDB(t)
	if _, err := Create(db, "alice", "hunter2", "alice@example.com"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, _, ok := CreateResetToken(db, "nobody@example.com"); ok {
		t.Fatal("CreateResetToken minted a token for an unknown email")
	}

	id, token, ok := CreateResetToken(db, "alice@example.com")
	if !ok || id == "" || token == "" {
		t.Fatalf("CreateResetToken ok=%v id=%q token=%q", ok, id, token)
	}

	if ResetPassword(db, id, "wrong-token", "newpass") {
		t.Fatal("ResetPassword accepted a wrong token")
	}
	if ResetPassword(db, id, token, "ab") {
		t.Fatal("ResetPassword accepted a too-short password")
	}
	if !ResetPassword(db, id, token, "newpass") {
		t.Fatal("ResetPassword rejected the correct token")
	}
	if _, ok := Authenticate(db, "alice", "newpass"); !ok {
		t.Fatal("new password does not authenticate")
	}
	if _, ok := Authenticate(db, "alice", "hunter2"); ok {
		t.Fatal("old password still authenticates")
	}

	// The token is single-use: SetPassword clears it.
	if ResetPassword(db, id, token, "thirdpass") {
		t.Fatal("reset token stayed valid after use")
	}
}

// TestResetTokenExpiry: an expired token is refused (TS compares
// resetToken.expiration < Date.now()).
func TestResetTokenExpiry(t *testing.T) {
	db := testDB(t)
	if _, err := Create(db, "bob", "hunter2", "bob@example.com"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, token, ok := CreateResetToken(db, "bob@example.com")
	if !ok {
		t.Fatal("CreateResetToken failed for a known email")
	}
	if _, err := db.Exec(`UPDATE accounts SET reset_token_exp=? WHERE id=?`, 1, id); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	if ResetPassword(db, id, token, "newpass") {
		t.Fatal("ResetPassword accepted an expired token")
	}
}

// TestNilDBSafe: every entry point tolerates a nil handle (dbConn == nil parity
// for servers booted without persistence).
func TestNilDBSafe(t *testing.T) {
	var db DB
	if EnsureTables(db); false {
		t.Fatal("unreachable")
	}
	if _, ok, err := ByUsername(db, "alice"); ok || err != nil {
		t.Fatal("ByUsername(nil) not empty")
	}
	if Exists(db, "alice") {
		t.Fatal("Exists(nil) = true")
	}
	if _, ok := Authenticate(db, "alice", "x"); ok {
		t.Fatal("Authenticate(nil) = true")
	}
	if _, _, ok := CreateResetToken(db, "a@b.com"); ok {
		t.Fatal("CreateResetToken(nil) = true")
	}
	if ResetPassword(db, "id", "token", "password") {
		t.Fatal("ResetPassword(nil) = true")
	}
}
