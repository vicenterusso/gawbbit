package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeWarehouse answers metadata and test queries and records statements.
type fakeWarehouse struct {
	database   string
	existing   map[string]string
	statements []string
	failOn     string
	failures   map[string]string
}

var targetColumns = []string{"event_id", "app_id", "environment", "event_name", "event_time", "event_date",
	"received_time", "user_id", "user_pseudo_id", "session_id", "platform", "app_version", "os_version",
	"device_model", "locale", "timezone", "event_params_json", "user_properties_json", "traffic_source_json",
	"geo_json", "consent_json", "ingest_request_id", "ingest_user_agent", "ingest_ip_hash", "nats_stream", "nats_sequence"}

func (f *fakeWarehouse) record(sql string) error {
	f.statements = append(f.statements, sql)
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeWarehouse) Exec(_ context.Context, sql string) error { return f.record(sql) }

func (f *fakeWarehouse) Query(_ context.Context, sql string) ([]map[string]any, error) {
	if err := f.record(sql); err != nil {
		return nil, err
	}
	switch {
	case strings.HasPrefix(sql, "EXISTS DATABASE"):
		return []map[string]any{{"result": float64(1)}}, nil
	case sql == RelationsQuery(f.database):
		var rows []map[string]any
		for name, engine := range f.existing {
			rows = append(rows, map[string]any{"name": name, "engine": engine})
		}
		return rows, nil
	case strings.HasPrefix(sql, "select name, type from system.columns"):
		var rows []map[string]any
		for _, c := range targetColumns {
			rows = append(rows, map[string]any{"name": c, "type": "String"})
		}
		return rows, nil
	case strings.HasPrefix(sql, "select\n      count(*) as failures"):
		for column, n := range f.failures {
			if strings.Contains(sql, "select "+column+"\n") || (column == "unique" && strings.Contains(sql, "having")) {
				return []map[string]any{{"failures": n}}, nil
			}
		}
		return []map[string]any{{"failures": "0"}}, nil
	}
	return nil, nil
}

var (
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`--[^\n]*`)
	scratchName  = regexp.MustCompile(`events__dbt_new_data_[0-9a-f_]{36}`)
)

// normalize applies the same normalization used when capturing dbt's SQL.
func normalize(sql string) string {
	sql = lineComment.ReplaceAllString(blockComment.ReplaceAllString(sql, " "), " ")
	sql = strings.Join(strings.Fields(sql), " ")
	return scratchName.ReplaceAllString(sql, "events__dbt_new_data_<invocation>")
}

type sqlGolden struct {
	Database   string            `json:"database"`
	Existing   map[string]string `json:"existing"`
	Vars       map[string]any    `json:"vars"`
	Statements []string          `json:"statements"`
}

// TestStatementsMatchDBTClickHouse compares every write and data-test
// statement with the SQL dbt-core 1.9.10 + dbt-clickhouse 1.9.8 actually sent
// (captured from system.query_log, whitespace/comment normalized).
func TestStatementsMatchDBTClickHouse(t *testing.T) {
	paths, _ := filepath.Glob("../../testdata/sql/*.json")
	if len(paths) == 0 {
		t.Fatal("no SQL goldens")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var golden sqlGolden
			if err := json.Unmarshal(data, &golden); err != nil {
				t.Fatal(err)
			}
			if v, ok := golden.Vars[VarAppID]; ok && v == nil {
				delete(golden.Vars, VarAppID)
			}
			plan, err := Compile(golden.Vars, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			wh := &fakeWarehouse{database: golden.Database, existing: golden.Existing}
			if err := Build(context.Background(), wh, golden.Database, plan, "0b9e7c1e-1111-4222-8333-944455556666", nil); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, s := range wh.statements {
				if strings.HasPrefix(s, "EXISTS DATABASE") || s == RelationsQuery(golden.Database) {
					continue
				}
				got = append(got, normalize(strings.TrimSuffix(s, " FORMAT JSON")))
			}
			if len(got) != len(golden.Statements) {
				t.Fatalf("statement count %d != %d\n%s", len(got), len(golden.Statements), strings.Join(got, "\n"))
			}
			for i := range got {
				if got[i] != golden.Statements[i] {
					t.Errorf("statement %d differs:\n go: %s\ndbt: %s", i, got[i], golden.Statements[i])
				}
			}
		})
	}
}

