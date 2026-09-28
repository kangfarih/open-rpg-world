// Package account — the identity layer (TS parity for the MongoDB account
// half of the server, reimplemented over the process's SQLite store).
//
// What TS has (read-only reference):
//
//   - packages/common/database/mongodb/mongodb.ts  login/register/exists,
//     createResetToken/resetPassword, IP bans
//   - packages/common/database/mongodb/creator.ts  verifyPlayer,
//     getPlayerWithHash (bcrypt via Utils.hash)
//   - packages/common/util/utils.ts                isValidUsername,
//     isValidPassword, isEmail, formatName
//   - packages/server/src/game/entity/character/player/incoming.ts
//     handleLogin: the opcode switch (Login/Register/Guest) and every
//     connection.reject reason
//
// What this package mirrors, and what it deliberately does not:
//
//   - Behaviour is mirrored exactly: the same validation rules, the same
//     reject reasons, the same anti-enumeration answer on password reset, and
//     the same one-hour reset-token lifetime.
//   - The HASH ALGORITHM is not mirrored. TS uses bcrypt; this port uses
//     PBKDF2-HMAC-SHA256 from the standard library (crypto/pbkdf2, Go 1.24+).
//     There is no shared database and no stored-hash interop, so the property
//     that matters is the one both share: plaintext passwords are never
//     stored, comparisons are constant-time, and every hash carries its own
//     salt and work factor.
//   - Storage is the existing SQLite file, one `accounts` table added
//     expand-only (see internal/persist EnsureSchema + CurrentSchemaVersion).
//
// Locking: every exported function takes a *sql.DB-shaped DB and issues its own
// SQL. The caller (internal/server) holds its dbMu across the call, matching
// the other subsystem direct-table seams (quests, abilities, social, m13
// flags).
package account

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"regexp"
	"strings"
	"time"
)

// DB is the SQLite handle the account table lives on. *sql.DB satisfies it; a
// nil DB disables persistence (the caller's dbConn == nil parity).
type DB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Hash parameters. BcryptCost has no Go analogue here: the work factor is the
// iteration count.
const (
	// AlgoPBKDF2 is the stored algorithm tag, so a future migration can add a
	// second algorithm without guessing at row shapes.
	AlgoPBKDF2 = "pbkdf2-sha256"
	// DefaultIterations is the PBKDF2 work factor (OWASP's 2023 floor for
	// PBKDF2-HMAC-SHA256).
	DefaultIterations = 210_000
	// SaltLen is the per-account random salt length in bytes.
	SaltLen = 16
	// KeyLen is the derived-key length in bytes.
	KeyLen = 32
	// ResetTokenTTL mirrors the TS one-hour reset window
	// (mongodb.ts createResetToken: expiration = Date.now() + 60*60*1000).
	ResetTokenTTL = time.Hour
	// ResetTokenLen is the plaintext reset-token length in bytes (TS
	// crypto.randomBytes(32).toString('hex')).
	ResetTokenLen = 32
)

// Account is one identity row.
type Account struct {
	ID        string
	Username  string
	Email     string
	CreatedAt int64
}

// ErrExists is returned by Create when the username is already taken.
var ErrExists = errors.New("account: username exists")

// ErrEmailExists is returned by Create when the email is already registered.
var ErrEmailExists = errors.New("account: email exists")

// AccountsDDL is the expand-only table for identities. The reset-token columns
// carry defaults so an older binary never names them.
const AccountsDDL = `CREATE TABLE IF NOT EXISTS accounts(
	id TEXT PRIMARY KEY,
	username TEXT UNIQUE,
	password_hash BLOB,
	salt BLOB,
	iterations INT DEFAULT 0,
	algo TEXT DEFAULT '',
	email TEXT DEFAULT '',
	email_norm TEXT DEFAULT '',
	created_at INT DEFAULT 0,
	reset_token_hash BLOB,
	reset_token_exp INT DEFAULT 0
)`

// GuestDevicesDDL maps device fingerprints to guest usernames so a returning
// device reuses the same guest identity (and its persisted state) across
// sessions. Separate from `accounts` because guests carry no password hash.
const GuestDevicesDDL = `CREATE TABLE IF NOT EXISTS guest_devices(
	device_id TEXT PRIMARY KEY,
	username TEXT UNIQUE NOT NULL,
	created_at INT DEFAULT 0,
	last_seen_at INT DEFAULT 0
)`

