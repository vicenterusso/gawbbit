package provision

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "..")
}

func schemaPath(t *testing.T) string {
	return filepath.Join(repoRoot(t), "quickstart/vm_rawbbit_two/clickhouse/schema_analytics_events.sql")
}

func mustSchema(t *testing.T) *CanonicalSchema {
	t.Helper()
	s, err := ParseCanonicalSchema(schemaPath(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// obj builds a *pyjson.Dict from alternating key/value pairs.
func obj(kv ...any) *pyjson.Dict {
	d := pyjson.NewDict()
	for i := 0; i < len(kv); i += 2 {
		d.Set(kv[i].(string), conv(kv[i+1]))
	}
	return d
}

func conv(v any) pyjson.Value {
	switch t := v.(type) {
	case int:
		return pyjson.Int(int64(t))
	case []any:
		out := make(pyjson.List, len(t))
		for i, item := range t {
			out[i] = conv(item)
		}
		return out
	case []string:
		out := make(pyjson.List, len(t))
		for i, item := range t {
			out[i] = item
		}
		return out
	}
	return v
}

func update(d *pyjson.Dict, kv ...any) *pyjson.Dict {
	for i := 0; i < len(kv); i += 2 {
		d.Set(kv[i].(string), conv(kv[i+1]))
	}
	return d
}

func baseDataset(kv ...any) *pyjson.Dict {
	return update(obj(
		"id", "team_a", "enabled", true, "mode", "managed", "endpoint_path", "/datasets/team_a/mcp",
		"database", "team_a", "table", "events", "create_table", true, "main_exposed", true,
		"mcp_user", "rawbbit_mcp_team_a", "mcp_role", "rawbbit_role_team_a",
		"mcp_password", "synthetic-clickhouse-password", "password_revision", 1,
		"limits", obj("max_query_rows", 500, "max_sample_rows", 50, "max_execution_seconds", 30),
	), kv...)
}

func baseRuntime(kv ...any) *pyjson.Dict {
	return update(obj(
		"id", "team_a", "enabled", true, "endpoint_path", "/datasets/team_a/mcp", "database", "team_a",
		"table", "events", "main_exposed", true, "clickhouse_user", "rawbbit_mcp_team_a",
		"clickhouse_password", "synthetic-clickhouse-password",
		"bearer_tokens", obj("agent", "synthetic-bearer-token"),
		"max_query_rows", 500, "max_sample_rows", 50, "max_execution_seconds", 30,
	), kv...)
}

func baseRegistry(kv ...any) *pyjson.Dict {
	return obj(
		"version", 1, "main_database", "analytics", "main_user", "rawbbit_mcp",
		"admin", obj("host", "127.0.0.1", "port", 8123, "username", "admin",
			"password", "synthetic-admin-password", "timeout_seconds", 15),
		"datasets", pyjson.List{baseDataset(kv...)},
	)
}

func runtimeDoc(datasets ...*pyjson.Dict) *pyjson.Dict {
	list := pyjson.List{}
	for _, d := range datasets {
		list = append(list, d)
	}
	return obj("version", 1, "datasets", list)
}

func expectError(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error matching %q", pattern)
	}
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("expected ProvisionError, got %T: %v", err, err)
	}
	if pattern != "" && !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("error %q does not match %q", err, pattern)
	}
}

func validatedDatasets(t *testing.T, config *pyjson.Dict) []*Dataset {
	t.Helper()
	_, datasets, err := ValidateRegistry(config)
	if err != nil {
		t.Fatal(err)
	}
	return datasets
}

func validatedRuntime(t *testing.T, doc *pyjson.Dict) map[string]RuntimeDataset {
	t.Helper()
	byID, _, err := ValidateRuntime(doc, "analytics")
	if err != nil {
		t.Fatal(err)
	}
	return byID
}

var testAdmin = &AdminConnection{Host: "127.0.0.1", Port: 8123, Username: "admin", Password: "synthetic-password", TimeoutSeconds: 10}

