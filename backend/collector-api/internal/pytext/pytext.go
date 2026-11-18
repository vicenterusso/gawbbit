// Package pytext holds the small Python string semantics the collector
// depends on (str.isspace/str.strip and Starlette's latin-1 header decoding).
package pytext

import (
	"strings"
	"unicode/utf8"
)

// IsSpace reports whether Python's str.isspace() is true for r.
func IsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// Strip is Python's str.strip() with no arguments.
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// Latin1 decodes raw header bytes as ISO-8859-1, as Starlette does, so that
// every byte becomes one code point.
func Latin1(raw string) string {
	ascii := true
	for i := 0; i < len(raw); i++ {
		if raw[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return raw
	}
	b := make([]byte, 0, len(raw)*2)
	for i := 0; i < len(raw); i++ {
		b = utf8.AppendRune(b, rune(raw[i]))
	}
	return string(b)
}
