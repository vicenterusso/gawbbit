package pyval

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

// TestUnicodeTablesMatchCPython compares every code point against str.isprintable() and
// str.isspace() ranges dumped from CPython 3.13.
func TestUnicodeTablesMatchCPython(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/python_unicode_tables.json")
	if err != nil {
		t.Fatal(err)
	}
	var tables struct {
		Printable [][2]rune `json:"printable"`
		Space     [][2]rune `json:"space"`
	}
	if err := json.Unmarshal(raw, &tables); err != nil {
		t.Fatal(err)
	}
	check := func(name string, ranges [][2]rune, pred func(rune) bool) {
		want := make([]bool, 0x110000)
		for _, r := range ranges {
			for cp := r[0]; cp <= r[1]; cp++ {
				want[cp] = true
			}
		}
		bad := 0
		for cp := rune(0); cp < 0x110000; cp++ {
			if pred(cp) != want[cp] {
				bad++
				if bad <= 10 {
					t.Errorf("%s(U+%04X) = %v, want %v", name, cp, pred(cp), want[cp])
				}
			}
		}
	}
	check("IsPrintable", tables.Printable, IsPrintable)
	check("IsSpace", tables.Space, IsSpace)
}

func TestFloatRepr(t *testing.T) {
	tests := map[float64]string{
		0: "0.0", 1: "1.0", -1.5: "-1.5", 0.1: "0.1", 1e16: "1e+16", 1e15: "1000000000000000.0",
		1e-4: "0.0001", 1e-5: "1e-05", 123456789012345678: "1.2345678901234568e+17", 5e-324: "5e-324",
		1.7976931348623157e308: "1.7976931348623157e+308", 100: "100.0", 1.5e-7: "1.5e-07",
		math.Inf(1): "inf", math.Inf(-1): "-inf", math.Copysign(0, -1): "-0.0",
	}
	for f, want := range tests {
		if got := FloatRepr(f); got != want {
			t.Errorf("FloatRepr(%v) = %q, want %q", f, got, want)
		}
	}
	if got := FloatRepr(math.NaN()); got != "nan" {
		t.Errorf("FloatRepr(NaN) = %q", got)
	}
}

func TestLoadsAndRender(t *testing.T) {
	tests := []struct {
		in, str, dumps string
	}{
		{`{"a":1,"b":2,"a":3}`, `{'a': 3, 'b': 2}`, `{"a":3,"b":2}`},
		{`[1.0, 1e2, -0, -0.0, 1E400, NaN, -Infinity]`, `[1.0, 100.0, 0, -0.0, inf, nan, -inf]`, `[1.0,100.0,0,-0.0,Infinity,NaN,-Infinity]`},
		{`["it's", "say \"hi\"", "both ' \"", "é\n\u0000\u007f\u0085"]`,
			`["it's", 'say "hi"', 'both \' "', 'é\n\x00\x7f\x85']`,
			`["it's","say \"hi\"","both ' \"","\u00e9\n\u0000\u007f\u0085"]`},
		{`["😀", "\ud800", "\udc00\ud800"]`, `['😀', '\ud800', '\udc00\ud800']`, `["\ud83d\ude00","\ud800","\udc00\ud800"]`},
		{`{"x": [true, false, null, {}]}`, `{'x': [True, False, None, {}]}`, `{"x":[true,false,null,{}]}`},
		{`123456789012345678901234567890`, `123456789012345678901234567890`, `123456789012345678901234567890`},
		{` "plain" `, `plain`, `"plain"`},
	}
	for _, tt := range tests {
		v, err := Loads([]byte(tt.in))
		if err != nil {
			t.Fatalf("Loads(%s): %v", tt.in, err)
		}
		if got := Str(v); got != tt.str {
			t.Errorf("Str(%s) = %s, want %s", tt.in, got, tt.str)
		}
		if got := Dumps(v); got != tt.dumps {
			t.Errorf("Dumps(%s) = %s, want %s", tt.in, got, tt.dumps)
		}
	}
}

func TestLoadsRejects(t *testing.T) {
	for _, in := range []string{
		"", " ", "{} x", "{,}", "[1,]", "{'a':1}", `{"a":01}`, `{"a":1.}`, `{"a":.5}`, `{"a":-}`, `{"a":nan}`,
		`{"a":+1}`, `{"a":"\x41"}`, "{\"a\":\"\t\"}", `{"a":"\u12"}`, "\ufeff{}", "\xff", `{"a":"\xc3\x28"}`,
		"\xed\xa0\x80", "1" + strings.Repeat("0", 4300), strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1),
	} {
		if _, err := Loads([]byte(in)); err == nil {
			t.Errorf("Loads(%.40q) succeeded", in)
		}
	}
	if _, err := Loads([]byte(strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth))); err != nil {
		t.Errorf("nesting at MaxDepth rejected: %v", err)
	}
}

func TestStrip(t *testing.T) {
	if got := Strip("\x1c　 a b \t"); got != "a b" {
		t.Errorf("Strip = %q", got)
	}
	if got := Strip(" ​ "); got != "​" {
		t.Errorf("Strip kept = %q", got)
	}
}
