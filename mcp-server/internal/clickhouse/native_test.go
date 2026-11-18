package clickhouse

import (
	"bytes"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
)

// native_types.bin is a ClickHouse 24.8 Native response (protocol 54405) for
// native_types.sql; native_types.json is what clickhouse-connect 0.10 +
// pydantic_core produced for the same query.
func TestDecodeNativeMatchesClickHouseConnect(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "native_types.bin"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "native_types.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := decodeNative(bytes.NewReader(body), timeContext{})
	if err != nil {
		t.Fatal(err)
	}
	list := make(pyval.List, len(rows))
	for i, r := range rows {
		list[i] = r
	}
	// FixedString is bytes in Python and serializes as UTF-8 text.
	got, err := pyval.EncodeJSON(list, pyval.NonFiniteConstants)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(want)) != string(got) {
		t.Fatalf("decoded rows differ:\nwant %s\n got %s", want, got)
	}
}

func TestDecimalString(t *testing.T) {
	// Expected values from str(decimal.Decimal(...)) as built by clickhouse-connect.
	cases := []struct {
		unscaled string
		scale    int
		want     string
	}{
		{"0", 3, "0.000"}, {"-150", 2, "-1.50"}, {"0", 10, "0E-10"},
		{"1234567890123456789012300", 5, "12345678901234567890.12300"}, {"100", 0, "100"}, {"100", 9, "1.00E-7"},
		{"-1", 6, "-0.000001"}, {"12345", 2, "123.45"}, {"-9999999999", 5, "-99999.99999"}, {"1", 5, "0.00001"},
		{"1", 6, "0.000001"}, {"99999999999999999999999999999999999999", 0, "99999999999999999999999999999999999999"}, {"5", 7, "5E-7"},
	}
	for _, c := range cases {
		n, _ := pyval.DecodeJSON([]byte(c.unscaled))
		if got := decimalString(n.(*big.Int), c.scale); got != c.want {
			t.Errorf("decimalString(%s, %d) = %s, want %s", c.unscaled, c.scale, got, c.want)
		}
	}
}

func TestParseTypeErrors(t *testing.T) {
	for name, want := range map[string]string{
		"Tuple()":                       "Unrecognized ClickHouse type base:  name: ",
		"Object('json')":                "Unrecognized ClickHouse type base: Object name: Object('json')",
		"AggregateFunction(sum, UInt8)": "AggregateFunction(sum, UInt8) deserialization not supported",
	} {
		if _, err := parseType(name); err == nil || err.Error() != want {
			t.Errorf("parseType(%s) = %v, want %s", name, err, want)
		}
	}
}

func TestRemoveSQLCommentsAndColumnsOnly(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT 1 LIMIT 0":                  true,
		"SELECT 1 limit 0  \n":              true,
		"SELECT 1 LIMIT 0 -- trailing note": true,
		"SELECT '-- x' LIMIT 0":             true,
		"SELECT 1 LIMIT 0 /* c */":          true,
		"SELECT 1 LIMIT 10":                 false,
		"SELECT 'LIMIT 0'":                  false,
	} {
		if got := columnsOnlyRe.MatchString(removeSQLComments(sql)); got != want {
			t.Errorf("%q: %v", sql, got)
		}
	}
}
