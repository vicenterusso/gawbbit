package schema

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"
)

// goldenRow is one input/output pair recorded from the Python service.
type goldenRow struct {
	In  string          `json:"in"`
	Out json.RawMessage `json:"out"`
	Err string          `json:"err"`
}

func readGolden(t *testing.T, path string) []goldenRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []goldenRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		var r goldenRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: no rows", path)
	}
	return rows
}

// TestDatetimeMatchesPydantic replays datetime inputs validated by pydantic.
func TestDatetimeMatchesPydantic(t *testing.T) {
	for _, row := range readGolden(t, "testdata/datetime_golden.jsonl") {
		v, err := pyjson.Parse([]byte(row.In))
		if err != nil {
			t.Fatalf("golden input %q does not parse", row.In)
		}
		dt, errType := laxDatetime(&v)
		if row.Err != "" {
			if errType != row.Err {
				t.Errorf("%s: got error %q (value %q), want %q", row.In, errType, dt.String(), row.Err)
			}
			continue
		}
		var want string
		_ = json.Unmarshal(row.Out, &want)
		if errType != "" || dt.String() != want {
			t.Errorf("%s: got %q/%q, want %q", row.In, dt.String(), errType, want)
		}
	}
}

// TestBoolMatchesPydantic replays nullable bool inputs.
func TestBoolMatchesPydantic(t *testing.T) {
	for _, row := range readGolden(t, "testdata/bool_golden.jsonl") {
		v, err := pyjson.Parse([]byte(row.In))
		if err != nil {
			t.Fatalf("golden input %q does not parse", row.In)
		}
		if v.Kind == pyjson.Null {
			if string(row.Out) != "null" {
				t.Errorf("%s: null accepted, want %s", row.In, row.Out)
			}
			continue
		}
		b, errType := laxBool(&v)
		if row.Err != "" {
			if errType != row.Err {
				t.Errorf("%s: got %q, want %q", row.In, errType, row.Err)
			}
			continue
		}
		var want bool
		_ = json.Unmarshal(row.Out, &want)
		if errType != "" || b != want {
			t.Errorf("%s: got %v/%q, want %v", row.In, b, errType, want)
		}
	}
}
