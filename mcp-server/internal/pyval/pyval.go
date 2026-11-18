// Package pyval models the small subset of Python object semantics the MCP
// server must reproduce byte-for-byte: values decoded from JSON tool
// arguments, Python repr() output used in pydantic validation messages, and
// the pydantic_core JSON encoding of query results.
package pyval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
)

// Value is one of: nil (None), bool, *big.Int (int), float64 (float),
// string (str), Bytes, List, Tuple, *Dict or Decimal.
type Value = any

// Bytes is a Python bytes object (clickhouse-connect returns FixedString
// columns as bytes).
type Bytes []byte

// List is a Python list.
type List []Value

// Tuple is a Python tuple; it encodes to JSON exactly like a list.
type Tuple []Value

// Decimal is a Python decimal.Decimal, already rendered with str().
type Decimal string

// Dict is an insertion-ordered Python dict.
type Dict struct {
	Keys   []Value
	Values []Value
}

// NewDict returns an empty dict with room for n items.
func NewDict(n int) *Dict {
	return &Dict{Keys: make([]Value, 0, n), Values: make([]Value, 0, n)}
}

// Set appends or replaces a key, keeping first-insertion order like Python.
func (d *Dict) Set(key string, value Value) {
	for i, existing := range d.Keys {
		if s, ok := existing.(string); ok && s == key {
			d.Values[i] = value
			return
		}
	}
	d.Keys = append(d.Keys, key)
	d.Values = append(d.Values, value)
}

// Get returns the value stored under a string key.
func (d *Dict) Get(key string) (Value, bool) {
	for i, existing := range d.Keys {
		if s, ok := existing.(string); ok && s == key {
			return d.Values[i], true
		}
	}
	return nil, false
}

// Len reports the number of items.
func (d *Dict) Len() int { return len(d.Keys) }

// Int returns a Python int.
func Int(v int64) *big.Int { return big.NewInt(v) }

// DecodeJSON parses JSON into Python values the way json.loads/pydantic do:
// objects keep key order (duplicate keys keep the first position and the
// last value), integers are arbitrary precision and other numbers are floats.
func DecodeJSON(data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected trailing JSON data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			d := NewDict(4)
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				d.Set(key, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return d, nil
		case '[':
			list := List{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", t)
	case json.Number:
		return parseNumber(string(t))
	default:
		return t, nil // nil, bool, string
	}
}

func parseNumber(literal string) (Value, error) {
	if !strings.ContainsAny(literal, ".eE") {
		n, ok := new(big.Int).SetString(literal, 10)
		if !ok {
			return nil, fmt.Errorf("invalid integer %q", literal)
		}
		return n, nil
	}
	return parseFloatLiteral(literal), nil
}

// TypeName mirrors type(value).__name__.
func TypeName(v Value) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case Bytes:
		return "bytes"
	case List:
		return "list"
	case Tuple:
		return "tuple"
	case *Dict:
		return "dict"
	case Decimal:
		return "Decimal"
	}
	return fmt.Sprintf("%T", v)
}
