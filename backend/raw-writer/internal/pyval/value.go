// Package pyval models decoded JSON the way Python's json module does and renders it
// the way Python's str(), repr() and json.dumps() do.
//
// The raw-writer output contract was defined by a Python implementation that stored
// str(value) for scalar columns and json.dumps(value, separators=(",", ":"),
// ensure_ascii=True) for nested columns. Reproducing those renderings exactly keeps the
// Parquet contents identical for every possible input, including odd payloads.
//
// Value representation (all held in an `any`):
//
//	nil      -> None
//	bool     -> bool
//	Int      -> int (canonical base-10 digits, arbitrary size)
//	float64  -> float
//	string   -> str, encoded as WTF-8 so lone surrogates from \uD800-style escapes survive
//	[]any    -> list
//	*Object  -> dict (insertion ordered)
package pyval

import (
	"math"
	"strconv"
	"strings"
)

// Int is a Python int kept as canonical decimal text ("0", "-12", ...).
type Int string

// Object is a Python dict decoded from a JSON object: keys keep first-insertion order
// and a repeated key overwrites the earlier value in place.
type Object struct {
	keys []string
	vals map[string]any
}

// NewObject returns an empty dict.
func NewObject() *Object {
	return &Object{vals: map[string]any{}}
}

// Set inserts or overwrites key with Python dict semantics.
func (o *Object) Set(key string, value any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
}

// Get returns the value for key and whether it exists (dict.get).
func (o *Object) Get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Len is len(dict).
func (o *Object) Len() int { return len(o.keys) }

// Keys returns the keys in insertion order.
func (o *Object) Keys() []string { return o.keys }

// Truthy reports Python truthiness (bool(value)).
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case Int:
		return x != "0"
	case float64:
		return x != 0 // NaN is truthy, like Python
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *Object:
		return x.Len() > 0
	}
	return true
}

// TypeName is type(value).__name__.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case *Object:
		return "dict"
	}
	return "object"
}

// Str is Python str(value).
func Str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

// Repr is Python repr(value).
func Repr(v any) string {
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

func writeRepr(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if x {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case Int:
		b.WriteString(string(x))
	case float64:
		b.WriteString(FloatRepr(x))
	case string:
		writeStrRepr(b, x)
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, item)
		}
		b.WriteByte(']')
	case *Object:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writeStrRepr(b, k)
			b.WriteString(": ")
			writeRepr(b, x.vals[k])
		}
		b.WriteByte('}')
	}
}

const hexDigits = "0123456789abcdef"

// writeStrRepr follows CPython's unicode_repr.
func writeStrRepr(b *strings.Builder, s string) {
	quote := byte('\'')
	if strings.IndexByte(s, '\'') >= 0 && strings.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, size := DecodeRune(s, i)
		i += size
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
		case r < ' ' || r == 0x7f:
			b.WriteString(`\x`)
			b.WriteByte(hexDigits[r>>4])
			b.WriteByte(hexDigits[r&0xf])
		case r < 0x7f:
			b.WriteByte(byte(r))
		case IsPrintable(r):
			b.WriteRune(r)
		case r <= 0xff:
			b.WriteString(`\x`)
			b.WriteByte(hexDigits[r>>4])
			b.WriteByte(hexDigits[r&0xf])
		case r <= 0xffff:
			b.WriteString(`\u`)
			writeHex(b, uint32(r), 4)
		default:
			b.WriteString(`\U`)
			writeHex(b, uint32(r), 8)
		}
	}
	b.WriteByte(quote)
}

func writeHex(b *strings.Builder, v uint32, width int) {
	for shift := (width - 1) * 4; shift >= 0; shift -= 4 {
		b.WriteByte(hexDigits[(v>>uint(shift))&0xf])
	}
}

// FloatRepr is Python repr(float): shortest round-trip digits, exponent notation when
// the decimal point position is <= -4 or > 16.
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // [-]d[.ddd]e±XX
	sign := ""
	if e[0] == '-' {
		sign = "-"
		e = e[1:]
	}
	mant, expPart, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	decpt := exp + 1

	var out string
	switch {
	case decpt <= -4 || decpt > 16:
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		expSign := "+"
		if exp < 0 {
			expSign = "-"
			exp = -exp
		}
		expText := strconv.Itoa(exp)
		if len(expText) < 2 {
			expText = "0" + expText
		}
		out += "e" + expSign + expText
	case decpt <= 0:
		out = "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	default:
		out = digits[:decpt] + "." + digits[decpt:]
	}
	return sign + out
}

// Dumps is json.dumps(value, separators=(",", ":"), ensure_ascii=True).
func Dumps(v any) string {
	var b strings.Builder
	writeJSON(&b, v)
	return b.String()
}

func writeJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case Int:
		b.WriteString(string(x))
	case float64:
		switch {
		case math.IsNaN(x):
			b.WriteString("NaN")
		case math.IsInf(x, 1):
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(FloatRepr(x))
		}
	case string:
		writeJSONString(b, x)
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, item)
		}
		b.WriteByte(']')
	case *Object:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, k)
			b.WriteByte(':')
			writeJSON(b, x.vals[k])
		}
		b.WriteByte('}')
	}
}

// writeJSONString follows CPython's ascii-only JSON string encoder.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := DecodeRune(s, i)
		i += size
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= ' ' && r <= '~':
			b.WriteByte(byte(r))
		case r >= 0x10000:
			v := r - 0x10000
			b.WriteString(`\u`)
			writeHex(b, uint32(0xd800|(v>>10)&0x3ff), 4)
			b.WriteString(`\u`)
			writeHex(b, uint32(0xdc00|v&0x3ff), 4)
		default:
			b.WriteString(`\u`)
			writeHex(b, uint32(r), 4)
		}
	}
	b.WriteByte('"')
}
