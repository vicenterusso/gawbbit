//go:build integration

// Differential parity suite. It replays scenarios against a real ClickHouse
// and compares exit codes, stdout/stderr, the ownership journal, every SQL
// statement the provisioner issued (from system.query_log) and the resulting
// access-control state with golden output recorded from the original Python
// provision_datasets.py.
//
//	PROVISION_PARITY_CLICKHOUSE=http://admin:adminpw@127.0.0.1:38123 \
//	  go test -tags integration ./cmd/rawbbit-provision-datasets
//
// Set PROVISION_PARITY_RECORD="python3 driver.py" to regenerate the goldens
// from another implementation (the driver must accept the same CLI).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/provision"
)

type step struct {
	Args   []string                   `json:"args"`
	Before []string                   `json:"before,omitempty"`
	Files  map[string]json.RawMessage `json:"files,omitempty"`
	Modes  map[string]string          `json:"modes,omitempty"`
}

type scenario struct {
	Name  string                     `json:"name"`
	Setup []string                   `json:"setup,omitempty"`
	Files map[string]json.RawMessage `json:"files"`
	Modes map[string]string          `json:"modes,omitempty"`
	Steps []step                     `json:"steps"`
}

type stepResult struct {
	Exit    int        `json:"exit"`
	Stdout  string     `json:"stdout"`
	Stderr  string     `json:"stderr"`
	Journal *string    `json:"journal"`
	SQL     [][]string `json:"sql"`
}

type outcome struct {
	Steps    []stepResult `json:"steps"`
	Snapshot []string     `json:"snapshot"`
}

type clickhouse struct {
	base *url.URL
	t    *testing.T
}

func (c *clickhouse) exec(sql string) string {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.base.String(), strings.NewReader(sql))
	req.Header.Set("User-Agent", "rawbbit-parity-harness")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("clickhouse: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("clickhouse %q: %s", sql, body)
	}
	return string(body)
}

func (c *clickhouse) reset() {
	for _, db := range strings.Fields(c.exec("SELECT name FROM system.databases WHERE name LIKE 'team_%' FORMAT TSV")) {
		c.exec("DROP DATABASE `" + db + "` SYNC")
	}
	for _, u := range strings.Fields(c.exec("SELECT name FROM system.users WHERE name LIKE 'rawbbit%' OR name LIKE 'ref_%' FORMAT TSV")) {
		c.exec("DROP USER `" + u + "`")
	}
	for _, r := range strings.Fields(c.exec("SELECT name FROM system.roles WHERE name LIKE 'rawbbit%' OR name LIKE 'ref_%' FORMAT TSV")) {
		c.exec("DROP ROLE `" + r + "`")
	}
	c.exec("CREATE DATABASE IF NOT EXISTS analytics")
	c.exec("CREATE USER rawbbit_mcp IDENTIFIED WITH sha256_password BY 'mainpw'")
	c.exec("GRANT SELECT ON analytics.* TO rawbbit_mcp")
}

var snapshotQueries = []string{
	"SELECT name, engine, toString(uuid) FROM system.databases WHERE name LIKE 'team_%' ORDER BY name",
	"SELECT database, name, engine, toString(uuid), partition_key, sorting_key FROM system.tables WHERE database LIKE 'team_%' ORDER BY database, name",
	"SELECT database, table, count() FROM system.columns WHERE database LIKE 'team_%' GROUP BY database, table ORDER BY database, table",
	"SELECT name, toString(id), default_roles_all, default_roles_list, default_roles_except FROM system.users WHERE name LIKE 'rawbbit%' OR name LIKE 'ref_%' ORDER BY name",
	"SELECT name, toString(id) FROM system.roles WHERE name LIKE 'rawbbit%' OR name LIKE 'ref_%' ORDER BY name",
	"SELECT * FROM system.grants WHERE user_name LIKE 'rawbbit%' OR role_name LIKE 'rawbbit%' OR user_name LIKE 'ref_%' OR role_name LIKE 'ref_%' ORDER BY ALL",
	"SELECT * FROM system.role_grants WHERE user_name LIKE 'rawbbit%' OR role_name LIKE 'rawbbit%' OR user_name LIKE 'ref_%' ORDER BY ALL",
	"SELECT user_name, setting_name, value FROM system.settings_profile_elements WHERE user_name LIKE 'rawbbit%' ORDER BY ALL",
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// normalizer maps UUIDs to stable placeholders by first appearance.
type normalizer struct{ seen map[string]string }

func (n *normalizer) apply(s string) string {
	return uuidRe.ReplaceAllStringFunc(s, func(u string) string {
		if u == "00000000-0000-0000-0000-000000000000" {
			return u
		}
		if _, ok := n.seen[u]; !ok {
			n.seen[u] = fmt.Sprintf("<uuid-%d>", len(n.seen)+1)
		}
		return n.seen[u]
	})
}

var tokenRe = regexp.MustCompile(`@(dbuuid|tableuuid):([A-Za-z0-9_.]+)|@fingerprint`)

// substitute resolves @dbuuid:<db>, @tableuuid:<db>.<table> and @fingerprint.
func (c *clickhouse) substitute(s, fingerprint string) string {
	return tokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		m := tokenRe.FindStringSubmatch(tok)
		switch m[1] {
		case "dbuuid":
			return strings.TrimSpace(c.exec("SELECT toString(uuid) FROM system.databases WHERE name = '" + m[2] + "' FORMAT TSV"))
		case "tableuuid":
			db, table, _ := strings.Cut(m[2], ".")
			return strings.TrimSpace(c.exec("SELECT toString(uuid) FROM system.tables WHERE database = '" + db + "' AND name = '" + table + "' FORMAT TSV"))
		}
		return fingerprint
	})
}

