package settings

import (
	"encoding/json"
	"os"
	"testing"
)

// TestRedactedURLGolden replays URLs through the original _redacted_url (urlsplit).
func TestRedactedURLGolden(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/redacted_url_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases [][2]*string
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got, err := RedactedURL(*c[0])
		switch {
		case c[1] == nil && err == nil:
			t.Errorf("%q: got %q, want ValueError", *c[0], got)
		case c[1] != nil && (err != nil || got != *c[1]):
			t.Errorf("%q: got %q (%v), want %q", *c[0], got, err, *c[1])
		}
	}
}
