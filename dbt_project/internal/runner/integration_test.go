//go:build integration

package runner

// Disposable ClickHouse 24.8 + S3 integration tests (never point at
// production). They replay testdata/differential/scenarios.json with the
// built dbt-job binary and compare every observable output with the goldens
// recorded from the original Python + dbt-core/dbt-clickhouse runner.
//
//	RAWBBIT_DBT_IT_CLICKHOUSE=http://127.0.0.1:48123 \
//	RAWBBIT_DBT_IT_S3=http://127.0.0.1:48333/raw \
//	go test -tags integration ./internal/runner/
//
// The ClickHouse server must define the rawbbit_raw_s3 named collection with
// url <RAWBBIT_DBT_IT_S3>/raw/. The admin user (RAWBBIT_DBT_IT_ADMIN_USER,
// default "default") creates the dbt_integration loader and drops and
// recreates the default_custom, team_a, team_b, fresh_db and missing_target
// databases, and the test deletes every object in the bucket.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/clickhouse"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/model"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
)

const (
	itUser     = "dbt_integration"
	itPassword = "disposable_only"
)

var itDatabases = []string{"default_custom", "team_a", "team_b", "fresh_db", "missing_target"}

type lab struct {
	t          *testing.T
	clickhouse string
	s3         string
	admin      [2]string
	binary     string
}

func newLab(t *testing.T) *lab {
	chURL, s3URL := os.Getenv("RAWBBIT_DBT_IT_CLICKHOUSE"), os.Getenv("RAWBBIT_DBT_IT_S3")
	if chURL == "" || s3URL == "" {
		t.Skip("set RAWBBIT_DBT_IT_CLICKHOUSE and RAWBBIT_DBT_IT_S3 for disposable integration tests")
	}
	l := &lab{t: t, clickhouse: strings.TrimRight(chURL, "/"), s3: strings.TrimRight(s3URL, "/"),
		admin: [2]string{os.Getenv("RAWBBIT_DBT_IT_ADMIN_USER"), os.Getenv("RAWBBIT_DBT_IT_ADMIN_PASSWORD")}}
	if l.admin[0] == "" {
		l.admin[0] = "default"
	}
	l.binary = filepath.Join(t.TempDir(), "dbt-job")
	build := exec.Command("go", "build", "-o", l.binary, "../../cmd/dbt-job")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	l.setupUser()
	return l
}

func (l *lab) sql(query string) string {
	l.t.Helper()
	out, err := l.trySQL(query)
	if err != nil {
		l.t.Fatalf("%v: %s", err, query)
	}
	return out
}

func (l *lab) trySQL(query string) (string, error) {
	req, _ := http.NewRequest(http.MethodPost, l.clickhouse+"/?wait_end_of_query=1", strings.NewReader(query))
	req.Header.Set("X-ClickHouse-User", l.admin[0])
	req.Header.Set("X-ClickHouse-Key", l.admin[1])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clickhouse %d: %.300s", resp.StatusCode, body)
	}
	return string(body), nil
}

func (l *lab) setupUser() {
	l.sql("DROP USER IF EXISTS " + itUser)
	l.sql("CREATE USER " + itUser + " IDENTIFIED WITH sha256_password BY '" + itPassword + "'")
	for _, db := range itDatabases {
		l.sql("GRANT SELECT, SHOW, INSERT, CREATE TABLE, CREATE VIEW, DROP TABLE, DROP VIEW ON " + db + ".* TO " + itUser)
		l.sql("GRANT ALTER DELETE ON " + db + ".* TO " + itUser)
		l.sql("GRANT ALTER UPDATE(_row_exists) ON " + db + ".* TO " + itUser)
	}
	l.sql("GRANT CREATE DATABASE ON fresh_db.* TO " + itUser)
	l.sql("GRANT CREATE TEMPORARY TABLE, S3 ON *.* TO " + itUser)
	l.sql("GRANT NAMED COLLECTION ON rawbbit_raw_s3 TO " + itUser)
	for _, table := range []string{"processes", "mutations", "query_log", "tables", "columns"} {
		l.sql("GRANT SELECT ON system." + table + " TO " + itUser)
	}
}

