package pyval

import (
	"math"
	"math/big"
	"strings"
	"testing"
)

// Expected values were produced by CPython 3.13 / pydantic_core 2.41.
var floatCases = []struct {
	in         float64
	repr, json string
}{
	{0.0, "0.0", "0.0"}, {math.Copysign(0, -1), "-0.0", "-0.0"}, {1.0, "1.0", "1.0"}, {0.1, "0.1", "0.1"},
	{0.30000000000000004, "0.30000000000000004", "0.30000000000000004"},
	{1e15, "1000000000000000.0", "1000000000000000.0"}, {1e16, "1e+16", "1e16"}, {1.25e16, "1.25e+16", "1.25e16"},
	{1e17, "1e+17", "1e17"}, {1e20, "1e+20", "1e20"}, {1e21, "1e+21", "1e21"}, {1e-5, "1e-05", "0.00001"},
	{1e-7, "1e-07", "1e-7"}, {0.0001, "0.0001", "0.0001"}, {12345.678, "12345.678", "12345.678"},
	{2.5e300, "2.5e+300", "2.5e300"}, {5e-324, "5e-324", "5e-324"},
	{1.7976931348623157e308, "1.7976931348623157e+308", "1.7976931348623157e308"}, {-1.5, "-1.5", "-1.5"},
	{123456789.123, "123456789.123", "123456789.123"}, {1e100, "1e+100", "1e100"}, {100.0, "100.0", "100.0"},
	{9007199254740993.0, "9007199254740992.0", "9007199254740992.0"}, {0.000123, "0.000123", "0.000123"},
}

func TestFloatFormatting(t *testing.T) {
	for _, c := range floatCases {
		if got := FloatRepr(c.in); got != c.repr {
			t.Errorf("FloatRepr(%v) = %s, want %s", c.in, got, c.repr)
		}
		if got := RyuFormat(c.in); got != c.json {
			t.Errorf("RyuFormat(%v) = %s, want %s", c.in, got, c.json)
		}
	}
	for mode, want := range map[NonFinite]string{NonFiniteConstants: "[NaN,Infinity,-Infinity]", NonFiniteNull: "[null,null,null]"} {
		got, _ := EncodeJSON(List{math.NaN(), math.Inf(1), math.Inf(-1)}, mode)
		if string(got) != want {
			t.Errorf("mode %d: %s", mode, got)
		}
	}
}