// EnsureTables creates the account table + email index (idempotent; called
// from the boot path next to the other subsystem EnsureTables).
func EnsureTables(db DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(AccountsDDL); err != nil {
		log.Printf("accounts: ddl: %v", err)
		return
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS accounts_email ON accounts(email_norm)`); err != nil {
		log.Printf("accounts: email index: %v", err)
	}
}

// EnsureGuestDevices creates the guest_devices table (idempotent).
func EnsureGuestDevices(db DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(GuestDevicesDDL); err != nil {
		log.Printf("guest_devices: ddl: %v", err)
	}
}

// CreateGuestDevice inserts or replaces a device→username mapping. The
// created_at column is only set on first insert (COALESCE preserves it).
func CreateGuestDevice(db DB, deviceID, username string) error {
	if db == nil || deviceID == "" || username == "" {
		return nil
	}
	now := time.Now().UnixMilli()
	_, err := db.Exec(
		`INSERT INTO guest_devices(device_id,username,created_at,last_seen_at) `+
			`VALUES(?,?,COALESCE((SELECT created_at FROM guest_devices WHERE device_id=?),?),?) `+
			`ON CONFLICT(device_id) DO UPDATE SET username=excluded.username,last_seen_at=excluded.last_seen_at`,
		deviceID, username, deviceID, now, now,
	)
	return err
}

// LookupGuestDevice returns the username previously mapped to deviceID, or
// ("", false) when no mapping exists.
func LookupGuestDevice(db DB, deviceID string) (string, bool) {
	if db == nil || deviceID == "" {
		return "", false
	}
	var username string
	err := db.QueryRow(`SELECT username FROM guest_devices WHERE device_id=?`, deviceID).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		return "", false
	}
	return username, true
}

// DeleteGuestDeviceByUser removes the device mapping for a given username
// (called when a guest upgrades to a real account).
func DeleteGuestDeviceByUser(db DB, username string) error {
	if db == nil || username == "" {
		return nil
	}
	_, err := db.Exec(`DELETE FROM guest_devices WHERE username=?`, username)
	return err
}

// ---------------------------------------------------------------------------
// Validation (Utils.ts parity)
// ---------------------------------------------------------------------------

var (
	// usernameRe mirrors Utils.isValidUsername: /^[\w ]+$/ (latin letters,
	// digits, underscore, spaces).
	usernameRe = regexp.MustCompile(`^[\w ]+$`)
	// emailRe mirrors Utils.isEmail.
	emailRe = regexp.MustCompile(`^(([^\s"(),.:;<>@[\\\]]+(\.[^\s"(),.:;<>@[\\\]]+)*)|(".+"))@((\[(?:\d{1,3}\.){3}\d{1,3}])|(([\dA-Za-z-]+\.)+[A-Za-z]{2,}))$`)
	// nameRe mirrors Utils.formatName's word pattern /\w\S*/g.
	nameRe = regexp.MustCompile(`\w\S*`)
)

// ValidUsername mirrors Utils.isValidUsername.
func ValidUsername(text string) bool {
	return text != "" && usernameRe.MatchString(text)
}

// ValidPassword mirrors Utils.isValidPassword (3..64 characters).
func ValidPassword(text string) bool { return len(text) >= 3 && len(text) <= 64 }

// ValidEmail mirrors Utils.isEmail.
func ValidEmail(email string) bool { return email != "" && emailRe.MatchString(email) }

// FormatName mirrors Utils.formatName: every word capitalised, the rest
// lowercased ("guest1" -> "Guest1", "the king" -> "The King"). This is the
// display name the Welcome/Spawn frames carry (player.ts:2382
// data.name = Utils.formatName(this.username)).
func FormatName(name string) string {
	return nameRe.ReplaceAllStringFunc(name, func(word string) string {
		r := []rune(word)
		if len(r) == 0 {
			return word
		}
		return strings.ToUpper(string(r[0])) + strings.ToLower(string(r[1:]))
	})
}

// ---------------------------------------------------------------------------
// Hashing
// ---------------------------------------------------------------------------

// HashPassword derives (hash, salt, iterations, algo) for a plaintext
// password. The salt is fresh per call, so two accounts sharing a password
// never share a hash.
func HashPassword(password string) (hash, salt []byte, iterations int, algo string, err error) {
	salt = make([]byte, SaltLen)
	if _, err = rand.Read(salt); err != nil {
		return nil, nil, 0, "", err
	}
	hash, err = pbkdf2.Key(sha256.New, password, salt, DefaultIterations, KeyLen)
	if err != nil {
		return nil, nil, 0, "", err
	}
	return hash, salt, DefaultIterations, AlgoPBKDF2, nil
}

// VerifyPassword reports whether password matches the stored derivation. The
// comparison is constant-time (subtle.ConstantTimeCompare) so a wrong password
// reveals nothing through timing.
func VerifyPassword(password string, hash, salt []byte, iterations int, algo string) bool {
	if len(hash) == 0 || len(salt) == 0 {
		return false
	}
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	if algo != "" && algo != AlgoPBKDF2 {
		// Unknown algorithm: never fall back to a weaker comparison.
		log.Printf("accounts: unknown hash algo %q, rejecting", algo)
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(hash))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, hash) == 1
}

// newID returns a 24-hex-character opaque account id. The shape is
// deliberately Mongo-ObjectId-like so the client's reset URL ({id, token})
// contract is unchanged; nothing parses it.
func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Provider adapts the package's DB-bound functions to the shape the api
// package's password-reset endpoints expect (api.AccountProvider). The
// satisfaction is structural, so this package never imports internal/api.
type Provider struct{ DB DB }

// CreateResetToken mirrors mongodb.ts createResetToken (see CreateResetToken).
func (p Provider) CreateResetToken(email string) (id, token string, ok bool) {
	return CreateResetToken(p.DB, email)
}

// ResetPassword mirrors mongodb.ts resetPassword (see ResetPassword).
func (p Provider) ResetPassword(id, token, password string) bool {
	return ResetPassword(p.DB, id, token, password)
}

// ValidEmail mirrors Utils.isEmail (the request-reset input check).
func (p Provider) ValidEmail(email string) bool { return ValidEmail(email) }

// ValidPassword mirrors Utils.isValidPassword (the reset-password input check).
func (p Provider) ValidPassword(password string) bool { return ValidPassword(password) }

// NormalizeUsername lowercases + trims a login name. The username IS the
// player-state key, so every path must agree on the normal form.
func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// normalizeEmail lowercases + trims an email for the uniqueness lookup.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ---------------------------------------------------------------------------
// Store operations
// ---------------------------------------------------------------------------

// ByUsername loads one account by login name. found=false means no such
// account (the caller rejects 'invalidlogin', exactly like
// mongodb.ts login's playerInfo.length === 0 branch).
func ByUsername(db DB, username string) (Account, bool, error) {
	if db == nil {
		return Account{}, false, nil
	}
	key := NormalizeUsername(username)
	if key == "" {
		return Account{}, false, nil
	}
	var a Account
	err := db.QueryRow(`SELECT id,username,email,created_at FROM accounts WHERE username=?`, key).
		Scan(&a.ID, &a.Username, &a.Email, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return a, true, nil
}

// ByEmail loads one account by email (reset-token lookup).
func ByEmail(db DB, email string) (Account, bool, error) {
	if db == nil {
		return Account{}, false, nil
	}
	key := normalizeEmail(email)
	if key == "" {
		return Account{}, false, nil
	}
	var a Account
	err := db.QueryRow(`SELECT id,username,email,created_at FROM accounts WHERE email_norm=?`, key).
		Scan(&a.ID, &a.Username, &a.Email, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return a, true, nil
}

// Exists reports whether the username is registered (mongodb.ts exists
// parity, used by the guest-upgrade duplicate check).
func Exists(db DB, username string) bool {
	_, ok, err := ByUsername(db, username)
	return err == nil && ok
}

// Create inserts a new account. It performs the two duplicate checks TS does
// and returns ErrExists / ErrEmailExists so the caller can emit 'userexists' /
// 'emailexists' separately (mongodb.ts register: emailCursor first, then
// usernameCursor).
func Create(db DB, username, password, email string) (Account, error) {
	if db == nil {
		return Account{}, errors.New("account: no database")
	}
	key := NormalizeUsername(username)
	if key == "" {
		return Account{}, errors.New("account: empty username")
	}
	if _, ok, _ := ByEmail(db, email); ok && email != "" {
		return Account{}, ErrEmailExists
	}
	if _, ok, _ := ByUsername(db, key); ok {
		return Account{}, ErrExists
	}
	hash, salt, iterations, algo, err := HashPassword(password)
	if err != nil {
		return Account{}, err
	}
	id, err := newID()
	if err != nil {
		return Account{}, err
	}
	created := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT INTO accounts(id,username,password_hash,salt,iterations,algo,email,email_norm,created_at) `+
			`VALUES(?,?,?,?,?,?,?,?,?)`,
		id, key, hash, salt, iterations, algo, email, normalizeEmail(email), created,
	); err != nil {
		return Account{}, err
	}
	return Account{ID: id, Username: key, Email: email, CreatedAt: created}, nil
}

