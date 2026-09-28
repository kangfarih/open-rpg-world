// Login identity layer — TS parity for the credential half of the server.
//
// Mirrors (read-only reference):
//
//	packages/server/src/game/entity/character/player/incoming.ts  handleLogin
//	packages/common/database/mongodb/mongodb.ts                   login/register/exists
//	packages/common/database/mongodb/creator.ts                   verifyPlayer
//	packages/common/util/utils.ts                                 credential validation
//	packages/server/src/main.ts                                   'disallowed'/'worldfull'
//	packages/server/src/network/network.ts                        'toofast'/'toomany'/'banned'
//
// The TS flow, in order (incoming.ts:193-243):
//
//  1. !completedHandshake            -> reject 'lost'
//  2. if (username):                 sanitize, password length -> 'invalidpassword',
//     world.isOnline            -> 'loggedin',
//     config.skipDatabase       -> load fresh and stop
//  3. opcode switch: Login -> database.login ('invalidlogin'),
//     Register -> 'disabledregister' / 'invalidinput' /
//     'emailexists' / 'userexists',
//     Guest -> isGuest, username guest<n>, fresh state
//
// Everything above is reproduced here. The account store is SQLite
// (internal/account) rather than MongoDB, so the one TS knob that cannot carry
// over verbatim is `config.skipDatabase`'s meaning: it still short-circuits
// credential verification exactly as TS does, and it defaults to ON because the
// repository's own .env.defaults ships SKIP_DATABASE=true.
package server

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"rpg-world-server/internal/account"
	gnet "rpg-world-server/internal/net"
	"rpg-world-server/internal/player/chat"
)

// Login opcodes (packages/common/network/opcodes.ts: Login, Register, Guest).
const (
	LoginOpcodeLogin    = 0
	LoginOpcodeRegister = 1
	LoginOpcodeGuest    = 2
)

// loginRequest is the C Login payload (messages/incoming.d.ts LoginPacket plus
// the TESTMAP seed hooks the e2e harnesses use).
type loginRequest struct {
	Opcode   int    `json:"opcode"`
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	DeviceID string `json:"deviceId,omitempty"` // device fingerprint for persistent guests

	SeedGold  int   `json:"seedGold,omitempty"`
	SeedArrow int   `json:"seedArrow,omitempty"`
	SeedRank  int   `json:"seedRank,omitempty"`
	SeedPos   []int `json:"seedPos,omitempty"`
}

// loginDecision is the resolved identity: the player-state key to use, whether
// the session is a guest (never persisted), and the TS close reason when the
// attempt is refused.
type loginDecision struct {
	Username string
	Guest    bool
	Reason   string
}

// loginEnv carries the config + seams resolveLogin needs, so the decision logic
// stays pure and unit-testable.
type loginEnv struct {
	DB              account.DB
	SkipDatabase    bool
	DisableRegister bool
	Online          func(string) bool
}

// ---------------------------------------------------------------------------
// Config (TS config parity; defaults are .env.defaults verbatim)
// ---------------------------------------------------------------------------

// envBool reads a boolean env var with a default.
func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// loginSkipDatabase mirrors config.skipDatabase (default true: .env.defaults
// ships SKIP_DATABASE=true, and TS with it on skips database.login/register
// entirely — incoming.ts:206).
func loginSkipDatabase() bool { return envBool("SKIP_DATABASE", true) }

// loginDisableRegister mirrors config.disableRegister (DISABLE_REGISTER=false
// by default).
func loginDisableRegister() bool { return envBool("DISABLE_REGISTER", false) }

