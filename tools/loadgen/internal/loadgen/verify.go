package loadgen

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Count is what ClickHouse holds for one event_name of a run.
type Count struct {
	Rows   int
	Unique int
}

// Querier runs the verification query and returns rows as maps.
type Querier interface {
	Query(ctx context.Context, sql string) ([]map[string]any, error)
}

var safeIdent = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)

// VerifySQL counts rows and distinct event_ids per event_name for a run.
// Values are inlined (MCP's run_readonly_sql has no parameters) and must
// match safeIdent.
func VerifySQL(table string, m Manifest) (string, error) {
	for _, v := range []string{table, m.AppID, m.RunID} {
		if !safeIdent.MatchString(v) {
			return "", fmt.Errorf("unsafe identifier %q", v)
		}
	}
	dateFrom := m.WindowStart.UTC().Add(-24 * time.Hour).Format("2006-01-02")
	dateTo := m.WindowEnd.UTC().Add(24 * time.Hour).Format("2006-01-02")
	return fmt.Sprintf(`SELECT event_name, toUInt32(count()) AS rows, toUInt32(uniqExact(event_id)) AS uniq
FROM %s
WHERE app_id = '%s'
  AND event_date BETWEEN toDate('%s') AND toDate('%s')
  AND JSONExtractString(ifNull(event_params_json, ''), '%s') = '%s'
GROUP BY event_name
ORDER BY event_name`, table, m.AppID, dateFrom, dateTo, RunParam, m.RunID), nil
}

// ParseCounts turns query rows into per-event_name counts.
func ParseCounts(rows []map[string]any) (map[string]Count, error) {
	out := map[string]Count{}
	for _, r := range rows {
		name, _ := r["event_name"].(string)
		rowsN, err1 := toInt(r["rows"])
		uniq, err2 := toInt(r["uniq"])
		if name == "" || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("unexpected row %v", r)
		}
		out[name] = Count{Rows: rowsN, Unique: uniq}
	}
	return out, nil
}

func toInt(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		return int(x), nil
	case json.Number:
		n, err := x.Int64()
		return int(n), err
	case string:
		return strconv.Atoi(x)
	}
	return 0, fmt.Errorf("not a number: %v", v)
}

// Diff compares expected counts with what ClickHouse holds. It returns one
// line per problem; empty means every accepted event is stored exactly once
// and nothing else carries the run tag.
func Diff(expected map[string]int, got map[string]Count) []string {
	names := map[string]bool{}
	for n := range expected {
		names[n] = true
	}
	for n := range got {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	var problems []string
	for _, n := range sorted {
		want, g := expected[n], got[n]
		switch {
		case g.Unique < want:
			problems = append(problems, fmt.Sprintf("%s: %d of %d events missing", n, want-g.Unique, want))
		case g.Unique > want:
			problems = append(problems, fmt.Sprintf("%s: %d unexpected events (rejected or unaccounted)", n, g.Unique-want))
		}
		if g.Rows > g.Unique {
			problems = append(problems, fmt.Sprintf("%s: %d duplicate rows", n, g.Rows-g.Unique))
		}
	}
	return problems
}

func httpClient(insecure bool, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for self-signed test stacks
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// ClickHouse queries the ClickHouse HTTP interface.
type ClickHouse struct {
	URL, User, Password, Host string
	HTTP                      *http.Client
}

// NewClickHouse creates a ClickHouse HTTP querier.
func NewClickHouse(rawURL, user, password, host string, insecure bool) *ClickHouse {
	return &ClickHouse{URL: rawURL, User: user, Password: password, Host: host, HTTP: httpClient(insecure, 60*time.Second)}
}

// Query implements Querier.
func (c *ClickHouse) Query(ctx context.Context, sql string) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, strings.NewReader(sql+"\nFORMAT JSON"))
	if err != nil {
		return nil, err
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	if c.Host != "" {
		req.Host = c.Host
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("clickhouse response: %w", err)
	}
	return out.Data, nil
}

// MCP queries through the Rawbbit MCP server's run_readonly_sql tool, so
// verification also exercises the public MCP endpoint.
type MCP struct {
	URL, Token, Host string
	HTTP             *http.Client
	session          string
	nextID           int
}

// NewMCP creates an MCP querier.
func NewMCP(rawURL, token, host string, insecure bool) *MCP {
	return &MCP{URL: rawURL, Token: token, Host: host, HTTP: httpClient(insecure, 60*time.Second)}
}

func (m *MCP) rpc(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if !notify {
		m.nextID++
		msg["id"] = m.nextID
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if m.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.Token)
	}
	if m.session != "" {
		req.Header.Set("Mcp-Session-Id", m.session)
	}
	if m.Host != "" {
		req.Host = m.Host
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		m.session = sid
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp %s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if notify {
		return nil, nil
	}
	payload := b
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		payload = nil
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				payload = []byte(strings.TrimSpace(d))
			}
		}
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("mcp %s: %w", method, err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("mcp %s: %s", method, env.Error.Message)
	}
	return env.Result, nil
}

func (m *MCP) connect(ctx context.Context) error {
	if m.session != "" {
		return nil
	}
	_, err := m.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "rawbbit-loadgen", "version": "1"},
	}, false)
	if err != nil {
		return err
	}
	_, err = m.rpc(ctx, "notifications/initialized", nil, true)
	return err
}

// Query implements Querier.
func (m *MCP) Query(ctx context.Context, sql string) ([]map[string]any, error) {
	if err := m.connect(ctx); err != nil {
		return nil, err
	}
	raw, err := m.rpc(ctx, "tools/call", map[string]any{
		"name":      "run_readonly_sql",
		"arguments": map[string]any{"sql": sql, "limit": 1000},
	}, false)
	if err != nil {
		m.session = "" // reconnect next time, e.g. after an MCP restart
		return nil, err
	}
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Structured *struct {
			Result []map[string]any `json:"result"`
		} `json:"structuredContent"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		return nil, err
	}
	if res.IsError {
		msg := "tool error"
		if len(res.Content) > 0 {
			msg = res.Content[0].Text
		}
		return nil, errors.New("run_readonly_sql: " + msg)
	}
	if res.Structured != nil {
		return res.Structured.Result, nil
	}
	return nil, errors.New("run_readonly_sql returned no structuredContent")
}

// MCPURL appends the MCP path when rawURL has none.
func MCPURL(rawURL, path string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return rawURL
	}
	return strings.TrimRight(rawURL, "/") + path
}
