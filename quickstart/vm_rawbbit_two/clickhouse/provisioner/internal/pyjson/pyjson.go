// Package pyjson reproduces the Python json/dict semantics the original
// provisioner relied on: insertion-ordered objects, arbitrary precision
// integers, Python equality and json.dumps(sort_keys=True, separators=(",", ":")).
package pyjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Value is nil, bool, *big.Int, float64, string, List or *Dict.
type Value = any

// List is a JSON array.
type List []Value

// Dict is an insertion-ordered JSON object.
type Dict struct {
	keys   []string
	values map[string]Value
}

// NewDict returns an empty dict.
func NewDict() *Dict { return &Dict{values: map[string]Value{}} }

// Set assigns a key, appending it when new (Python dict semantics).
func (d *Dict) Set(key string, v Value) {
	if _, ok := d.values[key]; !ok {
		d.keys = append(d.keys, key)
	}
	d.values[key] = v
}

// Get returns the value for key.
func (d *Dict) Get(key string) (Value, bool) {
	v, ok := d.values[key]
	return v, ok
}

// Lookup is dict.get(key) (nil when absent).
func (d *Dict) Lookup(key string) Value { return d.values[key] }

// Keys returns the keys in insertion order.
func (d *Dict) Keys() []string { return append([]string(nil), d.keys...) }

// Len is len(dict).
func (d *Dict) Len() int { return len(d.keys) }

// Int builds a Python int.
func Int(v int64) *big.Int { return big.NewInt(v) }

// Decode parses JSON like json.loads (NaN and Infinity literals included).
func Decode(data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(replaceNonFinite(data)))
	dec.UseNumber()
	v, err := decode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("extra data")
	}
	return v, nil
}

// Non-finite literals are accepted by json.loads; map them to sentinel
// strings outside of JSON strings so encoding/json can tokenize them.
const (
	nanSentinel    = "\x00NaN"
	posInfSentinel = "\x00Infinity"
	negInfSentinel = "\x00-Infinity"
)

func replaceNonFinite(data []byte) []byte {
	if !bytes.Contains(data, []byte("NaN")) && !bytes.Contains(data, []byte("Infinity")) {
		return data
	}
	var out bytes.Buffer
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(data) {
					i++
					out.WriteByte(data[i])
				}
			case '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out.WriteByte(c)
		case bytes.HasPrefix(data[i:], []byte("-Infinity")):
			out.WriteString(strconv.Quote(negInfSentinel))
			i += len("-Infinity") - 1
		case bytes.HasPrefix(data[i:], []byte("Infinity")):
			out.WriteString(strconv.Quote(posInfSentinel))
			i += len("Infinity") - 1
		case bytes.HasPrefix(data[i:], []byte("NaN")):
			out.WriteString(strconv.Quote(nanSentinel))
			i += len("NaN") - 1
		default:
			out.WriteByte(c)
		}
	}
	return out.Bytes()
}

func decode(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			d := NewDict()
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid key")
				}
				v, err := decode(dec)
				if err != nil {
					return nil, err
				}
				d.Set(key, v)
			}
			_, err := dec.Token()
			return d, err
		case '[':
			list := List{}
			for dec.More() {
				v, err := decode(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			_, err := dec.Token()
			return list, err
		}
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			n, ok := new(big.Int).SetString(s, 10)
			if !ok {
				return nil, fmt.Errorf("invalid number %q", s)
			}
			return n, nil
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f, nil
	case string:
		switch t {
		case nanSentinel:
			return math.NaN(), nil
		case posInfSentinel:
			return math.Inf(1), nil
		case negInfSentinel:
			return math.Inf(-1), nil
		}
		return t, nil
	default:
		return t, nil
	}
	return nil, errors.New("unexpected token")
}

// IsInt reports isinstance(v, int) and not isinstance(v, bool).
func IsInt(v Value) (*big.Int, bool) {
	n, ok := v.(*big.Int)
	return n, ok
}

// Equal implements Python == for JSON values.
func Equal(a, b Value) bool {
	if na, ok := number(a); ok {
		nb, ok := number(b)
		return ok && na.Cmp(nb) == 0 && !isNaN(a) && !isNaN(b)
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case List:
		y, ok := b.(List)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case *Dict:
		y, ok := b.(*Dict)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for _, k := range x.keys {
			yv, ok := y.values[k]
			if !ok || !Equal(x.values[k], yv) {
				return false
			}
		}
		return true
	}
	return false
}

func isNaN(v Value) bool {
	f, ok := v.(float64)
	return ok && math.IsNaN(f)
}

// number converts bool/int/float to a comparable big.Float.
func number(v Value) (*big.Float, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case *big.Int:
		return new(big.Float).SetInt(t), true
	case float64:
		if math.IsNaN(t) {
			return big.NewFloat(0), true
		}
		return big.NewFloat(t), true
	}
	return nil, false
}

// Truthy is bool(v).
func Truthy(v Value) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case *big.Int:
		return t.Sign() != 0
	case float64:
		return t != 0
	case string:
		return t != ""
	case List:
		return len(t) > 0
	case *Dict:
		return t.Len() > 0
	}
	return true
}

// Str is Python str(v).
func Str(v Value) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case *big.Int:
		return t.String()
	case float64:
		return floatRepr(t)
	case string:
		return t
	}
	return Repr(v)
}

// Repr is Python repr(v) for JSON values.
func Repr(v Value) string {
	switch t := v.(type) {
	case string:
		return strRepr(t)
	case List:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = Repr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Dict:
		parts := make([]string, 0, t.Len())
		for _, k := range t.keys {
			parts = append(parts, strRepr(k)+": "+Repr(t.values[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return Str(v)
}

func strRepr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch {
		case string(r) == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f || unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

func floatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	mant, expPart, _ := strings.Cut(s, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	decpt := exp + 1
	var out string
	if decpt <= -4 || decpt > 16 {
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		sign := "+"
		if exp < 0 {
			sign, exp = "-", -exp
		}
		out += fmt.Sprintf("e%s%02d", sign, exp)
	} else {
		switch {
		case decpt <= 0:
			out = "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			out = digits[:decpt] + "." + digits[decpt:]
		}
	}
	if math.Signbit(f) {
		out = "-" + out
	}
	return out
}

// Dumps is json.dumps(v, sort_keys=True, separators=(",", ":")) with the
// default ensure_ascii=True.
func Dumps(v Value) string {
	var b strings.Builder
	dumps(&b, v)
	return b.String()
}

func dumps(b *strings.Builder, v Value) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case *big.Int:
		b.WriteString(t.String())
	case float64:
		switch {
		case math.IsNaN(t):
			b.WriteString("NaN")
		case math.IsInf(t, 1):
			b.WriteString("Infinity")
		case math.IsInf(t, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(floatRepr(t))
		}
	case string:
		writeASCIIString(b, t)
	case List:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			dumps(b, item)
		}
		b.WriteByte(']')
	case *Dict:
		keys := t.Keys()
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeASCIIString(b, k)
			b.WriteByte(':')
			dumps(b, t.values[k])
		}
		b.WriteByte('}')
	}
}

func writeASCIIString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || (r > 0x7e && r <= 0xffff):
				fmt.Fprintf(b, `\u%04x`, r)
			case r > 0xffff:
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// IsSpace mirrors str.isspace() for one character.
func IsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// Strip mirrors str.strip().
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// SplitLines mirrors str.splitlines().
func SplitLines(s string) []string {
	var lines []string
	start := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, string(runes[start:i]))
			if runes[i] == '\r' && i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(runes) {
		lines = append(lines, string(runes[start:]))
	}
	return lines
}
