package model

import (
	"strings"
	"testing"
	"time"
)

func compileErrMsg(t *testing.T, vars map[string]any) string {
	t.Helper()
	_, err := Compile(vars, time.Date(2026, 1, 1, 5, 30, 0, 0, time.UTC))
	ce, ok := err.(*CompileError)
	if !ok {
		t.Fatalf("vars %v: expected compile error, got %v", vars, err)
	}
	return ce.Msg
}

func window(extra map[string]any) map[string]any {
	vars := map[string]any{VarWindowStart: "2026-01-01T00:00:00Z", VarWindowEnd: "2026-01-01T02:00:00Z"}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// Ported from tests/test_model_routing.py.
func TestDefaultExactIncludeFallbackAndExcludeModes(t *testing.T) {
	plan, err := Compile(window(map[string]any{VarAppID: "runner_rawbbit", VarExcludedAppIDs: []any{}}), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app_id=runner_rawbbit/event_date=2026-01-01/hour=00/*.parquet",
		"app_id=runner_rawbbit/event_date=2026-01-01/hour=01/*.parquet"}
	if strings.Join(plan.HourPaths, ",") != strings.Join(want, ",") || plan.Predicate != "and app_id = 'runner_rawbbit'" {
		t.Errorf("include: %v %q", plan.HourPaths, plan.Predicate)
	}
	plan, _ = Compile(window(map[string]any{VarExcludedAppIDs: []any{"runner_rawbbit", "other"}}), time.Now())
	if plan.HourPaths[0] != "app_id=*/event_date=2026-01-01/hour=00/*.parquet" ||
		plan.Predicate != "and app_id not in ('runner_rawbbit', 'other')" {
		t.Errorf("exclude: %v %q", plan.HourPaths, plan.Predicate)
	}
	plan, _ = Compile(window(nil), time.Now())
	if plan.Predicate != "" || strings.Contains(plan.SQL, "and app_id") {
		t.Errorf("default predicate %q", plan.Predicate)
	}
	for _, p := range plan.HourPaths {
		if !strings.Contains(plan.SQL, "filename = '"+p+"'") {
			t.Errorf("path %s not rendered", p)
		}
	}
	if strings.Count(plan.SQL, "union all") != 1 {
		t.Error("hour selects must be unioned")
	}
}

func TestRejectUnsafePathsAndMixedModes(t *testing.T) {
	for _, app := range []any{"../x", "a/b", "a'", "a*", "a?", "a{b}", "a\n", "", int64(3), "..", ".", "é", strings.Repeat("a", 129)} {
		if msg := compileErrMsg(t, window(map[string]any{VarAppID: app})); msg != "unsafe app partition identifier" {
			t.Errorf("%q: %s", app, msg)
		}
	}
	if msg := compileErrMsg(t, window(map[string]any{VarExcludedAppIDs: []any{"ok", "../x"}})); msg != "unsafe app partition identifier" {
		t.Error(msg)
	}
	if msg := compileErrMsg(t, window(map[string]any{VarAppID: "a", VarExcludedAppIDs: []any{"b"}})); msg != "include and exclude app modes are mutually exclusive" {
		t.Error(msg)
	}
	for _, bad := range []any{"a", map[string]any{"a": 1}, nil, int64(1)} {
		if msg := compileErrMsg(t, window(map[string]any{VarExcludedAppIDs: bad})); msg != "rawbbit_excluded_app_ids must be a list" {
			t.Errorf("%v: %s", bad, msg)
		}
	}
}

func TestWindowValidationMessages(t *testing.T) {
	cases := []struct {
		vars map[string]any
		want string
	}{
		{map[string]any{VarWindowStart: "2026-01-01T00:30:00Z"}, "rawbbit_window_start must be an hour-aligned UTC timestamp: 2026-01-01T00:30:00Z"},
		{map[string]any{VarWindowEnd: "tomorrow"}, "rawbbit_window_end must be an hour-aligned UTC timestamp: tomorrow"},
		{map[string]any{VarWindowStart: int64(5)}, "expected string or bytes-like object, got 'int'"},
		{map[string]any{VarWindowEnd: nil}, "expected string or bytes-like object, got 'NoneType'"},
		{map[string]any{VarWindowStart: "2026-13-01T00:00:00Z"}, "time data '2026-13-01T00:00:00Z' does not match format '%Y-%m-%dT%H:%M:%SZ'"},
		{map[string]any{VarWindowStart: "2026-01-01T24:00:00Z"}, "time data '2026-01-01T24:00:00Z' does not match format '%Y-%m-%dT%H:%M:%SZ'"},
		{map[string]any{VarWindowStart: "0000-01-01T00:00:00Z"}, "year 0 is out of range"},
		{map[string]any{VarWindowEnd: "2026-01-01T00:00:00Z"}, "rawbbit_window_end must be after rawbbit_window_start"},
		{map[string]any{VarWindowEnd: "2025-12-31T00:00:00Z"}, "rawbbit_window_end must be after rawbbit_window_start"},
		{map[string]any{VarWindowEnd: "2026-01-08T01:00:00Z"}, "Rawbbit dbt window exceeds 168 hours; use the chunked backfill wrapper"},
		{map[string]any{VarWindowEnd: "2026-01-01T03:00:00Z", VarMaxWindowHours: "2"}, "Rawbbit dbt window exceeds 2 hours; use the chunked backfill wrapper"},
		{map[string]any{VarWindowStart: "2026-02-30T00:00:00Z"}, "day is out of range for month"},
	}
	for _, c := range cases {
		if got := compileErrMsg(t, window(c.vars)); got != c.want {
			t.Errorf("%v: %s, want %s", c.vars, got, c.want)
		}
	}
	if _, err := Compile(window(map[string]any{VarWindowEnd: "2026-01-08T00:00:00Z"}), time.Now()); err != nil {
		t.Errorf("168h window rejected: %v", err)
	}
}

func TestDefaultWindowUsesLookbackFromRunStart(t *testing.T) {
	started := time.Date(2026, 3, 1, 0, 45, 12, 0, time.UTC)
	plan, err := Compile(nil, started)
	if err != nil || plan.Window.Start != "2026-02-28T21:00:00Z" || plan.Window.End != "2026-03-01T00:00:00Z" {
		t.Fatal(plan, err)
	}
	plan, _ = Compile(map[string]any{VarDefaultLookback: 1}, started)
	if plan.Window.Start != "2026-02-28T23:00:00Z" || len(plan.HourPaths) != 1 ||
		plan.HourPaths[0] != "app_id=*/event_date=2026-02-28/hour=23/*.parquet" {
		t.Fatal(plan.Window, plan.HourPaths)
	}
}

func TestJinjaIntFilter(t *testing.T) {
	for v, want := range map[any]int64{"5": 5, " 7 ": 7, "2.9": 2, "x": 0, 3.7: 3, true: 1, nil: 0, int64(9): 9, 4: 4} {
		if got := JinjaInt(v); got != want {
			t.Errorf("JinjaInt(%v) = %d, want %d", v, got, want)
		}
	}
}

func TestModelKeepsIncrementalAndReplayKey(t *testing.T) {
	plan, _ := Compile(window(nil), time.Now())
	for _, fragment := range []string{"partition by app_id, event_id", "received_time desc nulls last",
		"nats_sequence desc nulls last", "event_time desc nulls last", "ingest_request_id desc nulls last",
		"where _dedupe_rank = 1", "rawbbit_raw_s3", "format = 'Parquet'", "structure = '" + ParquetStructure + "'"} {
		if !strings.Contains(plan.SQL, fragment) {
			t.Errorf("model SQL lacks %q", fragment)
		}
	}
	if !strings.Contains(DeleteExisting("t", "n"), "(app_id, event_id) in (select app_id, event_id") ||
		!strings.Contains(CreateTableAs("r", "x"), "SETTINGS s3_throw_on_zero_files_match=0") {
		t.Error("delete_insert key or s3 settings missing")
	}
}
