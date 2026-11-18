package clickhouse

import (
	"fmt"
	"strconv"
	"strings"
)

// chType is a parsed ClickHouse type name such as
// "Map(String, Array(Nullable(DateTime64(3, 'UTC'))))".
type chType struct {
	full  string
	base  string
	args  []string  // raw top-level arguments
	inner []*chType // parsed nested types (container types only)
	names []string  // element names of named tuples

	// derived attributes
	scale    int              // DateTime64 / Decimal scale
	size     int              // FixedString length, Decimal byte size
	tz       string           // explicit DateTime / DateTime64 timezone
	hasTZ    bool             //
	enumVals map[int64]string // Enum8 / Enum16
}

type unrecognizedTypeError struct{ base, name string }

// unsupportedTypeError mirrors clickhouse-connect's UnsupportedType.
type unsupportedTypeError struct{ name string }

func (e *unsupportedTypeError) Error() string { return e.name + " deserialization not supported" }

func (e *unrecognizedTypeError) Error() string {
	return fmt.Sprintf("Unrecognized ClickHouse type base: %s name: %s", e.base, e.name)
}

// splitTopLevel splits on commas outside parentheses and quotes.
func splitTopLevel(s string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '`', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if tail := strings.TrimSpace(s[start:]); tail != "" {
		parts = append(parts, tail)
	}
	return parts
}

var aliases = map[string]string{
	"Point":           "Tuple(Float64, Float64)",
	"Ring":            "Array(Point)",
	"LineString":      "Array(Point)",
	"Polygon":         "Array(Ring)",
	"MultiLineString": "Array(LineString)",
	"MultiPolygon":    "Array(Polygon)",
	"Boolean":         "Bool",
}

func parseType(name string) (*chType, error) {
	name = strings.TrimSpace(name)
	if alias, ok := aliases[name]; ok {
		t, err := parseType(alias)
		if err != nil {
			return nil, err
		}
		t.full = name
		return t, nil
	}
	t := &chType{full: name, base: name}
	if open := strings.IndexByte(name, '('); open >= 0 && strings.HasSuffix(name, ")") {
		t.base = name[:open]
		t.args = splitTopLevel(name[open+1 : len(name)-1])
	}
	switch t.base {
	case "Nullable", "LowCardinality", "Array":
		if len(t.args) != 1 {
			return nil, &unrecognizedTypeError{t.base, name}
		}
		inner, err := parseType(t.args[0])
		if err != nil {
			return nil, err
		}
		t.inner = []*chType{inner}
	case "Map":
		if len(t.args) != 2 {
			return nil, &unrecognizedTypeError{t.base, name}
		}
		for _, a := range t.args {
			inner, err := parseType(a)
			if err != nil {
				return nil, err
			}
			t.inner = append(t.inner, inner)
		}
	case "Tuple", "Nested":
		if len(t.args) == 0 {
			// clickhouse-connect cannot parse the empty tuple type name.
			return nil, &unrecognizedTypeError{"", ""}
		}
		for _, a := range t.args {
			elemName, elemType := splitNamedElement(a)
			inner, err := parseType(elemType)
			if err != nil {
				return nil, err
			}
			t.names = append(t.names, elemName)
			t.inner = append(t.inner, inner)
		}
	case "AggregateFunction":
		return nil, &unsupportedTypeError{name}
	case "SimpleAggregateFunction":
		if len(t.args) < 2 {
			return nil, &unrecognizedTypeError{t.base, name}
		}
		return parseType(t.args[len(t.args)-1])
	case "FixedString":
		n, err := strconv.Atoi(firstArg(t))
		if err != nil {
			return nil, &unrecognizedTypeError{t.base, name}
		}
		t.size = n
	case "DateTime":
		if len(t.args) > 0 {
			t.tz, t.hasTZ = unquote(t.args[0]), true
		}
	case "DateTime64":
		n, err := strconv.Atoi(firstArg(t))
		if err != nil {
			return nil, &unrecognizedTypeError{t.base, name}
		}
		t.scale = n
		if len(t.args) > 1 {
			t.tz, t.hasTZ = unquote(t.args[1]), true
		}
	case "Decimal", "Decimal32", "Decimal64", "Decimal128", "Decimal256":
		if err := t.parseDecimal(); err != nil {
			return nil, err
		}
	case "Enum8", "Enum16":
		t.enumVals = map[int64]string{}
		for _, a := range t.args {
			eq := strings.LastIndexByte(a, '=')
			if eq < 0 {
				return nil, &unrecognizedTypeError{t.base, name}
			}
			v, err := strconv.ParseInt(strings.TrimSpace(a[eq+1:]), 10, 64)
			if err != nil {
				return nil, &unrecognizedTypeError{t.base, name}
			}
			t.enumVals[v] = unquote(strings.TrimSpace(a[:eq]))
		}
	case "Int8", "Int16", "Int32", "Int64", "Int128", "Int256",
		"UInt8", "UInt16", "UInt32", "UInt64", "UInt128", "UInt256",
		"Float32", "Float64", "Bool", "String", "Date", "Date32", "UUID", "IPv4", "IPv6", "Nothing",
		"IntervalNanosecond", "IntervalMicrosecond", "IntervalMillisecond", "IntervalSecond",
		"IntervalMinute", "IntervalHour", "IntervalDay", "IntervalWeek", "IntervalMonth",
		"IntervalQuarter", "IntervalYear":
	default:
		return nil, &unrecognizedTypeError{t.base, name}
	}
	return t, nil
}

func (t *chType) parseDecimal() error {
	var prec int
	var err error
	switch t.base {
	case "Decimal":
		if len(t.args) != 2 {
			return &unrecognizedTypeError{t.base, t.full}
		}
		if prec, err = strconv.Atoi(t.args[0]); err == nil {
			t.scale, err = strconv.Atoi(t.args[1])
		}
	default:
		prec = map[string]int{"Decimal32": 9, "Decimal64": 18, "Decimal128": 38, "Decimal256": 76}[t.base]
		t.scale, err = strconv.Atoi(firstArg(t))
	}
	if err != nil {
		return &unrecognizedTypeError{t.base, t.full}
	}
	switch {
	case prec <= 9:
		t.size = 4
	case prec <= 18:
		t.size = 8
	case prec <= 38:
		t.size = 16
	default:
		t.size = 32
	}
	return nil
}

func firstArg(t *chType) string {
	if len(t.args) == 0 {
		return ""
	}
	return t.args[0]
}

// splitNamedElement separates "name Type" tuple elements.
func splitNamedElement(a string) (string, string) {
	sp := strings.IndexByte(a, ' ')
	paren := strings.IndexByte(a, '(')
	if sp > 0 && (paren < 0 || sp < paren) {
		candidate := strings.Trim(a[:sp], "`")
		if _, err := parseType(strings.TrimSpace(a[sp+1:])); err == nil {
			return candidate, strings.TrimSpace(a[sp+1:])
		}
	}
	return "", a
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `\'`, `'`)
		s = strings.ReplaceAll(s, `\\`, `\`)
	}
	return s
}
