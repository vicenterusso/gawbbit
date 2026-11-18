package pyval

import (
	"unicode"
	"unicode/utf8"
)

// Strings are WTF-8: standard UTF-8 plus 3-byte encodings of surrogate code points,
// which Python str can hold after decoding a JSON "\ud800" escape.

// DecodeRune decodes one code point at s[i:], accepting encoded surrogates.
func DecodeRune(s string, i int) (rune, int) {
	if i+2 < len(s) && s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2]&0xc0 == 0x80 {
		return 0xd000 | rune(s[i+1]&0x3f)<<6 | rune(s[i+2]&0x3f), 3
	}
	return utf8.DecodeRuneInString(s[i:])
}

// AppendRune appends r, encoding surrogates the way WTF-8 does.
func AppendRune(buf []byte, r rune) []byte {
	if IsSurrogate(r) {
		return append(buf, 0xe0|byte(r>>12), 0x80|byte(r>>6)&0x3f, 0x80|byte(r)&0x3f)
	}
	return utf8.AppendRune(buf, r)
}

// IsSurrogate reports whether r is in U+D800..U+DFFF.
func IsSurrogate(r rune) bool { return r >= 0xd800 && r <= 0xdfff }

// HasSurrogate reports whether s holds a surrogate code point, which cannot be encoded
// as UTF-8.
func HasSurrogate(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf {
			return true
		}
	}
	return false
}

// Runes splits s into code points.
func Runes(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := DecodeRune(s, i)
		out = append(out, r)
		i += size
	}
	return out
}

// IsSpace is Python str.isspace() for one code point (Unicode 15.1 tables).
func IsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// Strip is Python str.strip() with no arguments.
func Strip(s string) string {
	start := 0
	for start < len(s) {
		r, size := DecodeRune(s, start)
		if !IsSpace(r) {
			break
		}
		start += size
	}
	end := len(s)
	for end > start {
		i := end - 1
		for i > start && !utf8.RuneStart(s[i]) {
			i--
		}
		r, _ := DecodeRune(s, i)
		if !IsSpace(r) {
			break
		}
		end = i
	}
	return s[start:end]
}

// IsPrintable is Python str.isprintable() for one code point. Go's unicode tables are
// Unicode 15.0 and CPython 3.13 uses 15.1; the characters assigned in 15.1 are added here.
func IsPrintable(r rune) bool {
	if IsSurrogate(r) {
		return false
	}
	if unicode.IsPrint(r) {
		return true
	}
	return (r >= 0x2ffc && r <= 0x2fff) || r == 0x31ef || (r >= 0x2ebf0 && r <= 0x2ee5d)
}
