// Package chat holds the PURE M7 chat pipeline: sanitization, display-name
// formatting, token-bucket math, rank-gated global cooldowns and command
// parsing tables. Transport-free: no *websocket.Conn, no send/broadcast/DB
// calls, no playerConn pointers. The root adapter (m7.go) keeps ALL wiring
// and joins this model to live conns; behavior (frames, rate limits,
// command outcomes) is frozen there.
//
// Port notes mirror m7.go: sanitizer.escape/sanitize combo, Utils.formatName,
// player.chat/global cooldowns (player.ts) and controllers/commands.ts tables.
package chat

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Ranks (Modules.Ranks, modules.ts:327) + titles (modules.ts:365).
// Values pin the TS enum order so persisted rank ints compare correctly.
// ---------------------------------------------------------------------------

const (
	RankNone        = 0
	RankModerator   = 1
	RankAdmin       = 2
	RankVeteran     = 3
	RankPatron      = 4
	RankArtist      = 5
	RankCheater     = 6
	RankTierOne     = 7
	RankTierTwo     = 8
	RankTierThree   = 9
	RankTierFour    = 10
	RankTierFive    = 11
	RankTierSix     = 12
	RankTierSeven   = 13
	RankHollowAdmin = 14
	RankBooster     = 15
)

// RankTitles prefixes chat display names for ranked players.
var RankTitles = map[int]string{
	RankModerator:   "Mod",
	RankAdmin:       "Admin",
	RankVeteran:     "Veteran",
	RankPatron:      "Patron",
	RankArtist:      "Artist",
	RankCheater:     "Cheater",
	RankTierOne:     "T1 Patron",
	RankTierTwo:     "T2 Patron",
	RankTierThree:   "T3 Patron",
	RankTierFour:    "T4 Patron",
	RankTierFive:    "T5 Patron",
	RankTierSix:     "T6 Patron",
	RankTierSeven:   "T7 Patron",
	RankHollowAdmin: "Admin",
	RankBooster:     "Booster",
}

// ---------------------------------------------------------------------------
// Rate limits + colours.
// ---------------------------------------------------------------------------

const (
	// BucketSize is the region-chat burst capacity (m7.go chatBucketSize).
	BucketSize = 3.0
	// RefillPerSec refills one message per 2 seconds (m7.go chatRefillPerSec).
	RefillPerSec = 1.0 / 2.0
	// GlobalCooldown is the default-rank global cooldown in ms
	// (player.ts getGlobalChatCooldown default: 60 minutes).
	GlobalCooldown = int64(60 * 60_000)
	// ModGlobalCooldown is the mod/admin global cooldown in ms.
	ModGlobalCooldown = int64(5000)
)

// GlobalColour is the default global/mod chat colour.
const GlobalColour = "rgba(191, 161, 63, 1.0)"

// PMColour is the private-message notify colour.
const PMColour = "aquamarine"

// ---------------------------------------------------------------------------
// Text helpers.
// ---------------------------------------------------------------------------

// WhitespaceRe reports whether text carries any visible character.
var WhitespaceRe = regexp.MustCompile(`\S`)

// WordRe matches words for Utils.formatName capitalization.
var WordRe = regexp.MustCompile(`\w\S*`)

// Sanitize ports sanitizer.escape + sanitize: HTML-escape the five XML
// entities and drop NULs. Node's `bo-wie/sanitize-html` escape pass.
func Sanitize(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#x27;",
	)
	return r.Replace(s)
}

// FormatName ports Utils.formatName: capitalize every word.
func FormatName(name string) string {
	return WordRe.ReplaceAllStringFunc(name, func(w string) string {
		if w == "" {
			return w
		}
		return strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	})
}

// HasVisibleText reports whether sanitized text carries a visible character.
func HasVisibleText(text string) bool {
	return WhitespaceRe.MatchString(text)
}

// IsCommand reports whether sanitized text is a command (/ or ; prefix,
// incoming.ts:476 — commands bypass chat entirely).
func IsCommand(text string) bool {
	return strings.HasPrefix(text, "/") || strings.HasPrefix(text, ";")
}

// ---------------------------------------------------------------------------
// Bucket math (token bucket over region chat).
// ---------------------------------------------------------------------------

// Bucket is one connection's refillable chat token balance. The root adapter
// owns synchronization; this type is pure math with an injectable clock.
type Bucket struct {
	Tokens     float64
	LastRefill time.Time
}

// Allow consumes one token, refilling elapsed time first. A zero LastRefill
// seeds a full bucket, mirroring chatState.allowChat.
func (b *Bucket) Allow(now time.Time) bool {
	ok, tokens, refill := AllowBucket(b.Tokens, b.LastRefill, now)
	b.Tokens, b.LastRefill = tokens, refill
	return ok
}

