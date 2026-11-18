package pyjson

import (
	"errors"
	"math"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrInvalid is returned for any input jiter would reject. The collector never
// exposes parser positions or messages, so a single error value is enough.
var ErrInvalid = errors.New("invalid JSON")

// maxDepth mirrors jiter's DEFAULT_RECURSION_LIMIT: at most 200 nested
// non-empty arrays/objects.
const maxDepth = 200

// maxIntChars mirrors jiter's 4300 character limit on integer literals
// (sign included), matching CPython's int string conversion limit.
const maxIntChars = 4300

// Parse parses a complete JSON document the way pydantic's
// model_validate_json does (jiter with allow_inf_nan=true).
func Parse(data []byte) (Value, error) {
	p := parser{data: data}
	p.skipWhitespace()
	v, err := p.value(0)
	if err != nil {
		return Value{}, err
	}
	p.skipWhitespace()
	if p.pos != len(p.data) {
		return Value{}, ErrInvalid
	}
	return v, nil
}

type parser struct {
	data []byte
	pos  int
}

func (p *parser) skipWhitespace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) peek() (byte, bool) {
	if p.pos < len(p.data) {
		return p.data[p.pos], true
	}
	return 0, false
}

func (p *parser) literal(word string) bool {
	if len(p.data)-p.pos >= len(word) && string(p.data[p.pos:p.pos+len(word)]) == word {
		p.pos += len(word)
		return true
	}
	return false
}

func (p *parser) value(depth int) (Value, error) {
	c, ok := p.peek()
	if !ok {
		return Value{}, ErrInvalid
	}
	switch {
	case c == 'n':
		if p.literal("null") {
			return Value{Kind: Null}, nil
		}
	case c == 't':
		if p.literal("true") {
			return Value{Kind: Bool, Bool: true}, nil
		}
	case c == 'f':
		if p.literal("false") {
			return Value{Kind: Bool, Bool: false}, nil
		}
	case c == '"':
		s, err := p.string()
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: String, Str: s}, nil
	case c == '[':
		return p.array(depth)
	case c == '{':
		return p.object(depth)
	case c == 'N':
		if p.literal("NaN") {
			return Value{Kind: Float, Float: math.NaN()}, nil
		}
	case c == 'I':
		if p.literal("Infinity") {
			return Value{Kind: Float, Float: math.Inf(1)}, nil
		}
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return Value{}, ErrInvalid
}

func (p *parser) array(depth int) (Value, error) {
	p.pos++ // '['
	p.skipWhitespace()
	if c, ok := p.peek(); ok && c == ']' {
		p.pos++
		return Value{Kind: Array, Array: []Value{}}, nil
	}
	if depth+1 > maxDepth {
		return Value{}, ErrInvalid
	}
	items := make([]Value, 0, 8)
	for {
		p.skipWhitespace()
		item, err := p.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		items = append(items, item)
		p.skipWhitespace()
		c, ok := p.peek()
		if !ok {
			return Value{}, ErrInvalid
		}
		p.pos++
		switch c {
		case ',':
			continue
		case ']':
			return Value{Kind: Array, Array: items}, nil
		default:
			return Value{}, ErrInvalid
		}
	}
}

func (p *parser) object(depth int) (Value, error) {
	p.pos++ // '{'
	p.skipWhitespace()
	c, ok := p.peek()
	if ok && c == '}' {
		p.pos++
		return Value{Kind: Object, Members: []Member{}}, nil
	}
	if depth+1 > maxDepth {
		return Value{}, ErrInvalid
	}
	members := make([]Member, 0, 8)
	for {
		p.skipWhitespace()
		if c, ok := p.peek(); !ok || c != '"' {
			return Value{}, ErrInvalid
		}
		key, err := p.string()
		if err != nil {
			return Value{}, err
		}
		p.skipWhitespace()
		if c, ok := p.peek(); !ok || c != ':' {
			return Value{}, ErrInvalid
		}
		p.pos++
		p.skipWhitespace()
		item, err := p.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		members = append(members, Member{Key: key, Value: item})
		p.skipWhitespace()
		c, ok := p.peek()
		if !ok {
			return Value{}, ErrInvalid
		}
		p.pos++
		switch c {
		case ',':
			continue
		case '}':
			return Value{Kind: Object, Members: members}, nil
		default:
			return Value{}, ErrInvalid
		}
	}
}

