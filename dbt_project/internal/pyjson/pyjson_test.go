package pyjson

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

type golden struct {
	Floats  [][2]json.RawMessage `json:"floats"`
	Strings [][2]string          `json:"strings"`
	Docs    [][3]string          `json:"docs"`
	Invalid []string             `json:"invalid"`
}

// TestMatchesCPythonJSON checks float repr, string escaping, decode/encode
// round trips and rejected documents against CPython outputs.
func TestMatchesCPythonJSON(t *testing.T) {
	data, err := os.ReadFile("../../testdata/unit/pyjson.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	for _, pair := range g.Floats {
		var bits uint64
		var want string
		_ = json.Unmarshal(pair[0], &bits)
		_ = json.Unmarshal(pair[1], &want)
		if got := FloatRepr(math.Float64frombits(bits)); got != want {
			t.Errorf("FloatRepr(%v) = %s, want %s", math.Float64frombits(bits), got, want)
		}
	}
	for _, pair := range g.Strings {
		if got := Encode(pair[0], EncodeOptions{}); got != pair[1] {
			t.Errorf("Encode(%q) = %s, want %s", pair[0], got, pair[1])
		}
	}
	for _, doc := range g.Docs {
		v, err := Decode(doc[0], DecodeOptions{})
		if err != nil {
			t.Fatalf("Decode(%s): %v", doc[0], err)
		}
		if got := Encode(v, EncodeOptions{}); got != doc[1] {
			t.Errorf("dumps(%s) = %s, want %s", doc[0], got, doc[1])
		}
		if got := Compact(v); got != doc[2] {
			t.Errorf("compact(%s) = %s, want %s", doc[0], got, doc[2])
		}
	}
	for _, doc := range g.Invalid {
		if _, err := Decode(doc, DecodeOptions{}); err == nil {
			t.Errorf("accepted invalid %q", doc)
		}
	}
	if _, err := Decode(`{"a":1,"a":2}`, DecodeOptions{RejectDuplicates: true}); err != ErrDuplicateKey {
		t.Errorf("duplicate key not rejected: %v", err)
	}
	if _, err := Decode("\ufeff{}", DecodeOptions{}); err == nil {
		t.Error("BOM accepted for str input")
	}
}

func TestPythonEqualityAndStr(t *testing.T) {
	one, _ := Decode("1", DecodeOptions{})
	oneFloat, _ := Decode("1.0", DecodeOptions{})
	if !Equal(one, oneFloat) || !Equal(true, one) || Equal("1", one) || Equal(nil, false) || Equal(math.NaN(), math.NaN()) {
		t.Error("Python equality semantics broken")
	}
	for v, want := range map[any]string{nil: "None", true: "True", int64(3): "3", 2.0: "2.0", "x": "x"} {
		if got := Str(v); got != want {
			t.Errorf("Str(%v) = %s", v, got)
		}
	}
}
