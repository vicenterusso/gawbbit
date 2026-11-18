package clickhouse

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/storage"
)

func testControl(t *testing.T) *Control {
	t.Helper()
	c, err := NewControl(config.Env{"CLICKHOUSE_DBT_USER": "loader", "CLICKHOUSE_DBT_PASSWORD": "secret"}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(time.Duration) {}
	return c
}

// Ported from tests/test_runtime.py ControlTests.
func TestControlQueriesDoNotSelfMatchAndKillIsOwnedTagged(t *testing.T) {
	c := testControl(t)
	var sqls []string
	responses := [][]map[string]any{{{"query_id": "owned"}}, {}, {}, {}, {}}
	c.Query = func(sql string) ([]map[string]any, error) {
		sqls = append(sqls, sql)
		r := responses[0]
		responses = responses[1:]
		return r, nil
	}
	if !c.Settle("attempt", []string{"team"}, t.TempDir()) {
		t.Fatal("not settled")
	}
	var kill string
	for _, s := range sqls {
		if strings.HasPrefix(s, "KILL") {
			kill = s
		}
	}
	if !strings.Contains(kill, "user = 'loader'") || !strings.Contains(kill, "Settings['log_comment'] = 'attempt'") || !strings.Contains(kill, " SYNC") {
		t.Errorf("kill %q", kill)
	}
	if !strings.Contains(strings.Join(sqls, "\n"), "{control_query_id:String}") {
		t.Error("loader process check matches its own control query")
	}
}

func saveRequest(t *testing.T, dir, id, status string) {
	t.Helper()
	if err := storage.Save(filepath.Join(dir, id+".json"), pyjson.NewObject("query_id", id, "status", status)); err != nil {
		t.Fatal(err)
	}
}

func TestNoProcessIsNotProofPendingRequestCompleted(t *testing.T) {
	c := testControl(t)
	dir := t.TempDir()
	saveRequest(t, dir, "q", "dispatched")
	var sqls []string
	c.Query = func(sql string) ([]map[string]any, error) { sqls = append(sqls, sql); return nil, nil }
	if c.Settle("attempt", []string{"team"}, dir) {
		t.Fatal("settled without terminal query_log proof")
	}
	if !strings.Contains(strings.Join(sqls, "\n"), "system.query_log") {
		t.Error("query_log not consulted")
	}
}

func TestTerminalLogEvidenceAllowsFailedHTTPRequest(t *testing.T) {
	c := testControl(t)
	dir := t.TempDir()
	saveRequest(t, dir, "q", "dispatched")
	saveRequest(t, dir, "ok", "confirmed")
	c.Query = func(sql string) ([]map[string]any, error) {
		if strings.Contains(sql, "system.query_log") {
			if !strings.Contains(sql, "query_id IN ('q')") {
				t.Errorf("unexpected pending set: %s", sql)
			}
			return []map[string]any{{"query_id": "q"}}, nil
		}
		return nil, nil
	}
	if !c.Settle("attempt", []string{"team"}, dir) {
		t.Fatal("terminal evidence ignored")
	}
}

func TestPendingMutationOrUnownedQueryNeverKilledOrTreatedSafe(t *testing.T) {
	for _, marker := range []string{"system.mutations", "control_query_id:String"} {
		c := testControl(t)
		var sqls []string
		c.Query = func(sql string) ([]map[string]any, error) {
			sqls = append(sqls, sql)
			if strings.Contains(sql, marker) {
				return []map[string]any{{"query_id": "unowned"}}, nil
			}
			return nil, nil
		}
		if c.Settle("attempt", []string{"team"}, t.TempDir()) {
			t.Errorf("%s: settled", marker)
		}
		for _, s := range sqls {
			if strings.HasPrefix(s, "KILL") {
				t.Errorf("%s: killed unowned work", marker)
			}
		}
	}
}

func TestDeniedMetadataOrKillIsUncertain(t *testing.T) {
	c := testControl(t)
	c.Query = func(string) ([]map[string]any, error) { return nil, errors.New("denied") }
	if c.Settle("attempt", []string{"team"}, t.TempDir()) {
		t.Fatal("denied observation treated as quiescence")
	}
	dir := t.TempDir()
	if err := storage.Save(filepath.Join(dir, "bad.json"), []any{1}); err != nil {
		t.Fatal(err)
	}
	c.Query = func(string) ([]map[string]any, error) { return nil, nil }
	if c.Settle("attempt", []string{"team"}, dir) {
		t.Fatal("unreadable request record treated as quiescence")
	}
}

func TestSchemaReadinessRejectsIncompatibleAndMissing(t *testing.T) {
	c := testControl(t)
	columns := func(override map[string]string) []map[string]any {
		var rows []map[string]any
		for name, kind := range ExpectedColumns {
			if v, ok := override[name]; ok {
				kind = v
			}
			if kind != "" {
				rows = append(rows, map[string]any{"name": name, "type": kind})
			}
		}
		return rows
	}
	table := []map[string]any{{"engine": "MergeTree", "partition_key": PartitionKey, "sorting_key": SortingKey}}
	ready := func(tableRows, columnRows []map[string]any, selectErr error) bool {
		c.Query = func(sql string) ([]map[string]any, error) {
			switch {
			case strings.Contains(sql, "system.tables"):
				return tableRows, nil
			case strings.Contains(sql, "system.columns"):
				return columnRows, nil
			}
			return nil, selectErr
		}
		return c.Ready("team")
	}
	if !ready(table, columns(nil), nil) {
		t.Error("compatible table rejected")
	}
	if !ready(table, columns(map[string]string{"app_id": "LowCardinality(String)", "platform": "LowCardinality(Nullable(String))"}), nil) {
		t.Error("LowCardinality variants rejected")
	}
	if ready(nil, nil, nil) || ready(table, columns(map[string]string{"nats_sequence": "Int64"}), nil) ||
		ready(table, columns(map[string]string{"geo_json": ""}), nil) ||
		ready([]map[string]any{{"engine": "ReplacingMergeTree", "partition_key": PartitionKey, "sorting_key": SortingKey}}, columns(nil), nil) ||
		ready(table, columns(nil), errors.New("denied")) {
		t.Error("incompatible or inaccessible table accepted")
	}
}

func TestEndpointValidationOrderAndMessages(t *testing.T) {
	base := config.Env{"CLICKHOUSE_DBT_USER": "u", "CLICKHOUSE_DBT_PASSWORD": "p"}
	with := func(k, v string) config.Env {
		e := config.Env{}
		for a, b := range base {
			e[a] = b
		}
		e[k] = v
		return e
	}
	for env, want := range map[*config.Env]string{
		ptr(with("CLICKHOUSE_SECURE", "true")):   "invalid ClickHouse TLS switches",
		ptr(with("CLICKHOUSE_VERIFY", "2")):      "invalid ClickHouse TLS switches",
		ptr(with("CLICKHOUSE_HOST", "")):         "invalid ClickHouse HTTP endpoint",
		ptr(with("CLICKHOUSE_HOST", "a@b")):      "invalid ClickHouse HTTP endpoint",
		ptr(with("CLICKHOUSE_PORT", "0")):        "invalid ClickHouse HTTP endpoint",
		ptr(with("CLICKHOUSE_PORT", "65536")):    "invalid ClickHouse HTTP endpoint",
		ptr(with("CLICKHOUSE_PORT", "-1")):       "invalid ClickHouse HTTP endpoint",
		ptr(with("CLICKHOUSE_DBT_PASSWORD", "")): "loader credentials required",
	} {
		if _, err := ParseEndpoint(*env); err == nil || err.Error() != want {
			t.Errorf("%v: %v, want %s", *env, err, want)
		}
	}
	ep, err := ParseEndpoint(with("CLICKHOUSE_SECURE", "1"))
	if err != nil || ep.URL != "https://clickhouse:8123" {
		t.Fatal(ep, err)
	}
}

func ptr(e config.Env) *config.Env { return &e }

func TestControlHTTPContractAndNoRedirects(t *testing.T) {
	var seen *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		if r.URL.Query().Get("redirect") == "" && strings.Contains(r.URL.RawQuery, "log_comment=rawbbit-dbt-control") {
			_, _ = w.Write([]byte(`{"data":[{"x":"1"}]}`))
		}
	}))
	defer server.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	c, err := NewControl(config.Env{"CLICKHOUSE_HOST": host, "CLICKHOUSE_PORT": port,
		"CLICKHOUSE_DBT_USER": "loader", "CLICKHOUSE_DBT_PASSWORD": "pw"}, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query("SELECT 1")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	q := seen.URL.Query()
	if q.Get("query_id") == "" || q.Get("param_control_query_id") != q.Get("query_id") || q.Get("max_execution_time") != "5" ||
		q.Get("wait_end_of_query") != "1" || seen.Header.Get("X-ClickHouse-User") != "loader" || seen.Header.Get("X-ClickHouse-Key") != "pw" {
		t.Errorf("unexpected control request %v %v", q, seen.Header)
	}
	redirect := httptest.NewServer(http.RedirectHandler(server.URL+"/?redirect=1", http.StatusFound))
	defer redirect.Close()
	host, port, _ = strings.Cut(strings.TrimPrefix(redirect.URL, "http://"), ":")
	c, _ = NewControl(config.Env{"CLICKHOUSE_HOST": host, "CLICKHOUSE_PORT": port,
		"CLICKHOUSE_DBT_USER": "loader", "CLICKHOUSE_DBT_PASSWORD": "pw"}, 5, 1)
	if _, err := c.Query("SELECT 1"); err == nil {
		t.Error("redirect followed")
	}
	if !IsUUID(UUID4()) || IsUUID(strings.ToUpper(UUID4())) {
		t.Error("UUID helpers")
	}
	if Literal(`a'b\c`) != `'a\'b\\c'` {
		t.Error(Literal(`a'b\c`))
	}
}