// fakeClient answers metadata queries through a function and records DDL.
type fakeClient struct {
	query      func(sql string) []*pyjson.Dict
	executions []string
	execute    func(sql string)
}

func (f *fakeClient) Query(sql string) ([]*pyjson.Dict, error) {
	if f.query == nil {
		panic("unexpected metadata query: " + sql)
	}
	return f.query(sql), nil
}

func (f *fakeClient) Execute(sql string) error {
	f.executions = append(f.executions, sql)
	if f.execute != nil {
		f.execute(sql)
	}
	return nil
}

// noClient fails the test on any ClickHouse access (Python's object()).
type noClient struct{ t *testing.T }

func (n noClient) Query(sql string) ([]*pyjson.Dict, error) {
	n.t.Fatalf("unexpected query: %s", sql)
	return nil, nil
}

func (n noClient) Execute(sql string) error {
	n.t.Fatalf("unexpected execute: %s", sql)
	return nil
}

func newProvisioner(t *testing.T, client Client, journal *pyjson.Dict) *Provisioner {
	t.Helper()
	return NewProvisioner(client, testAdmin, mustSchema(t), filepath.Join(t.TempDir(), "ownership.json"), journal, false, "rawbbit_mcp")
}

func normalizedTableDDL(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?is)CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+analytics\.events\s*.*?ORDER\s+BY\s*\(.*?\)(?:\s*;|\s*\z)`)
	m := re.FindString(string(data))
	if m == "" {
		t.Fatalf("canonical analytics.events DDL is missing from %s", path)
	}
	return strings.ToLower(strings.TrimRight(regexp.MustCompile(`\s+`).ReplaceAllString(m, ""), ";"))
}

func TestCanonicalManagedSchemaAndReferenceCopyStayInSync(t *testing.T) {
	parsed := mustSchema(t)
	if len(parsed.Columns) != 26 || parsed.Engine != "MergeTree" || parsed.Fingerprint == "" {
		t.Fatalf("unexpected schema: %d columns, engine %q", len(parsed.Columns), parsed.Engine)
	}
	reference := filepath.Join(repoRoot(t), "clickhouse/schema__analytics_events.sql")
	if normalizedTableDDL(t, schemaPath(t)) != normalizedTableDDL(t, reference) {
		t.Fatal("managed schema and reference copy differ")
	}
}

func TestRegistryRejectsUnsafeOrInconsistentConfig(t *testing.T) {
	cases := [][]any{
		{"id", "Team-A"},
		{"database", "team_a; DROP TABLE events"},
		{"endpoint_path", "/datasets/team_a/../mcp"},
		{"endpoint_path", "/datasets/team_a/mcp?admin=1"},
		{"mcp_user", "mcp.team_a"},
		{"mode", "managed", "create_table", true, "mcp_password", ""},
		{"mode", "reference", "create_table", true},
	}
	for _, c := range cases {
		config := baseRegistry(c...)
		if c[1] == "reference" {
			ds := config.Lookup("datasets").(pyjson.List)[0].(*pyjson.Dict)
			d2 := pyjson.NewDict()
			for _, k := range ds.Keys() {
				if k != "mcp_password" {
					d2.Set(k, ds.Lookup(k))
				}
			}
			config.Set("datasets", pyjson.List{d2})
		}
		_, _, err := ValidateRegistry(config)
		expectError(t, err, "")
	}
}

func TestRegistryRefusesNonLoopbackAdminAndUnknownFields(t *testing.T) {
	config := baseRegistry()
	config.Lookup("admin").(*pyjson.Dict).Set("host", "clickhouse.example.com")
	_, _, err := ValidateRegistry(config)
	expectError(t, err, "localhost")

	config = baseRegistry()
	config.Lookup("datasets").(pyjson.List)[0].(*pyjson.Dict).Set("admin_password", "must-not-be-accepted")
	_, _, err = ValidateRegistry(config)
	expectError(t, err, "unsupported fields")

	invalid := []*pyjson.Dict{
		update(baseRegistry(), "unexpected", true),
		update(baseRegistry(), "version", true),
		baseRegistry("limits", obj("max_rowz", 100)),
		baseRegistry("adopt", obj("admin_password", "synthetic-secret")),
		baseRegistry("mode", obj("managed", true)),
	}
	for _, c := range invalid {
		_, _, err := ValidateRegistry(c)
		expectError(t, err, "")
	}
}

func TestRegistryRejectsDatasetDatabaseSharedWithMainEndpoint(t *testing.T) {
	_, _, err := ValidateRegistry(baseRegistry("database", "analytics"))
	expectError(t, err, "differ from the main ClickHouse database")
}

func TestRuntimeRejectsDuplicateTokensAndMismatchedFields(t *testing.T) {
	_, _, err := ValidateRuntime(runtimeDoc(
		baseRuntime(),
		baseRuntime("id", "team_b", "endpoint_path", "/datasets/team_b/mcp", "database", "team_b",
			"clickhouse_user", "rawbbit_mcp_team_b", "bearer_tokens", obj("agent", "synthetic-bearer-token")),
	), "analytics")
	expectError(t, err, "unique")

	_, _, err = ValidateRuntime(runtimeDoc(baseRuntime("dataset_secret", "not-allowed")), "analytics")
	expectError(t, err, "unsupported fields")

	_, _, err = ValidateRuntime(obj("version", true, "datasets", pyjson.List{}), "analytics")
	expectError(t, err, "version")

	duplicateDatabase := baseRuntime("id", "team_b", "endpoint_path", "/datasets/team_b/mcp",
		"clickhouse_user", "rawbbit_mcp_team_b", "bearer_tokens", obj("agent", "team-b-token"))
	_, _, err = ValidateRuntime(runtimeDoc(baseRuntime(), duplicateDatabase), "analytics")
	expectError(t, err, "distinct ClickHouse database")

	_, _, err = ValidateRuntime(runtimeDoc(baseRuntime("database", "analytics")), "analytics")
	expectError(t, err, "differ from the main ClickHouse database")
}

func TestProtectedJSONReaderRequiresRestrictedModeAndHidesFileContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provision.json")
	if err := os.WriteFile(path, []byte(`{"password":"synthetic-secret"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := SecureReadJSON(path, false)
	expectError(t, err, "mode 0600")
	if strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("error leaked file contents")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := SecureReadJSON(path, false)
	if err != nil || doc.Lookup("password") != "synthetic-secret" {
		t.Fatalf("unexpected read result: %v %v", doc, err)
	}
}