func TestStrReprAndTruncation(t *testing.T) {
	for in, want := range map[string]string{
		"abc": "'abc'", "it's": `"it's"`, `"dq" and 'sq'`: `'"dq" and \'sq\''`, "tab\there\nnl\r": `'tab\there\nnl\r'`,
		"é\u00a0\u200b\x7f\x01\U0001F600": "'é\\xa0\\u200b\\x7f\\x01😀'", `\`: `'\\'`,
	} {
		if got := StrRepr(in); got != want {
			t.Errorf("StrRepr(%q) = %s, want %s", in, got, want)
		}
	}
	letters := strings.Repeat("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 2)
	for in, want := range map[string]string{
		letters[:48]:               "'" + letters[:48] + "'",
		letters[:49]:               "'abcdefghijklmnopqrstuvwx...ABCDEFGHIJKLMNOPQRSTUVW'",
		letters:                    "'abcdefghijklmnopqrstuvwx...NOPQRSTUVWXYZ0123456789'",
		strings.Repeat("é", 60):    "'éééééééééééé...ééééééééééé'",
		strings.Repeat("it's", 20): `"it'sit'sit'sit'sit'sit's...t'sit'sit'sit'sit'sit's"`,
	} {
		if got := TruncatedRepr(in); got != want {
			t.Errorf("TruncatedRepr(%q) = %s, want %s", in, got, want)
		}
	}
	d := NewDict(20)
	for i := 0; i < 20; i++ {
		d.Set("k"+big.NewInt(int64(i)).String(), Int(int64(i)))
	}
	if got := TruncatedRepr(d); got != "{'k0': 0, 'k1': 1, 'k2': ...7, 'k18': 18, 'k19': 19}" {
		t.Errorf("dict repr %s", got)
	}
	if got := Repr(List{nil, true, 1.0, "s", Tuple{Int(1)}}); got != "[None, True, 1.0, 's', (1,)]" {
		t.Errorf("list repr %s", got)
	}
}

func TestJSONStringEscaping(t *testing.T) {
	for in, want := range map[string]string{
		`a"b\c`:                      `"a\"b\\c"`,
		"\x00\x08\x0c\n\r\t\x1f\x7f": `"\u0000\b\f\n\r\t\u001f` + "\x7f\"",
		"é😀 <>&/":                    "\"é😀 <>&/\"",
	} {
		got, err := EncodeJSON(in, NonFiniteNull)
		if err != nil || string(got) != want {
			t.Errorf("EncodeJSON(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBytesUTF8Errors(t *testing.T) {
	for in, want := range map[string]string{
		"\xc3":         "Error serializing to JSON: incomplete utf-8 byte sequence from index 0",
		"\xff\x41":     "Error serializing to JSON: invalid utf-8 sequence of 1 bytes from index 0",
		"ok\xe2\x82":   "Error serializing to JSON: incomplete utf-8 byte sequence from index 2",
		"\xed\xa0\x80": "Error serializing to JSON: invalid utf-8 sequence of 1 bytes from index 0",
		"\xf0\x9f\x98": "Error serializing to JSON: incomplete utf-8 byte sequence from index 0",
	} {
		_, err := EncodeJSON(Bytes(in), NonFiniteNull)
		if err == nil || err.Error() != want {
			t.Errorf("bytes %q: %v, want %s", in, err, want)
		}
	}
	if got, err := EncodeJSON(Bytes("ab\x00"), NonFiniteNull); err != nil || string(got) != `"ab\u0000"` {
		t.Errorf("valid bytes: %s %v", got, err)
	}
}

func TestLaxCoercion(t *testing.T) {
	ints := map[string]string{"1_0": "10", "5.0": "5", "5.00": "5", " +3 ": "3", "-0": "0", "00012": "12", "-5.0": "-5",
		"9223372036854775808": "9223372036854775808"}
	for in, want := range ints {
		n, le := ValidateLaxInt(in)
		if le != nil || n.String() != want {
			t.Errorf("int %q: %v %v", in, n, le)
		}
	}
	for _, bad := range []string{"_1", "1__0", "1_", "5.", "5.5", "1e3", "0x10", "٣", "", "  ", "true"} {
		if _, le := ValidateLaxInt(bad); le == nil || le.Type != "int_parsing" {
			t.Errorf("int %q accepted: %v", bad, le)
		}
	}
	for in, want := range map[float64]string{1e19: "int_parsing_size", 9.3e18: "int_parsing_size", 2.5: "int_from_float", math.Inf(1): "finite_number"} {
		if _, le := ValidateLaxInt(in); le == nil || le.Type != want {
			t.Errorf("int %v: %v", in, le)
		}
	}
	if n, le := ValidateLaxInt(9.2e18); le != nil || n.String() != "9200000000000000000" {
		t.Errorf("9.2e18: %v %v", n, le)
	}
	for in, want := range map[string]bool{"TRUE": true, "On": true, "t": true, "Y": true, "no": false, "OFF": false, "0": false, "1": true, "falsE": false} {
		if b, le := ValidateLaxBool(in); le != nil || b != want {
			t.Errorf("bool %q: %v %v", in, b, le)
		}
	}
	for _, bad := range []Value{"2", " true", "true ", "", Int(2), Int(-1)} {
		if _, le := ValidateLaxBool(bad); le == nil || le.Type != "bool_parsing" {
			t.Errorf("bool %v: %v", bad, le)
		}
	}
	if _, le := ValidateLaxBool(0.5); le == nil || le.Type != "bool_type" {
		t.Errorf("bool 0.5: %v", le)
	}
}

func TestDecodeJSONKeepsOrderAndPrecision(t *testing.T) {
	v, err := DecodeJSON([]byte(`{"b":1,"a":99999999999999999999999,"b":2.0,"c":[true,null,"x"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Repr(v); got != "{'b': 2.0, 'a': 99999999999999999999999, 'c': [True, None, 'x']}" {
		t.Fatalf("repr %s", got)
	}
}