func (l *lab) reset(databases []string, legacy bool) {
	for _, db := range itDatabases {
		l.sql("DROP DATABASE IF EXISTS " + db + " SYNC")
	}
	schema, err := os.ReadFile("../../../quickstart/vm_rawbbit_two/clickhouse/schema_analytics_events.sql")
	if err != nil {
		l.t.Fatal(err)
	}
	for _, db := range databases {
		if !legacy {
			l.sql("CREATE DATABASE " + db)
			continue
		}
		for _, stmt := range strings.Split(strings.ReplaceAll(string(schema), "analytics", db), ";") {
			if strings.TrimSpace(stmt) != "" {
				l.sql(stmt)
			}
		}
	}
}

type listResult struct {
	Keys []string `xml:"Contents>Key"`
}

func (l *lab) s3Clear() {
	for {
		resp, err := http.Get(l.s3 + "?list-type=2&max-keys=1000")
		if err != nil {
			l.t.Fatal(err)
		}
		var list listResult
		err = xml.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil {
			l.t.Fatal(err)
		}
		if len(list.Keys) == 0 {
			return
		}
		for _, key := range list.Keys {
			req, _ := http.NewRequest(http.MethodDelete, l.s3+"/"+(&url.URL{Path: key}).EscapedPath(), nil)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
}

var rawColumns = strings.Split("event_id,app_id,environment,event_name,event_timestamp,received_at,user_id,user_pseudo_id,"+
	"session_id,platform,app_version,os_version,device_model,locale,timezone,event_params_json,user_properties_json,"+
	"traffic_source_json,geo_json,consent_json,ingest_request_id,ingest_user_agent,ingest_ip_hash,nats_stream,nats_sequence", ",")

// put writes raw-writer-shaped Parquet (all strings plus Int64 nats_sequence,
// all nullable) produced by ClickHouse itself, then uploads it.
func (l *lab) put(key string, rows []map[string]any) {
	var structure []string
	for _, c := range rawColumns {
		kind := "Nullable(String)"
		if c == "nats_sequence" {
			kind = "Nullable(Int64)"
		}
		structure = append(structure, c+" "+kind)
	}
	var lines []string
	for _, row := range rows {
		data, _ := json.Marshal(row)
		lines = append(lines, string(data))
	}
	query := "SELECT * FROM format(JSONEachRow, '" + strings.Join(structure, ", ") + "', $rawbbit$" +
		strings.Join(lines, "\n") + "$rawbbit$) SETTINGS output_format_parquet_string_as_string = 1 FORMAT Parquet"
	parquet := l.sql(query)
	req, _ := http.NewRequest(http.MethodPut, l.s3+"/raw/"+key, strings.NewReader(parquet))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		l.t.Fatalf("put %s: %d", key, resp.StatusCode)
	}
}

func (l *lab) dump(db string) any {
	ddl, err := l.trySQL("SHOW CREATE TABLE " + db + ".events FORMAT TSVRaw")
	if err != nil {
		return map[string]any{"ddl": nil, "rows": nil}
	}
	return map[string]any{"ddl": ddl,
		"columns": l.sql("SELECT name, type FROM system.columns WHERE database='" + db + "' AND table='events' ORDER BY position FORMAT TSV"),
		"rows":    l.sql("SELECT * FROM " + db + ".events ORDER BY ALL FORMAT JSONCompactEachRow")}
}

// normalizer mirrors the Python differential driver's normalization.
type normalizer struct {
	rt  string
	ids map[string]string
}

var (
	uuidRE    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	prefixRE  = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ (\[dbt-progress )`)
	elapsedRE = regexp.MustCompile(`elapsed_seconds=\d+\.\d{3}`)
)

func (n *normalizer) text(s string) string {
	s = strings.ReplaceAll(s, n.rt, "<rt>")
	return uuidRE.ReplaceAllStringFunc(s, func(v string) string {
		if _, ok := n.ids[v]; !ok {
			n.ids[v] = fmt.Sprintf("<id%d>", len(n.ids)+1)
		}
		return n.ids[v]
	})
}

func (n *normalizer) value(v any) any {
	switch x := v.(type) {
	case *pyjson.Object:
		out := map[string]any{}
		for _, k := range x.Keys() {
			item, _ := x.Get(k)
			if _, ok := item.(string); ok && (k == "created_at" || k == "finished_at") {
				out[n.text(k)] = "<ts>"
			} else {
				out[n.text(k)] = n.value(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = n.value(item)
		}
		return out
	case string:
		return n.text(x)
	}
	return v
}

func splitLines(s string) []any {
	out := []any{}
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if s != "" {
			out = append(out, line)
		}
	}
	return out
}

func (n *normalizer) snapshot(rt string) map[string]any {
	files := map[string]any{}
	_ = filepath.Walk(rt, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == rt {
			return nil
		}
		rel, _ := filepath.Rel(rt, path)
		top := strings.Split(rel, string(filepath.Separator))[0]
		mode := fmt.Sprintf("0o%o", info.Mode().Perm())
		if info.IsDir() {
			files[n.text(rel)+"/"] = mode
			if top == "requests" && rel != "requests" {
				summary := map[string]bool{}
				entries, _ := os.ReadDir(path)
				for _, e := range entries {
					data, _ := os.ReadFile(filepath.Join(path, e.Name()))
					value, _ := pyjson.Decode(string(data), pyjson.DecodeOptions{})
					obj := value.(*pyjson.Object)
					trimmed := pyjson.NewObject()
					keys := obj.Keys()
					sort.Strings(keys)
					for _, k := range obj.Keys() {
						if k != "query_id" {
							v, _ := obj.Get(k)
							trimmed.Set(k, v)
						}
					}
					summary[pyjson.Encode(n.value(trimmed), pyjson.EncodeOptions{SortKeys: true})] = true
					summary["keys="+strings.Join(keys, ",")] = true
				}
				list := []any{}
				for k := range summary {
					list = append(list, k)
				}
				sort.Slice(list, func(i, j int) bool { return list[i].(string) < list[j].(string) })
				files[n.text(rel)+"/*"] = list
				return filepath.SkipDir
			}
			return nil
		}
		data, _ := os.ReadFile(path)
		if value, err := pyjson.Decode(string(data), pyjson.DecodeOptions{}); err == nil {
			files[n.text(rel)] = map[string]any{"mode": mode, "json": n.value(value), "newline": bytes.HasSuffix(data, []byte("\n"))}
		} else {
			files[n.text(rel)] = map[string]any{"mode": mode, "raw": n.text(string(data))}
		}
		return nil
	})
	return files
}

func (l *lab) runJob(rt string, args []string, env map[string]string) (int, string, string) {
	cmd := exec.Command(l.binary, args...)
	u, _ := url.Parse(l.clickhouse)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + rt, "CLICKHOUSE_HOST=" + u.Hostname(), "CLICKHOUSE_PORT=" + u.Port(),
		"CLICKHOUSE_DATABASE=default_custom", "CLICKHOUSE_DBT_USER=" + itUser, "CLICKHOUSE_DBT_PASSWORD=" + itPassword,
		"RAWBBIT_RAW_LOAD_MODE=dbt", "RAWBBIT_DBT_ROUTING_ENABLED=0", "RAWBBIT_DBT_RUNTIME_DIR=" + rt,
		"RAWBBIT_DBT_ROUTES_FILE=" + rt + "/dbt-routes.json", "DBT_THREADS=1"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		l.t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

type step map[string]json.RawMessage

func (l *lab) replay(steps []step) []any {
	rt := l.t.TempDir()
	n := &normalizer{rt: rt, ids: map[string]string{}}
	var results []any
	var lock *os.File
	defer func() {
		if lock != nil {
			lock.Close()
		}
	}()
	for _, st := range steps {
		for kind, raw := range st {
			switch kind {
			case "reset":
				var arg struct {
					Databases []string `json:"databases"`
					Schema    string   `json:"schema"`
				}
				_ = json.Unmarshal(raw, &arg)
				l.reset(arg.Databases, arg.Schema == "legacy")
			case "put":
				var arg struct {
					Key  string           `json:"key"`
					Rows []map[string]any `json:"rows"`
				}
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber() // keep Int64 values exact
				_ = decoder.Decode(&arg)
				l.put(arg.Key, arg.Rows)
			case "sql":
				var q string
				_ = json.Unmarshal(raw, &q)
				l.sql(q)
			case "routes", "routes_raw", "routes_bytes_hex":
				path := filepath.Join(rt, "dbt-routes.json")
				var data []byte
				switch kind {
				case "routes":
					if string(raw) == "null" {
						os.Remove(path)
						continue
					}
					v, _ := pyjson.Decode(string(raw), pyjson.DecodeOptions{})
					data = []byte(pyjson.Encode(v, pyjson.EncodeOptions{}))
				case "routes_raw":
					var s string
					_ = json.Unmarshal(raw, &s)
					data = []byte(s)
				default:
					var s string
					_ = json.Unmarshal(raw, &s)
					fmt.Sscanf(s, "%x", &data)
				}
				if err := os.WriteFile(path, data, 0o644); err != nil {
					l.t.Fatal(err)
				}
			case "touch":
				var p string
				_ = json.Unmarshal(raw, &p)
				os.WriteFile(filepath.Join(rt, p), nil, 0o644)
			case "remove":
				var p string
				_ = json.Unmarshal(raw, &p)
				os.Remove(filepath.Join(rt, p))
			case "write":
				var arg struct{ Path, Content string }
				_ = json.Unmarshal(raw, &arg)
				os.MkdirAll(filepath.Dir(filepath.Join(rt, arg.Path)), 0o755)
				os.WriteFile(filepath.Join(rt, arg.Path), []byte(arg.Content), 0o644)
			case "show":
				var p string
				_ = json.Unmarshal(raw, &p)
				var content any
				if data, err := os.ReadFile(filepath.Join(rt, p)); err == nil {
					v, _ := pyjson.Decode(string(data), pyjson.DecodeOptions{})
					content = n.value(v)
				}
				results = append(results, map[string]any{"show": p, "content": content})
			case "lock":
				if string(raw) == "true" {
					lock, _ = os.OpenFile(filepath.Join(rt, "pipeline.lock"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
					_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
				} else {
					lock.Close()
					lock = nil
				}
			case "run":
				var arg struct {
					Args   []string          `json:"args"`
					Env    map[string]string `json:"env"`
					Ignore []string          `json:"ignore"`
				}
				_ = json.Unmarshal(raw, &arg)
				code, stdout, stderr := l.runJob(rt, arg.Args, arg.Env)
				var errLines []any
				for _, line := range splitLines(stderr) {
					s := prefixRE.ReplaceAllString(line.(string), "$1")
					errLines = append(errLines, n.text(elapsedRE.ReplaceAllString(s, "elapsed_seconds=<s>")))
				}
				if errLines == nil {
					errLines = []any{}
				}
				var spec any
				_ = json.Unmarshal(raw, &spec)
				result := map[string]any{"run": spec, "exit": int64(code), "stdout": splitLines(n.text(stdout)),
					"stderr": errLines, "state": n.snapshot(rt)}
				for _, key := range arg.Ignore {
					result[key] = "<ignored: documented deviation>"
				}
				results = append(results, result)
			case "dump":
				var dbs []string
				_ = json.Unmarshal(raw, &dbs)
				dumps := map[string]any{}
				for _, db := range dbs {
					dumps[db] = l.dump(db)
				}
				results = append(results, map[string]any{"dump": dumps})
			default:
				l.t.Fatalf("unknown step %s", kind)
			}
		}
	}
	return results
}

// canonical renders Go results exactly like the goldens (CPython json).
func canonical(v any) string {
	return pyjson.Encode(v, pyjson.EncodeOptions{SortKeys: true})
}

func dropRoutesInput(v any) {
	switch x := v.(type) {
	case map[string]any:
		if state, ok := x["state"].(map[string]any); ok {
			delete(state, "dbt-routes.json") // harness input, not runner output
		}
	case *pyjson.Object:
		if state, ok := x.Get("state"); ok {
			if obj, ok := state.(*pyjson.Object); ok {
				clean := pyjson.NewObject()
				for _, k := range obj.Keys() {
					if k != "dbt-routes.json" {
						item, _ := obj.Get(k)
						clean.Set(k, item)
					}
				}
				x.Set("state", clean)
			}
		}
	}
}

// clockScenarios derive their windows from the wall clock, so the goldens hold
// the capture-time hours. Their window values are masked here; window
// arithmetic for arbitrary clocks is covered by the offline window goldens.
var (
	clockScenarios = map[string]bool{"hourly_daily_no_data": true}
	clockRE        = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:00:00Z`)
)

func TestDifferentialScenariosMatchPythonGoldens(t *testing.T) {
	l := newLab(t)
	data, err := os.ReadFile("../../testdata/differential/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []struct {
		Name  string `json:"name"`
		Steps []step `json:"steps"`
	}
	if err := json.Unmarshal(data, &scenarios); err != nil {
		t.Fatal(err)
	}
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			l.t = t
			l.s3Clear()
			got := l.replay(sc.Steps)
			raw, err := os.ReadFile("../../testdata/differential/golden/" + sc.Name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			golden, err := pyjson.Decode(string(raw), pyjson.DecodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := golden.([]any)
			if len(got) != len(want) {
				t.Fatalf("result count %d != %d", len(got), len(want))
			}
			for i := range got {
				dropRoutesInput(got[i])
				dropRoutesInput(want[i])
				g := canonical(got[i])
				w := pyjson.Encode(want[i], pyjson.EncodeOptions{SortKeys: true})
				if clockScenarios[sc.Name] {
					g, w = clockRE.ReplaceAllString(g, "<clock>"), clockRE.ReplaceAllString(w, "<clock>")
				}
				if g != w {
					k := 0
					for k < len(g) && k < len(w) && g[k] == w[k] {
						k++
					}
					t.Errorf("step result %d differs at byte %d\n go: ...%.600s\npy: ...%.600s", i, k, g[max(0, k-200):], w[max(0, k-200):])
				}
			}
		})
	}
}

