package pyval

import (
	"errors"
	"math"
	"strconv"
	"unicode/utf8"
)

// MaxDepth is the deepest array/object nesting (counting the event object itself) that
// json.loads accepted inside the CPython 3.13 raw-writer loop before raising
// RecursionError; deeper messages were TERMed as undecodable.
const MaxDepth = 9994

// maxIntDigits is CPython's default int max_str_digits; longer integer literals make
// json.loads raise ValueError.
const maxIntDigits = 4300

// ErrDecode reports bytes that json.loads(data.decode("utf-8")) rejects.
var ErrDecode = errors.New("json decode failed")

// Loads decodes data like json.loads(data.decode("utf-8")): strict UTF-8, NaN and
// Infinity literals, arbitrary-size integers, duplicate keys keep the last value and
// lone surrogate escapes are preserved.
func Loads(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, ErrDecode
	}
	d := decoder{s: data}
	d.skipSpace()
	v, err := d.value(0)
	if err != nil {
		return nil, err
	}
	d.skipSpace()
	if d.i != len(d.s) {
		return nil, ErrDecode
	}
	return v, nil
}

type decoder struct {
	s []byte
	i int
}

func (d *decoder) skipSpace() {
	for d.i < len(d.s) {
		switch d.s[d.i] {
		case ' ', '\t', '\n', '\r':
			d.i++
		default:
			return
		}
	}
}

func (d *decoder) literal(word string) bool {
	if len(d.s)-d.i >= len(word) && string(d.s[d.i:d.i+len(word)]) == word {
		d.i += len(word)
		return true
	}
	return false
}

func (d *decoder) value(depth int) (any, error) {
	if d.i >= len(d.s) {
		return nil, ErrDecode
	}
	switch c := d.s[d.i]; {
	case c == '"':
		d.i++
		return d.str()
	case c == '{':
		if depth >= MaxDepth {
			return nil, ErrDecode
		}
		d.i++
		return d.object(depth + 1)
	case c == '[':
		if depth >= MaxDepth {
			return nil, ErrDecode
		}
		d.i++
		return d.array(depth + 1)
	case c == 'n' && d.literal("null"):
		return nil, nil
	case c == 't' && d.literal("true"):
		return true, nil
	case c == 'f' && d.literal("false"):
		return false, nil
	case c == 'N' && d.literal("NaN"):
		return nan(), nil
	case c == 'I' && d.literal("Infinity"):
		return inf(1), nil
	case c == '-' && d.i+1 < len(d.s) && d.s[d.i+1] == 'I':
		d.i++
		if d.literal("Infinity") {
			return inf(-1), nil
		}
		return nil, ErrDecode
	case c == '-' || (c >= '0' && c <= '9'):
		return d.number()
	}
	return nil, ErrDecode
}

func (d *decoder) object(depth int) (any, error) {
	obj := NewObject()
	d.skipSpace()
	if d.i < len(d.s) && d.s[d.i] == '}' {
		d.i++
		return obj, nil
	}
	for {
		if d.i >= len(d.s) || d.s[d.i] != '"' {
			return nil, ErrDecode
		}
		d.i++
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		d.skipSpace()
		if d.i >= len(d.s) || d.s[d.i] != ':' {
			return nil, ErrDecode
		}
		d.i++
		d.skipSpace()
		v, err := d.value(depth)
		if err != nil {
			return nil, err
		}
		obj.Set(key.(string), v)
		d.skipSpace()
		if d.i >= len(d.s) {
			return nil, ErrDecode
		}
		switch d.s[d.i] {
		case '}':
			d.i++
			return obj, nil
		case ',':
			d.i++
			d.skipSpace()
		default:
			return nil, ErrDecode
		}
	}
}