func validRecord(kv ...any) *pyjson.Dict {
	return update(obj(
		"database", "team_a", "table", "events", "role", "rawbbit_role_team_a", "user", "rawbbit_mcp_team_a",
		"privileges", []string{"SELECT"}, "schema_fingerprint", strings.Repeat("0", 64), "object_ids", pyjson.NewDict(),
		"password_revision", 1, "state", "pending", "pending_action", "create_database",
	), kv...)
}

func TestAtomicJournalIsPrivateAndRecoveryConflictsFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "ownership.json")
	if err := AtomicWriteJSON(path, obj("version", 1, "datasets", pyjson.NewDict()), false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode: %v %v", info.Mode(), err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{\"datasets\":{},\"version\":1}\n" {
		t.Fatalf("unexpected journal bytes %q", data)
	}

	malformed := obj("version", 1, "datasets", obj("team_a", obj("object_ids", pyjson.List{})))
	if err := AtomicWriteJSON(path, malformed, false); err != nil {
		t.Fatal(err)
	}
	_, err = ReadJournal(path, false)
	expectError(t, err, "ownership journal")

	for _, invalid := range []*pyjson.Dict{
		obj("version", true, "datasets", pyjson.NewDict()),
		obj("version", 1, "datasets", obj("team_a", validRecord("state", pyjson.List{}, "pending_action", nil))),
	} {
		if err := AtomicWriteJSON(path, invalid, false); err != nil {
			t.Fatal(err)
		}
		_, err = ReadJournal(path, false)
		expectError(t, err, "ownership journal")
	}

	err = AssertRecordedID(obj("object_ids", pyjson.NewDict(), "pending_action", "create_table"), "table",
		obj("object_id", "00000000-0000-0000-0000-000000000001"))
	expectError(t, err, "recovery required")
	err = AssertRecordedID(obj("object_ids", pyjson.NewDict(), "pending_action", "create_table"), "database", nil)
	expectError(t, err, "identity")
}

func TestJournalValidatesRetainedMainAccessTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ownership.json")
	valid := obj("version", 1, "datasets", pyjson.NewDict(), "main_access_targets", pyjson.List{obj("database", "team_a", "table", "events")})
	if err := AtomicWriteJSON(path, valid, false); err != nil {
		t.Fatal(err)
	}
	read, err := ReadJournal(path, false)
	if err != nil || !pyjson.Equal(read, valid) {
		t.Fatalf("journal round trip failed: %v", err)
	}
	for _, targets := range []pyjson.Value{
		"not-a-list",
		pyjson.List{obj("database", "team_a")},
		pyjson.List{obj("database", "team_a", "table", "events; DROP TABLE x")},
		pyjson.List{obj("database", "team_a", "table", "events"), obj("database", "team_a", "table", "events")},
	} {
		if err := AtomicWriteJSON(path, obj("version", 1, "datasets", pyjson.NewDict(), "main_access_targets", targets), false); err != nil {
			t.Fatal(err)
		}
		_, err := ReadJournal(path, false)
		expectError(t, err, "ownership journal")
	}
}

func TestPreflightStopsOnAnyInterruptedManagedMutation(t *testing.T) {
	datasets := validatedDatasets(t, baseRegistry())
	runtimeByID := validatedRuntime(t, runtimeDoc(baseRuntime()))
	journal := obj("version", 1, "datasets", obj("team_a", obj(
		"database", "team_a", "table", "events", "role", "rawbbit_role_team_a", "user", "rawbbit_mcp_team_a",
		"privileges", []string{"SELECT"}, "schema_fingerprint", "synthetic-schema-fingerprint",
		"object_ids", pyjson.NewDict(), "state", "applied", "pending_action", "rotate_user_password",
	)))
	err := newProvisioner(t, noClient{t}, journal).Preflight(datasets, runtimeByID)
	expectError(t, err, "interrupted managed operation")
}

func TestPendingCreationRecoveryRequiresAbsenceOrMatchingRecordedID(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000001"
	cases := []struct {
		metadata  []*pyjson.Dict
		objectIDs *pyjson.Dict
		raises    bool
	}{
		{nil, pyjson.NewDict(), false},
		{[]*pyjson.Dict{obj("name", "team_a", "object_id", id)}, obj("database", id), false},
		{[]*pyjson.Dict{obj("name", "team_a", "object_id", id)}, pyjson.NewDict(), true},
		{nil, obj("database", id), true},
	}
	for i, c := range cases {
		client := &fakeClient{query: func(sql string) []*pyjson.Dict {
			switch {
			case strings.Contains(sql, "FROM system.users"):
				return []*pyjson.Dict{obj("name", "rawbbit_mcp")}
			case strings.Contains(sql, "FROM system.role_grants"), strings.Contains(sql, "FROM system.grants"):
				return nil
			}
			if !strings.Contains(sql, "system.databases") {
				t.Fatalf("unexpected query %s", sql)
			}
			return c.metadata
		}}
		journal := obj("version", 1, "datasets", obj("team_a", validRecord("object_ids", c.objectIDs)))
		err := newProvisioner(t, client, journal).Preflight(nil, map[string]RuntimeDataset{})
		if c.raises {
			expectError(t, err, "recovery required|ownership conflict")
		} else if err != nil {
			t.Fatalf("case %d: unexpected error %v", i, err)
		}
	}
}

func TestPendingNonCreationMutationRequiresOperatorReview(t *testing.T) {
	journal := obj("version", 1, "datasets", obj("team_a", validRecord("pending_action", "rotate_user_password")))
	err := newProvisioner(t, noClient{t}, journal).Preflight(nil, map[string]RuntimeDataset{})
	expectError(t, err, "operator-reviewed recovery")
}

