package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGeneratorEvents(t *testing.T) {
	g := NewGenerator("abc123", "com.example.game", "loadgen", 5, 42)
	now := time.Date(2026, 11, 18, 19, 5, 6, 789_000_000, time.UTC)
	seen := map[string]bool{}
	names := map[string]int{}
	for i := 0; i < 2000; i++ {
		ev := g.Next(now)
		if !uuidRe.MatchString(ev.EventID) {
			t.Fatalf("event_id %q is not a v4 UUID", ev.EventID)
		}
		if seen[ev.EventID] {
			t.Fatalf("duplicate event_id %s", ev.EventID)
		}
		seen[ev.EventID] = true
		names[ev.EventName]++
		if ev.EventParams[RunParam] != "abc123" || ev.EventParams["loadgen_seq"] != i+1 {
			t.Fatalf("bad params %v", ev.EventParams)
		}
		if ev.EventTimestamp != "2026-11-18T19:05:06.789Z" {
			t.Fatalf("timestamp %q", ev.EventTimestamp)
		}
		if !strings.HasPrefix(ev.User["user_pseudo_id"].(string), "lg-abc123-p") {
			t.Fatalf("user %v", ev.User)
		}
	}
	if len(names) != len(eventMix) {
		t.Fatalf("expected every event name, got %v", names)
	}
}

func TestHourWindow(t *testing.T) {
	a := time.Date(2026, 11, 18, 19, 59, 0, 0, time.UTC)
	b := time.Date(2026, 11, 18, 20, 0, 1, 0, time.UTC)
	s, e := HourWindow(a, b)
	if !s.Equal(time.Date(2026, 11, 18, 19, 0, 0, 0, time.UTC)) || !e.Equal(time.Date(2026, 11, 18, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("window %s %s", s, e)
	}
}

func TestVerifySQL(t *testing.T) {
	m := Manifest{RunID: "abc123", AppID: "com.example.game",
		WindowStart: time.Date(2026, 11, 18, 19, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)}
	sql, err := VerifySQL("analytics.events", m)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FROM analytics.events FINAL", "uniqExact(app_id, event_id)", "toDate('2026-11-17') AND toDate('2026-11-19')", "'loadgen_run') = 'abc123'"} {
		if !strings.Contains(sql, want) {
			t.Errorf("sql lacks %q:\n%s", want, sql)
		}
	}
	m.AppID = "x' OR 1=1 --"
	if _, err := VerifySQL("analytics.events", m); err == nil {
		t.Fatal("expected an unsafe identifier error")
	}
}

func TestParseCountsAndDiff(t *testing.T) {
	rows := []map[string]any{
		{"event_name": "a", "rows": json.Number("3"), "uniq": json.Number("3")},
		{"event_name": "b", "rows": "5", "uniq": "4"},
		{"event_name": "c", "rows": float64(1), "uniq": float64(1)},
	}
	got, err := ParseCounts(rows)
	if err != nil {
		t.Fatal(err)
	}
	problems := Diff(map[string]int{"a": 3, "b": 4, "d": 2}, got)
	want := []string{
		"b: 1 duplicate rows",
		"c: 1 unexpected events (rejected or unaccounted)",
		"d: 2 of 2 events missing",
	}
	if strings.Join(problems, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", problems)
	}
	if p := Diff(map[string]int{"a": 3}, map[string]Count{"a": {3, 3}}); len(p) != 0 {
		t.Fatalf("unexpected problems %v", p)
	}
	if _, err := ParseCounts([]map[string]any{{"event_name": "a"}}); err == nil {
		t.Fatal("expected an error for a row without counts")
	}
}

// fakeCollector mimics Caddy + Vector: 401 for a missing or unknown API key,
// 413 above maxBytes, 400 for invalid JSON, 200 with no body otherwise.
// Invalid events and events for another app_id are dropped silently; stored
// events are keyed by event_id, like the ReplacingMergeTree table.
type fakeCollector struct {
	mu       sync.Mutex
	key, app string
	stored   map[string]Event
	requests atomic.Int64
	fail503  atomic.Int64 // answer the next N requests with 503
	maxBytes int
}