// writeFiles materializes scenario files: a JSON string is written as text
// ("@canonical" is the canonical schema), null deletes the file and any other
// JSON value is written verbatim.
func writeFiles(t *testing.T, ch *clickhouse, dir string, files map[string]json.RawMessage, modes map[string]string, schemaSource string) {
	schema, err := provision.ParseCanonicalSchema(filepath.Join("..", "..", "..", "schema_analytics_events.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		data := []byte(content)
		var text string
		switch {
		case string(content) == "null":
			_ = os.Remove(path)
			continue
		case json.Unmarshal(content, &text) == nil:
			data = []byte(text)
			if text == "@canonical" {
				data = []byte(schemaSource)
			}
		}
		data = []byte(ch.substitute(string(data), schema.Fingerprint))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o600)
		if m, ok := modes[name]; ok {
			var parsed uint32
			fmt.Sscanf(m, "%o", &parsed)
			mode = os.FileMode(parsed)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
}

func runScenario(t *testing.T, ch *clickhouse, sc scenario, python []string, schemaSource string) outcome {
	ch.reset()
	for _, sql := range sc.Setup {
		ch.exec(sql)
	}
	dir := t.TempDir()
	writeFiles(t, ch, dir, sc.Files, sc.Modes, schemaSource)
	norm := &normalizer{seen: map[string]string{}}
	var out outcome
	for _, st := range sc.Steps {
		for _, sql := range st.Before {
			ch.exec(sql)
		}
		writeFiles(t, ch, dir, st.Files, st.Modes, schemaSource)
		args := make([]string, len(st.Args))
		for i, a := range st.Args {
			args[i] = strings.ReplaceAll(a, "@dir", dir)
		}
		start := strings.TrimSpace(ch.exec("SELECT toString(now64(6))"))
		var stdout, stderr bytes.Buffer
		var code int
		if python != nil {
			cmd := exec.Command(python[0], append(python[1:], args...)...)
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
		} else {
			code = run("provision_datasets.py", args, &stdout, &stderr, provision.Options{RequireRoot: false})
		}
		ch.exec("SYSTEM FLUSH LOGS")
		logRows := ch.exec("SELECT user, query FROM system.query_log WHERE event_time_microseconds >= toDateTime64('" + start +
			"', 6) AND type IN ('QueryStart', 'ExceptionBeforeStart') AND http_user_agent != 'rawbbit-parity-harness' " +
			"AND query NOT LIKE '%system.query_log%' ORDER BY event_time_microseconds, query_id FORMAT JSONCompactEachRow")
		res := stepResult{Exit: code, Stdout: stdout.String(), Stderr: stderr.String(), SQL: [][]string{}}
		for _, line := range strings.Split(strings.TrimSpace(logRows), "\n") {
			if line == "" {
				continue
			}
			var row []string
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatal(err)
			}
			row[1] = norm.apply(row[1])
			res.SQL = append(res.SQL, row)
		}
		if data, err := os.ReadFile(filepath.Join(dir, "journal.json")); err == nil {
			s := norm.apply(string(data))
			res.Journal = &s
		}
		out.Steps = append(out.Steps, res)
	}
	for _, q := range snapshotQueries {
		out.Snapshot = append(out.Snapshot, norm.apply(ch.exec(q+" FORMAT TSV")))
	}
	return out
}

func TestProvisionerParity(t *testing.T) {
	base := os.Getenv("PROVISION_PARITY_CLICKHOUSE")
	if base == "" {
		t.Skip("set PROVISION_PARITY_CLICKHOUSE=http://user:password@host:port to run the parity suite")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	ch := &clickhouse{base: u, t: t}
	schema, err := os.ReadFile(filepath.Join("..", "..", "..", "schema_analytics_events.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var python []string
	if rec := os.Getenv("PROVISION_PARITY_RECORD"); rec != "" {
		python = strings.Fields(rec)
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "parity", "scenarios", "*.json"))
	if len(files) == 0 {
		t.Fatal("no parity scenarios found")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var sc scenario
		if err := json.Unmarshal(data, &sc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		t.Run(sc.Name, func(t *testing.T) {
			ch.t = t
			golden := filepath.Join("testdata", "parity", "golden", filepath.Base(file))
			got := runScenario(t, ch, sc, python, string(schema))
			encoded, _ := json.MarshalIndent(got, "", "  ")
			if python != nil {
				if err := os.WriteFile(golden, append(encoded, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden %s: %v", golden, err)
			}
			if strings.TrimSpace(string(want)) != strings.TrimSpace(string(encoded)) {
				diffPath := filepath.Join(t.TempDir(), "got.json")
				_ = os.WriteFile(diffPath, encoded, 0o644)
				t.Fatalf("parity mismatch for %s (got written to %s)\n%s", sc.Name, diffPath, firstDiff(string(want), string(encoded)))
			}
		})
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return fmt.Sprintf("line %d:\n- %s\n+ %s", i+1, al[i], bl[i])
		}
	}
	return fmt.Sprintf("length differs: %d vs %d lines", len(al), len(bl))
}