func (d *decoder) array(depth int) (any, error) {
	list := []any{}
	d.skipSpace()
	if d.i < len(d.s) && d.s[d.i] == ']' {
		d.i++
		return list, nil
	}
	for {
		v, err := d.value(depth)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
		d.skipSpace()
		if d.i >= len(d.s) {
			return nil, ErrDecode
		}
		switch d.s[d.i] {
		case ']':
			d.i++
			return list, nil
		case ',':
			d.i++
			d.skipSpace()
		default:
			return nil, ErrDecode
		}
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// number matches -?(0|[1-9]\d*)(\.\d+)?([eE][-+]?\d+)? like CPython's scanner.
func (d *decoder) number() (any, error) {
	start := d.i
	if d.s[d.i] == '-' {
		d.i++
	}
	if d.i >= len(d.s) || !isDigit(d.s[d.i]) {
		return nil, ErrDecode
	}
	if d.s[d.i] == '0' {
		d.i++
	} else {
		for d.i < len(d.s) && isDigit(d.s[d.i]) {
			d.i++
		}
	}
	isFloat := false
	if d.i+1 < len(d.s) && d.s[d.i] == '.' && isDigit(d.s[d.i+1]) {
		isFloat = true
		d.i += 2
		for d.i < len(d.s) && isDigit(d.s[d.i]) {
			d.i++
		}
	}
	if d.i < len(d.s) && (d.s[d.i] == 'e' || d.s[d.i] == 'E') {
		j := d.i + 1
		if j < len(d.s) && (d.s[j] == '+' || d.s[j] == '-') {
			j++
		}
		if j < len(d.s) && isDigit(d.s[j]) {
			for j < len(d.s) && isDigit(d.s[j]) {
				j++
			}
			isFloat = true
			d.i = j
		}
	}
	text := string(d.s[start:d.i])
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, ErrDecode
		}
		return f, nil
	}
	digits := text
	if digits[0] == '-' {
		digits = digits[1:]
	}
	if len(digits) > maxIntDigits {
		return nil, ErrDecode
	}
	if digits == "0" {
		return Int("0"), nil
	}
	return Int(text), nil
}

func hexValue(b []byte) (rune, bool) {
	var v rune
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// str scans a string body after the opening quote (strict mode: no raw control chars).
func (d *decoder) str() (any, error) {
	var buf []byte
	for {
		if d.i >= len(d.s) {
			return nil, ErrDecode
		}
		c := d.s[d.i]
		switch {
		case c == '"':
			d.i++
			return string(buf), nil
		case c < 0x20:
			return nil, ErrDecode
		case c != '\\':
			buf = append(buf, c)
			d.i++
			continue
		}
		d.i++
		if d.i >= len(d.s) {
			return nil, ErrDecode
		}
		esc := d.s[d.i]
		d.i++
		switch esc {
		case '"', '\\', '/':
			buf = append(buf, esc)
		case 'b':
			buf = append(buf, '\b')
		case 'f':
			buf = append(buf, '\f')
		case 'n':
			buf = append(buf, '\n')
		case 'r':
			buf = append(buf, '\r')
		case 't':
			buf = append(buf, '\t')
		case 'u':
			if d.i+4 > len(d.s) {
				return nil, ErrDecode
			}
			r, ok := hexValue(d.s[d.i : d.i+4])
			if !ok {
				return nil, ErrDecode
			}
			d.i += 4
			if r >= 0xd800 && r <= 0xdbff && d.i+6 <= len(d.s) && d.s[d.i] == '\\' && d.s[d.i+1] == 'u' {
				low, ok := hexValue(d.s[d.i+2 : d.i+6])
				if !ok {
					return nil, ErrDecode
				}
				if low >= 0xdc00 && low <= 0xdfff {
					r = 0x10000 + (r-0xd800)<<10 | (low - 0xdc00)
					d.i += 6
				}
			}
			buf = AppendRune(buf, r)
		default:
			return nil, ErrDecode
		}
	}
}

func nan() float64 { return math.NaN() }

func inf(sign int) float64 { return math.Inf(sign) }