// AllowBucket is the shared bucket step: refill elapsed time, clamp to
// BucketSize, then consume one token when available.
func AllowBucket(tokens float64, lastRefill, now time.Time) (bool, float64, time.Time) {
	if lastRefill.IsZero() {
		lastRefill = now
		tokens = BucketSize
	}
	tokens += now.Sub(lastRefill).Seconds() * RefillPerSec
	if tokens > BucketSize {
		tokens = BucketSize
	}
	lastRefill = now
	if tokens < 1 {
		return false, tokens, lastRefill
	}
	return true, tokens - 1, lastRefill
}

// ---------------------------------------------------------------------------
// Global cooldown (player.ts canGlobalChat / getGlobalChatDuration).
// ---------------------------------------------------------------------------

// CooldownFor returns the global-chat cooldown for a rank (player.ts
// getGlobalChatCooldown parity: patron tiers 15–55min, mods/admins 5s,
// everyone else 60min).
func CooldownFor(rank int) int64 {
	switch rank {
	case RankTierOne:
		return 55 * 60_000
	case RankTierTwo:
		return 50 * 60_000
	case RankTierThree:
		return 45 * 60_000
	case RankTierFour:
		return 40 * 60_000
	case RankTierFive:
		return 35 * 60_000
	case RankTierSix:
		return 30 * 60_000
	case RankTierSeven:
		return 15 * 60_000
	case RankModerator, RankHollowAdmin, RankAdmin:
		return ModGlobalCooldown
	default:
		return GlobalCooldown
	}
}

// GlobalReady ports canGlobalChat at an explicit now (ms).
func GlobalReady(rank int, lastGlobalChat, now int64) bool {
	return now-lastGlobalChat > CooldownFor(rank)
}

// GlobalDuration ports getGlobalChatDuration: whole minutes left on the
// cooldown, minimum 1, at an explicit now (ms).
func GlobalDuration(rank int, lastGlobalChat, now int64) int {
	d := (CooldownFor(rank) - (now - lastGlobalChat)) / 1000 / 60
	if d < 1 {
		d = 1
	}
	return int(d)
}

// NowMillis returns the current Unix time in milliseconds.
func NowMillis() int64 {
	return time.Now().UnixMilli()
}

// ---------------------------------------------------------------------------
// Display names.
// ---------------------------------------------------------------------------

// DisplayName applies rank prefixing to the formatted username
// (m7Chat name block, colour resolved separately).
func DisplayName(username string, rank int) string {
	name := FormatName(username)
	if rank != RankNone {
		if title, ok := RankTitles[rank]; ok {
			name = "[" + title + "] " + name
		}
	}
	return name
}

// ResolveColour applies the ranked default colour when none is set.
func ResolveColour(rank int, colour string) string {
	if rank != RankNone && colour == "" {
		return GlobalColour
	}
	return colour
}

// GlobalSource builds the world.globalMessage source frame header.
func GlobalSource(displayName string) string {
	return "[Global] " + displayName
}

// ---------------------------------------------------------------------------
// Command parsing (controllers/commands.ts).
// ---------------------------------------------------------------------------

// SplitCommand ports Commands.parse: strip the single prefix char, split on
// spaces. ok is false for an empty command ("/" alone).
// VERIFY (double-strip): TS does rawText.slice(1) — exactly one char — while
// the old TrimPrefix(TrimPrefix()) pair stripped two ("/;cmd" → "cmd" instead
// of ";cmd"). Single-strip matches TS.
func SplitCommand(rawText string) (command string, args []string, ok bool) {
	if len(rawText) > 0 && (rawText[0] == '/' || rawText[0] == ';') {
		rawText = rawText[1:]
	}
	blocks := strings.Split(rawText, " ")
	if len(blocks) == 0 || blocks[0] == "" {
		return "", nil, false
	}
	return blocks[0], blocks[1:], true
}

// ParsePrivateMessage ports the /pm|/msg username resolution: username is the
// text between the two `*` markers, and the message is every block after the
// username's blocks.
// VERIFY (PM comment fix): the old comment claimed the delivered message
// keeps the `*username*` wrapper (a "Node quirk"). It does not — TS slices
// the username's blocks off (blocks.slice(username.split(' ').length)), so
// "*bob* hello" delivers "hello" and "*bob smith* hi" delivers "hi". Comment
// corrected; parsing already matched TS. The username is lowercased like
// the m7 caller.
func ParsePrivateMessage(blocks []string) (username, message string, ok bool) {
	joined := strings.Join(blocks, " ")
	parts := strings.Split(joined, "*")
	if len(parts) < 2 || parts[1] == "" {
		return "", "", false
	}
	username = parts[1]
	usernameBlocks := len(strings.Fields(username))
	message = strings.Join(blocks[usernameBlocks:], " ")
	return strings.ToLower(username), message, true
}

// ParseTeleportArgs parses /teleport x y integer coordinates.
func ParseTeleportArgs(blocks []string) (x, y int, ok bool) {
	if len(blocks) < 2 {
		return 0, 0, false
	}
	_, errX := fmt.Sscanf(blocks[0], "%d", &x)
	_, errY := fmt.Sscanf(blocks[1], "%d", &y)
	if errX != nil || errY != nil {
		return 0, 0, false
	}
	return x, y, true
}
