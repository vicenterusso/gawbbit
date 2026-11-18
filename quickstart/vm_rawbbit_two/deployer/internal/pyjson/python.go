package pyjson

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// DecodeBytes mirrors json.loads(bytes) encoding detection (UTF-8 with or
// without BOM, UTF-16 and UTF-32 in either byte order) and decoding.
func DecodeBytes(b []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff}):
		return utf32(b[4:], binary.BigEndian)
	case bytes.HasPrefix(b, []byte{0xff, 0xfe, 0, 0}):
		return utf32(b[4:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		return utf16s(b[2:], binary.BigEndian)
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
		return utf16s(b[2:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}):
		b = b[3:]
	case len(b) >= 4 && b[0] == 0:
		if b[1] != 0 {
			return utf16s(b, binary.BigEndian)
		}
		return utf32(b, binary.BigEndian)
	case len(b) >= 4 && b[1] == 0:
		if b[2] != 0 || b[3] != 0 {
			return utf16s(b, binary.LittleEndian)
		}
		return utf32(b, binary.LittleEndian)
	case len(b) == 2 && b[0] == 0:
		return utf16s(b, binary.BigEndian)
	case len(b) == 2 && b[1] == 0:
		return utf16s(b, binary.LittleEndian)
	}
	if !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

func utf16s(b []byte, order binary.ByteOrder) (string, bool) {
	if len(b)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units)), true
}

func utf32(b []byte, order binary.ByteOrder) (string, bool) {
	if len(b)%4 != 0 {
		return "", false
	}
	var s strings.Builder
	for i := 0; i < len(b); i += 4 {
		r := rune(order.Uint32(b[i:]))
		if !utf8.ValidRune(r) {
			return "", false
		}
		s.WriteRune(r)
	}
	return s.String(), true
}

// Contains mirrors Python's `needle in container` for decoded JSON values:
// list membership, substring, or object key.
func Contains(container any, needle string) bool {
	switch c := container.(type) {
	case []any:
		for _, v := range c {
			if Equal(v, needle) {
				return true
			}
		}
	case string:
		return strings.Contains(c, needle)
	case *Object:
		_, ok := c.Get(needle)
		return ok
	}
	return false
}
