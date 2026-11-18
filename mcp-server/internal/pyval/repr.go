package pyval

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func parseFloatLiteral(literal string) float64 {
	// strconv reports overflow as ±Inf with ErrRange, matching Python's float().
	f, _ := strconv.ParseFloat(literal, 64)
	return f
}

// Repr renders a value like Python's repr().
func Repr(v Value) string {
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

func writeRepr(b *strings.Builder, v Value) {
	switch t := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if t {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case *big.Int:
		b.WriteString(t.String())
	case float64:
		b.WriteString(FloatRepr(t))
	case string:
		b.WriteString(StrRepr(t))
	case Decimal:
		b.WriteString("Decimal(" + StrRepr(string(t)) + ")")
	case Bytes:
		b.WriteString(bytesRepr(t))
	case List:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, item)
		}
		b.WriteByte(']')
	case Tuple:
		b.WriteByte('(')
		for i, item := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, item)
		}
		if len(t) == 1 {
			b.WriteByte(',')
		}
		b.WriteByte(')')
	case *Dict:
		b.WriteByte('{')
		for i := range t.Keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, t.Keys[i])
			b.WriteString(": ")
			writeRepr(b, t.Values[i])
		}
		b.WriteByte('}')
	default:
		fmt.Fprintf(b, "%v", t)
	}
}

// FloatRepr renders a float like Python's repr(float).
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	digits, exp, neg := shortestDigits(f)
	// decpt is the position of the decimal point relative to the digits.
	decpt := exp + 1
	var out string
	if decpt <= -4 || decpt > 16 {
		mantissa := digits[:1]
		if len(digits) > 1 {
			mantissa += "." + digits[1:]
		}
		e := decpt - 1
		sign := "+"
		if e < 0 {
			sign = "-"
			e = -e
		}
		out = fmt.Sprintf("%se%s%02d", mantissa, sign, e)
	} else {
		out = fixedNotation(digits, decpt)
		if !strings.Contains(out, ".") {
			out += ".0"
		}
	}
	if neg {
		out = "-" + out
	}
	return out
}

// shortestDigits returns the shortest round-trip decimal digits of |f| and
// the decimal exponent of the first digit (f = 0.d1d2... * 10^(exp+1)).
func shortestDigits(f float64) (digits string, exp int, neg bool) {
	neg = math.Signbit(f)
	s := strconv.FormatFloat(math.Abs(f), 'e', -1, 64) // d.ddde±XX
	mant, expPart, _ := strings.Cut(s, "e")
	digits = strings.Replace(mant, ".", "", 1)
	exp, _ = strconv.Atoi(expPart)
	return digits, exp, neg
}

func fixedNotation(digits string, decpt int) string {
	switch {
	case decpt <= 0:
		return "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		return digits + strings.Repeat("0", decpt-len(digits))
	default:
		return digits[:decpt] + "." + digits[decpt:]
	}
}

// StrRepr renders a string like Python's repr(str).
func StrRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
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
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case isPrintable(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// isPrintable approximates str.isprintable(): every letter, mark, number,
// punctuation and symbol, but no separators or control/format characters.
func isPrintable(r rune) bool {
	return r != utf8.RuneError && unicode.IsPrint(r)
}

func bytesRepr(data []byte) string {
	quote := byte('\'')
	if strings.Contains(string(data), "'") && !strings.Contains(string(data), "\"") {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte('b')
	b.WriteByte(quote)
	for _, c := range data {
		switch {
		case c == quote || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < ' ' || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// TruncatedRepr mirrors pydantic_core's truncate_safe_repr: representations
// longer than 50 bytes keep the first 25 and the last 24 bytes.
func TruncatedRepr(v Value) string {
	const maxLen = 50
	s := Repr(v)
	if len(s) <= maxLen {
		return s
	}
	mid := (maxLen + 1) / 2
	left := mid
	for left > 0 && !utf8.RuneStart(s[left]) {
		left--
	}
	right := len(s) - (mid - 1)
	for right < len(s) && !utf8.RuneStart(s[right]) {
		right++
	}
	return s[:left] + "..." + s[right:]
}