func TestManagedAdoptionRejectsNonAtomicOrUnidentifiedDatabase(t *testing.T) {
	for _, c := range [][2]string{
		{"Ordinary", "00000000-0000-0000-0000-000000000001"},
		{"Atomic", "00000000-0000-0000-0000-000000000000"},
	} {
		datasets := validatedDatasets(t, baseRegistry("adopt", obj("database_uuid", c[1])))
		runtimeByID := validatedRuntime(t, runtimeDoc(baseRuntime()))
		client := &fakeClient{query: func(sql string) []*pyjson.Dict {
			if strings.Contains(sql, "FROM system.databases") {
				return []*pyjson.Dict{obj("name", "team_a", "engine", c[0], "object_id", c[1])}
			}
			return nil
		}}
		err := newProvisioner(t, client, obj("version", 1, "datasets", pyjson.NewDict())).Preflight(datasets, runtimeByID)
		expectError(t, err, "Atomic database with a non-nil UUID")
	}
}

func TestManagedAdoptionRejectsTableWithoutStableUUID(t *testing.T) {
	databaseUUID := "00000000-0000-0000-0000-000000000001"
	nilUUID := "00000000-0000-0000-0000-000000000000"
	datasets := validatedDatasets(t, baseRegistry("adopt", obj(
		"database_uuid", databaseUUID, "table_uuid", nilUUID, "schema_fingerprint", strings.Repeat("a", 64))))
	runtimeByID := validatedRuntime(t, runtimeDoc(baseRuntime()))
	client := &fakeClient{query: func(sql string) []*pyjson.Dict {
		switch {
		case strings.Contains(sql, "FROM system.databases"):
			return []*pyjson.Dict{obj("name", "team_a", "engine", "Atomic", "object_id", databaseUUID)}
		case strings.Contains(sql, "FROM system.tables"):
			return []*pyjson.Dict{obj("database", "team_a", "name", "events", "object_id", nilUUID,
				"engine", "MergeTree", "partition_key", "", "sorting_key", "event_time")}
		}
		return nil
	}}
	err := newProvisioner(t, client, obj("version", 1, "datasets", pyjson.NewDict())).Preflight(datasets, runtimeByID)
	expectError(t, err, "table adoption requires a non-nil ClickHouse UUID")
}

