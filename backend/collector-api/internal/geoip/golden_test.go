package geoip

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// TestIPParsingMatchesPython replays ipaddress.ip_address() and the
// collector's _parse_ip_candidate() results.
func TestIPParsingMatchesPython(t *testing.T) {
	f, err := os.Open("testdata/ip_golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var row struct {
			In        string
			Valid     bool
			Candidate *string
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		if _, ok := ParseIP(row.In); ok != row.Valid {
			t.Errorf("ParseIP(%q) = %v, want %v", row.In, ok, row.Valid)
		}
		got, ok := parseIPCandidate(row.In)
		want := ""
		if row.Candidate != nil {
			want = *row.Candidate
		}
		if ok != (row.Candidate != nil) || got != want {
			t.Errorf("parseIPCandidate(%q) = %q/%v, want %v", row.In, got, ok, row.Candidate)
		}
	}
	if n == 0 {
		t.Fatal("no rows")
	}
}