func (l *lab) loaderEnv() config.Env {
	u, _ := url.Parse(l.clickhouse)
	return config.Env{"CLICKHOUSE_HOST": u.Hostname(), "CLICKHOUSE_PORT": u.Port(),
		"CLICKHOUSE_DBT_USER": itUser, "CLICKHOUSE_DBT_PASSWORD": itPassword}
}

// Ported from tests/test_adapter_integration.py: every loader request is
// journaled with the exact server query ID and carries the attempt settings.
func TestEveryLoaderRequestHasExactServerQueryIDAndSettings(t *testing.T) {
	l := newLab(t)
	l.s3Clear()
	l.reset([]string{"default_custom"}, true)
	l.put("app_id=a1/event_date=2026-07-01/hour=00/f.parquet", []map[string]any{{"event_id": "e", "app_id": "a1",
		"event_timestamp": "2026-07-01T00:15:00Z"}})
	tag := "rawbbit-dbt:" + clickhouse.UUID4()
	requests := t.TempDir()
	vars := map[string]any{model.VarWindowStart: "2026-07-01T00:00:00Z", model.VarWindowEnd: "2026-07-01T01:00:00Z"}
	if err := Load(context.Background(), l.loaderEnv(), "default_custom", tag, requests, vars, nil); err != nil {
		t.Fatal(err)
	}
	control, err := clickhouse.NewControl(l.loaderEnv(), 10, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !control.Settle(tag, []string{"default_custom"}, requests) {
		t.Fatal("loader work not settled")
	}
	l.sql("SYSTEM FLUSH LOGS")
	journal := map[string]bool{}
	entries, _ := os.ReadDir(requests)
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(requests, e.Name()))
		var record struct {
			QueryID           string            `json:"query_id"`
			Status            string            `json:"status"`
			TransportSettings map[string]string `json:"transport_settings"`
		}
		_ = json.Unmarshal(data, &record)
		if record.Status != "confirmed" || record.TransportSettings["log_comment"] != tag ||
			record.TransportSettings["mutations_sync"] != "2" || record.TransportSettings["lightweight_deletes_sync"] != "2" {
			t.Errorf("journal record %s", data)
		}
		journal[record.QueryID] = true
	}
	rows := l.sql("SELECT query_id, Settings['mutations_sync'], Settings['log_comment'], query FROM system.query_log WHERE log_comment = " +
		clickhouse.Literal(tag) + " AND type = 'QueryFinish' FORMAT JSONCompactEachRow")
	server := map[string]bool{}
	writes := 0
	for _, line := range strings.Split(strings.TrimSpace(rows), "\n") {
		var row []string
		_ = json.Unmarshal([]byte(line), &row)
		server[row[0]] = true
		q := strings.ToUpper(strings.TrimSpace(row[3]))
		if strings.HasPrefix(q, "CREATE") || strings.HasPrefix(q, "INSERT") || strings.HasPrefix(q, "DELETE") || strings.HasPrefix(q, "DROP") {
			writes++
			if row[1] != "2" || row[2] != tag {
				t.Errorf("write without attempt settings: %v", row)
			}
		}
	}
	for id := range journal {
		if !server[id] {
			t.Errorf("journaled query %s has no terminal server record", id)
		}
	}
	if len(journal) == 0 || writes < 6 {
		t.Fatalf("journal %d, writes %d", len(journal), writes)
	}
}

