package provision

import (
	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

var (
	journalFields = set("database", "table", "role", "user", "privileges", "schema_fingerprint",
		"object_ids", "password_revision", "state", "pending_action")
	journalObjectIDFields = set("database", "table", "role", "user")
	journalPendingActions = set("create_database", "create_table", "create_role", "create_user",
		"alter_user_settings", "rotate_user_password", "revoke_managed_role_assignment",
		"grant_dataset_role_to_user", "set_default_dataset_role",
		"revoke_obsolete_managed_grant", "grant_dataset_select")
	journalTopFields  = set("version", "datasets", "main_access_targets")
	accessTargetField = set("database", "table")
	selectOnly        = pyjson.List{"SELECT"}
)

// NewJournal is the empty ownership journal.
func NewJournal() *pyjson.Dict {
	j := pyjson.NewDict()
	j.Set("version", pyjson.Int(1))
	j.Set("datasets", pyjson.NewDict())
	return j
}

// ReadJournal mirrors _read_journal.
func ReadJournal(path string, requireRoot bool) (*pyjson.Dict, error) {
	exists, err := pathExists(pyPath(path))
	if err != nil {
		return nil, err
	}
	if !exists {
		return NewJournal(), nil
	}
	journal, err := SecureReadJSON(path, requireRoot)
	if err != nil {
		return nil, err
	}
	if err := ValidateJournal(journal); err != nil {
		return nil, err
	}
	return journal, nil
}

// ValidateJournal checks a parsed journal document.
func ValidateJournal(journal *pyjson.Dict) error {
	datasets, ok := journal.Lookup("datasets").(*pyjson.Dict)
	if !strictVersionOne(journal.Lookup("version")) || !ok {
		return provisionErr("ownership journal has an unsupported or invalid format")
	}
	if err := rejectUnknownFields(journal, journalTopFields, "ownership journal"); err != nil {
		return err
	}
	managed := map[string]string{}
	for _, id := range datasets.Keys() {
		if _, err := datasetID(id); err != nil {
			return err
		}
		record, ok := datasets.Lookup(id).(*pyjson.Dict)
		if !ok {
			return provisionErr("ownership journal contains an invalid dataset record")
		}
		if err := rejectUnknownFields(record, journalFields, "ownership journal dataset"); err != nil {
			return err
		}
		for _, name := range []string{"database", "table", "role", "user"} {
			value, err := identifier(record.Lookup(name), "ownership journal "+name)
			if err != nil {
				return err
			}
			if name == "database" {
				if _, dup := managed[value]; dup {
					return provisionErr("ownership journal assigns a database to multiple dataset IDs")
				}
				managed[value] = id
			}
		}
		if !pyjson.Equal(record.Lookup("privileges"), selectOnly) {
			return provisionErr("ownership journal has an unsupported managed privilege set")
		}
		if fp, ok := record.Lookup("schema_fingerprint").(string); !ok || !sha256Re.MatchString(fp) {
			return provisionErr("ownership journal has an invalid schema fingerprint")
		}
		objectIDs, ok := record.Lookup("object_ids").(*pyjson.Dict)
		if !ok || rejectUnknownFields(objectIDs, journalObjectIDFields, "") != nil {
			return provisionErr("ownership journal has an invalid object identity map")
		}
		for _, k := range objectIDs.Keys() {
			if s, ok := objectIDs.Lookup(k).(string); !ok || pyjson.Strip(s) == "" {
				return provisionErr("ownership journal contains an invalid ClickHouse object ID")
			}
		}
		if _, ok := positiveInt(record.Lookup("password_revision")); !ok {
			return provisionErr("ownership journal has an invalid password revision")
		}
		state, ok := record.Lookup("state").(string)
		if !ok || (state != "pending" && state != "applied") {
			return provisionErr("ownership journal has an invalid provisioning state")
		}
		pending := record.Lookup("pending_action")
		if pending != nil {
			if action, ok := pending.(string); !ok || !journalPendingActions[action] {
				return provisionErr("ownership journal has an unsupported pending operation")
			}
		}
		if state == "applied" && pending != nil {
			return provisionErr("ownership journal has an applied record with a pending operation")
		}
	}
	targets, ok := getDefault(journal, "main_access_targets", pyjson.List{}).(pyjson.List)
	if !ok {
		return provisionErr("ownership journal has invalid main-access target records")
	}
	seen := map[[2]string]bool{}
	for _, item := range targets {
		target, ok := item.(*pyjson.Dict)
		if !ok {
			return provisionErr("ownership journal has invalid main-access target records")
		}
		if err := rejectUnknownFields(target, accessTargetField, "ownership journal main-access target"); err != nil {
			return err
		}
		database, err := identifier(target.Lookup("database"), "ownership journal main-access database")
		if err != nil {
			return err
		}
		table, err := identifier(target.Lookup("table"), "ownership journal main-access table")
		if err != nil {
			return err
		}
		if seen[[2]string{database, table}] {
			return provisionErr("ownership journal has duplicate main-access target records")
		}
		seen[[2]string{database, table}] = true
	}
	return nil
}
