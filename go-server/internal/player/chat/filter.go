package chat

// Profanity filter — ports packages/common/util/filter.ts (Filter.isProfane /
// Filter.clean) and the word list from packages/common/text/profanity.json.
//
// TS behavior (frozen):
//   - isProfane: lowercase substring check against each profanity.
//   - clean: NFC normalize → URI-encode → strip combining mark sequences
//     (anti-evasion: combining diacriticals %CC, Thai %E0%B9) → URI-decode →
//     regex-replace each profanity with '*' × word length (case-insensitive).
//
// The URI-encode/decode round-trip exposes the underlying text after combining
// marks are stripped, so "a\u0301" (a + combining accent) becomes "a" before
// filtering. The three regex patterns target combining sequences before a
// space, before a word char, and at end-of-string.

import (
	_ "embed"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

//go:embed profanity.json
var profanityData []byte

var profanities []string

func init() {
	if err := json.Unmarshal(profanityData, &profanities); err != nil {
		panic("profanity.json: " + err.Error())
	}
}

// IsProfane reports whether message contains any profanity (substring match,
// case-insensitive). Ports TS Filter.isProfane.
func IsProfane(message string) bool {
	lower := strings.ToLower(message)
	for _, p := range profanities {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// Clean strips combining marks (anti-evasion) then replaces each profanity
// with '*' × length. Ports TS Filter.clean.
func Clean(message string) string {
	// NFC normalize (TS message.normalize() defaults to NFC).
	message = norm.NFC.String(message)

	// URI-encode (match JS encodeURIComponent).
	encoded := encodeURIComponent(message)

	// Strip combining mark sequences (port of TS regex patterns).
	// %CC = UTF-8 prefix for U+0300-U+037F (Combining Diacritical Marks).
	// %E0%B9 = UTF-8 prefix for U+0E00-U+0E7F (Thai, includes combining vowels/tone marks).
	re1 := regexp.MustCompile(`(%CC|%E0%B9)(%[\dA-F]{2})+%20`)
	re2 := regexp.MustCompile(`(%CC|%E0%B9)(%[\dA-F]{2})+(\w)`)
	re3 := regexp.MustCompile(`(%CC|%E0%B9)(%[\dA-F]{2})+$`)

	encoded = re1.ReplaceAllString(encoded, " ")
	encoded = re2.ReplaceAllString(encoded, "$2")
	encoded = re3.ReplaceAllString(encoded, "")

	// URI-decode.
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		// Malformed URI (partial UTF-8 from regex 2's $2 replacement) — fall
		// back to the pre-encode normalized message. This matches TS behavior
		// where decodeURIComponent would throw on malformed sequences.
		decoded = message
	}
	message = decoded

	// Replace each profanity with '*' × length (case-insensitive substring).
	for _, p := range profanities {
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(p))
		message = re.ReplaceAllString(message, strings.Repeat("*", len(p)))
	}

	return message
}

// encodeURIComponent matches JS encodeURIComponent: encodes everything except
// A-Z a-z 0-9 - _ . ! ~ * ' ( ). Go's url.PathEscape encodes additional chars
// (notably ~), so we roll our own to match TS byte-for-byte.
func encodeURIComponent(s string) string {
	var buf strings.Builder
	for _, b := range []byte(s) {
		if isUnescapedByte(b) {
			buf.WriteByte(b)
		} else {
			// Manually format as %XX to avoid url.PathEscape's UTF-8 assumptions.
			buf.WriteString("%")
			buf.WriteString(hexDigit(b >> 4))
			buf.WriteString(hexDigit(b & 0x0F))
		}
	}
	return buf.String()
}

// hexDigit returns the hex character for a 4-bit value.
func hexDigit(b byte) string {
	const hex = "0123456789ABCDEF"
	return string(hex[b])
}

// isUnescapedByte reports whether b is in the JS encodeURIComponent unescaped set.
func isUnescapedByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') ||
		b == '-' || b == '_' || b == '.' || b == '!' || b == '~' || b == '*' || b == '\'' || b == '(' || b == ')'
}