func (p *parser) string() (string, error) {
	p.pos++ // opening quote
	start := p.pos
	// Fast path: no escapes.
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '"' {
			raw := p.data[start:p.pos]
			p.pos++
			if !utf8.Valid(raw) {
				return "", ErrInvalid
			}
			return string(raw), nil
		}
		if c == '\\' {
			break
		}
		if c < 0x20 {
			return "", ErrInvalid
		}
		p.pos++
	}
	buf := append([]byte(nil), p.data[start:p.pos]...)
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			if !utf8.Valid(buf) {
				return "", ErrInvalid
			}
			return string(buf), nil
		case c < 0x20:
			return "", ErrInvalid
		case c == '\\':
			p.pos++
			if p.pos >= len(p.data) {
				return "", ErrInvalid
			}
			e := p.data[p.pos]
			p.pos++
			switch e {
			case '"', '\\', '/':
				buf = append(buf, e)
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
				r, err := p.unicodeEscape()
				if err != nil {
					return "", err
				}
				buf = utf8.AppendRune(buf, r)
			default:
				return "", ErrInvalid
			}
		default:
			buf = append(buf, c)
			p.pos++
		}
	}
	return "", ErrInvalid
}

// unicodeEscape decodes the hex digits after "\u"; surrogates must form a
// valid pair, as in jiter.
func (p *parser) unicodeEscape() (rune, error) {
	hi, ok := p.hex4()
	if !ok {
		return 0, ErrInvalid
	}
	switch {
	case hi >= 0xDC00 && hi <= 0xDFFF:
		return 0, ErrInvalid
	case hi >= 0xD800 && hi <= 0xDBFF:
		if !p.literal(`\u`) {
			return 0, ErrInvalid
		}
		lo, ok := p.hex4()
		if !ok || lo < 0xDC00 || lo > 0xDFFF {
			return 0, ErrInvalid
		}
		return utf16.DecodeRune(rune(hi), rune(lo)), nil
	default:
		return rune(hi), nil
	}
}

func (p *parser) hex4() (uint16, bool) {
	if len(p.data)-p.pos < 4 {
		return 0, false
	}
	var n uint16
	for _, c := range p.data[p.pos : p.pos+4] {
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		n = n<<4 | uint16(d)
	}
	p.pos += 4
	return n, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// number parses a JSON number. Integers with up to 18 digits become Int,
// longer ones BigInt (jiter's chunking makes every 19+ digit literal a big
// integer even when it would fit in int64).
func (p *parser) number() (Value, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
		if p.literal("Infinity") {
			return Value{Kind: Float, Float: math.Inf(-1)}, nil
		}
	}
	digitsStart := p.pos
	c, ok := p.peek()
	switch {
	case !ok:
		return Value{}, ErrInvalid
	case c == '0':
		p.pos++
		if c, ok := p.peek(); ok && isDigit(c) {
			return Value{}, ErrInvalid
		}
	case c >= '1' && c <= '9':
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	default:
		return Value{}, ErrInvalid
	}
	intEnd := p.pos
	digits := intEnd - digitsStart
	if intEnd-start > maxIntChars {
		return Value{}, ErrInvalid
	}
	isFloat := false
	if c, ok := p.peek(); ok && c == '.' {
		isFloat = true
		p.pos++
		if !p.digits() {
			return Value{}, ErrInvalid
		}
	}
	if c, ok := p.peek(); ok && (c == 'e' || c == 'E') {
		isFloat = true
		p.pos++
		if c, ok := p.peek(); ok && (c == '+' || c == '-') {
			p.pos++
		}
		if !p.digits() {
			return Value{}, ErrInvalid
		}
	}
	text := string(p.data[start:p.pos])
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return Value{}, ErrInvalid
		}
		return Value{Kind: Float, Float: f}, nil
	}
	if digits <= 18 {
		n, _ := strconv.ParseInt(text, 10, 64)
		return Value{Kind: Int, Int: n}, nil
	}
	return Value{Kind: BigInt, Str: text}, nil
}

func (p *parser) digits() bool {
	start := p.pos
	for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
		p.pos++
	}
	return p.pos > start
}
