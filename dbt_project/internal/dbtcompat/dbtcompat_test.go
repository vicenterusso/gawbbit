package dbtcompat

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
)

func run(env config.Env, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Main(args, env, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestParseAcceptsDeploymentInvocationAndValidatesVars(t *testing.T) {
	code, out, _ := run(config.Env{}, "parse", "--project-dir", "/app", "--profiles-dir", "/app")
	if code != 0 || !strings.Contains(out, "Found 1 model, 4 data tests") {
		t.Fatal(code, out)
	}
	code, out, _ = run(config.Env{}, "parse", "--vars", `{rawbbit_window_start: "2026-07-01T00:00:00Z", rawbbit_window_end: "2026-07-09T01:00:00Z"}`)
	if code != 2 || !strings.Contains(out, "Rawbbit dbt window exceeds 168 hours; use the chunked backfill wrapper") {
		t.Fatal(code, out)
	}
	code, out, _ = run(config.Env{}, "parse", "--vars={rawbbit_app_id: '../x'}")
	if code != 2 || !strings.Contains(out, "unsafe app partition identifier") {
		t.Fatal(code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"build", "--selector", "other"}, {"build", "--bogus"}, {"compile"},
		{"build", "--vars", "[1, 2]"}, {"build", "--target", "dev"}, {"build", "--vars"}, {"build", "extra", "x"}} {
		if code, _, stderr := run(config.Env{}, args...); code != 2 || !strings.Contains(stderr, "Error: ") {
			t.Errorf("%v: %d %q", args, code, stderr)
		}
	}
	if code, out, _ := run(config.Env{}, "--version"); code != 0 || !strings.Contains(out, "dbt-core and dbt-clickhouse are not installed") {
		t.Error(code, out)
	}
}

func clickhouseEnv(t *testing.T, handler http.HandlerFunc) config.Env {
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	return config.Env{"CLICKHOUSE_HOST": host, "CLICKHOUSE_PORT": port, "CLICKHOUSE_DBT_USER": "u",
		"CLICKHOUSE_DBT_PASSWORD": "p", "CLICKHOUSE_DATABASE": "default_custom"}
}

func TestDebugChecksTheLoaderConnection(t *testing.T) {
	env := clickhouseEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ClickHouse-Key") != "p" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"1":1}]}`))
	})
	if code, out, _ := run(env, "debug", "--project-dir", "/app", "--profiles-dir", "/app"); code != 0 || !strings.Contains(out, "All checks passed!") {
		t.Fatal(code, out)
	}
	env["CLICKHOUSE_DBT_PASSWORD"] = "wrong"
	if code, out, _ := run(env, "debug"); code != 1 || strings.Contains(out, "wrong") {
		t.Fatal(code, out)
	}
}

func TestBuildExitCodes(t *testing.T) {
	vars := `{"rawbbit_window_start": "2026-07-01T00:00:00Z", "rawbbit_window_end": "2026-07-01T01:00:00Z"}`
	failing := clickhouseEnv(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if code, out, _ := run(failing, "build", "--selector", "rawbbit_ingestion", "--vars", vars); code != 2 ||
		!strings.Contains(out, "Database Error") {
		t.Fatal(code, out)
	}
	var sqls []string
	testFails := clickhouseEnv(t, func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		sql := buf.String()
		sqls = append(sqls, sql)
		switch {
		case strings.HasPrefix(sql, "EXISTS DATABASE"):
			_, _ = w.Write([]byte(`{"data":[{"result":1}]}`))
		case strings.HasPrefix(sql, "select name, engine"):
			_, _ = w.Write([]byte(`{"data":[{"name":"events","engine":"MergeTree"}]}`))
		case strings.HasPrefix(sql, "select name, type"):
			_, _ = w.Write([]byte(`{"data":[{"name":"event_id","type":"String"}]}`))
		case strings.Contains(sql, "where event_id is null"):
			_, _ = w.Write([]byte(`{"data":[{"failures":"3"}]}`))
		case strings.Contains(sql, "failures"):
			_, _ = w.Write([]byte(`{"data":[{"failures":"0"}]}`))
		}
	})
	code, out, _ := run(testFails, "build", "--fail-fast", "--vars", vars)
	if code != 1 || !strings.Contains(out, "Failure in test not_null_rawbbit_events_load_event_id") {
		t.Fatal(code, out)
	}
	if strings.Contains(strings.Join(sqls, "\n"), "event_time is null") {
		t.Fatal("fail-fast did not stop after the failing test")
	}
	if code, _, _ := run(config.Env{}, "build", "--vars", vars); code != 2 {
		t.Fatal("missing credentials must be a runtime error")
	}
}
