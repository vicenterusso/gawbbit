package provision

import (
	"sort"
	"strings"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

// objectNames are the four managed names of a dataset (or pending record).
type objectNames struct {
	Database string
	Table    string
	Role     string
	User     string
}

func (d *Dataset) names() objectNames {
	return objectNames{Database: d.Database, Table: d.Table, Role: d.MCPRole, User: d.MCPUser}
}

func singleRow(c Client, sql string) (*pyjson.Dict, error) {
	rows, err := c.Query(sql)
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, provisionErr("ClickHouse metadata returned duplicate object names")
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func objectMetadata(c Client, n objectNames, kind string) (*pyjson.Dict, error) {
	switch kind {
	case "database":
		return singleRow(c, "SELECT name, engine, toString(uuid) AS object_id FROM system.databases "+
			"WHERE name = "+quoteString(n.Database))
	case "table":
		return singleRow(c, "SELECT database, name, toString(uuid) AS object_id, engine, partition_key, sorting_key "+
			"FROM system.tables "+
			"WHERE database = "+quoteString(n.Database)+" AND name = "+quoteString(n.Table))
	case "role":
		return singleRow(c, "SELECT name, toString(id) AS object_id FROM system.roles "+
			"WHERE name = "+quoteString(n.Role))
	case "user":
		return singleRow(c, "SELECT name, toString(id) AS object_id FROM system.users "+
			"WHERE name = "+quoteString(n.User))
	}
	return nil, provisionErr("unsupported managed object type")
}

func actualColumns(c Client, d *Dataset) ([]Column, error) {
	rows, err := c.Query("SELECT name, type FROM system.columns " +
		"WHERE database = " + quoteString(d.Database) + " " +
		"AND table = " + quoteString(d.Table) + " ORDER BY position")
	if err != nil {
		return nil, err
	}
	out := make([]Column, len(rows))
	for i, r := range rows {
		out[i] = Column{Name: pyjson.Str(r.Lookup("name")), Type: pyjson.Str(r.Lookup("type"))}
	}
	return out, nil
}

// orEmpty mirrors str(value or "").
func orEmpty(v pyjson.Value) string {
	if !pyjson.Truthy(v) {
		return ""
	}
	return pyjson.Str(v)
}

func schemaMatches(c Client, d *Dataset, s *CanonicalSchema) (bool, error) {
	table, err := objectMetadata(c, d.names(), "table")
	if err != nil || table == nil {
		return false, err
	}
	columns, err := actualColumns(c, d)
	if err != nil {
		return false, err
	}
	if len(columns) != len(s.Columns) {
		return false, nil
	}
	for i := range columns {
		if columns[i] != s.Columns[i] {
			return false, nil
		}
	}
	return pyLower(pyjson.Str(table.Lookup("engine"))) == pyLower(s.Engine) &&
		normalizeExpression(orEmpty(table.Lookup("partition_key"))) == s.PartitionKey &&
		normalizeExpression(orEmpty(table.Lookup("sorting_key"))) == s.SortingKey, nil
}

func schemaFingerprint(c Client, d *Dataset, s *CanonicalSchema) (string, error) {
	table, err := objectMetadata(c, d.names(), "table")
	if err != nil {
		return "", err
	}
	if table != nil {
		ok, err := schemaMatches(c, d, s)
		if err != nil {
			return "", err
		}
		if ok {
			return s.Fingerprint, nil
		}
	}
	return "", provisionErr("dataset table is not compatible with the canonical Rawbbit events schema")
}

func ownershipRecordMatches(record *pyjson.Dict, d *Dataset) bool {
	return pyjson.Equal(record.Lookup("database"), d.Database) &&
		pyjson.Equal(record.Lookup("table"), d.Table) &&
		pyjson.Equal(record.Lookup("role"), d.MCPRole) &&
		pyjson.Equal(record.Lookup("user"), d.MCPUser) &&
		pyjson.Equal(record.Lookup("privileges"), selectOnly)
}

func objectIDs(record *pyjson.Dict) *pyjson.Dict {
	if ids, ok := record.Lookup("object_ids").(*pyjson.Dict); ok {
		return ids
	}
	return pyjson.NewDict()
}

// AssertRecordedID mirrors _assert_recorded_id (record may be nil).
func AssertRecordedID(record *pyjson.Dict, kind string, metadata *pyjson.Dict) error {
	if record == nil {
		return nil
	}
	if (kind == "database" || kind == "table") && metadata != nil && !hasStableUUID(metadata.Lookup("object_id")) {
		return provisionErr("recovery required: " + kind + " has no stable ClickHouse UUID")
	}
	recorded := objectIDs(record).Lookup(kind)
	if recorded == nil {
		if metadata != nil {
			return provisionErr("recovery required: " + kind + " exists without a recorded ClickHouse object ID")
		}
		pending := record.Lookup("pending_action")
		if pending == nil || pyjson.Equal(pending, "create_"+kind) {
			return nil
		}
		return provisionErr("ownership journal is inconsistent for the " + kind + " identity")
	}
	if metadata == nil || pyjson.Str(metadata.Lookup("object_id")) != pyjson.Str(recorded) {
		return provisionErr("ownership conflict: recorded " + kind + " identity does not match ClickHouse")
	}
	return nil
}

func isInt(v pyjson.Value, n int64) bool { return pyjson.Equal(v, pyjson.Int(n)) }

// VerifyExactManagedAccess mirrors _verify_exact_managed_access.
func VerifyExactManagedAccess(c Client, d *Dataset) error {
	role, user := d.MCPRole, d.MCPUser
	grants, err := c.Query("SELECT user_name, role_name, access_type, database, table, column, " +
		"is_partial_revoke, grant_option FROM system.grants " +
		"WHERE role_name = " + quoteString(role) + " OR user_name = " + quoteString(user))
	if err != nil {
		return err
	}
	if len(grants) != 1 || !rowMatches(grants[0], []field{
		{"user_name", nil}, {"role_name", role}, {"access_type", "SELECT"}, {"database", d.Database},
		{"table", d.Table}, {"column", nil}, {"is_partial_revoke", pyjson.Int(0)}, {"grant_option", pyjson.Int(0)},
	}) {
		return provisionErr("managed ClickHouse role has unexpected or ineffective grants")
	}
	assignments, err := c.Query("SELECT user_name, role_name, granted_role_name, granted_role_is_default, with_admin_option " +
		"FROM system.role_grants " +
		"WHERE user_name = " + quoteString(user) + " OR role_name = " + quoteString(role) + " " +
		"OR granted_role_name = " + quoteString(role))
	if err != nil {
		return err
	}
	if len(assignments) != 1 || !rowMatches(assignments[0], []field{
		{"user_name", user}, {"role_name", nil}, {"granted_role_name", role},
		{"granted_role_is_default", pyjson.Int(1)}, {"with_admin_option", pyjson.Int(0)},
	}) {
		return provisionErr("managed ClickHouse user has unexpected role assignments")
	}
	return nil
}

type field struct {
	key   string
	value pyjson.Value
}

func rowMatches(row *pyjson.Dict, expected []field) bool {
	for _, f := range expected {
		if !pyjson.Equal(row.Lookup(f.key), f.value) {
			return false
		}
	}
	return true
}

func inValues(v pyjson.Value, options ...pyjson.Value) bool {
	for _, o := range options {
		if pyjson.Equal(v, o) {
			return true
		}
	}
	return false
}

// VerifyReferenceAccess mirrors _verify_reference_access.
func VerifyReferenceAccess(c Client, d *Dataset) error {
	user := d.MCPUser
	assignments, err := c.Query("SELECT user_name, role_name, granted_role_name, with_admin_option FROM system.role_grants " +
		"WHERE user_name = " + quoteString(user) + " OR role_name = " + quoteString(user))
	if err != nil {
		return err
	}
	userRows, err := c.Query("SELECT user_name, role_name, access_type, database, table, column, " +
		"is_partial_revoke, grant_option FROM system.grants " +
		"WHERE user_name = " + quoteString(user))
	if err != nil {
		return err
	}
	roleSet := map[string]bool{}
	for _, row := range assignments {
		if pyjson.Equal(row.Lookup("user_name"), user) {
			roleSet[pyjson.Str(row.Lookup("granted_role_name"))] = true
		}
	}
	for _, row := range assignments {
		if pyjson.Equal(row.Lookup("user_name"), user) && !isInt(row.Lookup("with_admin_option"), 0) {
			return provisionErr("reference user has a role assignment with ADMIN OPTION")
		}
	}
	for _, row := range assignments {
		if pyjson.Equal(row.Lookup("role_name"), user) {
			return provisionErr("reference user is inherited by a role and cannot be safely scoped")
		}
	}
	effective := append([]*pyjson.Dict(nil), userRows...)
	// Python iterates a set here; sorted order keeps runs reproducible.
	roleNames := make([]string, 0, len(roleSet))
	for r := range roleSet {
		roleNames = append(roleNames, r)
	}
	sort.Strings(roleNames)
	for _, role := range roleNames {
		rows, err := c.Query("SELECT user_name, role_name, access_type, database, table, column, " +
			"is_partial_revoke, grant_option FROM system.grants " +
			"WHERE role_name = " + quoteString(role))
		if err != nil {
			return err
		}
		effective = append(effective, rows...)
		nested, err := c.Query("SELECT granted_role_name FROM system.role_grants " +
			"WHERE role_name = " + quoteString(role))
		if err != nil {
			return err
		}
		if len(nested) > 0 {
			return provisionErr("reference user uses nested roles; verify direct least-privilege grants manually")
		}
	}
	bad := len(effective) == 0
	for _, row := range effective {
		if !pyjson.Equal(row.Lookup("access_type"), "SELECT") ||
			!pyjson.Equal(row.Lookup("database"), d.Database) ||
			!pyjson.Equal(row.Lookup("table"), d.Table) ||
			!inValues(row.Lookup("column"), nil, "") ||
			!isInt(row.Lookup("is_partial_revoke"), 0) ||
			!isInt(row.Lookup("grant_option"), 0) {
			bad = true
		}
	}
	if bad {
		return provisionErr("reference ClickHouse credentials have privileges outside the target table")
	}
	return nil
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = quoteString(n)
	}
	return strings.Join(quoted, ", ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// VerifyMainUserCannotReadUnexposedDatasets mirrors
// _verify_main_user_cannot_read_unexposed_datasets.
func VerifyMainUserCannotReadUnexposedDatasets(c Client, mainUser string, datasets []*Dataset, journal *pyjson.Dict) error {
	unexposed := map[[2]string]bool{}
	exposed := map[[2]string]bool{}
	for _, d := range datasets {
		pair := [2]string{d.Database, d.Table}
		if !d.Enabled || !d.MainExposed {
			unexposed[pair] = true
		} else {
			exposed[pair] = true
		}
	}
	var historical []pyjson.Value
	if journal != nil {
		if targets, ok := journal.Lookup("main_access_targets").(pyjson.List); ok {
			historical = append(historical, targets...)
		}
		if records, ok := journal.Lookup("datasets").(*pyjson.Dict); ok {
			for _, k := range records.Keys() {
				if r, ok := records.Lookup(k).(*pyjson.Dict); ok {
					historical = append(historical, r)
				}
			}
		}
	}
	for _, item := range historical {
		target, ok := item.(*pyjson.Dict)
		if !ok {
			return provisionErr("ownership journal has invalid main-access target records")
		}
		database, err := identifier(target.Lookup("database"), "ownership journal main-access database")
		if err != nil {
			return err
		}
		table, err := identifier(target.Lookup("table"), "ownership journal main-access table")
		if err != nil {
			return err
		}
		if pair := [2]string{database, table}; !exposed[pair] {
			unexposed[pair] = true
		}
	}
	targets := make([][2]string, 0, len(unexposed))
	for t := range unexposed {
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i][0] != targets[j][0] {
			return targets[i][0] < targets[j][0]
		}
		return targets[i][1] < targets[j][1]
	})
	if len(targets) == 0 {
		return nil
	}

	row, err := singleRow(c, "SELECT name FROM system.users "+
		"WHERE name = "+quoteString(mainUser))
	if err != nil {
		return err
	}
	if row == nil {
		return provisionErr("main MCP ClickHouse user is missing; cannot verify dataset isolation")
	}

	roles := map[string]bool{}
	direct, err := c.Query("SELECT user_name, role_name, granted_role_name FROM system.role_grants " +
		"WHERE user_name = " + quoteString(mainUser))
	if err != nil {
		return err
	}
	for _, r := range direct {
		if s, ok := r.Lookup("granted_role_name").(string); ok {
			roles[s] = true
		}
	}
	checked := map[string]bool{}
	for {
		var current []string
		for _, r := range sortedKeys(roles) {
			if !checked[r] {
				current = append(current, r)
			}
		}
		if len(current) == 0 {
			break
		}
		currentSet := map[string]bool{}
		for _, r := range current {
			checked[r], currentSet[r] = true, true
		}
		nested, err := c.Query("SELECT user_name, role_name, granted_role_name FROM system.role_grants " +
			"WHERE role_name IN (" + quotedList(current) + ")")
		if err != nil {
			return err
		}
		for _, r := range nested {
			name, isStr := r.Lookup("role_name").(string)
			granted, grantedStr := r.Lookup("granted_role_name").(string)
			if isStr && currentSet[name] && grantedStr {
				roles[granted] = true
			}
		}
	}

	filter := "user_name = " + quoteString(mainUser)
	if len(roles) > 0 {
		filter += " OR role_name IN (" + quotedList(sortedKeys(roles)) + ")"
	}
	grants, err := c.Query("SELECT user_name, role_name, access_type, database, table, column, is_partial_revoke " +
		"FROM system.grants WHERE " + filter)
	if err != nil {
		return err
	}
	for _, g := range grants {
		if isInt(g.Lookup("is_partial_revoke"), 1) || !inValues(g.Lookup("access_type"), "SELECT", "ALL") {
			continue
		}
		roleName, _ := g.Lookup("role_name").(string)
		_, roleIsStr := g.Lookup("role_name").(string)
		if !inValues(g.Lookup("user_name"), nil, mainUser) && !(roleIsStr && roles[roleName]) {
			continue
		}
		database, table := g.Lookup("database"), g.Lookup("table")
		for _, t := range targets {
			if inValues(database, nil, "", "*", t[0]) && inValues(table, nil, "", "*", t[1]) {
				return provisionErr("main MCP ClickHouse user can read an unexposed dataset; " +
					"narrow its existing direct or inherited grants before provisioning")
			}
		}
	}
	return nil
}
