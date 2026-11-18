// Package model is the Go reimplementation of the former dbt project: the
// rawbbit_events_load incremental model (dbt-clickhouse 1.9.8 delete_insert
// on (app_id, event_id)), its window/app-selection macros, and its data
// tests. Variable names, defaults and compile-error messages are unchanged.
package model

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
)

// Project-level variable defaults (formerly dbt_project.yml vars).
const (
	DefaultLookbackHours  = 3
	DefaultMaxWindowHours = 168
)

// Variable names accepted via --vars (formerly dbt vars).
const (
	VarWindowStart     = "rawbbit_window_start"
	VarWindowEnd       = "rawbbit_window_end"
	VarAppID           = "rawbbit_app_id"
	VarExcludedAppIDs  = "rawbbit_excluded_app_ids"
	VarDefaultLookback = "rawbbit_default_lookback_hours"
	VarMaxWindowHours  = "rawbbit_max_model_window_hours"
)

// CompileError is a model compilation failure (dbt raise_compiler_error).
// It happens before any warehouse request.
type CompileError struct{ Msg string }

func (e *CompileError) Error() string { return e.Msg }

func compileErr(msg string) error { return &CompileError{Msg: msg} }

var (
	timestampRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:00:00Z$`)
	isoLayout   = "2006-01-02T15:04:05Z"
)

// ParquetStructure is the explicit raw-writer Parquet schema.
const ParquetStructure = "event_id Nullable(String), app_id Nullable(String), environment Nullable(String), event_name Nullable(String), " +
	"event_timestamp Nullable(String), received_at Nullable(String), user_id Nullable(String), " +
	"user_pseudo_id Nullable(String), session_id Nullable(String), platform Nullable(String), " +
	"app_version Nullable(String), os_version Nullable(String), device_model Nullable(String), " +
	"locale Nullable(String), timezone Nullable(String), event_params_json Nullable(String), " +
	"user_properties_json Nullable(String), traffic_source_json Nullable(String), geo_json Nullable(String), " +
	"consent_json Nullable(String), ingest_request_id Nullable(String), ingest_user_agent Nullable(String), " +
	"ingest_ip_hash Nullable(String), nats_stream Nullable(String), nats_sequence Nullable(Int64)"

// Window is a validated, hour-aligned UTC load window.
type Window struct {
	Start, End string
	StartTime  time.Time
	HourCount  int
}

// Selection is the validated app include/exclude mode.
type Selection struct {
	Include *string
	Exclude []string
}

// Plan is a compiled model: window, selection and rendered select SQL.
type Plan struct {
	Window    Window
	Selection Selection
	HourPaths []string
	Predicate string
	SQL       string
}

// Compile validates vars exactly like the former Jinja macros and renders
// the model select. runStartedAt supplies the default window.
func Compile(vars map[string]any, runStartedAt time.Time) (*Plan, error) {
	window, err := compileWindow(vars, runStartedAt)
	if err != nil {
		return nil, err
	}
	selection, err := compileSelection(vars)
	if err != nil {
		return nil, err
	}
	partition := "*"
	if selection.Include != nil {
		partition = *selection.Include
	}
	paths := make([]string, 0, window.HourCount)
	for offset := 0; offset < window.HourCount; offset++ {
		hour := window.StartTime.Add(time.Duration(offset) * time.Hour)
		paths = append(paths, "app_id="+partition+"/event_date="+hour.Format("2006-01-02")+
			"/hour="+hour.Format("15")+"/*.parquet")
	}
	plan := &Plan{Window: window, Selection: selection, HourPaths: paths, Predicate: predicate(selection)}
	plan.SQL = render("rawbbit_events_load.sql", map[string]any{
		"HourPaths": paths, "Structure": ParquetStructure, "Predicate": plan.Predicate})
	return plan, nil
}

func predicate(s Selection) string {
	if s.Include != nil {
		return "and app_id = '" + *s.Include + "'"
	}
	if len(s.Exclude) > 0 {
		quoted := make([]string, len(s.Exclude))
		for i, app := range s.Exclude {
			quoted[i] = "'" + app + "'"
		}
		return "and app_id not in (" + strings.Join(quoted, ", ") + ")"
	}
	return ""
}

func lookup(vars map[string]any, key string, def any) any {
	if v, ok := vars[key]; ok {
		return v
	}
	return def
}

func compileWindow(vars map[string]any, runStartedAt time.Time) (Window, error) {
	lookback := JinjaInt(lookup(vars, VarDefaultLookback, int64(DefaultLookbackHours)))
	defaultEnd := runStartedAt.UTC().Truncate(time.Hour)
	defaultStart := defaultEnd.Add(-time.Duration(lookback) * time.Hour)
	start := lookup(vars, VarWindowStart, defaultStart.Format(isoLayout))
	end := lookup(vars, VarWindowEnd, defaultEnd.Format(isoLayout))
	// re.match on a non-string raises TypeError before any custom message.
	startText, ok := start.(string)
	if !ok {
		return Window{}, compileErr("expected string or bytes-like object, got '" + pyTypeName(start) + "'")
	}
	if !timestampRE.MatchString(startText) {
		return Window{}, compileErr("rawbbit_window_start must be an hour-aligned UTC timestamp: " + startText)
	}
	endText, ok := end.(string)
	if !ok {
		return Window{}, compileErr("expected string or bytes-like object, got '" + pyTypeName(end) + "'")
	}
	if !timestampRE.MatchString(endText) {
		return Window{}, compileErr("rawbbit_window_end must be an hour-aligned UTC timestamp: " + endText)
	}
	startTime, err := strptime(startText)
	if err != nil {
		return Window{}, err
	}
	endTime, err := strptime(endText)
	if err != nil {
		return Window{}, err
	}
	seconds := int64(endTime.Sub(startTime) / time.Second)
	maxHours := JinjaInt(lookup(vars, VarMaxWindowHours, int64(DefaultMaxWindowHours)))
	if seconds <= 0 {
		return Window{}, compileErr("rawbbit_window_end must be after rawbbit_window_start")
	}
	if seconds%3600 != 0 {
		return Window{}, compileErr("Rawbbit dbt windows must contain whole hours")
	}
	if seconds > maxHours*3600 {
		return Window{}, compileErr("Rawbbit dbt window exceeds " + strconv.FormatInt(maxHours, 10) +
			" hours; use the chunked backfill wrapper")
	}
	return Window{Start: startText, End: endText, StartTime: startTime, HourCount: int(seconds / 3600)}, nil
}

// strptime mirrors datetime.strptime(value, '%Y-%m-%dT%H:%M:%SZ') for values
// that already matched the hour-aligned pattern, including its messages.
func strptime(value string) (time.Time, error) {
	year, _ := strconv.Atoi(value[0:4])
	month, _ := strconv.Atoi(value[5:7])
	day, _ := strconv.Atoi(value[8:10])
	hour, _ := strconv.Atoi(value[11:13])
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 {
		return time.Time{}, compileErr(fmt.Sprintf("time data %s does not match format '%%Y-%%m-%%dT%%H:%%M:%%SZ'", pyRepr(value)))
	}
	if year < 1 {
		return time.Time{}, compileErr(fmt.Sprintf("year %d is out of range", year))
	}
	t := time.Date(year, time.Month(month), day, hour, 0, 0, 0, time.UTC)
	if t.Day() != day {
		return time.Time{}, compileErr("day is out of range for month")
	}
	return t, nil
}

// pyTypeName is Python's type(v).__name__ for decoded var values.
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case int, int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	case map[string]any, *pyjson.Object:
		return "dict"
	}
	return "str"
}

func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

func compileSelection(vars map[string]any) (Selection, error) {
	include := lookup(vars, VarAppID, nil)
	exclude := lookup(vars, VarExcludedAppIDs, []any{})
	list, ok := exclude.([]any)
	if !ok {
		return Selection{}, compileErr("rawbbit_excluded_app_ids must be a list")
	}
	if include != nil && len(list) > 0 {
		return Selection{}, compileErr("include and exclude app modes are mutually exclusive")
	}
	apps := append([]any{}, list...)
	if include != nil {
		apps = append(apps, include)
	}
	for _, app := range apps {
		s, ok := app.(string)
		if !ok || !config.IsAppID(s) || strings.Contains(s, "\n") {
			return Selection{}, compileErr("unsafe app partition identifier")
		}
	}
	selection := Selection{Exclude: make([]string, len(list))}
	for i, app := range list {
		selection.Exclude[i] = app.(string)
	}
	if include != nil {
		s := include.(string)
		selection.Include = &s
	}
	return selection, nil
}

// JinjaInt mirrors Jinja's int filter: ints pass, numeric strings and floats
// truncate, anything else becomes 0.
func JinjaInt(v any) int64 {
	switch x := v.(type) {
	case bool:
		if x {
			return 1
		}
		return 0
	case int:
		return int64(x)
	case int64:
		return x
	case *big.Int:
		if x.IsInt64() {
			return x.Int64()
		}
		return 0
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0
		}
		return int64(x)
	case string:
		s := strings.TrimSpace(x)
		if n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return int64(f)
		}
	}
	return 0
}