func (f *fakeCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		fmt.Fprint(w, `{"status":"ok"}`)
		return
	}
	f.requests.Add(1)
	if f.fail503.Load() > 0 {
		f.fail503.Add(-1)
		http.Error(w, `{"detail":"Failed to publish events"}`, 503)
		return
	}
	switch r.Header.Get("X-API-Key") {
	case "":
		http.Error(w, "", 401)
		return
	case f.key:
	default:
		http.Error(w, "", 401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	if len(body) > f.maxBytes {
		http.Error(w, "", 413)
		return
	}
	var raw map[string][]map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		http.Error(w, "", 400)
		return
	}
	var b Batch
	_ = json.Unmarshal(body, &b)
	f.mu.Lock()
	for i, ev := range b.Events {
		_, hasName := raw["events"][i]["event_name"]
		_, tsErr := time.Parse(time.RFC3339, ev.EventTimestamp)
		if hasName && tsErr == nil && ev.AppID == f.app {
			f.stored[ev.EventID] = ev
		}
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func newFake() *fakeCollector {
	return &fakeCollector{key: "k", app: "com.example.game", stored: map[string]Event{}, maxBytes: 1 << 20}
}

func TestSendDeliversWithRetriesAndDuplicates(t *testing.T) {
	f := newFake()
	f.fail503.Store(3)
	srv := httptest.NewServer(f)
	defer srv.Close()
	res, err := Send(context.Background(), SendConfig{
		CollectorURL: srv.URL, APIKey: "k", AppID: "com.example.game", Environment: "loadgen", RunID: "r1",
		Events: 1003, BatchSize: 50, Concurrency: 4, Players: 10, Duplicates: 0.5, Seed: 7,
		MaxRetries: 5, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Manifest
	if m.Total != 1003 || len(f.stored) != 1003 || res.Failed != 0 {
		t.Fatalf("total=%d stored=%d failed=%d", m.Total, len(f.stored), res.Failed)
	}
	sum := 0
	for _, n := range m.Expected {
		sum += n
	}
	if sum != 1003 {
		t.Fatalf("expected sums to %d", sum)
	}
	if res.Retries < 3 || res.DuplicateSends == 0 {
		t.Fatalf("retries=%d duplicates=%d", res.Retries, res.DuplicateSends)
	}
	if int(f.requests.Load()) != res.Requests {
		t.Fatalf("server saw %d requests, result says %d", f.requests.Load(), res.Requests)
	}
	if m.WindowStart.IsZero() || !m.WindowEnd.After(m.WindowStart) {
		t.Fatalf("window %v %v", m.WindowStart, m.WindowEnd)
	}
}

func TestSendReportsUnacceptedEvents(t *testing.T) {
	f := newFake()
	f.fail503.Store(1 << 30)
	srv := httptest.NewServer(f)
	defer srv.Close()
	res, err := Send(context.Background(), SendConfig{
		CollectorURL: srv.URL, APIKey: "k", AppID: "com.example.game", RunID: "r2",
		Events: 20, BatchSize: 10, Concurrency: 1, MaxRetries: 1, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 20 || res.Manifest.Total != 0 {
		t.Fatalf("failed=%d total=%d", res.Failed, res.Manifest.Total)
	}
}

func TestSendRateLimit(t *testing.T) {
	srv := httptest.NewServer(newFake())
	defer srv.Close()
	start := time.Now()
	_, err := Send(context.Background(), SendConfig{
		CollectorURL: srv.URL, APIKey: "k", AppID: "com.example.game", RunID: "r3",
		Events: 100, BatchSize: 10, Rate: 200, Concurrency: 2, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 10 batches at 20 batches/s: the 10th leaves after ~450 ms.
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("rate limit ignored: %s", d)
	}
}

func TestRunNegative(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := NewClient(srv.URL, "", "k", time.Second, false)
	cases := RunNegative(context.Background(), c, "k", "r4", "com.example.game", "loadgen", 1<<20)
	if len(cases) != 7 {
		t.Fatalf("got %d cases", len(cases))
	}
	for _, nc := range cases {
		if !nc.OK() {
			t.Errorf("%s: want %d got %d %s", nc.Name, nc.WantStatus, nc.Got, nc.Err)
		}
	}
	if len(f.stored) != 0 {
		t.Fatalf("rejected or dropped requests stored %d events", len(f.stored))
	}
}

func TestClickHouseQuerier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		body, _ := io.ReadAll(r.Body)
		if u != "reader" || p != "pw" || !strings.HasSuffix(string(body), "FORMAT JSON") {
			http.Error(w, "Code: 516. Authentication failed", 516)
			return
		}
		fmt.Fprint(w, `{"meta":[],"data":[{"event_name":"a","rows":2,"uniq":2}],"rows":1}`)
	}))
	defer srv.Close()
	rows, err := NewClickHouse(srv.URL, "reader", "pw", "", false).Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCounts(rows)
	if err != nil || got["a"] != (Count{2, 2}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := NewClickHouse(srv.URL, "reader", "bad", "", false).Query(context.Background(), "SELECT 1"); err == nil ||
		!strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestMCPQuerierSSE(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "", 401)
			return
		}
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		method := msg["method"].(string)
		calls = append(calls, method)
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%v,\"result\":{}}\n\n", msg["id"])
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "s1" {
				http.Error(w, `{"error":"Missing session ID"}`, 400)
				return
			}
			args := msg["params"].(map[string]any)["arguments"].(map[string]any)
			if !strings.HasPrefix(args["sql"].(string), "SELECT") {
				t.Errorf("sql %v", args["sql"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%v,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"[]\"}],"+
				"\"structuredContent\":{\"result\":[{\"event_name\":\"a\",\"rows\":4,\"uniq\":4}]},\"isError\":false}}\n\n", msg["id"])
		}
	}))
	defer srv.Close()
	q := NewMCP(MCPURL(srv.URL, "/mcp"), "tok", "", false)
	if q.URL != srv.URL+"/mcp" {
		t.Fatalf("url %s", q.URL)
	}
	q.URL = srv.URL
	for i := 0; i < 2; i++ {
		rows, err := q.Query(context.Background(), "SELECT 1")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := ParseCounts(rows)
		if got["a"] != (Count{4, 4}) {
			t.Fatalf("got %v", got)
		}
	}
	if strings.Join(calls, ",") != "initialize,notifications/initialized,tools/call,tools/call" {
		t.Fatalf("calls %v", calls)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	p := t.TempDir() + "/m.json"
	in := Manifest{RunID: "r", AppID: "a", Expected: map[string]int{"x": 1}, Total: 1,
		WindowStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)}
	if err := WriteManifest(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadManifest(p)
	if err != nil || out.Total != 1 || !out.WindowEnd.Equal(in.WindowEnd) {
		t.Fatalf("%v %v", out, err)
	}
}
