// Package pyjson parses and encodes JSON with the exact semantics the Python
// collector had: parsing follows pydantic-core's jiter parser (NaN/Infinity
// accepted, big integers preserved, duplicate keys kept), and encoding follows
// Python's json.dumps(separators=(",", ":")) with ensure_ascii=True.
package pyjson

import "math"

// Kind identifies the JSON type of a Value.
type Kind uint8

const (
	Null Kind = iota
	Bool
	Int    // fits in int64
	BigInt // integer outside int64, kept as its decimal text
	Float
	String
	Array
	Object
)

// Value is a parsed JSON value.
type Value struct {
	Kind  Kind
	Bool  bool
	Int   int64
	Float float64
	// Str holds the decoded string for String and the digits for BigInt.
	Str   string
	Array []Value
	// Members keeps every key/value pair in document order, duplicates included.
	Members []Member
}

// Member is one key/value pair of a JSON object.
type Member struct {
	Key   string
	Value Value
}

// Get returns the value for key using pydantic's model field lookup rule:
// with duplicate keys the last occurrence wins.
func (v *Value) Get(key string) (*Value, bool) {
	for i := len(v.Members) - 1; i >= 0; i-- {
		if v.Members[i].Key == key {
			return &v.Members[i].Value, true
		}
	}
	return nil, false
}

// NullValue returns a JSON null.
func NullValue() Value { return Value{Kind: Null} }

// StringValue returns a JSON string.
func StringValue(s string) Value { return Value{Kind: String, Str: s} }

// OptString returns a JSON string, or null when s is nil.
func OptString(s *string) Value {
	if s == nil {
		return NullValue()
	}
	return StringValue(*s)
}

// Dict converts parsed JSON into what Python builds for a dict/Any value:
// duplicate keys keep the position of the first occurrence and the value of
// the last one, recursively. Non-finite floats become null, as pydantic's
// model_dump(mode="json") does with the default ser_json_inf_nan="null".
func (v Value) Dict() Value {
	switch v.Kind {
	case Float:
		if math.IsNaN(v.Float) || math.IsInf(v.Float, 0) {
			return NullValue()
		}
		return v
	case Array:
		out := make([]Value, len(v.Array))
		for i, item := range v.Array {
			out[i] = item.Dict()
		}
		return Value{Kind: Array, Array: out}
	case Object:
		obj := NewObject(len(v.Members))
		for _, m := range v.Members {
			obj.Set(m.Key, m.Value.Dict())
		}
		return obj.Value()
	default:
		return v
	}
}

// ObjectBuilder builds an ordered JSON object with Python dict semantics:
// setting an existing key replaces its value in place.
type ObjectBuilder struct {
	members []Member
	index   map[string]int
}

// NewObject returns an empty ObjectBuilder.
func NewObject(capacity int) *ObjectBuilder {
	return &ObjectBuilder{members: make([]Member, 0, capacity), index: make(map[string]int, capacity)}
}

// Set inserts or replaces key.
func (o *ObjectBuilder) Set(key string, value Value) {
	if i, ok := o.index[key]; ok {
		o.members[i].Value = value
		return
	}
	o.index[key] = len(o.members)
	o.members = append(o.members, Member{Key: key, Value: value})
}

// Get returns the current value of key.
func (o *ObjectBuilder) Get(key string) (Value, bool) {
	if i, ok := o.index[key]; ok {
		return o.members[i].Value, true
	}
	return Value{}, false
}

// Delete removes key if present (dict.pop(key, None)).
func (o *ObjectBuilder) Delete(key string) {
	i, ok := o.index[key]
	if !ok {
		return
	}
	o.members = append(o.members[:i], o.members[i+1:]...)
	delete(o.index, key)
	for k, j := range o.index {
		if j > i {
			o.index[k] = j - 1
		}
	}
}

// Value returns the built object.
func (o *ObjectBuilder) Value() Value {
	return Value{Kind: Object, Members: o.members}
}
