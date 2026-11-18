// Package pyjson reproduces the parts of CPython's json module that the
// original runner relied on: the exact decoder grammar (including NaN and
// Infinity literals and arbitrary-size integers), insertion-ordered objects,
// optional duplicate-key rejection, and json.dumps byte output (ensure_ascii,
// sort_keys, both separator styles and float repr).
//
// Decoded values are: nil, bool, int64, *big.Int, float64, string, []any and
// *Object.
package pyjson

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Object is a JSON object that keeps key insertion order. Duplicate keys
// (when allowed) keep the first position and the last value, like dict.
type Object struct {
	keys   []string
	values map[string]any
}

// NewObject builds an object from alternating key/value pairs.
func NewObject(pairs ...any) *Object {
	o := &Object{values: map[string]any{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

// Set inserts or replaces a key.
func (o *Object) Set(key string, value any) {
	if o.values == nil {
		o.values = map[string]any{}
	}
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// Get returns a value and whether the key exists.
func (o *Object) Get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

// Keys returns the keys in insertion order.
func (o *Object) Keys() []string { return append([]string(nil), o.keys...) }

// Len returns the number of keys.
func (o *Object) Len() int { return len(o.keys) }

// HasExactKeys reports whether the key set equals want (Python set(d) == {...}).
func (o *Object) HasExactKeys(want ...string) bool {
	if len(o.keys) != len(want) {
		return false
	}
	for _, key := range want {
		if _, ok := o.values[key]; !ok {
			return false
		}
	}
	return true
}

// Clone returns a shallow copy.
func (o *Object) Clone() *Object {
	c := &Object{keys: append([]string(nil), o.keys...), values: make(map[string]any, len(o.values))}
	for k, v := range o.values {
		c.values[k] = v
	}
	return c
}

// ErrDuplicateKey is returned by Decode when RejectDuplicates is set.
var ErrDuplicateKey = errors.New("duplicate JSON field")

// SyntaxError mirrors json.JSONDecodeError (a ValueError in Python).
type SyntaxError struct {
	Msg string
	Pos int
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("%s: char %d", e.Msg, e.Pos) }

// DecodeOptions controls decoding.
type DecodeOptions struct {
	RejectDuplicates bool
}

// Decode parses a complete JSON document from a Python str (valid UTF-8).
func Decode(s string, opts DecodeOptions) (any, error) {
	if !utf8.ValidString(s) {
		return nil, &SyntaxError{Msg: "invalid UTF-8", Pos: 0}
	}
	d := &decoder{s: s, opts: opts}
	if strings.HasPrefix(s, "\ufeff") {
		return nil, &SyntaxError{Msg: "Unexpected UTF-8 BOM (decode using utf-8-sig)", Pos: 0}
	}
	d.skip()
	value, err := d.value()
	if err != nil {
		return nil, err
	}
	d.skip()
	if d.i != len(d.s) {
		return nil, &SyntaxError{Msg: "Extra data", Pos: d.i}
	}
	return value, nil
}

type decoder struct {
	s    string
	i    int
	opts DecodeOptions
}

func (d *decoder) skip() {
	for d.i < len(d.s) {
		switch d.s[d.i] {
		case ' ', '\t', '\n', '\r':
			d.i++
		default:
			return
		}
	}
}

func (d *decoder) fail(msg string) error { return &SyntaxError{Msg: msg, Pos: d.i} }

func (d *decoder) value() (any, error) {
	if d.i >= len(d.s) {
		return nil, d.fail("Expecting value")
	}
	switch c := d.s[d.i]; {
	case c == '"':
		d.i++
		return d.str()
	case c == '{':
		d.i++
		return d.object()
	case c == '[':
		d.i++
		return d.array()
	case strings.HasPrefix(d.s[d.i:], "null"):
		d.i += 4
		return nil, nil
	case strings.HasPrefix(d.s[d.i:], "true"):
		d.i += 4
		return true, nil
	case strings.HasPrefix(d.s[d.i:], "false"):
		d.i += 5
		return false, nil
	case strings.HasPrefix(d.s[d.i:], "NaN"):
		d.i += 3
		return math.NaN(), nil
	case strings.HasPrefix(d.s[d.i:], "Infinity"):
		d.i += 8
		return math.Inf(1), nil
	case strings.HasPrefix(d.s[d.i:], "-Infinity"):
		d.i += 9
		return math.Inf(-1), nil
	case c == '-' || (c >= '0' && c <= '9'):
		return d.number()
	}
	return nil, d.fail("Expecting value")
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (d *decoder) number() (any, error) {
	start := d.i
	j := d.i
	if j < len(d.s) && d.s[j] == '-' {
		j++
	}
	if j >= len(d.s) || !isDigit(d.s[j]) {
		return nil, d.fail("Expecting value")
	}
	if d.s[j] == '0' {
		j++
	} else {
		for j < len(d.s) && isDigit(d.s[j]) {
			j++
		}
	}
	isFloat := false
	if j+1 < len(d.s) && d.s[j] == '.' && isDigit(d.s[j+1]) {
		isFloat = true
		j += 2
		for j < len(d.s) && isDigit(d.s[j]) {
			j++
		}
	}
	if j < len(d.s) && (d.s[j] == 'e' || d.s[j] == 'E') {
		k := j + 1
		if k < len(d.s) && (d.s[k] == '+' || d.s[k] == '-') {
			k++
		}
		if k < len(d.s) && isDigit(d.s[k]) {
			isFloat = true
			for k < len(d.s) && isDigit(d.s[k]) {
				k++
			}
			j = k
		}
	}
	d.i = j
	lit := d.s[start:j]
	if isFloat {
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil {
			var numErr *strconv.NumError
			if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
				return f, nil // Python float() overflows to inf / underflows to 0.
			}
			return nil, d.fail("Expecting value")
		}
		return f, nil
	}
	if n, err := strconv.ParseInt(lit, 10, 64); err == nil {
		return n, nil
	}
	n, ok := new(big.Int).SetString(lit, 10)
	if !ok {
		return nil, d.fail("Expecting value")
	}
	return n, nil
}

func hex4(s string) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s[:4], 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

func (d *decoder) str() (string, error) {
	var b strings.Builder
	for {
		if d.i >= len(d.s) {
			return "", d.fail("Unterminated string starting at")
		}
		c := d.s[d.i]
		switch {
		case c == '"':
			d.i++
			return b.String(), nil
		case c == '\\':
			d.i++
			if d.i >= len(d.s) {
				return "", d.fail("Unterminated string starting at")
			}
			e := d.s[d.i]
			d.i++
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, ok := hex4(d.s[d.i:])
				if !ok {
					return "", d.fail("Invalid \\uXXXX escape")
				}
				d.i += 4
				if utf16.IsSurrogate(r) && r < 0xdc00 && strings.HasPrefix(d.s[d.i:], "\\u") {
					if r2, ok := hex4(d.s[d.i+2:]); ok && r2 >= 0xdc00 && r2 <= 0xdfff {
						d.i += 6
						r = utf16.DecodeRune(r, r2)
					}
				}
				// Lone surrogates cannot be represented in Go strings.
				b.WriteRune(r)
			default:
				return "", d.fail("Invalid \\escape")
			}
		case c < 0x20:
			return "", d.fail("Invalid control character at")
		default:
			b.WriteByte(c)
			d.i++
		}
	}
}

func (d *decoder) object() (any, error) {
	obj := &Object{values: map[string]any{}}
	d.skip()
	if d.i < len(d.s) && d.s[d.i] == '}' {
		d.i++
		return obj, nil
	}
	for {
		d.skip()
		if d.i >= len(d.s) || d.s[d.i] != '"' {
			return nil, d.fail("Expecting property name enclosed in double quotes")
		}
		d.i++
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		d.skip()
		if d.i >= len(d.s) || d.s[d.i] != ':' {
			return nil, d.fail("Expecting ':' delimiter")
		}
		d.i++
		d.skip()
		value, err := d.value()
		if err != nil {
			return nil, err
		}
		if _, dup := obj.values[key]; dup && d.opts.RejectDuplicates {
			return nil, ErrDuplicateKey
		}
		obj.Set(key, value)
		d.skip()
		if d.i < len(d.s) && d.s[d.i] == '}' {
			d.i++
			return obj, nil
		}
		if d.i >= len(d.s) || d.s[d.i] != ',' {
			return nil, d.fail("Expecting ',' delimiter")
		}
		d.i++
	}
}

func (d *decoder) array() (any, error) {
	list := []any{}
	d.skip()
	if d.i < len(d.s) && d.s[d.i] == ']' {
		d.i++
		return list, nil
	}
	for {
		d.skip()
		value, err := d.value()
		if err != nil {
			return nil, err
		}
		list = append(list, value)
		d.skip()
		if d.i < len(d.s) && d.s[d.i] == ']' {
			d.i++
			return list, nil
		}
		if d.i >= len(d.s) || d.s[d.i] != ',' {
			return nil, d.fail("Expecting ',' delimiter")
		}
		d.i++
	}
}

// EncodeOptions mirrors json.dumps keyword arguments.
type EncodeOptions struct {
	SortKeys bool
	Compact  bool // separators=(',', ':') instead of (', ', ': ')
}

// Encode renders v like json.dumps(v, ensure_ascii=True, allow_nan=True).
// Go maps are always rendered with sorted keys.
func Encode(v any, opts EncodeOptions) string {
	var b strings.Builder
	encode(&b, v, opts)
	return b.String()
}

// Compact is json.dumps(v, sort_keys=True, separators=(',', ':')).
func Compact(v any) string { return Encode(v, EncodeOptions{SortKeys: true, Compact: true}) }

func encode(b *strings.Builder, v any, opts EncodeOptions) {
	item, kv := ", ", ": "
	if opts.Compact {
		item, kv = ",", ":"
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case *big.Int:
		b.WriteString(x.String())
	case float64:
		b.WriteString(FloatRepr(x))
	case string:
		writeString(b, x)
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(item)
			}
			writeString(b, s)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(item)
			}
			encode(b, e, opts)
		}
		b.WriteByte(']')
	case *Object:
		keys := x.Keys()
		if opts.SortKeys {
			sort.Strings(keys)
		}
		writeObject(b, keys, func(k string) any { return x.values[k] }, opts, item, kv)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		writeObject(b, keys, func(k string) any { return x[k] }, opts, item, kv)
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		writeObject(b, keys, func(k string) any { return x[k] }, opts, item, kv)
	default:
		panic(fmt.Sprintf("pyjson: unsupported type %T", v))
	}
}

