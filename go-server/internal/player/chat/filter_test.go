package chat

import "testing"

func TestIsProfane(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"hello world", false},
		{"ass", true},
		{"ASS", true},
		{"class", true},        // substring match: "ass" in "class"
		{"what the fuck", true}, // multi-word
		{"w h a t", false},     // spaced out, no match
		{"a s s", true},        // explicit spaced profanity in word list
	}
	for _, tt := range tests {
		if got := IsProfane(tt.msg); got != tt.want {
			t.Errorf("IsProfane(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
}

func TestClean(t *testing.T) {
	tests := []struct {
		msg  string
		want string
	}{
		// Basic replacement (case-insensitive, '*' × length).
		{"fuck", "****"},
		{"FUCK", "****"},
		{"ass", "***"},
		{"class", "cl***"}, // substring: "ass" → "***"

		// Multi-word.
		{"what the fuck", "what the ****"},

		// No profanity → unchanged.
		{"hello world", "hello world"},

		// Combining mark stripping (anti-evasion).
		// "a\u0301" (a + combining acute) → NFC → "á" → URI-encode → "%C3%A1"
		// No combining mark pattern matches (no %CC prefix after NFC), so "á" survives.
		// If "á" were in the profanity list it would be replaced; since it's not, unchanged.
		{"a\u0301", "á"},

		// Zalgo-style combining marks after NFC: some survive NFC if they
		// don't compose. The regex strips %CC sequences in URI-encoded form.
	}
	for _, tt := range tests {
		got := Clean(tt.msg)
		if got != tt.want {
			t.Errorf("Clean(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

func TestCleanSubstringReplacement(t *testing.T) {
	// "class" contains "ass" → "cl***". This matches TS behavior where
	// substring replacement is intentional (the TS regex is global, not
	// word-boundary).
	got := Clean("class")
	want := "cl***"
	if got != want {
		t.Errorf("Clean(\"class\") = %q, want %q", got, want)
	}
}