// Ported from integration_tests: a client timeout requires the server
// request to be proven terminal (KILLed if still owned) before fallback.
func TestClientTimeoutRequiresServerCompletionBeforeFallback(t *testing.T) {
	l := newLab(t)
	l.s3Clear()
	l.reset([]string{"default_custom", "team_a"}, true)
	l.put("app_id=route_a/event_date=2026-07-01/hour=00/f.parquet", []map[string]any{{"event_id": "a1", "app_id": "route_a",
		"event_timestamp": "2026-07-01T00:15:00Z"}})
	rt := t.TempDir()
	routes := `{"version":1,"routes":[{"app_id":"route_a","dataset_id":"a","database":"team_a","table":"events"}]}`
	os.WriteFile(filepath.Join(rt, "dbt-routes.json"), []byte(routes), 0o600)
	env := l.loaderEnv()
	for k, v := range map[string]string{"RAWBBIT_DBT_RUNTIME_DIR": rt, "RAWBBIT_RAW_LOAD_MODE": "dbt",
		"RAWBBIT_DBT_ROUTING_ENABLED": "1", "RAWBBIT_DBT_ROUTES_FILE": filepath.Join(rt, "dbt-routes.json"),
		"CLICKHOUSE_DATABASE": "default_custom"} {
		env[k] = v
	}
	var stdout bytes.Buffer
	r := New(env, &stdout, io.Discard)
	slowSQL := "INSERT INTO team_a.events (event_id,app_id,event_time,event_date) SELECT toString(number), 'route_a', " +
		"toDateTime64('2026-07-01 00:00:00',3,'UTC'), toDate('2026-07-01') FROM numbers(25) WHERE sleepEachRow(0.1)=0"
	r.Loader = func(ctx context.Context, env config.Env, database, tag, requestDir string, vars map[string]any, obs model.Observer) error {
		if vars[model.VarAppID] != "route_a" || database != "team_a" {
			return Load(ctx, env, database, tag, requestDir, vars, obs)
		}
		ep, _ := clickhouse.ParseEndpoint(env)
		exec := model.NewHTTPExecutor(ep, tag, 2, requestDir)
		return exec.Exec(ctx, slowSQL)
	}
	r.Env["RAWBBIT_DBT_ATTEMPT_TIMEOUT_SECONDS"] = "1"
	code, err := r.Run([]string{"backfill", "2026-07-01T00:00:00Z", "2026-07-01T01:00:00Z"})
	if err != nil || code != 2 {
		t.Fatalf("code %d err %v\n%s", code, err, stdout.String())
	}
	if exists(r.Fence) {
		t.Fatal("fenced although server work was proven terminal")
	}
	var primary *pyjson.Object
	for _, o := range r.outcomes() {
		if obj := o.(*pyjson.Object); str(obj, "kind") == "primary" {
			primary = obj
		}
	}
	record := load(t, filepath.Join(rt, "attempts", str(primary, "attempt_id")+".json"))
	if str(record, "error_class") != "timeout" {
		t.Fatal(pyjson.Compact(record))
	}
	if !strings.Contains(l.sql("SELECT app_id, event_id FROM default_custom.events FORMAT TSV"), "route_a\ta1") {
		t.Fatal("fallback did not load the routed app into the default table")
	}
}

