package transformer

import (
	"bufio"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/pyval"
)

// goldenCase is one message of testdata/transform_golden.jsonl.gz, recorded by running
// the original Python json.loads + event_to_row on a corpus of fixtures, contract-shaped
// events, boundary cases and randomized payloads.
type goldenCase struct {
	Data string `json:"data"`
	Meta *struct {
		Stream string `json:"stream"`
		Seq    uint64 `json:"seq"`
	} `json:"meta"`
	Outcome    string            `json:"outcome"`
	ErrorClass string            `json:"error_class"`
	Key        []string          `json:"key"`
	Row        []json.RawMessage `json:"row"`
}

var goldenNow = time.Date(2030, 5, 6, 7, 8, 9, 0, time.UTC)

func TestGoldenParity(t *testing.T) {
	fh, err := os.Open("../../testdata/transform_golden.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	count, failures := 0, 0
	for scanner.Scan() {
		count++
		var c goldenCase
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		data, _ := base64.StdEncoding.DecodeString(c.Data)
		msg := metaMsg{}
		if c.Meta != nil {
			msg.meta = &Metadata{Stream: c.Meta.Stream, StreamSequence: c.Meta.Seq}
		}
		diff := ""
		event, err := pyval.Loads(data)
		if err != nil {
			diff = expectOutcome(c, "decode_failed", "")
		} else if key, row, err := EventToRow(event, msg, func() time.Time { return goldenNow }); err != nil {
			diff = expectOutcome(c, "transform_failed", err.(*pyval.Error).Class)
		} else if diff = expectOutcome(c, "row", ""); diff == "" {
			diff = compareRow(c, key, row)
		}
		if diff != "" {
			failures++
			if failures <= 20 {
				t.Errorf("case %d (%q): %s", count, truncate(string(data)), diff)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count < 6000 {
		t.Fatalf("golden corpus too small: %d", count)
	}
	if failures > 0 {
		t.Fatalf("%d of %d golden cases diverge", failures, count)
	}
}

func expectOutcome(want goldenCase, outcome, errorClass string) string {
	if want.Outcome != outcome || want.ErrorClass != errorClass {
		return fmt.Sprintf("outcome %s/%s, want %s/%s", outcome, errorClass, want.Outcome, want.ErrorClass)
	}
	return ""
}

func compareRow(want goldenCase, key PartitionKey, row *Row) string {
	if got := []string{key.AppID, key.EventDate, key.Hour}; !slices.Equal(got, want.Key) {
		return fmt.Sprintf("key %q, want %q", got, want.Key)
	}
	for i, field := range row.StringFields() {
		var expected *string
		if err := json.Unmarshal(want.Row[i], &expected); err != nil {
			return err.Error()
		}
		if (expected == nil) != (*field == nil) || (expected != nil && *expected != **field) {
			return fmt.Sprintf("column %d: %s, want %s", i, truncate(show(*field)), truncate(show(expected)))
		}
	}
	if got := strconv.FormatInt(row.NATSSequence, 10); got != string(want.Row[24]) {
		return fmt.Sprintf("nats_sequence %s, want %s", got, want.Row[24])
	}
	return ""
}

func show(s *string) string {
	if s == nil {
		return "<null>"
	}
	return strconv.Quote(*s)
}

func truncate(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

type metaMsg struct{ meta *Metadata }

func (m metaMsg) Metadata() (*Metadata, error) { return m.meta, nil }
