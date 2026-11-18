package pyval

import (
	"strings"
	"unicode"
)

// IsSpace mirrors str.isspace() for a single character.
func IsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Strip mirrors str.strip() without arguments.
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// IsWordChar mirrors the Unicode \w class of Python's re module.
func IsWordChar(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// Lower mirrors str.lower(), including the one unconditional multi-character
// lowercase mapping (U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE).
func Lower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0x130 {
			b.WriteString("i̇")
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
