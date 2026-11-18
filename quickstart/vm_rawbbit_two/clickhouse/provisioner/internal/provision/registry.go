package provision

import (
	"math/big"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/clickhouse/provisioner/internal/pyjson"
)

// AdminConnection is the loopback-only privileged ClickHouse endpoint.
type AdminConnection struct {
	Host           string
	Port           int64
	Username       string
	Password       string
	TimeoutSeconds int64
}

// Dataset is one validated provisioning registry entry.
type Dataset struct {
	ID               string
	Enabled          bool
	Mode             string // "managed" or "reference"
	Database         string
	Table            string
	EndpointPath     string
	CreateTable      bool
	MCPUser          string
	MCPRole          string
	PasswordRevision *big.Int
	MainExposed      bool
	Limits           map[string]*big.Int
	Adopt            *pyjson.Dict
	// MCPPassword is the raw "mcp_password" value (nil when absent).
	MCPPassword pyjson.Value
}

var limitNames = []string{"max_query_rows", "max_sample_rows", "max_execution_seconds"}

var (
	registryFields = set("version", "admin", "main_database", "main_user", "datasets")
	adminFields    = set("host", "port", "username", "password", "timeout_seconds")
	datasetFields  = set("id", "enabled", "mode", "endpoint_path", "database", "table", "create_table",
		"main_exposed", "mcp_user", "mcp_role", "password_revision", "mcp_password", "limits", "adopt")
	limitFields    = set(limitNames...)
	adoptionFields = set("database_uuid", "table_uuid", "schema_fingerprint")
	runtimeFields  = set("version", "datasets")
	runtimeDataset = set("id", "enabled", "endpoint_path", "database", "table", "main_exposed",
		"clickhouse_user", "clickhouse_password", "bearer_tokens", "max_query_rows",
		"max_sample_rows", "max_execution_seconds")
)

func getDefault(d *pyjson.Dict, key string, def pyjson.Value) pyjson.Value {
	if v, ok := d.Get(key); ok {
		return v
	}
	return def
}

func strictVersionOne(v pyjson.Value) bool {
	n, ok := pyjson.IsInt(v)
	return ok && n.IsInt64() && n.Int64() == 1
}

// ValidateRegistry mirrors _validate_registry.
func ValidateRegistry(config *pyjson.Dict) (*AdminConnection, []*Dataset, error) {
	if err := rejectUnknownFields(config, registryFields, "provisioning registry"); err != nil {
		return nil, nil, err
	}
	if !strictVersionOne(config.Lookup("version")) {
		return nil, nil, provisionErr("unsupported provisioning registry version")
	}
	mainDatabase, err := identifier(config.Lookup("main_database"), "main database")
	if err != nil {
		return nil, nil, err
	}
	mainUser, err := identifier(config.Lookup("main_user"), "main MCP user")
	if err != nil {
		return nil, nil, err
	}
	admin, okAdmin := config.Lookup("admin").(*pyjson.Dict)
	rawDatasets, okDatasets := config.Lookup("datasets").(pyjson.List)
	if !okAdmin || !okDatasets {
		return nil, nil, provisionErr("provisioning registry requires admin and datasets fields")
	}
	if err := rejectUnknownFields(admin, adminFields, "admin config"); err != nil {
		return nil, nil, err
	}
	host, err := requiredString(admin.Lookup("host"), "admin host")
	if err != nil {
		return nil, nil, err
	}
	if host != "127.0.0.1" && host != "localhost" {
		return nil, nil, provisionErr("admin host must be localhost so credentials remain on the host")
	}
	username, err := identifier(admin.Lookup("username"), "admin username")
	if err != nil {
		return nil, nil, err
	}
	password, err := requiredString(admin.Lookup("password"), "admin password")
	if err != nil {
		return nil, nil, err
	}
	if mainUser == username {
		return nil, nil, provisionErr("main MCP user must differ from the privileged admin user")
	}
	port, ok := intInRange(getDefault(admin, "port", pyjson.Int(8123)), 1, 65535)
	if !ok {
		return nil, nil, provisionErr("admin port must be an integer from 1 to 65535")
	}
	timeout, ok := intInRange(getDefault(admin, "timeout_seconds", pyjson.Int(15)), 1, 120)
	if !ok {
		return nil, nil, provisionErr("admin timeout_seconds must be an integer from 1 to 120")
	}
	conn := &AdminConnection{Host: host, Port: port.Int64(), Username: username, Password: password, TimeoutSeconds: timeout.Int64()}

	var out []*Dataset
	ids, databases, roles, users := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	names := map[[2]string]bool{}
	for _, item := range rawDatasets {
		raw, ok := item.(*pyjson.Dict)
		if !ok {
			return nil, nil, provisionErr("each dataset entry must be an object")
		}
		d, err := validateDataset(raw, mainDatabase, mainUser, username, ids, databases, roles, users, names)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, d)
	}
	return conn, out, nil
}