// loginMaxPlayers mirrors config.maxPlayers (MAX_PLAYERS=200 by default).
func loginMaxPlayers() int {
	if v := strings.TrimSpace(os.Getenv("MAX_PLAYERS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return opsMaxPlayers
}

// ---------------------------------------------------------------------------
// Online presence (world.isOnline / world.isFull parity)
// ---------------------------------------------------------------------------

// usernameOnline is world.isOnline: this process first (the existing
// playerByName registry lookup the shard client already uses as its local
// presence check), then the other shards through the router.
func usernameOnline(name string) bool {
	if name == "" {
		return false
	}
	if playerByName(name) != nil {
		return true
	}
	return crossShardOnline(name)
}

// loggedInCount is the number of logged-in usernames (TS world.isFull uses
// entities.getPlayerUsernames().length).
func loggedInCount() int { return len(playerUsernames()) }

// loginWorldFull mirrors world.isFull (main.ts:65 rejects 'worldfull').
func loginWorldFull() bool { return loggedInCount() >= loginMaxPlayers() }

// ---------------------------------------------------------------------------
// Identity resolution
// ---------------------------------------------------------------------------

// sanitizeUsername mirrors incoming.ts:199:
// Filter.clean(username.toLowerCase().slice(0, 32).trim()).
func sanitizeUsername(raw string) string {
	name := strings.ToLower(raw)
	if len(name) > 32 {
		name = name[:32]
	}
	name = strings.TrimSpace(name)
	return chat.Clean(name)
}

// loginGuestSeq mirrors Utils.counter++ (guest0, guest1, ...).
var loginGuestSeq atomic.Int64

// nextGuestName mints the TS guest name (incoming.ts:239).
func nextGuestName() string { return fmt.Sprintf("guest%d", loginGuestSeq.Add(1)-1) }

// resolveLogin applies the TS handleLogin decision tree.
func resolveLogin(req loginRequest, env loginEnv) loginDecision {
	name := sanitizeUsername(req.Username)

	// TS: `if (username) { ... }` — the credential half runs for every opcode
	// that carries a username.
	if name != "" {
		// incoming.ts:202 — password length runs BEFORE anything else.
		if !account.ValidPassword(req.Password) {
			return loginDecision{Reason: gnet.ReasonInvalidPassword}
		}
		// incoming.ts:208 — the duplicate-session check runs before the
		// skip-database shortcut, so it applies in both modes.
		if env.Online != nil && env.Online(name) {
			return loginDecision{Reason: gnet.ReasonLoggedIn}
		}
		// incoming.ts:206 — skip-database loads default data and returns.
		if env.SkipDatabase {
			return loginDecision{Username: name}
		}
		switch req.Opcode {
		case LoginOpcodeLogin:
			if _, ok := account.Authenticate(env.DB, name, req.Password); !ok {
				// mongodb.ts:101 — unknown user and wrong password share one
				// reason so accounts cannot be enumerated.
				return loginDecision{Reason: gnet.ReasonInvalidLogin}
			}
			return loginDecision{Username: name}
		case LoginOpcodeRegister:
			return registerDecision(env, name, req)
		case LoginOpcodeGuest:
			// Guest with explicit username: still mint a fresh guest name
			// (TS behaviour), but resolve device_id first for persistence.
			return guestDeviceDecision(req, env)
		default:
			return loginDecision{Reason: gnet.ReasonInvalidLogin}
		}
	}

	// No username: TS reaches the opcode switch with an empty name.
	switch req.Opcode {
	case LoginOpcodeGuest:
		return guestDeviceDecision(req, env)
	case LoginOpcodeRegister:
		return loginDecision{Reason: gnet.ReasonInvalidInput}
	default:
		return loginDecision{Reason: gnet.ReasonInvalidLogin}
	}
}

// guestDeviceDecision resolves a guest login with device fingerprint support.
// If a device_id is provided and a mapping exists, reuse the existing guest
// identity. Otherwise mint a new guest name and record the mapping.
func guestDeviceDecision(req loginRequest, env loginEnv) loginDecision {
	if req.DeviceID != "" && env.DB != nil {
		if existing, ok := account.LookupGuestDevice(env.DB, req.DeviceID); ok {
			// Returning device: reuse the same guest identity.
			return loginDecision{Username: existing, Guest: true}
		}
	}
	name := nextGuestName()
	if req.DeviceID != "" && env.DB != nil {
		_ = account.CreateGuestDevice(env.DB, req.DeviceID, name)
	}
	return loginDecision{Username: name, Guest: true}
}

// registerDecision mirrors mongodb.ts register.
func registerDecision(env loginEnv, name string, req loginRequest) loginDecision {
	if env.DisableRegister {
		return loginDecision{Reason: gnet.ReasonDisabledRegister}
	}
	// Creator.verifyPlayer + Filter.isProfane -> 'invalidinput'. Email is
	// optional in the client, so an empty one is accepted (TS validates
	// player.email, which is '' when the form left it blank).
	if !account.ValidUsername(name) || chat.IsProfane(name) {
		return loginDecision{Reason: gnet.ReasonInvalidInput}
	}
	if req.Email != "" && !account.ValidEmail(req.Email) {
		return loginDecision{Reason: gnet.ReasonInvalidInput}
	}
	if _, err := account.Create(env.DB, name, req.Password, req.Email); err != nil {
		switch {
		case errors.Is(err, account.ErrEmailExists):
			return loginDecision{Reason: gnet.ReasonEmailExists}
		case errors.Is(err, account.ErrExists):
			return loginDecision{Reason: gnet.ReasonUserExists}
		default:
			log.Printf("login: create account %s: %v", name, err)
			return loginDecision{Reason: gnet.ReasonError}
		}
	}
	log.Printf("login: created account %s", name)
	return loginDecision{Username: name}
}

// ---------------------------------------------------------------------------
// Guest session bookkeeping
// ---------------------------------------------------------------------------

// guestKeys is the set of player-state keys belonging to guest sessions. TS
// suppresses persistence for guests in player.save (player.ts:2358); Go's
// equivalent choke points are markDirty/savePlayerSync, which consult this set.
var guestKeys sync.Map

// persistentGuests is the subset of guestKeys whose state IS persisted —
// device-identified guests that should survive disconnect/reconnect. The
// isGuestKey guard in markDirty/savePlayerSync is relaxed for these.
var persistentGuests sync.Map

// markGuestKey records a guest session's state key as non-persistent.
func markGuestKey(key string) {
	if key != "" {
		guestKeys.Store(key, struct{}{})
	}
}

// isGuestKey reports whether key belongs to a guest session.
func isGuestKey(key string) bool {
	if key == "" {
		return false
	}
	_, ok := guestKeys.Load(key)
	return ok
}

// markPersistentGuest marks a guest as device-identified (state persists).
func markPersistentGuest(key string) {
	if key != "" {
		persistentGuests.Store(key, struct{}{})
	}
}

// isPersistentGuest reports whether key is a device-identified guest whose
// state should be persisted across sessions.
func isPersistentGuest(key string) bool {
	if key == "" {
		return false
	}
	_, ok := persistentGuests.Load(key)
	return ok
}

// ---------------------------------------------------------------------------
// Root wiring (globals -> resolveLogin)
// ---------------------------------------------------------------------------

// ensureAccountTables creates the accounts table + email index + guest_devices
// table (called from Boot next to the other subsystem EnsureTables).
func ensureAccountTables() {
	if dbConn == nil {
		return
	}
	account.EnsureTables(dbConn)
	account.EnsureGuestDevices(dbConn)
}

// accountHandle returns the account table handle. Callers issue SQL through it
// exactly like the other direct-table seams (quests/abilities/social/m13
// flags); *sql.DB is safe for concurrent use, and the account table only ever
// grows one row per registration.
func accountHandle() account.DB {
	if dbConn == nil {
		return nil
	}
	return dbConn
}

// loginEnvFor builds the live config/env for identity resolution.
func loginEnvFor() loginEnv {
	return loginEnv{
		DB:              accountHandle(),
		SkipDatabase:    loginSkipDatabase(),
		DisableRegister: loginDisableRegister(),
		Online:          usernameOnline,
	}
}

// loginApply resolves one Login frame, rejects it with the TS reason on
// failure, and installs the identity on success. It returns false when the
// caller must stop handling the connection.
func loginApply(c *playerConn, req loginRequest) bool {
	decision := resolveLogin(req, loginEnvFor())
	if decision.Reason != "" {
		log.Printf("login rejected instance=%s user=%q reason=%s", c.Instance, req.Username, decision.Reason)
		_ = gnet.Reject(c.WS, decision.Reason)
		return false
	}
	c.Username = decision.Username
	c.Guest = decision.Guest
	if decision.Guest {
		markGuestKey(decision.Username)
		// Device-identified guests persist state across sessions.
		if req.DeviceID != "" && dbConn != nil {
			if _, ok := account.LookupGuestDevice(dbConn, req.DeviceID); ok {
				markPersistentGuest(decision.Username)
				log.Printf("login: persistent guest %s (device %s, state restored)", decision.Username, req.DeviceID[:8]+"...")
			} else {
				log.Printf("login: new guest %s (device %s, fresh state)", decision.Username, req.DeviceID[:8]+"...")
			}
		} else {
			log.Printf("login: guest session %s (not persisted)", decision.Username)
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Guest -> account upgrade (the improvement over TS)
// ---------------------------------------------------------------------------

// upgradeGuest converts an active guest session into a real account in place.
//
// TS has no equivalent: a guest there is a dead end — the session is never
// persisted (player.ts:2358) and `Creator.serialize` is the only state it ever
// has, so becoming a real player means disconnecting and losing everything.
// Here the live session keeps its position, inventory, skills and quest
// progress, which are re-keyed from the guest name onto the new account.
//
// Validation is the Register path verbatim (same reasons), so a taken name
// answers 'userexists', a taken email 'emailexists', and so on. Returns the new
// username on success, or the TS reject reason.
func upgradeGuest(c *playerConn, req loginRequest) (string, string) {
	if c == nil || !c.loggedIn {
		return "", gnet.ReasonInvalidLogin
	}
	if !c.Guest {
		// Already an account: an identity switch mid-socket is refused exactly
		// like a second Login frame.
		return "", gnet.ReasonLoggedIn
	}
	name := sanitizeUsername(req.Username)
	req.Opcode = LoginOpcodeRegister
	env := loginEnvFor()
	decision := registerDecision(env, name, req)
	if decision.Reason != "" {
		return "", decision.Reason
	}
	old := c.Username
	if !rekeyPlayerState(old, decision.Username) {
		log.Printf("upgrade: no live state for %s (identity created anyway)", old)
	}
	// Clean up the guest_devices mapping so the device can't re-resolve to the
	// old guest name (the upgraded account is now the canonical identity).
	if dbConn != nil {
		_ = account.DeleteGuestDeviceByUser(dbConn, old)
	}
	c.Username = decision.Username
	c.Guest = false
	log.Printf("upgrade: guest %s -> account %s", old, decision.Username)
	return decision.Username, ""
}

// guestRegisterCommand is the `/register <username> <password> [email]` chat
// trigger for the upgrade. A chat command is used rather than a new client
// screen so the stock client gains the capability with no packages/* edit and
// no new packet id (the protocol affordance is a second Login.Register frame on
// the live guest socket — see the PacketLogin case in boot.go).
//
// Returns true when the command was handled (so the normal command tables are
// skipped).
func guestRegisterCommand(c *playerConn, command string, args []string) bool {
	if !strings.EqualFold(strings.TrimSpace(command), "register") {
		return false
	}
	if c == nil {
		return true
	}
	if !c.Guest {
		notifyPlayer(c, "You are already playing on a registered account.")
		return true
	}
	if len(args) < 2 {
		notifyPlayer(c, "Usage: /register <username> <password> [email]")
		return true
	}
	req := loginRequest{
		Opcode:   LoginOpcodeRegister,
		Username: args[0],
		Password: args[1],
	}
	if len(args) > 2 {
		req.Email = args[2]
	}
	name, reason := upgradeGuest(c, req)
	if reason != "" {
		notifyPlayer(c, upgradeFailureText(reason))
		return true
	}
	notifyPlayer(c, fmt.Sprintf(
		"Account %s created — your progress is saved from now on. Log in with it next time.",
		account.FormatName(name)))
	return true
}

// upgradeFailureText renders the TS reject reason as player-facing text (the
// close-reason strings are wire values, not chat strings).
func upgradeFailureText(reason string) string {
	switch reason {
	case gnet.ReasonUserExists:
		return "That username is already taken."
	case gnet.ReasonEmailExists:
		return "That email address is already in use."
	case gnet.ReasonDisabledRegister:
		return "Registration is currently disabled."
	case gnet.ReasonInvalidInput:
		return "Invalid input: usernames allow letters, numbers, spaces and underscores; passwords are 3-64 characters."
	case gnet.ReasonLoggedIn:
		return "This session is already registered."
	default:
		return "Could not create the account (" + reason + ")."
	}
}

// rekeyPlayerState moves live player state from one key to another (guest ->
// account upgrade) and clears the guest marker so the upgraded identity
// persists like any account. Returns false when the source key is unknown.
func rekeyPlayerState(oldKey, newKey string) bool {
	if oldKey == "" || newKey == "" || oldKey == newKey {
		return false
	}
	pstateMu.Lock()
	st, ok := pstates[oldKey]
	if ok && st != nil {
		pstates[newKey] = st
		delete(pstates, oldKey)
	}
	pstateMu.Unlock()
	if !ok {
		return false
	}
	guestKeys.Delete(oldKey)
	persistentGuests.Delete(oldKey)
	// The upgraded identity now persists like any account.
	markDirty(newKey)
	return true
}