// Ported from integration_tests: a delete-before-insert failure falls back
// per table; a later successful replay repairs the intended table.
func TestDeleteBeforeInsertFailureFallsBackAndReconcilesPerTable(t *testing.T) {
	for _, partial := range []bool{false, true} {
		l := newLab(t)
		l.s3Clear()
		l.reset([]string{"default_custom", "team_a"}, true)
		l.put("app_id=route_a/event_date=2026-07-01/hour=00/f.parquet", []map[string]any{{"event_id": "a1", "app_id": "route_a",
			"event_name": "view", "event_timestamp": "2026-07-01T00:15:00Z"}})
		rt := t.TempDir()
		os.WriteFile(filepath.Join(rt, "dbt-routes.json"),
			[]byte(`{"version":1,"routes":[{"app_id":"route_a","dataset_id":"a","database":"team_a","table":"events"}]}`), 0o600)
		args := []string{"backfill", "2026-07-01T00:00:00Z", "2026-07-01T01:00:00Z"}
		envs := map[string]string{"RAWBBIT_DBT_ROUTING_ENABLED": "1"}
		if code, out, _ := l.runJob(rt, args, envs); code != 0 {
			t.Fatal(code, out)
		}
		env := l.loaderEnv()
		for k, v := range map[string]string{"RAWBBIT_DBT_RUNTIME_DIR": rt, "RAWBBIT_RAW_LOAD_MODE": "dbt",
			"RAWBBIT_DBT_ROUTING_ENABLED": "1", "RAWBBIT_DBT_ROUTES_FILE": filepath.Join(rt, "dbt-routes.json"),
			"CLICKHOUSE_DATABASE": "default_custom"} {
			env[k] = v
		}
		r := New(env, io.Discard, io.Discard)
		r.Loader = func(ctx context.Context, env config.Env, database, tag, requestDir string, vars map[string]any, obs model.Observer) error {
			if database != "team_a" {
				return Load(ctx, env, database, tag, requestDir, vars, obs)
			}
			ep, _ := clickhouse.ParseEndpoint(env)
			exec := model.NewHTTPExecutor(ep, tag, 2, requestDir)
			if err := exec.Exec(ctx, "DELETE FROM team_a.events WHERE app_id = 'route_a'"); err != nil {
				return err
			}
			if partial {
				if err := exec.Exec(ctx, "INSERT INTO team_a.events (event_id,app_id,event_name,event_time,event_date) "+
					"VALUES ('a1','route_a','partial','2026-07-01 00:15:00','2026-07-01')"); err != nil {
					return err
				}
			}
			return fmt.Errorf("injected failure")
		}
		if code, err := r.Run(args); err != nil || code != 2 {
			t.Fatal(code, err)
		}
		teamA := l.sql("SELECT app_id, event_id, event_name FROM team_a.events FORMAT TSV")
		if partial != strings.Contains(teamA, "route_a\ta1\tpartial") {
			t.Fatalf("partial=%v team_a=%q", partial, teamA)
		}
		if !strings.Contains(l.sql("SELECT app_id, event_id FROM default_custom.events FORMAT TSV"), "route_a\ta1") {
			t.Fatal("fallback copy missing")
		}
		for i := 0; i < 2; i++ {
			if code, out, _ := l.runJob(rt, args, envs); code != 0 {
				t.Fatal(code, out)
			}
		}
		if got := l.sql("SELECT app_id, event_id, event_name FROM team_a.events FORMAT TSV"); got != "route_a\ta1\tview\n" {
			t.Fatalf("replay did not repair team_a: %q", got)
		}
		for _, db := range []string{"default_custom", "team_a"} {
			if dup := l.sql("SELECT count() FROM (SELECT app_id, event_id FROM " + db + ".events GROUP BY app_id, event_id HAVING count() > 1)"); strings.TrimSpace(dup) != "0" {
				t.Fatalf("%s has duplicate keys", db)
			}
		}
	}
}

