package partitioning

import (
	"bufio"
	"compress/gzip"
	"os"
	"testing"
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/pyval"
)

func fixedNow() time.Time { return time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC) }

func TestEventPartitionUsesEventTimestamp(t *testing.T) {
	date, hour, err := EventPartition("2026-03-16T18:42:15.123Z", "2026-03-16T20:00:00Z", fixedNow)
	if err != nil || date != "2026-03-16" || hour != "18" {
		t.Fatalf("got %q %q %v", date, hour, err)
	}
}

func TestEventPartitionFallsBack(t *testing.T) {
	date, hour, err := EventPartition("bad-date", "2026-03-16T20:00:00Z", fixedNow)
	if err != nil || date != "2026-03-16" || hour != "20" {
		t.Fatalf("got %q %q %v", date, hour, err)
	}
}

func TestEventPartitionFallsBackToNow(t *testing.T) {
	date, hour, err := EventPartition(nil, "", fixedNow)
	if err != nil || date != "2030-01-02" || hour != "03" {
		t.Fatalf("got %q %q %v", date, hour, err)
	}
}

func TestObjectPartitionPath(t *testing.T) {
	tests := []struct{ prefix, want string }{
		{"raw/", "raw/app_id=mygame/event_date=2026-03-16/hour=20/part-1.parquet"},
		{"", "app_id=mygame/event_date=2026-03-16/hour=20/part-1.parquet"},
		{"//a/b//", "a/b/app_id=mygame/event_date=2026-03-16/hour=20/part-1.parquet"},
	}
	for _, tt := range tests {
		if got := ObjectPartitionPath(tt.prefix, "mygame", "2026-03-16", "20", "part-1.parquet"); got != tt.want {
			t.Errorf("prefix %q: got %q, want %q", tt.prefix, got, tt.want)
		}
	}
}

// TestISOFormatGolden replays 50k timestamp strings (valid shapes plus random mutations
// with separators, NULs, surrogates and non-ASCII) recorded from the original Python
// parse_iso_datetime on CPython 3.13.
func TestISOFormatGolden(t *testing.T) {
	fh, err := os.Open("../../testdata/isoformat_golden.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(gz)
	count, failures := 0, 0
	for scanner.Scan() {
		count++
		// pyval keeps lone surrogates that encoding/json would replace.
		v, err := pyval.Loads(scanner.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		s, _ := v.(*pyval.Object).Get("s")
		want, _ := v.(*pyval.Object).Get("r")
		got := "none"
		parsed, ok, err := ParseISODatetime(s)
		switch {
		case err != nil:
			got = err.(*pyval.Error).Class
		case ok:
			got = FormatDate(parsed) + parsed.Format("T15:04:05.000000")
		}
		if got != want {
			failures++
			if failures <= 20 {
				t.Errorf("%q: got %s, want %s", s, got, want)
			}
		}
	}
	if count < 50000 || failures > 0 {
		t.Fatalf("%d of %d cases diverge", failures, count)
	}
}