func validateDataset(raw *pyjson.Dict, mainDatabase, mainUser, adminUser string,
	ids, databases, roles, users map[string]bool, names map[[2]string]bool) (*Dataset, error) {
	if err := rejectUnknownFields(raw, datasetFields, "dataset config"); err != nil {
		return nil, err
	}
	id, err := datasetID(raw.Lookup("id"))
	if err != nil {
		return nil, err
	}
	if ids[id] {
		return nil, provisionErr("dataset ids must be unique")
	}
	ids[id] = true
	enabled, ok := getDefault(raw, "enabled", true).(bool)
	if !ok {
		return nil, provisionErr("dataset enabled must be a boolean")
	}
	mode, ok := raw.Lookup("mode").(string)
	if !ok || (mode != "managed" && mode != "reference") {
		return nil, provisionErr("dataset mode must be managed or reference")
	}
	database, err := identifier(raw.Lookup("database"), "database")
	if err != nil {
		return nil, err
	}
	if database == mainDatabase {
		return nil, provisionErr("dataset database must differ from the main ClickHouse database")
	}
	table, err := identifier(getDefault(raw, "table", "events"), "table")
	if err != nil {
		return nil, err
	}
	if databases[database] {
		return nil, provisionErr("each managed dataset must use a distinct database")
	}
	databases[database] = true
	if names[[2]string{database, table}] {
		return nil, provisionErr("each dataset must target a unique database and table")
	}
	names[[2]string{database, table}] = true
	endpointPath, err := requiredString(raw.Lookup("endpoint_path"), "endpoint_path")
	if err != nil {
		return nil, err
	}
	if !validEndpointPath(endpointPath, id) {
		return nil, provisionErr("dataset endpoint_path must use /datasets/<id>/<MCP_PATH>")
	}
	createTable, ok := getDefault(raw, "create_table", false).(bool)
	if !ok {
		return nil, provisionErr("create_table must be a boolean")
	}
	if mode == "reference" && createTable {
		return nil, provisionErr("reference datasets cannot request table creation")
	}
	mcpUser, err := identifier(raw.Lookup("mcp_user"), "MCP user")
	if err != nil {
		return nil, err
	}
	mcpRole, err := identifier(raw.Lookup("mcp_role"), "MCP role")
	if err != nil {
		return nil, err
	}
	if mcpUser == adminUser {
		return nil, provisionErr("dataset MCP users must be distinct from the privileged admin user")
	}
	if mcpUser == mainUser {
		return nil, provisionErr("dataset MCP users must be distinct from the main MCP user")
	}
	if users[mcpUser] || roles[mcpRole] {
		return nil, provisionErr("MCP user and role names must be unique")
	}
	users[mcpUser], roles[mcpRole] = true, true
	revision, ok := positiveInt(getDefault(raw, "password_revision", pyjson.Int(1)))
	if !ok {
		return nil, provisionErr("password_revision must be a positive integer")
	}
	mainExposed, ok := getDefault(raw, "main_exposed", false).(bool)
	if !ok {
		return nil, provisionErr("main_exposed must be a boolean")
	}
	limits, ok := getDefault(raw, "limits", pyjson.NewDict()).(*pyjson.Dict)
	if !ok {
		return nil, provisionErr("dataset limits must be an object")
	}
	if err := rejectUnknownFields(limits, limitFields, "dataset limits"); err != nil {
		return nil, err
	}
	checkedLimits := map[string]*big.Int{}
	for _, name := range limitNames {
		if v := limits.Lookup(name); v != nil {
			n, ok := positiveInt(v)
			if !ok {
				return nil, provisionErr(name + " must be a positive integer")
			}
			checkedLimits[name] = n
		}
	}
	adoption, ok := getDefault(raw, "adopt", pyjson.NewDict()).(*pyjson.Dict)
	if !ok {
		return nil, provisionErr("adopt must be an object")
	}
	if err := rejectUnknownFields(adoption, adoptionFields, "adoption config"); err != nil {
		return nil, err
	}
	for _, name := range []string{"database_uuid", "table_uuid", "schema_fingerprint"} {
		if v := adoption.Lookup(name); v != nil {
			if s, ok := v.(string); !ok || pyjson.Strip(s) == "" {
				return nil, provisionErr("adopt." + name + " must be a non-empty string")
			}
		}
	}
	if mode == "reference" {
		for _, k := range adoption.Keys() {
			if pyjson.Truthy(adoption.Lookup(k)) {
				return nil, provisionErr("reference datasets do not use managed-object adoption")
			}
		}
	}
	if enabled && mode == "managed" {
		if _, err := requiredString(raw.Lookup("mcp_password"), "managed MCP password"); err != nil {
			return nil, err
		}
	}
	return &Dataset{
		ID: id, Enabled: enabled, Mode: mode, Database: database, Table: table, EndpointPath: endpointPath,
		CreateTable: createTable, MCPUser: mcpUser, MCPRole: mcpRole, PasswordRevision: revision,
		MainExposed: mainExposed, Limits: checkedLimits, Adopt: adoption, MCPPassword: raw.Lookup("mcp_password"),
	}, nil
}

