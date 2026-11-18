package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/clickhouse"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/datasets"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

// recordingGateway mirrors the Python RecordingGateway test double.
type recordingGateway struct {
	cfg *settings.Settings
	rec *recorder
}

type call struct{ database, sql string }

type recorder struct {
	mu    sync.Mutex
	calls []call
}

func (r *recorder) all() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]call(nil), r.calls...)
}

func (g *recordingGateway) QueryRows(_ context.Context, sql string) ([]*pyval.Dict, error) {
	g.rec.mu.Lock()
	g.rec.calls = append(g.rec.calls, call{g.cfg.ClickHouseDatabase, sql})
	g.rec.mu.Unlock()
	row := pyval.NewDict(2)
	row.Set("ok", pyval.Int(1))
	row.Set("database", g.cfg.ClickHouseDatabase)
	return []*pyval.Dict{row}, nil
}

func (r *recorder) factory() GatewayFactory {
	return func(cfg *settings.Settings) clickhouse.Querier { return &recordingGateway{cfg: cfg, rec: r} }
}

func testSettings(t *testing.T) *settings.Settings {
	t.Helper()
	s, err := settings.FromValues(map[string]string{
		"MCP_API_KEYS_JSON":   `{"operator":"main-token","analyst":"analyst-token"}`,
		"CLICKHOUSE_DATABASE": "analytics",
		"CLICKHOUSE_TABLE":    "events",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testRegistry(t *testing.T) *datasets.Registry {
	t.Helper()
	doc := `{"version":1,"datasets":[
	 {"id":"team_a","endpoint_path":"/datasets/team_a/mcp","database":"team_a","table":"events","main_exposed":true,
	  "clickhouse_user":"rawbbit_mcp_team_a","clickhouse_password":"team-a-db-secret","bearer_tokens":{"agent":"team-a-token"}},
	 {"id":"team_b","endpoint_path":"/datasets/team_b/mcp","database":"team_b","table":"events","main_exposed":false,
	  "clickhouse_user":"rawbbit_mcp_team_b","clickhouse_password":"team-b-db-secret","bearer_tokens":{"agent":"team-b-token"}}]}`
	v, err := pyval.DecodeJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := datasets.Parse(v)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startApp(t *testing.T, cfg *settings.Settings, reg *datasets.Registry, factory GatewayFactory) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	log, err := NewLogger(logs, "INFO")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(cfg, reg, factory, log))
	t.Cleanup(srv.Close)
	return srv, logs
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// connect opens an MCP client session with a bearer token.
func connect(t *testing.T, base, path, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "dataset-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             base + path,
		HTTPClient:           &http.Client{Transport: bearer{token, http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", path, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func toolsByName(t *testing.T, s *mcp.ClientSession) (map[string]*mcp.Tool, []string) {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	var names []string
	for _, tool := range res.Tools {
		out[tool.Name] = tool
		names = append(names, tool.Name)
	}
	return out, names
}

func hasProperty(t *testing.T, tool *mcp.Tool, name string) bool {
	t.Helper()
	raw, _ := json.Marshal(tool.InputSchema)
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	_, ok := schema.Properties[name]
	return ok
}

func callTool(t *testing.T, s *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func resultText(r *mcp.CallToolResult) string {
	raw, _ := json.Marshal(r)
	return string(raw)
}

func rawPost(t *testing.T, url, token string, body string, extra map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestEmptyRegistryKeepsDefaultEndpointToolSchema(t *testing.T) {
	rec := &recorder{}
	srv, _ := startApp(t, testSettings(t), datasets.Empty(), rec.factory())
	tools, names := toolsByName(t, connect(t, srv.URL, "/mcp", "main-token"))
	want := []string{"healthcheck", "table_overview", "list_event_names", "discover_json_keys", "sample_events",
		"run_readonly_sql", "calculate_dau", "calculate_funnel"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools %v", names)
	}
	if hasProperty(t, tools["run_readonly_sql"], "dataset_id") {
		t.Fatal("default endpoint must not expose dataset_id")
	}
}

func TestMainDiscoveryAndScopedEndpointsHaveDistinctAuthAndTools(t *testing.T) {
	rec := &recorder{}
	srv, _ := startApp(t, testSettings(t), testRegistry(t), rec.factory())
	for _, c := range [][2]string{{"/mcp", "team-a-token"}, {"/datasets/team_a/mcp", "main-token"},
		{"/datasets/team_a/mcp", "team-b-token"}, {"/datasets/team_b/mcp", "team-a-token"}} {
		resp := rawPost(t, srv.URL+c[0], c[1], "{}", nil)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusUnauthorized || string(body) != `{"error":"Unauthorized"}` ||
			resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%v: %d %s", c, resp.StatusCode, body)
		}
	}
	main := connect(t, srv.URL, "/mcp", "main-token")
	second := connect(t, srv.URL, "/mcp", "analyst-token")
	mainTools, _ := toolsByName(t, main)
	if mainTools["list_datasets"] == nil || !hasProperty(t, mainTools["run_readonly_sql"], "dataset_id") {
		t.Fatal("main endpoint must expose list_datasets and dataset_id")
	}
	listed := resultText(callTool(t, main, "list_datasets", map[string]any{}))
	if !strings.Contains(listed, "default") || !strings.Contains(listed, "team_a") || strings.Contains(listed, "team_b") {
		t.Fatalf("list_datasets: %s", listed)
	}
	if again := resultText(callTool(t, second, "list_datasets", map[string]any{})); again != listed {
		t.Fatalf("second session differs: %s", again)
	}
	scoped := connect(t, srv.URL, "/datasets/team_a/mcp", "team-a-token")
	scopedTools, _ := toolsByName(t, scoped)
	if scopedTools["list_datasets"] != nil || hasProperty(t, scopedTools["run_readonly_sql"], "dataset_id") {
		t.Fatal("scoped endpoint must not expose dataset selection")
	}
	callTool(t, main, "run_readonly_sql", map[string]any{"sql": "SELECT 1", "dataset_id": "team_a"})
	callTool(t, scoped, "run_readonly_sql", map[string]any{"sql": "SELECT 1"})
	calls := rec.all()
	if calls[len(calls)-2].database != "team_a" || calls[len(calls)-1].database != "team_a" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestMainRejectsNonExposedDatasetWithoutQueryFallback(t *testing.T) {
	rec := &recorder{}
	srv, _ := startApp(t, testSettings(t), testRegistry(t), rec.factory())
	res := callTool(t, connect(t, srv.URL, "/mcp", "main-token"), "run_readonly_sql", map[string]any{"sql": "SELECT 1", "dataset_id": "team_b"})
	if !res.IsError || len(rec.all()) != 0 {
		t.Fatalf("expected isError without queries: %s %v", resultText(res), rec.all())
	}
	if text := res.Content[0].(*mcp.TextContent).Text; text != "Error calling tool 'run_readonly_sql': dataset_id is unknown, disabled, or not exposed on the main MCP endpoint" {
		t.Fatalf("text %q", text)
	}
}

func TestDiscoverJSONKeysCoercesNullableJSONColumnToNonNullableString(t *testing.T) {
	// Ingested JSON columns are Nullable(String). ClickHouse rejects
	// JSONExtractKeys on a Nullable argument, so every branch must be non-nullable.
	rec := &recorder{}
	srv, _ := startApp(t, testSettings(t), testRegistry(t), rec.factory())
	res := callTool(t, connect(t, srv.URL, "/datasets/team_a/mcp", "team-a-token"), "discover_json_keys", map[string]any{"json_column": "event_params_json"})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	calls := rec.all()
	sql := strings.Join(strings.Fields(calls[len(calls)-1].sql), " ")
	if !strings.Contains(sql, "JSONExtractKeys(ifNull(nullIf(event_params_json, ''), '{}'))") || strings.Contains(sql, "'{}', event_params_json)") {
		t.Fatalf("sql %s", sql)
	}
}

func TestToolAuditLogsSafeMetadataWithoutArgumentsOrCredentials(t *testing.T) {
	rec := &recorder{}
	srv, logs := startApp(t, testSettings(t), testRegistry(t), rec.factory())
	marker := "private-sql-marker"
	callTool(t, connect(t, srv.URL, "/datasets/team_a/mcp", "team-a-token"), "run_readonly_sql", map[string]any{"sql": "SELECT '" + marker + "'"})
	var records []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "tool_call") {
			records = append(records, line)
		}
	}
	found := false
	for _, m := range records {
		if strings.Contains(m, "dataset_id=team_a") && strings.Contains(m, "endpoint=scoped") && strings.Contains(m, "tool=run_readonly_sql") &&
			strings.Contains(m, "actor=agent") && strings.Contains(m, "result=ok") && strings.Contains(m, "duration_ms=") {
			found = true
		}
	}
	joined := strings.Join(records, "\n")
	if !found || strings.Contains(joined, marker) || strings.Contains(joined, "team-a-token") || strings.Contains(joined, "team-a-db-secret") {
		t.Fatalf("audit records:\n%s", joined)
	}
}

func TestSessionsAndConcurrentCallsDoNotCrossDatasetContext(t *testing.T) {
	rec := &recorder{}
	srv, _ := startApp(t, testSettings(t), testRegistry(t), rec.factory())
	teamA := connect(t, srv.URL, "/datasets/team_a/mcp", "team-a-token")
	connect(t, srv.URL, "/datasets/team_b/mcp", "team-b-token")
	resp := rawPost(t, srv.URL+"/datasets/team_b/mcp", "team-b-token", `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`,
		map[string]string{"Mcp-Session-Id": teamA.ID()})
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reused session status %d", resp.StatusCode)
	}
	results := make([]string, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path, token := "/datasets/team_a/mcp", "team-a-token"
			if i%2 == 1 {
				path, token = "/datasets/team_b/mcp", "team-b-token"
			}
			s := connect(t, srv.URL, path, token)
			results[i] = resultText(callTool(t, s, "run_readonly_sql", map[string]any{"sql": "SELECT 1"}))
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		want := "team_a"
		if i%2 == 1 {
			want = "team_b"
		}
		if !strings.Contains(r, want) || strings.Contains(r, "db-secret") {
			t.Fatalf("result %d: %s", i, r)
		}
	}
	seen := map[string]bool{}
	for _, c := range rec.all() {
		seen[c.database] = true
	}
	if fmt.Sprint(len(seen)) != "2" || !seen["team_a"] || !seen["team_b"] {
		t.Fatalf("databases %v", seen)
	}
}