type observed struct{ events []string }

func (o *observed) NodeStart(resource, node string) {
	o.events = append(o.events, "START "+resource+" "+node)
}
func (o *observed) NodeFinish(resource, node, status string, _ time.Duration) {
	o.events = append(o.events, status+" "+resource+" "+node)
}

func TestBuildFailFastAndLifecycle(t *testing.T) {
	plan, err := Compile(map[string]any{VarWindowStart: "2026-07-01T00:00:00Z", VarWindowEnd: "2026-07-01T01:00:00Z"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		wh     *fakeWarehouse
		phase  Phase
		fail   bool
		events string
	}{
		{"success", &fakeWarehouse{}, -1, false, "START model rawbbit_events_load|OK model rawbbit_events_load|" +
			"START test not_null_app_id|PASS test not_null_app_id|START test not_null_event_id|PASS test not_null_event_id|" +
			"START test not_null_event_time|PASS test not_null_event_time"},
		{"setup", &fakeWarehouse{failOn: "EXISTS DATABASE"}, PhaseSetup, false, ""},
		{"model", &fakeWarehouse{failOn: "insert into"}, PhaseModel, false,
			"START model rawbbit_events_load|ERROR model rawbbit_events_load"},
		{"not_null", &fakeWarehouse{failures: map[string]string{"event_id": "2"}}, PhaseTest, true,
			"START model rawbbit_events_load|OK model rawbbit_events_load|START test not_null_app_id|PASS test not_null_app_id|" +
				"START test not_null_event_id|FAIL test not_null_event_id"},
		{"unique_silent", &fakeWarehouse{failures: map[string]string{"unique": "1"}}, PhaseTest, true,
			"START model rawbbit_events_load|OK model rawbbit_events_load|START test not_null_app_id|PASS test not_null_app_id|" +
				"START test not_null_event_id|PASS test not_null_event_id|START test not_null_event_time|PASS test not_null_event_time"},
		{"test_error", &fakeWarehouse{failOn: "select app_id\n"}, PhaseTest, false,
			"START model rawbbit_events_load|OK model rawbbit_events_load|START test not_null_app_id|ERROR test not_null_app_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.wh.database = "db"
			obs := &observed{}
			err := Build(context.Background(), c.wh, "db", plan, "0b9e7c1e-1111-4222-8333-944455556666", obs)
			if got := strings.Join(obs.events, "|"); got != c.events {
				t.Errorf("events:\n%s\nwant:\n%s", got, c.events)
			}
			if c.phase < 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			be, ok := err.(*BuildError)
			if !ok || be.Phase != c.phase || be.Fail != c.fail {
				t.Fatalf("error %#v", err)
			}
		})
	}
}

func TestMissingDatabaseIsCreatedBeforeNodes(t *testing.T) {
	plan, _ := Compile(map[string]any{VarWindowStart: "2026-07-01T00:00:00Z", VarWindowEnd: "2026-07-01T01:00:00Z"}, time.Now())
	wh := &missingDB{fakeWarehouse{database: "fresh"}}
	if err := Build(context.Background(), wh, "fresh", plan, "0b9e7c1e-1111-4222-8333-944455556666", nil); err != nil {
		t.Fatal(err)
	}
	if wh.statements[1] != "CREATE DATABASE IF NOT EXISTS `fresh`" {
		t.Fatalf("statements: %v", wh.statements[:3])
	}
}

type missingDB struct{ fakeWarehouse }

func (m *missingDB) Query(ctx context.Context, sql string) ([]map[string]any, error) {
	if strings.HasPrefix(sql, "EXISTS DATABASE") {
		m.statements = append(m.statements, sql)
		return []map[string]any{{"result": float64(0)}}, nil
	}
	return m.fakeWarehouse.Query(ctx, sql)
}