// Ported from tests/test_adapter_integration.py: a killed writer whose
// terminal state cannot be observed (no query_log grant) persists a fence.
func TestKilledWriterAndDeniedTerminalProofPersistRealFence(t *testing.T) {
	l := newLab(t)
	l.reset([]string{"default_custom"}, true)
	user := "routing_runtime_test_" + strings.ReplaceAll(clickhouse.UUID4(), "-", "")
	l.sql("CREATE USER " + user + " IDENTIFIED WITH plaintext_password BY 'test-only'")
	defer l.sql("DROP USER IF EXISTS " + user)
	l.sql("GRANT SELECT, INSERT ON default_custom.events TO " + user)
	l.sql("GRANT SELECT ON system.processes TO " + user)
	l.sql("GRANT SELECT ON system.mutations TO " + user)
	rt := t.TempDir()
	env := l.loaderEnv()
	for k, v := range map[string]string{"CLICKHOUSE_DBT_USER": user, "CLICKHOUSE_DBT_PASSWORD": "test-only",
		"CLICKHOUSE_DATABASE": "default_custom", "RAWBBIT_DBT_RUNTIME_DIR": rt, "RAWBBIT_DBT_ROUTING_ENABLED": "0",
		"RAWBBIT_RAW_LOAD_MODE": "dbt", "RAWBBIT_DBT_SETTLE_TIMEOUT_SECONDS": "1", "RAWBBIT_DBT_ATTEMPT_TIMEOUT_SECONDS": "1"} {
		env[k] = v
	}
	r := New(env, io.Discard, io.Discard)
	r.Loader = func(ctx context.Context, env config.Env, _, tag, requestDir string, _ map[string]any, _ model.Observer) error {
		ep, _ := clickhouse.ParseEndpoint(env)
		return model.NewHTTPExecutor(ep, tag, 2, requestDir).Exec(ctx,
			"INSERT INTO default_custom.events (event_id,app_id,event_time,event_date) SELECT toString(number), 'runner_rawbbit', "+
				"toDateTime64('2026-01-01 00:00:00',3,'UTC'), toDate('2026-01-01') FROM numbers(20) WHERE sleepEachRow(0.1)=0")
	}
	if code, err := r.Run([]string{"hourly"}); err != nil || code != 4 || !exists(r.Fence) {
		t.Fatal(code, err)
	}
	if str(r.outcomes()[0].(*pyjson.Object), "status") != "unknown" {
		t.Fatal("unproven writer not reported unknown")
	}
	next := New(env, io.Discard, io.Discard)
	if code, _ := next.Run([]string{"hourly"}); code != 4 || str(next.Job, "status") != "fenced" {
		t.Fatal(code, str(next.Job, "status"))
	}
	time.Sleep(2500 * time.Millisecond) // let the abandoned INSERT finish before dropping the user
}
