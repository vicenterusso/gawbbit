package datasets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

func entry(updates map[string]any) map[string]any {
	v := map[string]any{
		"id": "team_a", "endpoint_path": "/datasets/team_a/mcp", "database": "team_a", "table": "events",
		"main_exposed": true, "clickhouse_user": "rawbbit_mcp_team_a", "clickhouse_password": "dataset-db-password",
		"bearer_tokens": map[string]any{"agent": "team-a-token"},
	}
	for k, val := range updates {
		v[k] = val
	}
	return v
}

func parse(t *testing.T, doc map[string]any) (*Registry, error) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyval.DecodeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return Parse(v)
}

func registryDoc(entries ...map[string]any) map[string]any {
	list := make([]any, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	return map[string]any{"version": 1, "datasets": list}
}

func baseSettings(t *testing.T, extra map[string]string) *settings.Settings {
	t.Helper()
	values := map[string]string{"MCP_API_KEYS_JSON": `{"operator":"main-token"}`}
	for k, v := range extra {
		values[k] = v
	}
	s, err := settings.FromValues(values)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func expectValidation(t *testing.T, err error, substr string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, substr) {
		t.Fatalf("expected validation error containing %q, got %v", substr, err)
	}
}

func TestRegistryRejectsDuplicateIDsPathsAndTokens(t *testing.T) {
	e := entry(nil)
	_, err := parse(t, registryDoc(e, e))
	expectValidation(t, err, "")
	_, err = parse(t, registryDoc(e, entry(map[string]any{"id": "team_b", "endpoint_path": "/datasets/team_a/mcp"})))
	expectValidation(t, err, "")
	_, err = parse(t, registryDoc(e, entry(map[string]any{"id": "team_b", "endpoint_path": "/datasets/team_b/mcp"})))
	expectValidation(t, err, "")
	shared := entry(map[string]any{"id": "team_b", "endpoint_path": "/datasets/team_b/mcp", "database": "team_b",
		"bearer_tokens": map[string]any{"agent": "team-b-token"}})
	_, err = parse(t, registryDoc(e, shared))
	expectValidation(t, err, "distinct ClickHouse user")
}

func TestRuntimeDatasetRejectsInvalidOrDisabledConfig(t *testing.T) {
	for _, update := range []map[string]any{
		{"id": "Team-A"}, {"id": "default"}, {"id": " team_a"}, {"database": "team_a; DROP TABLE events"},
		{"clickhouse_user": "admin.user"}, {"endpoint_path": "/datasets/team_a/../mcp"}, {"endpoint_path": " /datasets/team_a/mcp"},
		{"bearer_tokens": map[string]any{}}, {"bearer_tokens": map[string]any{"team A": "token"}},
		{"bearer_tokens": map[string]any{"agent": " token "}}, {"clickhouse_password": ""}, {"enabled": false},
		{"enabled": 1}, {"unknown": true}, {"max_sample_rows": 0}, {"max_execution_seconds": 1.5},
	} {
		_, err := parse(t, registryDoc(entry(update)))
		expectValidation(t, err, "")
	}
}

func TestRuntimeRegistryRejectsDuplicatePhysicalTargetsAndNonStrictLimits(t *testing.T) {
	first := entry(nil)
	second := entry(map[string]any{"id": "team_b", "endpoint_path": "/datasets/team_b/mcp",
		"clickhouse_user": "rawbbit_mcp_team_b", "bearer_tokens": map[string]any{"agent": "team-b-token"}})
	_, err := parse(t, registryDoc(first, second))
	expectValidation(t, err, "distinct ClickHouse table")
	second["table"] = "other_events"
	_, err = parse(t, registryDoc(first, second))
	expectValidation(t, err, "distinct ClickHouse database")
	_, err = parse(t, registryDoc(entry(map[string]any{"max_query_rows": true})))
	expectValidation(t, err, "")
}

func TestRegistryRejectsMainPathCollisionAndDuplicateMainToken(t *testing.T) {
	reg, err := parse(t, registryDoc(entry(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.ValidateForSettings(baseSettings(t, nil)); err != nil {
		t.Fatal(err)
	}
	check := func(r *Registry, s *settings.Settings, substr string) {
		t.Helper()
		var ce *ConfigurationError
		if err := r.ValidateForSettings(s); !errors.As(err, &ce) || !strings.Contains(ce.Msg, substr) {
			t.Fatalf("expected %q, got %v", substr, err)
		}
	}
	dup, _ := parse(t, registryDoc(entry(map[string]any{"bearer_tokens": map[string]any{"agent": "main-token"}})))
	check(dup, baseSettings(t, nil), "distinct")
	malformed, _ := parse(t, registryDoc(entry(map[string]any{"endpoint_path": "/datasets/other/mcp"})))
	check(malformed, baseSettings(t, nil), "match its dataset id")
	check(reg, baseSettings(t, map[string]string{"CLICKHOUSE_USER": "rawbbit_mcp_team_a"}), "distinct ClickHouse users")
	check(reg, baseSettings(t, map[string]string{"CLICKHOUSE_DATABASE": "team_a"}), "distinct ClickHouse databases")
	check(reg, baseSettings(t, map[string]string{"MCP_PATH": "/other"}), "must end with the configured MCP_PATH")
}

func TestRuntimeFileRequiresRestrictedPermissionsAndDoesNotEchoSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "datasets.json")
	raw, _ := json.Marshal(registryDoc(entry(nil)))
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o640)
	if _, err := Load(path, baseSettings(t, nil)); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("got %v", err)
	}
	_ = os.Chmod(path, 0o600)
	loaded, err := Load(path, baseSettings(t, nil))
	if err != nil || loaded.Datasets[0].ID != "team_a" {
		t.Fatalf("load failed: %v", err)
	}
	for _, rendered := range []string{fmt.Sprintf("%v", loaded.Datasets), fmt.Sprintf("%+v", loaded.Datasets[0]), fmt.Sprintf("%#v", loaded.Datasets[0])} {
		if strings.Contains(rendered, "dataset-db-password") || strings.Contains(rendered, "team-a-token") {
			t.Fatalf("secret leaked: %s", rendered)
		}
	}
}

func TestMissingRuntimeFileFailsClosedWithoutRevealingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "private-datasets.json")
	_, err := Load(missing, baseSettings(t, nil))
	if err == nil || !strings.Contains(err.Error(), "invalid registry") || strings.Contains(err.Error(), missing) {
		t.Fatalf("got %v", err)
	}
	link := filepath.Join(t.TempDir(), "link.json")
	_ = os.Symlink(missing, link)
	if _, err := Load(link, baseSettings(t, nil)); err == nil || err.Error() != "MCP_DATASETS_FILE must not be a symlink" {
		t.Fatalf("got %v", err)
	}
	if _, err := Load(t.TempDir(), baseSettings(t, nil)); err == nil || err.Error() != "MCP_DATASETS_FILE must be a regular file" {
		t.Fatalf("got %v", err)
	}
	if reg, err := Load("  ", baseSettings(t, nil)); err != nil || len(reg.Datasets) != 0 {
		t.Fatalf("blank path: %v", err)
	}
}