// RuntimeDataset is one validated runtime (MCP) registry entry.
type RuntimeDataset = *pyjson.Dict

// ValidateRuntime mirrors _validate_runtime; the result keeps file order.
func ValidateRuntime(runtime *pyjson.Dict, mainDatabase string) (map[string]RuntimeDataset, []string, error) {
	if err := rejectUnknownFields(runtime, runtimeFields, "runtime registry"); err != nil {
		return nil, nil, err
	}
	list, isList := runtime.Lookup("datasets").(pyjson.List)
	if !strictVersionOne(runtime.Lookup("version")) || !isList {
		return nil, nil, provisionErr("runtime registry version or datasets field is invalid")
	}
	if _, err := identifier(mainDatabase, "main database"); err != nil {
		return nil, nil, err
	}
	byID := map[string]RuntimeDataset{}
	var order []string
	tokens, databases, users := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, item := range list {
		d, ok := item.(*pyjson.Dict)
		if !ok {
			return nil, nil, provisionErr("runtime dataset entry must be an object")
		}
		if err := rejectUnknownFields(d, runtimeDataset, "runtime dataset config"); err != nil {
			return nil, nil, err
		}
		id, err := datasetID(d.Lookup("id"))
		if err != nil {
			return nil, nil, err
		}
		if _, dup := byID[id]; dup || d.Lookup("enabled") != true {
			return nil, nil, provisionErr("runtime registry must contain unique enabled datasets only")
		}
		database, err := identifier(d.Lookup("database"), "runtime database")
		if err != nil {
			return nil, nil, err
		}
		if database == mainDatabase {
			return nil, nil, provisionErr("runtime dataset database must differ from the main ClickHouse database")
		}
		if databases[database] {
			return nil, nil, provisionErr("each runtime dataset must use a distinct ClickHouse database")
		}
		databases[database] = true
		if _, err := identifier(getDefault(d, "table", "events"), "runtime table"); err != nil {
			return nil, nil, err
		}
		user, err := identifier(d.Lookup("clickhouse_user"), "runtime ClickHouse user")
		if err != nil {
			return nil, nil, err
		}
		if users[user] {
			return nil, nil, provisionErr("runtime datasets must use distinct ClickHouse users")
		}
		users[user] = true
		path, err := requiredString(d.Lookup("endpoint_path"), "runtime endpoint_path")
		if err != nil {
			return nil, nil, err
		}
		if !validEndpointPath(path, id) {
			return nil, nil, provisionErr("runtime endpoint_path must use /datasets/<id>/<MCP_PATH>")
		}
		if _, ok := d.Lookup("main_exposed").(bool); !ok {
			return nil, nil, provisionErr("runtime main_exposed must be a boolean")
		}
		if pw, ok := d.Lookup("clickhouse_password").(string); !ok || pyjson.Strip(pw) == "" {
			return nil, nil, provisionErr("runtime dataset ClickHouse password is required")
		}
		bearer, ok := d.Lookup("bearer_tokens").(*pyjson.Dict)
		if !ok || bearer.Len() == 0 {
			return nil, nil, provisionErr("runtime dataset requires scoped bearer tokens")
		}
		for _, label := range bearer.Keys() {
			token, isStr := bearer.Lookup(label).(string)
			if !tokenLabelRe.MatchString(label) || !isStr || pyjson.Strip(token) == "" || token != pyjson.Strip(token) {
				return nil, nil, provisionErr("runtime scoped bearer token labels and values must be non-empty")
			}
			if tokens[token] {
				return nil, nil, provisionErr("runtime scoped bearer token values must be unique")
			}
			tokens[token] = true
		}
		for _, name := range limitNames {
			if v := d.Lookup(name); v != nil {
				if _, ok := positiveInt(v); !ok {
					return nil, nil, provisionErr("runtime " + name + " must be a positive integer")
				}
			}
		}
		byID[id] = d
		order = append(order, id)
	}
	return byID, order, nil
}
