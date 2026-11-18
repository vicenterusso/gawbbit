package pyjson

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// Encode renders v exactly like Python's
// json.dumps(v, separators=(",", ":")) with the default ensure_ascii=True.
func Encode(v Value) []byte {
	return AppendValue(make([]byte, 0, 256), v)
}

// AppendValue appends the encoding of v to dst.
func AppendValue(dst []byte, v Value) []byte {
	switch v.Kind {
	case Null:
		return append(dst, "null"...)
	case Bool:
		if v.Bool {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case Int:
		return strconv.AppendInt(dst, v.Int, 10)
	case BigInt:
		return append(dst, v.Str...)
	case Float:
		return appendFloat(dst, v.Float)
	case String:
		return AppendString(dst, v.Str)
	case Array:
		dst = append(dst, '[')
		for i, item := range v.Array {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendValue(dst, item)
		}
		return append(dst, ']')
	case Object:
		dst = append(dst, '{')
		for i, m := range v.Members {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendString(dst, m.Key)
			dst = append(dst, ':')
			dst = AppendValue(dst, m.Value)
		}
		return append(dst, '}')
	}
	return dst
}

// appendFloat matches json.dumps for floats (allow_nan=True).
func appendFloat(dst []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(dst, "NaN"...)
	case math.IsInf(f, 1):
		return append(dst, "Infinity"...)
	case math.IsInf(f, -1):
		return append(dst, "-Infinity"...)
	}
	return append(dst, FloatRepr(f)...)
}

// FloatRepr returns Python's repr() of a finite float: the shortest
// round-tripping digits, in positional notation when the decimal exponent is
// in [-4, 16), otherwise in scientific notation with a two-digit exponent.
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // e.g. "-1.2345e+16"
	var b strings.Builder
	if s[0] == '-' {
		b.WriteByte('-')
		s = s[1:]
	}
	ePos := strings.IndexByte(s, 'e')
	exp, _ := strconv.Atoi(s[ePos+1:])
	digits := strings.Replace(s[:ePos], ".", "", 1)
	decpt := exp + 1
	if decpt <= -4 || decpt > 16 {
		b.WriteByte(digits[0])
		if len(digits) > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		if exp < 0 {
			b.WriteByte('-')
			exp = -exp
		} else {
			b.WriteByte('+')
		}
		if exp < 10 {
			b.WriteByte('0')
		}
		b.WriteString(strconv.Itoa(exp))
		return b.String()
	}
	switch {
	case decpt <= 0:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -decpt))
		b.WriteString(digits)
	case decpt >= len(digits):
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", decpt-len(digits)))
		b.WriteString(".0")
	default:
		b.WriteString(digits[:decpt])
		b.WriteByte('.')
		b.WriteString(digits[decpt:])
	}
	return b.String()
}

// AppendString appends s as a JSON string using Python's ensure_ascii
// escaping: everything outside printable ASCII becomes \uXXXX (UTF-16
// surrogate pairs above the BMP), with lowercase hex digits.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				if c < 0x20 || c == 0x7f {
					dst = appendUnicodeEscape(dst, rune(c))
				} else {
					dst = append(dst, c)
				}
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r > 0xFFFF {
			r -= 0x10000
			dst = appendUnicodeEscape(dst, 0xD800+(r>>10))
			dst = appendUnicodeEscape(dst, 0xDC00+(r&0x3FF))
		} else {
			dst = appendUnicodeEscape(dst, r)
		}
	}
	return append(dst, '"')
}

func appendUnicodeEscape(dst []byte, r rune) []byte {
	return append(dst, '\\', 'u',
		hexDigits[(r>>12)&0xF], hexDigits[(r>>8)&0xF], hexDigits[(r>>4)&0xF], hexDigits[r&0xF])
}