func TestPendingManagedCreationWithIncompleteOwnedObjectsCanResume(t *testing.T) {
	datasets := validatedDatasets(t, baseRegistry())
	runtimeByID := validatedRuntime(t, runtimeDoc(baseRuntime()))
	record := validRecord("schema_fingerprint", mustSchema(t).Fingerprint, "pending_action", nil)
	client := &fakeClient{query: func(string) []*pyjson.Dict { return nil }}
	if err := newProvisioner(t, client, obj("version", 1, "datasets", obj("team_a", record))).Preflight(datasets, runtimeByID); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightRequiresMainExposureAndLimitContractsToMatch(t *testing.T) {
	datasets := validatedDatasets(t, baseRegistry())
	runtimeByID := validatedRuntime(t, runtimeDoc(baseRuntime("main_exposed", false)))
	err := newProvisioner(t, noClient{t}, obj("version", 1, "datasets", pyjson.NewDict())).Preflight(datasets, runtimeByID)
	expectError(t, err, "main-exposure")
}

func managedGrant(role string) *pyjson.Dict {
	return obj("user_name", nil, "role_name", role, "access_type", "SELECT", "database", "team_a",
		"table", "events", "column", nil, "is_partial_revoke", 0, "grant_option", 0)
}

func TestManagedAccessRejectsRoleAssignmentWithAdminOption(t *testing.T) {
	d := validatedDatasets(t, baseRegistry())[0]
	client := &fakeClient{query: func(sql string) []*pyjson.Dict {
		switch {
		case strings.Contains(sql, "FROM system.grants"):
			return []*pyjson.Dict{managedGrant("rawbbit_role_team_a")}
		case strings.Contains(sql, "FROM system.role_grants"):
			return []*pyjson.Dict{obj("user_name", "rawbbit_mcp_team_a", "role_name", nil,
				"granted_role_name", "rawbbit_role_team_a", "granted_role_is_default", 1, "with_admin_option", 1)}
		}
		t.Fatalf("unexpected metadata query %s", sql)
		return nil
	}}
	expectError(t, VerifyExactManagedAccess(client, d), "unexpected role assignments")
}

func TestReferenceAccessRejectsRoleAssignmentWithAdminOption(t *testing.T) {
	config := baseRegistry("mode", "reference", "create_table", false)
	d := validatedDatasets(t, config)[0]
	client := &fakeClient{query: func(sql string) []*pyjson.Dict {
		switch {
		case strings.Contains(sql, "FROM system.role_grants"):
			return []*pyjson.Dict{obj("user_name", "rawbbit_mcp_team_a", "role_name", nil,
				"granted_role_name", "reference_read_role", "with_admin_option", 1)}
		case strings.Contains(sql, "FROM system.grants"):
			return []*pyjson.Dict{update(managedGrant(""), "user_name", "rawbbit_mcp_team_a", "role_name", nil)}
		}
		t.Fatalf("unexpected metadata query %s", sql)
		return nil
	}}
	expectError(t, VerifyReferenceAccess(client, d), "ADMIN OPTION")
}

func TestMainUserUnexposedDatasetCheckCoversTransitiveRoleGrants(t *testing.T) {
	client := &fakeClient{query: func(sql string) []*pyjson.Dict {
		switch {
		case strings.Contains(sql, "FROM system.users"):
			return []*pyjson.Dict{obj("name", "rawbbit_mcp")}
		case strings.Contains(sql, "FROM system.role_grants") && strings.Contains(sql, "WHERE user_name"):
			return []*pyjson.Dict{obj("user_name", "rawbbit_mcp", "role_name", nil, "granted_role_name", "main_reader")}
		case strings.Contains(sql, "FROM system.role_grants") && strings.Contains(sql, "main_reader"):
			return []*pyjson.Dict{obj("user_name", nil, "role_name", "main_reader", "granted_role_name", "nested_reader")}
		case strings.Contains(sql, "FROM system.role_grants"):
			return nil
		case strings.Contains(sql, "FROM system.grants"):
			if !strings.Contains(sql, "nested_reader") {
				t.Fatalf("nested role missing from %s", sql)
			}
			return []*pyjson.Dict{obj("user_name", nil, "role_name", "nested_reader", "access_type", "SELECT",
				"database", "team_a", "table", nil, "column", nil, "is_partial_revoke", 0)}
		}
		t.Fatalf("unexpected metadata query: %s", sql)
		return nil
	}}
	datasets := validatedDatasets(t, baseRegistry("main_exposed", false))
	expectError(t, VerifyMainUserCannotReadUnexposedDatasets(client, "rawbbit_mcp", datasets, nil), "unexposed dataset")
}

func TestMainUserUnexposedDatasetCheckFailsClosedAndIgnoresExposedTargets(t *testing.T) {
	missing := &fakeClient{query: func(sql string) []*pyjson.Dict {
		if !strings.Contains(sql, "FROM system.users") {
			t.Fatalf("unexpected query %s", sql)
		}
		return nil
	}}
	err := VerifyMainUserCannotReadUnexposedDatasets(missing, "rawbbit_mcp", validatedDatasets(t, baseRegistry("main_exposed", false)), nil)
	expectError(t, err, "main MCP ClickHouse user is missing")

	if err := VerifyMainUserCannotReadUnexposedDatasets(noClient{t}, "rawbbit_mcp", validatedDatasets(t, baseRegistry("main_exposed", true)), nil); err != nil {
		t.Fatal(err)
	}
}

func mainSelectClient(t *testing.T, grant *pyjson.Dict) *fakeClient {
	return &fakeClient{query: func(sql string) []*pyjson.Dict {
		switch {
		case strings.Contains(sql, "FROM system.users"):
			return []*pyjson.Dict{obj("name", "rawbbit_mcp")}
		case strings.Contains(sql, "FROM system.role_grants"):
			return nil
		case strings.Contains(sql, "FROM system.grants"):
			return []*pyjson.Dict{grant}
		}
		t.Fatalf("unexpected metadata query: %s", sql)
		return nil
	}}
}

func directSelect() *pyjson.Dict {
	return obj("user_name", "rawbbit_mcp", "role_name", nil, "access_type", "SELECT", "database", "team_a",
		"table", "events", "column", nil, "is_partial_revoke", 0)
}

func TestMainUserCheckRetainsTargetsFromRemovedRegistrations(t *testing.T) {
	oldTargets := obj("version", 1, "main_access_targets", pyjson.List{obj("database", "team_a", "table", "events")}, "datasets", pyjson.NewDict())
	expectError(t, VerifyMainUserCannotReadUnexposedDatasets(mainSelectClient(t, directSelect()), "rawbbit_mcp", nil, oldTargets), "unexposed dataset")

	// Older journals have managed object records but no exposure-target list.
	legacy := obj("version", 1, "datasets", obj("team_a", obj("database", "team_a", "table", "events")))
	expectError(t, VerifyMainUserCannotReadUnexposedDatasets(mainSelectClient(t, directSelect()), "rawbbit_mcp", nil, legacy), "unexposed dataset")
}

func TestPreflightChecksMainGrantsForRemovedJournalTargets(t *testing.T) {
	journal := obj("version", 1, "main_access_targets", pyjson.List{obj("database", "team_a", "table", "events")}, "datasets", pyjson.NewDict())
	err := newProvisioner(t, mainSelectClient(t, directSelect()), journal).Preflight(nil, map[string]RuntimeDataset{})
	expectError(t, err, "unexposed dataset")
}

func TestPreflightRejectsDisabledDatasetAccessThroughMainUser(t *testing.T) {
	datasets := validatedDatasets(t, baseRegistry("enabled", false, "main_exposed", true))
	runtimeByID := validatedRuntime(t, runtimeDoc())
	grant := obj("user_name", "rawbbit_mcp", "role_name", nil, "access_type", "ALL", "database", nil,
		"table", nil, "column", nil, "is_partial_revoke", 0)
	err := newProvisioner(t, mainSelectClient(t, grant), obj("version", 1, "datasets", pyjson.NewDict())).Preflight(datasets, runtimeByID)
	expectError(t, err, "unexposed dataset")
}

func TestProvisionerRetainsRegisteredTargetsAfterDisableOrRemoval(t *testing.T) {
	disabled := validatedDatasets(t, baseRegistry("enabled", false))
	journal := obj("version", 1, "datasets", pyjson.NewDict())
	p := newProvisioner(t, noClient{t}, journal)
	if err := p.Provision(disabled, map[string]RuntimeDataset{}); err != nil {
		t.Fatal(err)
	}
	want := pyjson.List{obj("database", "team_a", "table", "events")}
	if !pyjson.Equal(journal.Lookup("main_access_targets"), want) {
		t.Fatalf("targets: %s", pyjson.Dumps(journal))
	}
	written, err := SecureReadJSON(p.JournalPath, false)
	if err != nil || !pyjson.Equal(written.Lookup("main_access_targets"), want) {
		t.Fatalf("written journal mismatch: %v", err)
	}
	// An empty registry never erases previous isolation targets.
	next := NewProvisioner(noClient{t}, testAdmin, mustSchema(t), p.JournalPath, journal, false, "rawbbit_mcp")
	if err := next.Provision(nil, map[string]RuntimeDataset{}); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(journal.Lookup("main_access_targets"), want) {
		t.Fatal("targets were erased")
	}
}

func TestMainUserMustBeDistinctFromAdminAndDatasetUsers(t *testing.T) {
	config := baseRegistry()
	config.Set("main_user", "admin")
	_, _, err := ValidateRegistry(config)
	expectError(t, err, "privileged admin user")

	config = baseRegistry()
	config.Lookup("datasets").(pyjson.List)[0].(*pyjson.Dict).Set("mcp_user", "rawbbit_mcp")
	_, _, err = ValidateRegistry(config)
	expectError(t, err, "main MCP user")
}

func TestManagedGrantReconciliationRemovesPartialRevokes(t *testing.T) {
	d := validatedDatasets(t, baseRegistry())[0]
	role, user := d.MCPRole, d.MCPUser
	grants := []*pyjson.Dict{
		update(managedGrant(role), "table", nil),
		update(managedGrant(role), "is_partial_revoke", 1),
	}
	assignments := []*pyjson.Dict{obj("user_name", user, "role_name", nil, "granted_role_name", role,
		"granted_role_is_default", 1, "with_admin_option", 0)}
	client := &fakeClient{}
	client.query = func(sql string) []*pyjson.Dict {
		switch {
		case strings.Contains(sql, "FROM system.role_grants"):
			return assignments
		case strings.Contains(sql, "FROM system.settings_profile_elements"):
			return []*pyjson.Dict{obj("setting_name", "readonly", "value", "1"), obj("setting_name", "max_execution_time", "value", "30"),
				obj("setting_name", "max_result_rows", "value", "500"), obj("setting_name", "result_overflow_mode", "value", "break")}
		case strings.Contains(sql, "FROM system.users"):
			return []*pyjson.Dict{obj("default_roles_all", 0, "default_roles_list", []string{role}, "default_roles_except", []string{})}
		case strings.Contains(sql, "FROM system.grants"):
			return append([]*pyjson.Dict(nil), grants...)
		}
		t.Fatalf("unexpected metadata query: %s", sql)
		return nil
	}
	client.execute = func(sql string) {
		switch {
		case strings.HasPrefix(sql, "REVOKE ALL ON"):
			// ClickHouse removes the broad grant and its partial-revoke row
			// when the managed role's whole database scope is reconciled.
			grants = nil
		case strings.HasPrefix(sql, "GRANT SELECT ON"):
			grants = []*pyjson.Dict{managedGrant(role)}
		}
	}
	record := validRecord("schema_fingerprint", "synthetic-schema-fingerprint", "state", "applied", "pending_action", nil)
	p := newProvisioner(t, client, obj("version", 1, "datasets", obj(d.ID, record)))
	if err := p.ReconcileRoleAndUser(d, record); err != nil {
		t.Fatal(err)
	}
	var revoked, granted bool
	for _, sql := range client.executions {
		revoked = revoked || strings.HasPrefix(sql, "REVOKE ALL ON")
		granted = granted || strings.HasPrefix(sql, "GRANT SELECT ON")
	}
	if !revoked || !granted || len(grants) != 1 || !pyjson.Equal(grants[0], managedGrant(role)) {
		t.Fatalf("unexpected reconciliation: %v %s", client.executions, pyjson.Dumps(pyjson.List{grants[0]}))
	}
}

func TestClickHouseQueryUsesPostRequestWithoutReturningHTTPErrorBody(t *testing.T) {
	var method, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, auth = r.Method, r.Header.Get("Authorization")
		if strings.Contains(r.URL.RawQuery, "fail") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Code: 60. DB::Exception: Table secret_table_name does not exist"))
			return
		}
		_, _ = w.Write([]byte("1\n"))
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(srv.URL[strings.LastIndexByte(srv.URL, ':')+1:])
	client := NewHTTPClient(&AdminConnection{Host: "127.0.0.1", Port: int64(port), Username: "admin", Password: "synthetic-secret", TimeoutSeconds: 7})
	body, err := client.request("SELECT 1")
	if err != nil || body != "1\n" || method != http.MethodPost || auth == "" {
		t.Fatalf("unexpected request: %q %v %s %q", body, err, method, auth)
	}
	if client.http.Timeout.Seconds() != 7 {
		t.Fatalf("timeout %v", client.http.Timeout)
	}
	client.url += "?fail=1"
	_, err = client.request("SELECT 1")
	expectError(t, err, `^ClickHouse rejected a request \(HTTP 404, error code 60\)$`)
}
