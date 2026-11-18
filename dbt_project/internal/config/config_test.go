package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
)

// TestSnapshotMatchesPythonGolden replays snapshot() results (revision or
// exact error message) recorded from the original runner over a mutated
// corpus of route snapshot documents.
func TestSnapshotMatchesPythonGolden(t *testing.T) {
	data, err := os.ReadFile("../../testdata/unit/snapshots.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Raw      string            `json:"raw"`
		Pad      int               `json:"pad"`
		Env      map[string]string `json:"env"`
		Revision string            `json:"revision"`
		Document string            `json:"document"`
		Error    string            `json:"error"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.json")
	for i, c := range cases {
		if err := os.WriteFile(path, []byte(c.Raw+strings.Repeat(" ", c.Pad)), 0o600); err != nil {
			t.Fatal(err)
		}
		env := Env{"RAWBBIT_DBT_ROUTING_ENABLED": "1", "RAWBBIT_RAW_LOAD_MODE": "dbt",
			"CLICKHOUSE_DATABASE": "default_custom", "RAWBBIT_DBT_ROUTES_FILE": path}
		for k, v := range c.Env {
			env[k] = strings.ReplaceAll(v, "/missing.json", "")
			if strings.HasSuffix(v, "/missing.json") {
				env[k] = filepath.Join(dir, "missing.json")
			}
		}
		snap, err := Load(env)
		if c.Error != "" {
			if cfg, ok := AsError(err); !ok || cfg.Msg != c.Error {
				t.Errorf("case %d %.80q: error %v, want %q", i, c.Raw, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d %.80q: unexpected %v", i, c.Raw, err)
			continue
		}
		if snap.Revision != c.Revision || pyjson.Encode(snap.Document, pyjson.EncodeOptions{SortKeys: true}) != c.Document {
			t.Errorf("case %d: revision/document mismatch\n%s\n%s", i, pyjson.Encode(snap.Document, pyjson.EncodeOptions{SortKeys: true}), c.Document)
		}
	}
}

func TestDisabledIgnoresMissingSnapshotAndPositiveBounds(t *testing.T) {
	snap, err := Load(Env{"RAWBBIT_DBT_ROUTES_FILE": "/nonexistent"})
	if err != nil || snap.Enabled || snap.Default != "analytics" || len(snap.Routes) != 0 {
		t.Fatal(snap, err)
	}
	for value, ok := range map[string]bool{"1": true, "300": true, "7200": true, "7201": false, "0": false,
		"01": false, "+1": false, " 1": false, "": false, "abc": false, "99999999999999999999": false} {
		_, err := Positive(Env{"K": value}, "K", 1, 7200)
		if (err == nil) != ok {
			t.Errorf("%q: %v", value, err)
		}
		if err != nil && err.Error() != "invalid bounded positive integer: K" {
			t.Error(err)
		}
	}
	if n, err := Positive(Env{}, "K", 300, 7200); n != 300 || err != nil {
		t.Error(n, err)
	}
}
