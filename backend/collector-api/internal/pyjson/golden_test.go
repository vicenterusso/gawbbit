package pyjson

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
)

func readLines(t *testing.T, path string, fn func([]byte)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	n := 0
	for sc.Scan() {
		fn(sc.Bytes())
		n++
	}
	if n == 0 {
		t.Fatalf("%s: no rows", path)
	}
}

// dictAny mirrors TypeAdapter(dict[str, Any]).validate_json + dump(mode="json")
// + json.dumps(separators=(",", ":")) in the original code.
func dictAny(data []byte) (string, string) {
	v, err := Parse(data)
	if err != nil {
		return "", "json_invalid"
	}
	if v.Kind != Object {
		return "", "dict_type"
	}
	return string(Encode(v.Dict())), ""
}

func TestJSONAnyMatchesPydantic(t *testing.T) {
	readLines(t, "testdata/json_any_golden.jsonl", func(line []byte) {
		var row struct{ In, Out, Err string }
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		out, errType := dictAny([]byte(row.In))
		if out != row.Out || errType != row.Err {
			t.Errorf("%q: got %q/%q, want %q/%q", row.In, out, errType, row.Out, row.Err)
		}
	})
}

func TestJSONBytesMatchPydantic(t *testing.T) {
	readLines(t, "testdata/json_bytes_golden.jsonl", func(line []byte) {
		var row struct{ Hex, Res string }
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		raw, _ := hex.DecodeString(row.Hex)
		_, errType := dictAny(raw)
		if errType == "" {
			errType = "ok"
		}
		if errType != row.Res {
			t.Errorf("%x: got %q, want %q", raw, errType, row.Res)
		}
	})
}

func TestFloatReprMatchesPython(t *testing.T) {
	readLines(t, "testdata/float_repr_golden.jsonl", func(line []byte) {
		var row struct {
			Bits uint64
			Repr string
		}
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		f := math.Float64frombits(row.Bits)
		if got := FloatRepr(f); got != row.Repr {
			t.Errorf("%v: got %q, want %q", f, got, row.Repr)
		}
	})
}
