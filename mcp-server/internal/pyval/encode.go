package pyval

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// NonFinite selects how NaN and infinities are written.
type NonFinite int

const (
	// NonFiniteConstants writes NaN, Infinity and -Infinity (pydantic
	// inf_nan_mode="constants", used for the text content of tool results).
	NonFiniteConstants NonFinite = iota
	// NonFiniteNull writes null (pydantic's default, used by the MCP library
	// when it serializes structuredContent).
	NonFiniteNull
)

// SerializationError mirrors pydantic_core.PydanticSerializationError.
type SerializationError struct{ msg string }

func (e *SerializationError) Error() string { return "Error serializing to JSON: " + e.msg }

// EncodeJSON renders a value like pydantic_core.to_json: compact separators,
// non-ASCII kept verbatim and floats formatted with Rust's ryu rules.
func EncodeJSON(v Value, mode NonFinite) ([]byte, error) {
	var b strings.Builder
	if err := encode(&b, v, mode); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func encode(b *strings.Builder, v Value, mode NonFinite) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case *big.Int:
		b.WriteString(t.String())
	case float64:
		b.WriteString(jsonFloat(t, mode))
	case string:
		writeJSONString(b, t)
	case Decimal:
		writeJSONString(b, string(t))
	case Bytes:
		if err := validateUTF8(t); err != nil {
			return err
		}
		writeJSONString(b, string(t))
	case List:
		return encodeSeq(b, t, mode)
	case Tuple:
		return encodeSeq(b, t, mode)
	case *Dict:
		b.WriteByte('{')
		for i := range t.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			key, err := dictKey(t.Keys[i])
			if err != nil {
				return err
			}
			writeJSONString(b, key)
			b.WriteByte(':')
			if err := encode(b, t.Values[i], mode); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return &SerializationError{msg: fmt.Sprintf("Unable to serialize unknown type: %T", v)}
	}
	return nil
}

func encodeSeq(b *strings.Builder, items []Value, mode NonFinite) error {
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		if err := encode(b, item, mode); err != nil {
			return err
		}
	}
	b.WriteByte(']')
	return nil
}

func dictKey(k Value) (string, error) {
	switch t := k.(type) {
	case string:
		return t, nil
	case Decimal:
		return string(t), nil
	case *big.Int:
		return t.String(), nil
	case bool:
		return strconv.FormatBool(t), nil
	case nil:
		return "None", nil
	case float64:
		return jsonFloat(t, NonFiniteConstants), nil
	case Bytes:
		if err := validateUTF8(t); err != nil {
			return "", err
		}
		return string(t), nil
	}
	return "", &SerializationError{msg: fmt.Sprintf("`%s` not valid as object key", TypeName(k))}
}

func jsonFloat(f float64, mode NonFinite) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		if mode == NonFiniteNull {
			return "null"
		}
		switch {
		case math.IsNaN(f):
			return "NaN"
		case f > 0:
			return "Infinity"
		default:
			return "-Infinity"
		}
	}
	return RyuFormat(f)
}

// RyuFormat formats a finite float like Rust's ryu crate (used by serde_json
// and pydantic_core): 1.0, 0.30000000000000004, 1e20, 1e-7, 1.5e300.
func RyuFormat(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	digits, exp, neg := shortestDigits(f)
	length := len(digits)
	k := exp - length + 1 // f = digits * 10^k
	kk := length + k
	var out string
	switch {
	case k >= 0 && kk <= 16:
		out = digits + strings.Repeat("0", k) + ".0"
	case kk > 0 && kk <= 16:
		out = digits[:kk] + "." + digits[kk:]
	case kk > -5 && kk <= 0:
		out = "0." + strings.Repeat("0", -kk) + digits
	case length == 1:
		out = digits + "e" + strconv.Itoa(kk-1)
	default:
		out = digits[:1] + "." + digits[1:] + "e" + strconv.Itoa(kk-1)
	}
	if neg {
		out = "-" + out
	}
	return out
}

const hexDigits = "0123456789abcdef"

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var esc string
		switch {
		case c == '"':
			esc = `\"`
		case c == '\\':
			esc = `\\`
		case c == '\b':
			esc = `\b`
		case c == '\f':
			esc = `\f`
		case c == '\n':
			esc = `\n`
		case c == '\r':
			esc = `\r`
		case c == '\t':
			esc = `\t`
		case c < 0x20:
			esc = `\u00` + string(hexDigits[c>>4]) + string(hexDigits[c&0xf])
		default:
			continue
		}
		b.WriteString(s[start:i])
		b.WriteString(esc)
		start = i + 1
	}
	b.WriteString(s[start:])
	b.WriteByte('"')
}

// validateUTF8 reproduces Rust's core::str::from_utf8 error messages.
func validateUTF8(data []byte) error {
	n := len(data)
	for i := 0; i < n; {
		c := data[i]
		if c < 0x80 {
			i++
			continue
		}
		width, lo, hi := utf8Lead(c)
		if width == 0 {
			return utf8Invalid(i, 1)
		}
		for k := 1; k < width; k++ {
			if i+k >= n {
				return &SerializationError{msg: fmt.Sprintf("incomplete utf-8 byte sequence from index %d", i)}
			}
			cc := data[i+k]
			low, high := byte(0x80), byte(0xbf)
			if k == 1 {
				low, high = lo, hi
			}
			if cc < low || cc > high {
				return utf8Invalid(i, k)
			}
		}
		i += width
	}
	return nil
}

func utf8Invalid(index, length int) error {
	return &SerializationError{msg: fmt.Sprintf("invalid utf-8 sequence of %d bytes from index %d", length, index)}
}

// utf8Lead returns the sequence width and the valid range of the second byte.
func utf8Lead(c byte) (width int, lo, hi byte) {
	switch {
	case c >= 0xc2 && c <= 0xdf:
		return 2, 0x80, 0xbf
	case c == 0xe0:
		return 3, 0xa0, 0xbf
	case c >= 0xe1 && c <= 0xec, c == 0xee, c == 0xef:
		return 3, 0x80, 0xbf
	case c == 0xed:
		return 3, 0x80, 0x9f
	case c == 0xf0:
		return 4, 0x90, 0xbf
	case c >= 0xf1 && c <= 0xf3:
		return 4, 0x80, 0xbf
	case c == 0xf4:
		return 4, 0x80, 0x8f
	}
	return 0, 0, 0
}
