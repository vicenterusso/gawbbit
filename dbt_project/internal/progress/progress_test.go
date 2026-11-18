package progress

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const poison = "SECRET https://user:password@host/private?token=xxx SELECT * FROM private\nINJECT"

func TestNodeBodyStatusMappingAndCategories(t *testing.T) {
	cases := []struct {
		resource, node, status string
		elapsed                float64
		want                   string
	}{
		{"model", "rawbbit_events_load", "START", -1, "START model rawbbit_events_load"},
		{"model", "rawbbit_events_load", "OK", 2.4, "OK model rawbbit_events_load elapsed_seconds=2.400"},
		{"model", "rawbbit_events_load", "ERROR", 0.1234, "ERROR model rawbbit_events_load elapsed_seconds=0.123 category=node_error"},
		{"model", "rawbbit_events_load", "SKIP", -1, "SKIP model rawbbit_events_load"},
		{"model", "rawbbit_events_load", "PARTIAL", 1, "PARTIAL model rawbbit_events_load elapsed_seconds=1.000"},
		{"test", "not_null_event_id", "PASS", 0.0005, "PASS test not_null_event_id elapsed_seconds=0.001"},
		{"test", "not_null_app_id", "FAIL", 0.04, "FAIL test not_null_app_id elapsed_seconds=0.040 category=test_failed"},
		{"test", "not_null_event_time", "ERROR", 0, "ERROR test not_null_event_time elapsed_seconds=0.000 category=node_error"},
		{"test", "not_null_event_time", "WARN", 172801, "WARN test not_null_event_time"},
		{"test", "rawbbit_events_load_unique", "FAIL", 1, ""},
		{"model", "not_null_event_id", "OK", 1, ""},
		{"test", "not_null_event_id", "OK", 1, ""},
		{"seed", "rawbbit_events_load", "OK", 1, ""},
		{"model", poison, "OK", 1, ""},
	}
	for _, c := range cases {
		if got := NodeBody(c.resource, c.node, c.status, c.elapsed); got != c.want {
			t.Errorf("NodeBody(%s,%s,%s) = %q, want %q", c.resource, c.node, c.status, got, c.want)
		}
	}
}

func TestPrefixFixedParentOwnedIdentifiers(t *testing.T) {
	job, attempt := "a8ac4a04-54c9-4bee-b353-6fd4c98e86ea", "a7925a6e-0a05-4b01-8a8a-058bd8080c8a"
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	got, err := Prefix(now, job, attempt, "primary", "team_a")
	if err != nil || got != "2026-10-05T12:00:00Z [dbt-progress job="+job+" attempt="+attempt+" kind=primary db=team_a] " {
		t.Fatal(got, err)
	}
	for _, bad := range [][4]string{{poison, attempt, "primary", "team_a"}, {job, poison, "primary", "team_a"},
		{job, attempt, poison, "team_a"}, {job, attempt, "primary", poison},
		{strings.ToUpper(job), attempt, "default", "db"}, {job, attempt, "default", "1db"}} {
		if _, err := Prefix(now, bad[0], bad[1], bad[2], bad[3]); !errors.Is(err, ErrInvalidContext) {
			t.Errorf("accepted %v", bad)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestEmitterRejectsOversizedAndNonASCIILines(t *testing.T) {
	sink := &syncBuffer{}
	e := NewEmitter(sink)
	e.Emit(strings.Repeat("x", MaxLine))
	e.Emit("☃")
	e.Emit("fixed safe line")
	e.Close()
	if got := sink.String(); got != "fixed safe line\n" {
		t.Fatalf("%q", got)
	}
	e.Emit("after close")
	if got := sink.String(); got != "fixed safe line\n" {
		t.Fatalf("emitted after close: %q", got)
	}
}

type blockingSink struct{ release chan struct{} }

func (b blockingSink) Write(p []byte) (int, error) {
	<-b.release
	return len(p), nil
}

func TestBlockedSinkIsBoundedAndCloseDoesNotHang(t *testing.T) {
	sink := blockingSink{release: make(chan struct{})}
	defer close(sink.release)
	e := NewEmitter(sink)
	start := time.Now()
	for i := 0; i < 10000; i++ {
		e.Emit("fixed safe line")
	}
	if len(e.queue) > QueueLines {
		t.Fatal("queue exceeded bound")
	}
	e.Close()
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("blocked sink delayed shutdown by %v", elapsed)
	}
}

type failingSink struct{ writes int }

func (f *failingSink) Write([]byte) (int, error) {
	f.writes++
	return 0, errors.New("broken pipe")
}

func TestClosedSinkStopsWritingWithoutRetry(t *testing.T) {
	sink := &failingSink{}
	e := NewEmitter(sink)
	e.Emit("one")
	time.Sleep(20 * time.Millisecond)
	e.Emit("two")
	e.Close()
	if sink.writes != 1 {
		t.Fatalf("writes after sink failure: %d", sink.writes)
	}
}