// Authenticate verifies a login attempt. It returns the account on success and
// (false) for an unknown user or a wrong password — the caller maps both to
// the single TS reason 'invalidlogin' so the client cannot enumerate users.
func Authenticate(db DB, username, password string) (Account, bool) {
	if db == nil {
		return Account{}, false
	}
	a, ok, err := ByUsername(db, username)
	if err != nil || !ok {
		return Account{}, false
	}
	var hash, salt []byte
	var iterations int
	var algo string
	if err := db.QueryRow(
		`SELECT password_hash,salt,iterations,algo FROM accounts WHERE username=?`, a.Username,
	).Scan(&hash, &salt, &iterations, &algo); err != nil {
		return Account{}, false
	}
	if !VerifyPassword(password, hash, salt, iterations, algo) {
		return Account{}, false
	}
	return a, true
}

// SetPassword replaces the derivation for one account.
func SetPassword(db DB, accountID, password string) error {
	if db == nil {
		return errors.New("account: no database")
	}
	hash, salt, iterations, algo, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`UPDATE accounts SET password_hash=?,salt=?,iterations=?,algo=?,reset_token_hash=NULL,reset_token_exp=0 WHERE id=?`,
		hash, salt, iterations, algo, accountID,
	)
	return err
}

// CreateResetToken mirrors mongodb.ts createResetToken: unknown emails get no
// token (the caller still answers success — anti-enumeration), and the stored
// value is a hash of the plaintext handed to the mailer, with a one-hour
// expiry.
func CreateResetToken(db DB, email string) (accountID, token string, ok bool) {
	if db == nil {
		return "", "", false
	}
	a, found, err := ByEmail(db, email)
	if err != nil || !found {
		return "", "", false
	}
	raw := make([]byte, ResetTokenLen)
	if _, err := rand.Read(raw); err != nil {
		return "", "", false
	}
	token = hex.EncodeToString(raw)
	hash, salt, iterations, algo, err := HashPassword(token)
	if err != nil {
		return "", "", false
	}
	// The salt/iterations columns describe the LOGIN password; the reset hash
	// therefore carries its own salt inline (salt||hash) so the two never
	// overwrite each other.
	blob := append(append([]byte{}, salt...), hash...)
	_ = iterations
	_ = algo
	if _, err := db.Exec(
		`UPDATE accounts SET reset_token_hash=?,reset_token_exp=? WHERE id=?`,
		blob, time.Now().Add(ResetTokenTTL).UnixMilli(), a.ID,
	); err != nil {
		return "", "", false
	}
	return a.ID, token, true
}

// ResetPassword mirrors mongodb.ts resetPassword: id + token must match, the
// token must not be expired, and the new password must satisfy the same length
// rule. Returns false for every failure so the caller answers a single
// 'invalid' status.
func ResetPassword(db DB, accountID, token, password string) bool {
	if db == nil || accountID == "" || token == "" || !ValidPassword(password) {
		return false
	}
	var blob []byte
	var exp int64
	if err := db.QueryRow(
		`SELECT reset_token_hash,reset_token_exp FROM accounts WHERE id=?`, accountID,
	).Scan(&blob, &exp); err != nil {
		return false
	}
	if len(blob) < SaltLen || exp == 0 || exp < time.Now().UnixMilli() {
		return false
	}
	salt := blob[:SaltLen]
	hash := blob[SaltLen:]
	got, err := pbkdf2.Key(sha256.New, token, salt, DefaultIterations, len(hash))
	if err != nil || subtle.ConstantTimeCompare(got, hash) != 1 {
		return false
	}
	return SetPassword(db, accountID, password) == nil
}
