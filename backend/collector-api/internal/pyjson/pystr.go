package pyjson

import (
	"strconv"
	"strings"
	"unicode"
)

// PyStr returns Python's str() of the object json.loads would produce for v.
// It is used where the original code applied str() to JSON-decoded values.
func PyStr(v Value) string {
	if v.Kind == String {
		return v.Str
	}
	return pyRepr(v)
}

func pyRepr(v Value) string {
	switch v.Kind {
	case Null:
		return "None"
	case Bool:
		if v.Bool {
			return "True"
		}
		return "False"
	case Int:
		return strconv.FormatInt(v.Int, 10)
	case BigInt:
		return v.Str
	case Float:
		return FloatRepr(v.Float)
	case String:
		return reprString(v.Str)
	case Array:
		parts := make([]string, len(v.Array))
		for i, item := range v.Array {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case Object:
		keys, values := dedupMembers(v.Members)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = reprString(k) + ": " + pyRepr(values[i])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

// dedupMembers applies Python dict insertion semantics to duplicate keys:
// first position, last value.
func dedupMembers(members []Member) ([]string, []Value) {
	index := make(map[string]int, len(members))
	var keys []string
	var values []Value
	for _, m := range members {
		if i, ok := index[m.Key]; ok {
			values[i] = m.Value
			continue
		}
		index[m.Key] = len(keys)
		keys = append(keys, m.Key)
		values = append(values, m.Value)
	}
	return keys, values
}

// Repr returns Python's repr() of a str (CPython's unicode_repr).
func Repr(s string) string { return reprString(s) }

func reprString(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x`)
			b.WriteString(hexPad(int(r), 2))
		case r < 0x7f || unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			b.WriteString(`\x`)
			b.WriteString(hexPad(int(r), 2))
		case r < 0x10000:
			b.WriteString(`\u`)
			b.WriteString(hexPad(int(r), 4))
		default:
			b.WriteString(`\U`)
			b.WriteString(hexPad(int(r), 8))
		}
	}
	b.WriteByte(quote)
	return b.String()
}

func hexPad(n, width int) string {
	s := strconv.FormatInt(int64(n), 16)
	return strings.Repeat("0", width-len(s)) + s
}