func writeObject(b *strings.Builder, keys []string, get func(string) any, opts EncodeOptions, item, kv string) {
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(item)
		}
		writeString(b, k)
		b.WriteString(kv)
		encode(b, get(k), opts)
	}
	b.WriteByte('}')
}

func writeString(b *strings.Builder, s string) {
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
			case r > 0xffff:
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, r1, r2)
			case r < 0x20 || r > 0x7e: // Python escapes everything outside ' '..'~'.
				fmt.Fprintf(b, `\u%04x`, r)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// FloatRepr formats f exactly like Python's float.__repr__ (and json.dumps).
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64) // e.g. -1.2345e+06
	sign := ""
	if sci[0] == '-' {
		sign, sci = "-", sci[1:]
	}
	mant, expPart, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expPart)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}
	return sign + digits[:exp+1] + "." + digits[exp+1:]
}

// IsInt reports whether v is a Python int (bool excluded) equal to want.
func IsInt(v any, want int64) bool {
	switch x := v.(type) {
	case int64:
		return x == want
	case int:
		return int64(x) == want
	}
	return false
}

// Equal implements Python == for decoded JSON values (1 == 1.0 == True).
func Equal(a, b any) bool {
	if na, ok := numeric(a); ok {
		nb, ok := numeric(b)
		return ok && na.Cmp(nb) == 0 && !isNaN(a) && !isNaN(b)
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case *Object:
		y, ok := b.(*Object)
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
	return reflect.DeepEqual(a, b)
}

func isNaN(v any) bool {
	f, ok := v.(float64)
	return ok && math.IsNaN(f)
}

func numeric(v any) (*big.Float, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case int:
		return new(big.Float).SetInt64(int64(x)), true
	case int64:
		return new(big.Float).SetInt64(x), true
	case *big.Int:
		return new(big.Float).SetInt(x), true
	case float64:
		if math.IsNaN(x) {
			return big.NewFloat(0), true
		}
		return big.NewFloat(x), true
	}
	return nil, false
}

// Str renders a decoded value like Python's str().
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case float64:
		if math.IsNaN(x) {
			return "nan"
		}
		if math.IsInf(x, 1) {
			return "inf"
		}
		if math.IsInf(x, -1) {
			return "-inf"
		}
		return FloatRepr(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case *big.Int:
		return x.String()
	}
	return Encode(v, EncodeOptions{})
}
