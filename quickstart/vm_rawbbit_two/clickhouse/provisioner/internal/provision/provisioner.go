package provision

import (
	"sort"
	"strings"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

// Provisioner mirrors DatasetProvisioner.
type Provisioner struct {
	Client      Client
	Admin       *AdminConnection
	Schema      *CanonicalSchema
	JournalPath string
	Journal     *pyjson.Dict
	RequireRoot bool
	MainUser    string
	Changed     bool
	// Restricted opens a client with a dataset's own credentials.
	Restricted func(RuntimeDataset, *AdminConnection) Client
}

// NewProvisioner wires the default restricted-client factory.
func NewProvisioner(client Client, admin *AdminConnection, schema *CanonicalSchema, journalPath string,
	journal *pyjson.Dict, requireRoot bool, mainUser string) *Provisioner {
	return &Provisioner{
		Client: client, Admin: admin, Schema: schema, JournalPath: journalPath, Journal: journal,
		RequireRoot: requireRoot, MainUser: mainUser,
		Restricted: func(r RuntimeDataset, a *AdminConnection) Client { return restrictedClient(r, a) },
	}
}

func (p *Provisioner) records() *pyjson.Dict {
	if d, ok := p.Journal.Lookup("datasets").(*pyjson.Dict); ok {
		return d
	}
	return pyjson.NewDict()
}

func (p *Provisioner) save() error { return AtomicWriteJSON(p.JournalPath, p.Journal, p.RequireRoot) }

// recordedEntity mirrors _recorded_entity.
func (p *Provisioner) recordedEntity(kind, name string) (string, bool) {
	records := p.records()
	for _, id := range records.Keys() {
		record, ok := records.Lookup(id).(*pyjson.Dict)
		if !ok || !pyjson.Equal(record.Lookup(kind), name) {
			continue
		}
		if objectID := objectIDs(record).Lookup(kind); objectID != nil {
			return pyjson.Str(objectID), true
		}
	}
	return "", false
}

func (p *Provisioner) identityObjectID(kind, name string) error {
	owned, ok := p.recordedEntity(kind, name)
	if !ok {
		return provisionErr("cross-dataset access references an unowned identity")
	}
	row, err := singleRow(p.Client, "SELECT toString(id) AS object_id FROM system."+kind+"s WHERE name = "+quoteString(name))
	if err != nil {
		return err
	}
	if row == nil || pyjson.Str(row.Lookup("object_id")) != owned {
		return provisionErr("cross-dataset access references a stale managed identity")
	}
	return nil
}

func (p *Provisioner) validateReconcilableAccess(d *Dataset) error {
	user, role := d.MCPUser, d.MCPRole
	assignments, err := p.Client.Query("SELECT user_name, role_name, granted_role_name FROM system.role_grants " +
		"WHERE user_name = " + quoteString(user) + " OR role_name = " + quoteString(role) + " " +
		"OR granted_role_name = " + quoteString(role))
	if err != nil {
		return err
	}
	for _, a := range assignments {
		assigned, grantee, granteeRole := a.Lookup("granted_role_name"), a.Lookup("user_name"), a.Lookup("role_name")
		if pyjson.Equal(grantee, user) && granteeRole == nil && pyjson.Equal(assigned, role) {
			continue
		}
		switch {
		case pyjson.Equal(assigned, role) && grantee != nil:
			if !pyjson.Equal(grantee, user) {
				if _, ok := p.recordedEntity("user", pyjson.Str(grantee)); !ok {
					return provisionErr("managed role is assigned to an unowned ClickHouse user")
				}
				if err := p.identityObjectID("user", pyjson.Str(grantee)); err != nil {
					return err
				}
			}
		case pyjson.Equal(assigned, role) && granteeRole != nil:
			if _, ok := p.recordedEntity("role", pyjson.Str(granteeRole)); !ok {
				return provisionErr("managed role is inherited by an unowned ClickHouse role")
			}
			if err := p.identityObjectID("role", pyjson.Str(granteeRole)); err != nil {
				return err
			}
		case pyjson.Equal(grantee, user) && assigned != nil:
			if err := p.identityObjectID("role", pyjson.Str(assigned)); err != nil {
				return err
			}
		case pyjson.Equal(granteeRole, role) && assigned != nil:
			if err := p.identityObjectID("role", pyjson.Str(assigned)); err != nil {
				return err
			}
		default:
			return provisionErr("ClickHouse role metadata is not safely reconcilable")
		}
	}
	grants, err := p.Client.Query("SELECT user_name, role_name, database, table FROM system.grants " +
		"WHERE user_name = " + quoteString(user) + " OR role_name = " + quoteString(role))
	if err != nil {
		return err
	}
	for _, g := range grants {
		if !pyjson.Equal(g.Lookup("role_name"), role) && !pyjson.Equal(g.Lookup("user_name"), user) {
			return provisionErr("ClickHouse grants include an unowned principal")
		}
		if err := checkGrantScope(g); err != nil {
			return err
		}
	}
	return nil
}

// grantScope mirrors str(grant.get(x) or "*") with identifier checks.
func grantScope(g *pyjson.Dict) (string, string, error) {
	database, table := "*", "*"
	if v := g.Lookup("database"); pyjson.Truthy(v) {
		database = pyjson.Str(v)
	}
	if v := g.Lookup("table"); pyjson.Truthy(v) {
		table = pyjson.Str(v)
	}
	if database != "*" {
		if _, err := identifier(database, "managed grant database"); err != nil {
			return "", "", err
		}
	}
	if table != "*" {
		if _, err := identifier(table, "managed grant table"); err != nil {
			return "", "", err
		}
	}
	return database, table, nil
}

func checkGrantScope(g *pyjson.Dict) error {
	_, _, err := grantScope(g)
	return err
}

func (p *Provisioner) validatePendingOperation(id string, record *pyjson.Dict) error {
	action := record.Lookup("pending_action")
	if action == nil {
		return nil
	}
	s, ok := action.(string)
	if !ok || !strings.HasPrefix(s, "create_") {
		return provisionErr("recovery required: dataset " + id + " has an interrupted managed operation; " +
			"preserve ClickHouse and the journal, then complete an operator-reviewed recovery")
	}
	kind := strings.TrimPrefix(s, "create_")
	if !journalObjectIDFields[kind] {
		return provisionErr("recovery required: dataset " + id + " has an unsupported pending creation")
	}
	names := objectNames{
		Database: pyjson.Str(record.Lookup("database")),
		Table:    pyjson.Str(record.Lookup("table")),
		Role:     pyjson.Str(record.Lookup("role")),
		User:     pyjson.Str(record.Lookup("user")),
	}
	metadata, err := objectMetadata(p.Client, names, kind)
	if err != nil {
		return err
	}
	// A creation is safe to retry only when the object is still absent. If it
	// now exists, continue only when its identity was durably journaled.
	return AssertRecordedID(record, kind, metadata)
}

func (p *Provisioner) mutate(record *pyjson.Dict, action, sql string, updates map[string]pyjson.Value) error {
	record.Set("state", "pending")
	record.Set("pending_action", action)
	if err := p.save(); err != nil {
		return err
	}
	if err := p.Client.Execute(sql); err != nil {
		return err
	}
	p.Changed = true
	for k, v := range updates {
		record.Set(k, v)
	}
	record.Set("pending_action", nil)
	return p.save()
}

func runtimeString(r RuntimeDataset, key string) pyjson.Value {
	if key == "table" {
		return getDefault(r, "table", "events")
	}
	return r.Lookup(key)
}

func probeSQL(d *Dataset) string {
	return "SELECT 1 AS ok FROM " + quoteIdentifierUnchecked(d.Database) + "." + quoteIdentifierUnchecked(d.Table) + " LIMIT 0"
}

// Preflight mirrors DatasetProvisioner.preflight.
func (p *Provisioner) Preflight(datasets []*Dataset, runtimeByID map[string]RuntimeDataset) error {
	records := p.records()
	enabled := map[string]bool{}
	for _, d := range datasets {
		if d.Enabled {
			enabled[d.ID] = true
		}
	}
	same := len(enabled) == len(runtimeByID)
	for id := range runtimeByID {
		same = same && enabled[id]
	}
	if !same {
		return provisionErr("enabled provisioning and runtime dataset IDs must match exactly")
	}
	managed := map[string]string{}
	for _, id := range records.Keys() {
		record, ok := records.Lookup(id).(*pyjson.Dict)
		if !ok {
			return provisionErr("ownership journal contains an invalid dataset record")
		}
		if err := p.validatePendingOperation(id, record); err != nil {
			return err
		}
		for _, key := range []string{"database", "role", "user"} {
			if name, ok := record.Lookup(key).(string); ok {
				managed[key+":"+name] = id
			}
		}
		db, dbOK := record.Lookup("database").(string)
		table, tableOK := record.Lookup("table").(string)
		if dbOK && tableOK {
			managed["table:"+db+"."+table] = id
		}
	}

	for _, d := range datasets {
		if !d.Enabled {
			continue
		}
		runtime := runtimeByID[d.ID]
		if runtime == nil {
			return provisionErr("every enabled provisioned dataset must exist in the runtime registry")
		}
		for _, pair := range [][2]string{{d.Database, "database"}, {d.Table, "table"}, {d.MCPUser, "clickhouse_user"}, {d.EndpointPath, "endpoint_path"}} {
			if !pyjson.Equal(pair[0], runtimeString(runtime, pair[1])) {
				return provisionErr("provisioning and runtime registry metadata do not match")
			}
		}
		if d.Mode == "managed" && !pyjson.Equal(d.MCPPassword, runtime.Lookup("clickhouse_password")) {
			return provisionErr("managed provisioning and runtime ClickHouse passwords do not match")
		}
		if !pyjson.Equal(d.MainExposed, runtime.Lookup("main_exposed")) {
			return provisionErr("provisioning and runtime main-exposure settings do not match")
		}
		for _, name := range limitNames {
			var limit pyjson.Value
			if n, ok := d.Limits[name]; ok {
				limit = n
			}
			if !pyjson.Equal(limit, runtime.Lookup(name)) {
				return provisionErr("provisioning and runtime query limits do not match")
			}
		}

		var record *pyjson.Dict
		if r, ok := records.Get(d.ID); ok {
			record, _ = r.(*pyjson.Dict)
		}
		if record != nil && !ownershipRecordMatches(record, d) {
			return provisionErr("ownership journal dataset names or privileges conflict with configuration")
		}
		if record != nil && !pyjson.Equal(record.Lookup("schema_fingerprint"), p.Schema.Fingerprint) {
			return provisionErr("canonical schema fingerprint changed for an owned dataset; operator review is required")
		}
		for _, kn := range [][2]string{{"database", d.Database}, {"table", d.Database + "." + d.Table}, {"role", d.MCPRole}, {"user", d.MCPUser}} {
			if owner, ok := managed[kn[0]+":"+kn[1]]; ok && owner != d.ID {
				return provisionErr("a ClickHouse object name is already recorded under another dataset id")
			}
		}

		meta := map[string]*pyjson.Dict{}
		for _, kind := range []string{"database", "table", "role", "user"} {
			m, err := objectMetadata(p.Client, d.names(), kind)
			if err != nil {
				return err
			}
			meta[kind] = m
		}
		if d.Mode == "reference" {
			matches := false
			if meta["database"] != nil && meta["table"] != nil {
				var err error
				if matches, err = schemaMatches(p.Client, d, p.Schema); err != nil {
					return err
				}
			}
			if !matches {
				return provisionErr("reference dataset is missing or has an incompatible events table")
			}
			if _, err := p.Restricted(runtime, p.Admin).Query(probeSQL(d)); err != nil {
				return err
			}
			if err := VerifyReferenceAccess(p.Client, d); err != nil {
				return err
			}
			continue
		}

		if db := meta["database"]; db != nil && (!pyjson.Equal(db.Lookup("engine"), "Atomic") || !hasStableUUID(db.Lookup("object_id"))) {
			return provisionErr("managed dataset adoption requires an Atomic database with a non-nil UUID")
		}
		if record == nil {
			if err := p.preflightUnrecorded(d, meta); err != nil {
				return err
			}
			continue
		}
		for _, kind := range []string{"database", "table", "role", "user"} {
			if err := AssertRecordedID(record, kind, meta[kind]); err != nil {
				return err
			}
		}
		if !d.CreateTable && meta["table"] == nil {
			return provisionErr("owned managed events table is missing")
		}
		if meta["table"] != nil {
			if _, err := schemaFingerprint(p.Client, d, p.Schema); err != nil {
				return err
			}
		}
		if meta["role"] == nil || meta["user"] == nil {
			if !pyjson.Equal(record.Lookup("state"), "pending") {
				return provisionErr("an applied managed dataset is missing an owned ClickHouse access identity")
			}
		} else if err := p.validateReconcilableAccess(d); err != nil {
			return err
		}
		if meta["user"] != nil && pyjson.Equal(record.Lookup("password_revision"), d.PasswordRevision) {
			if _, err := p.Restricted(runtime, p.Admin).Query(probeSQL(d)); err != nil {
				if _, isProvision := err.(*Error); isProvision {
					return provisionErr("managed ClickHouse credential does not match its recorded password revision; " +
						"restore the prior secret or increment password_revision for an intentional rotation")
				}
				return err
			}
		}
	}
	return VerifyMainUserCannotReadUnexposedDatasets(p.Client, p.MainUser, datasets, p.Journal)
}

func (p *Provisioner) preflightUnrecorded(d *Dataset, meta map[string]*pyjson.Dict) error {
	if meta["role"] != nil || meta["user"] != nil {
		return provisionErr("unrecorded same-named ClickHouse user or role requires manual recovery")
	}
	adoption := d.Adopt
	if db := meta["database"]; db != nil {
		if !pyjson.Equal(adoption.Lookup("database_uuid"), db.Lookup("object_id")) {
			return provisionErr("pre-existing managed database requires explicit matching adoption UUID")
		}
	} else if pyjson.Truthy(adoption.Lookup("database_uuid")) {
		return provisionErr("adoption database UUID does not exist in ClickHouse")
	}
	if table := meta["table"]; table != nil {
		if !hasStableUUID(table.Lookup("object_id")) {
			return provisionErr("managed table adoption requires a non-nil ClickHouse UUID")
		}
		fingerprint, err := schemaFingerprint(p.Client, d, p.Schema)
		if err != nil {
			return err
		}
		if !pyjson.Equal(adoption.Lookup("table_uuid"), table.Lookup("object_id")) ||
			!pyjson.Equal(adoption.Lookup("schema_fingerprint"), fingerprint) {
			return provisionErr("pre-existing managed table requires explicit matching adoption UUID and schema fingerprint")
		}
	} else if pyjson.Truthy(adoption.Lookup("table_uuid")) || pyjson.Truthy(adoption.Lookup("schema_fingerprint")) {
		return provisionErr("adoption table identity does not exist in ClickHouse")
	}
	if meta["table"] == nil && !d.CreateTable {
		return provisionErr("managed events table is missing and create_table is disabled")
	}
	return nil
}

func (p *Provisioner) recordFor(d *Dataset) (*pyjson.Dict, error) {
	records := p.records()
	if r, ok := records.Lookup(d.ID).(*pyjson.Dict); ok {
		return r, nil
	}
	record := pyjson.NewDict()
	record.Set("database", d.Database)
	record.Set("table", d.Table)
	record.Set("role", d.MCPRole)
	record.Set("user", d.MCPUser)
	record.Set("privileges", pyjson.List{"SELECT"})
	record.Set("schema_fingerprint", p.Schema.Fingerprint)
	record.Set("object_ids", pyjson.NewDict())
	record.Set("password_revision", d.PasswordRevision)
	record.Set("state", "pending")
	record.Set("pending_action", nil)
	records.Set(d.ID, record)
	db, err := objectMetadata(p.Client, d.names(), "database")
	if err != nil {
		return nil, err
	}
	if db != nil && pyjson.Equal(d.Adopt.Lookup("database_uuid"), db.Lookup("object_id")) {
		objectIDs(record).Set("database", pyjson.Str(db.Lookup("object_id")))
	}
	table, err := objectMetadata(p.Client, d.names(), "table")
	if err != nil {
		return nil, err
	}
	if table != nil && pyjson.Equal(d.Adopt.Lookup("table_uuid"), table.Lookup("object_id")) {
		objectIDs(record).Set("table", pyjson.Str(table.Lookup("object_id")))
	}
	if err := p.save(); err != nil {
		return nil, err
	}
	p.Changed = true
	return record, nil
}

func (p *Provisioner) setPending(record *pyjson.Dict, action string) error {
	record.Set("pending_action", action)
	return p.save()
}

func (p *Provisioner) confirmCreated(record *pyjson.Dict, kind string, metadata *pyjson.Dict) error {
	objectIDs(record).Set(kind, pyjson.Str(metadata.Lookup("object_id")))
	record.Set("pending_action", nil)
	return p.save()
}

func (p *Provisioner) ensureDatabase(d *Dataset, record *pyjson.Dict) error {
	metadata, err := objectMetadata(p.Client, d.names(), "database")
	if err != nil {
		return err
	}
	if err := AssertRecordedID(record, "database", metadata); err != nil {
		return err
	}
	if metadata != nil {
		return nil
	}
	if err := p.setPending(record, "create_database"); err != nil {
		return err
	}
	q, err := quoteIdentifier(d.Database)
	if err != nil {
		return err
	}
	if err := p.Client.Execute("CREATE DATABASE " + q + " ENGINE = Atomic"); err != nil {
		return err
	}
	p.Changed = true
	if metadata, err = objectMetadata(p.Client, d.names(), "database"); err != nil {
		return err
	}
	if metadata == nil {
		return provisionErr("ClickHouse did not confirm database creation")
	}
	return p.confirmCreated(record, "database", metadata)
}

func (p *Provisioner) ensureTable(d *Dataset, record *pyjson.Dict) error {
	metadata, err := objectMetadata(p.Client, d.names(), "table")
	if err != nil {
		return err
	}
	if err := AssertRecordedID(record, "table", metadata); err != nil {
		return err
	}
	if metadata != nil {
		ok, err := schemaMatches(p.Client, d, p.Schema)
		if err != nil {
			return err
		}
		if !ok {
			return provisionErr("owned managed events table no longer matches the canonical schema")
		}
		return nil
	}
	if !d.CreateTable {
		return provisionErr("managed events table is missing and create_table is disabled")
	}
	if err := p.setPending(record, "create_table"); err != nil {
		return err
	}
	if _, err := quoteIdentifier(d.Database); err != nil {
		return err
	}
	if _, err := quoteIdentifier(d.Table); err != nil {
		return err
	}
	createSQL := p.Schema.bindCreateTable(d.Database, d.Table)
	if strings.Contains(createSQL, "analytics.events") {
		return provisionErr("failed to bind canonical schema to the configured dataset")
	}
	if err := p.Client.Execute(createSQL); err != nil {
		return err
	}
	p.Changed = true
	if metadata, err = objectMetadata(p.Client, d.names(), "table"); err != nil {
		return err
	}
	matches := false
	if metadata != nil {
		if matches, err = schemaMatches(p.Client, d, p.Schema); err != nil {
			return err
		}
	}
	if !matches {
		return provisionErr("ClickHouse did not confirm canonical events table creation")
	}
	return p.confirmCreated(record, "table", metadata)
}

func userSettings(d *Dataset) string {
	parts := []string{"readonly = 1"}
	if v, ok := d.Limits["max_execution_seconds"]; ok {
		parts = append(parts, "max_execution_time = "+v.String())
	}
	if v, ok := d.Limits["max_query_rows"]; ok {
		parts = append(parts, "max_result_rows = "+v.String(), "result_overflow_mode = 'break'")
	}
	return "SETTINGS " + strings.Join(parts, ", ")
}

func expectedUserSettings(d *Dataset) [][2]string {
	expected := [][2]string{{"readonly", "1"}}
	if v, ok := d.Limits["max_execution_seconds"]; ok {
		expected = append(expected, [2]string{"max_execution_time", v.String()})
	}
	if v, ok := d.Limits["max_query_rows"]; ok {
		expected = append(expected, [2]string{"max_result_rows", v.String()}, [2]string{"result_overflow_mode", "break"})
	}
	return expected
}

func (p *Provisioner) userSettingsApplied(d *Dataset) (bool, error) {
	rows, err := p.Client.Query("SELECT setting_name, value FROM system.settings_profile_elements " +
		"WHERE user_name = " + quoteString(d.MCPUser) + " AND role_name IS NULL")
	if err != nil {
		return false, err
	}
	actual := map[string]string{}
	for _, r := range rows {
		actual[pyjson.Str(r.Lookup("setting_name"))] = pyjson.Str(r.Lookup("value"))
	}
	for _, kv := range expectedUserSettings(d) {
		if v, ok := actual[kv[0]]; !ok || v != kv[1] {
			return false, nil
		}
	}
	return true, nil
}

func (p *Provisioner) ensureAccessIdentity(d *Dataset, record *pyjson.Dict) error {
	roleMeta, err := objectMetadata(p.Client, d.names(), "role")
	if err != nil {
		return err
	}
	userMeta, err := objectMetadata(p.Client, d.names(), "user")
	if err != nil {
		return err
	}
	if err := AssertRecordedID(record, "role", roleMeta); err != nil {
		return err
	}
	if err := AssertRecordedID(record, "user", userMeta); err != nil {
		return err
	}
	role := quoteIdentifierUnchecked(d.MCPRole)
	user := quoteIdentifierUnchecked(d.MCPUser)
	if roleMeta == nil {
		if err := p.setPending(record, "create_role"); err != nil {
			return err
		}
		if err := p.Client.Execute("CREATE ROLE " + role); err != nil {
			return err
		}
		p.Changed = true
		if roleMeta, err = objectMetadata(p.Client, d.names(), "role"); err != nil {
			return err
		}
		if roleMeta == nil {
			return provisionErr("ClickHouse did not confirm role creation")
		}
		if err := p.confirmCreated(record, "role", roleMeta); err != nil {
			return err
		}
	}
	if userMeta == nil {
		if err := p.setPending(record, "create_user"); err != nil {
			return err
		}
		password, err := requiredString(d.MCPPassword, "managed MCP password")
		if err != nil {
			return err
		}
		if err := p.Client.Execute("CREATE USER " + user + " " +
			"IDENTIFIED WITH sha256_password BY " + quoteString(password) + " " + userSettings(d)); err != nil {
			return err
		}
		p.Changed = true
		if userMeta, err = objectMetadata(p.Client, d.names(), "user"); err != nil {
			return err
		}
		if userMeta == nil {
			return provisionErr("ClickHouse did not confirm user creation")
		}
		objectIDs(record).Set("user", pyjson.Str(userMeta.Lookup("object_id")))
		record.Set("password_revision", d.PasswordRevision)
		record.Set("pending_action", nil)
		if err := p.save(); err != nil {
			return err
		}
	} else {
		applied, err := p.userSettingsApplied(d)
		if err != nil {
			return err
		}
		if !applied {
			if err := p.mutate(record, "alter_user_settings", "ALTER USER "+user+" "+userSettings(d), nil); err != nil {
				return err
			}
		}
		if !pyjson.Equal(record.Lookup("password_revision"), d.PasswordRevision) {
			password, err := requiredString(d.MCPPassword, "managed MCP password")
			if err != nil {
				return err
			}
			if err := p.mutate(record, "rotate_user_password",
				"ALTER USER "+user+" IDENTIFIED WITH sha256_password BY "+quoteString(password),
				map[string]pyjson.Value{"password_revision": d.PasswordRevision}); err != nil {
				return err
			}
		}
	}
	return p.ReconcileRoleAndUser(d, record)
}

func (p *Provisioner) roleAssignments(d *Dataset) ([]*pyjson.Dict, error) {
	return p.Client.Query("SELECT user_name, role_name, granted_role_name, granted_role_is_default, with_admin_option " +
		"FROM system.role_grants " +
		"WHERE user_name = " + quoteString(d.MCPUser) + " OR role_name = " + quoteString(d.MCPRole) + " " +
		"OR granted_role_name = " + quoteString(d.MCPRole))
}

// ReconcileRoleAndUser mirrors _reconcile_role_and_user.
func (p *Provisioner) ReconcileRoleAndUser(d *Dataset, record *pyjson.Dict) error {
	role, user, database, table := d.MCPRole, d.MCPUser, d.Database, d.Table
	assigned, err := p.roleAssignments(d)
	if err != nil {
		return err
	}
	records := p.records()
	managedRoles := map[string]bool{}
	for _, id := range records.Keys() {
		if r, ok := records.Lookup(id).(*pyjson.Dict); ok {
			if name, ok := r.Lookup("role").(string); ok {
				managedRoles[name] = true
			}
		}
	}
	for _, a := range assigned {
		assignedRole := a.Lookup("granted_role_name")
		if pyjson.Equal(a.Lookup("user_name"), user) && pyjson.Equal(assignedRole, role) && a.Lookup("role_name") == nil && isInt(a.Lookup("with_admin_option"), 0) {
			continue
		}
		roleName, isStr := assignedRole.(string)
		if !isStr || !managedRoles[roleName] {
			return provisionErr("managed user or role has an unowned role assignment; resolve manually")
		}
		var managedRecord *pyjson.Dict
		for _, id := range records.Keys() {
			if r, ok := records.Lookup(id).(*pyjson.Dict); ok && pyjson.Equal(r.Lookup("role"), roleName) {
				managedRecord = r
				break
			}
		}
		roleMeta, err := singleRow(p.Client, "SELECT toString(id) AS object_id FROM system.roles "+
			"WHERE name = "+quoteString(roleName))
		if err != nil {
			return err
		}
		if roleMeta == nil || pyjson.Str(roleMeta.Lookup("object_id")) != pyjson.Str(objectIDs(managedRecord).Lookup("role")) {
			return provisionErr("unexpected role assignment does not match a recorded managed identity")
		}
		var grantee string
		if _, ok := p.recordedEntity("user", pyjson.Str(a.Lookup("user_name"))); a.Lookup("user_name") != nil && ok {
			if err := p.identityObjectID("user", pyjson.Str(a.Lookup("user_name"))); err != nil {
				return err
			}
			grantee, err = quoteIdentifier(pyjson.Str(a.Lookup("user_name")))
		} else if _, ok := p.recordedEntity("role", pyjson.Str(a.Lookup("role_name"))); a.Lookup("role_name") != nil && ok {
			if err := p.identityObjectID("role", pyjson.Str(a.Lookup("role_name"))); err != nil {
				return err
			}
			grantee, err = quoteIdentifier(pyjson.Str(a.Lookup("role_name")))
		} else {
			return provisionErr("managed role is assigned to an unowned ClickHouse principal")
		}
		if err != nil {
			return err
		}
		quotedRole, err := quoteIdentifier(roleName)
		if err != nil {
			return err
		}
		if err := p.mutate(record, "revoke_managed_role_assignment", "REVOKE "+quotedRole+" FROM "+grantee, nil); err != nil {
			return err
		}
	}
	remaining, err := p.roleAssignments(d)
	if err != nil {
		return err
	}
	exact := func(r *pyjson.Dict) bool {
		return pyjson.Equal(r.Lookup("user_name"), user) && r.Lookup("role_name") == nil && pyjson.Equal(r.Lookup("granted_role_name"), role)
	}
	for _, r := range remaining {
		if exact(r) && !isInt(r.Lookup("with_admin_option"), 0) {
			return provisionErr("managed role assignment retained ADMIN OPTION after revocation")
		}
	}
	hasAssignment := false
	for _, r := range remaining {
		if exact(r) && isInt(r.Lookup("with_admin_option"), 0) {
			hasAssignment = true
			break
		}
	}
	quotedRole, quotedUser := quoteIdentifierUnchecked(role), quoteIdentifierUnchecked(user)
	if !hasAssignment {
		if err := p.mutate(record, "grant_dataset_role_to_user", "GRANT "+quotedRole+" TO "+quotedUser, nil); err != nil {
			return err
		}
	}
	userMeta, err := singleRow(p.Client, "SELECT default_roles_all, default_roles_list, default_roles_except "+
		"FROM system.users "+
		"WHERE name = "+quoteString(user))
	if err != nil {
		return err
	}
	if userMeta == nil || !isInt(userMeta.Lookup("default_roles_all"), 0) ||
		!pyjson.Equal(userMeta.Lookup("default_roles_list"), pyjson.List{role}) ||
		!pyjson.Equal(userMeta.Lookup("default_roles_except"), pyjson.List{}) {
		if err := p.mutate(record, "set_default_dataset_role", "SET DEFAULT ROLE "+quotedRole+" TO "+quotedUser, nil); err != nil {
			return err
		}
	}

	grants, err := p.Client.Query("SELECT user_name, role_name, access_type, database, table, column, " +
		"is_partial_revoke, grant_option FROM system.grants " +
		"WHERE role_name = " + quoteString(role) + " OR user_name = " + quoteString(user))
	if err != nil {
		return err
	}
	extra := map[[3]string]bool{}
	for _, g := range grants {
		expected := g.Lookup("user_name") == nil && pyjson.Equal(g.Lookup("role_name"), role) &&
			pyjson.Equal(g.Lookup("access_type"), "SELECT") &&
			pyjson.Equal(g.Lookup("database"), database) && pyjson.Equal(g.Lookup("table"), table) &&
			inValues(g.Lookup("column"), nil, "") && isInt(g.Lookup("is_partial_revoke"), 0) && isInt(g.Lookup("grant_option"), 0)
		if expected {
			continue
		}
		grantDB, grantTable, err := grantScope(g)
		if err != nil {
			return err
		}
		var principal string
		switch {
		case pyjson.Equal(g.Lookup("role_name"), role):
			principal = role
		case pyjson.Equal(g.Lookup("user_name"), user):
			principal = user
		default:
			return provisionErr("managed grant metadata contains an unexpected principal")
		}
		extra[[3]string{principal, grantDB, grantTable}] = true
	}
	scopes := make([][3]string, 0, len(extra))
	for s := range extra {
		scopes = append(scopes, s)
	}
	sort.Slice(scopes, func(i, j int) bool {
		for k := 0; k < 3; k++ {
			if scopes[i][k] != scopes[j][k] {
				return scopes[i][k] < scopes[j][k]
			}
		}
		return false
	})
	for _, s := range scopes {
		var scope string
		switch {
		case s[1] == "*":
			scope = "*.*"
		case s[2] == "*":
			scope = quoteIdentifierUnchecked(s[1]) + ".*"
		default:
			scope = quoteIdentifierUnchecked(s[1]) + "." + quoteIdentifierUnchecked(s[2])
		}
		if err := p.mutate(record, "revoke_obsolete_managed_grant", "REVOKE ALL ON "+scope+" FROM "+quoteIdentifierUnchecked(s[0]), nil); err != nil {
			return err
		}
	}
	remainingGrants, err := p.Client.Query("SELECT access_type, database, table, is_partial_revoke FROM system.grants " +
		"WHERE role_name = " + quoteString(role))
	if err != nil {
		return err
	}
	stable := len(remainingGrants) == 1 &&
		pyjson.Equal(remainingGrants[0].Lookup("access_type"), "SELECT") &&
		pyjson.Equal(remainingGrants[0].Lookup("database"), database) &&
		pyjson.Equal(remainingGrants[0].Lookup("table"), table) &&
		isInt(remainingGrants[0].Lookup("is_partial_revoke"), 0)
	if !stable {
		if err := p.mutate(record, "grant_dataset_select", "GRANT SELECT ON "+quoteIdentifierUnchecked(database)+"."+
			quoteIdentifierUnchecked(table)+" TO "+quotedRole, nil); err != nil {
			return err
		}
	}
	return VerifyExactManagedAccess(p.Client, d)
}

// Provision mirrors DatasetProvisioner.provision.
func (p *Provisioner) Provision(datasets []*Dataset, runtimeByID map[string]RuntimeDataset) error {
	for _, d := range datasets {
		if !d.Enabled || d.Mode == "reference" {
			continue
		}
		record, err := p.recordFor(d)
		if err != nil {
			return err
		}
		if err := p.ensureDatabase(d, record); err != nil {
			return err
		}
		if err := p.ensureTable(d, record); err != nil {
			return err
		}
		if err := p.ensureAccessIdentity(d, record); err != nil {
			return err
		}
		if _, err := schemaFingerprint(p.Client, d, p.Schema); err != nil {
			return err
		}
		if err := VerifyExactManagedAccess(p.Client, d); err != nil {
			return err
		}
		if _, err := p.Restricted(runtimeByID[d.ID], p.Admin).Query(probeSQL(d)); err != nil {
			return err
		}
		if !pyjson.Equal(record.Lookup("state"), "applied") || record.Lookup("pending_action") != nil ||
			!pyjson.Equal(record.Lookup("schema_fingerprint"), p.Schema.Fingerprint) {
			record.Set("state", "applied")
			record.Set("pending_action", nil)
			record.Set("schema_fingerprint", p.Schema.Fingerprint)
			if err := p.save(); err != nil {
				return err
			}
			p.Changed = true
		}
	}

	targets := map[[2]string]bool{}
	existing, _ := getDefault(p.Journal, "main_access_targets", pyjson.List{}).(pyjson.List)
	for _, item := range existing {
		if t, ok := item.(*pyjson.Dict); ok {
			targets[[2]string{pyjson.Str(t.Lookup("database")), pyjson.Str(t.Lookup("table"))}] = true
		}
	}
	records := p.records()
	for _, id := range records.Keys() {
		if r, ok := records.Lookup(id).(*pyjson.Dict); ok {
			targets[[2]string{pyjson.Str(r.Lookup("database")), pyjson.Str(r.Lookup("table"))}] = true
		}
	}
	for _, d := range datasets {
		targets[[2]string{d.Database, d.Table}] = true
	}
	pairs := make([][2]string, 0, len(targets))
	for t := range targets {
		pairs = append(pairs, t)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	serialized := make(pyjson.List, len(pairs))
	for i, pr := range pairs {
		t := pyjson.NewDict()
		t.Set("database", pr[0])
		t.Set("table", pr[1])
		serialized[i] = t
	}
	if !pyjson.Equal(serialized, getDefault(p.Journal, "main_access_targets", pyjson.List{})) {
		p.Journal.Set("main_access_targets", serialized)
		if err := p.save(); err != nil {
			return err
		}
		p.Changed = true
	}
	return nil
}
