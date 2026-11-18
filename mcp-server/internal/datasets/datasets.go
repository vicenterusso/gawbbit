// Package datasets loads and validates the protected runtime registry of
// isolated dataset endpoints (MCP_DATASETS_FILE).
package datasets

import (
	"errors"
	"math/big"
	"os"
	"regexp"
	"strings"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

var (
	identifierRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	datasetIDRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	datasetPathRe = regexp.MustCompile(`^/datasets/[a-z][a-z0-9_]{0,39}(?:/[A-Za-z0-9_-]+)+$`)
	tokenLabelRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// ConfigurationError is a safe-to-report registry error (no secrets, no paths).
type ConfigurationError struct{ Msg string }

func (e *ConfigurationError) Error() string { return e.Msg }

// ValidationError is a registry contract violation (pydantic ValidationError
// in the original); its message is never shown outside tests.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Token is a labeled scoped bearer token.
type Token struct {
	Label string
	Value string
}

// Dataset is one enabled dataset's MCP runtime configuration.
type Dataset struct {
	ID                  string
	Enabled             bool
	EndpointPath        string
	Database            string
	Table               string
	MainExposed         bool
	ClickHouseUser      string
	ClickHousePassword  string
	BearerTokens        []Token
	MaxQueryRows        *int64
	MaxSampleRows       *int64
	MaxExecutionSeconds *int64
}

// Registry is the versioned runtime registry.
type Registry struct {
	Version  int64
	Datasets []*Dataset
}

// Empty is the registry used when MCP_DATASETS_FILE is unset.
func Empty() *Registry { return &Registry{Version: 1} }

var datasetFields = map[string]bool{
	"id": true, "enabled": true, "endpoint_path": true, "database": true, "table": true,
	"main_exposed": true, "clickhouse_user": true, "clickhouse_password": true,
	"bearer_tokens": true, "max_query_rows": true, "max_sample_rows": true,
	"max_execution_seconds": true,
}

func invalid(msg string) error { return &ValidationError{Msg: msg} }

// Parse validates a decoded registry document like DatasetRegistry.model_validate.
func Parse(doc pyval.Value) (*Registry, error) {
	obj, ok := doc.(*pyval.Dict)
	if !ok {
		return nil, invalid("registry must be an object")
	}
	for _, k := range obj.Keys {
		if k != "version" && k != "datasets" {
			return nil, invalid("extra fields not permitted")
		}
	}
	rawVersion, ok := obj.Get("version")
	if !ok {
		return nil, invalid("version is required")
	}
	version, ok := rawVersion.(*big.Int)
	if !ok {
		return nil, invalid("version must be a strict integer")
	}
	reg := &Registry{}
	var errs []string
	if raw, ok := obj.Get("datasets"); ok {
		list, ok := raw.(pyval.List)
		if !ok {
			return nil, invalid("datasets must be a list")
		}
		for _, item := range list {
			d, err := parseDataset(item)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			reg.Datasets = append(reg.Datasets, d)
		}
	}
	if len(errs) > 0 {
		return nil, invalid(strings.Join(errs, "\n"))
	}
	if !version.IsInt64() || version.Int64() != 1 {
		return nil, invalid("unsupported dataset registry version")
	}
	reg.Version = 1
	if err := reg.validateUniqueness(); err != nil {
		return nil, err
	}
	return reg, nil
}

func parseDataset(item pyval.Value) (*Dataset, error) {
	obj, ok := item.(*pyval.Dict)
	if !ok {
		return nil, invalid("dataset must be an object")
	}
	for _, k := range obj.Keys {
		if !datasetFields[k.(string)] {
			return nil, invalid("extra fields not permitted")
		}
	}
	d := &Dataset{Enabled: true, Table: "events"}
	var errs []string
	fail := func(msg string) { errs = append(errs, msg) }

	str := func(name string, required bool) (string, bool) {
		raw, ok := obj.Get(name)
		if !ok {
			if required {
				fail(name + " field required")
			}
			return "", false
		}
		s, ok := raw.(string)
		if !ok {
			fail(name + " must be a string")
			return "", false
		}
		return s, true
	}
	strictBool := func(name string, target *bool) {
		if raw, ok := obj.Get(name); ok {
			b, ok := raw.(bool)
			if !ok {
				fail(name + " must be a strict boolean")
				return
			}
			*target = b
		}
	}
	limit := func(name string) *int64 {
		raw, ok := obj.Get(name)
		if !ok || raw == nil {
			return nil
		}
		n, ok := raw.(*big.Int)
		if !ok || !n.IsInt64() {
			fail(name + " must be a strict integer")
			return nil
		}
		if n.Int64() < 1 {
			fail(name + " must be greater than or equal to 1")
			return nil
		}
		v := n.Int64()
		return &v
	}

	if id, ok := str("id", true); ok {
		switch {
		case id != pyval.Strip(id):
			fail("dataset id must not contain leading or trailing whitespace")
		case !datasetIDRe.MatchString(id):
			fail("dataset id must be a lowercase slug using letters, digits, and underscores")
		case id == "default":
			fail("dataset id 'default' is reserved for the existing main dataset")
		default:
			d.ID = id
		}
	}
	strictBool("enabled", &d.Enabled)
	if p, ok := str("endpoint_path", true); ok {
		switch {
		case p != pyval.Strip(p):
			fail("endpoint_path must not contain leading or trailing whitespace")
		case !strings.HasPrefix(p, "/") || p == "/" || strings.HasSuffix(p, "/"):
			fail("endpoint_path must be an absolute path without a trailing slash")
		case !datasetPathRe.MatchString(p):
			fail("endpoint_path must be normalized")
		default:
			d.EndpointPath = p
		}
	}
	for _, name := range []string{"database", "table", "clickhouse_user"} {
		v, ok := str(name, name != "table")
		if !ok {
			continue
		}
		switch {
		case v != pyval.Strip(v):
			fail("ClickHouse identifiers must not contain leading or trailing whitespace")
		case !identifierRe.MatchString(v):
			fail("database, table, and user names must be simple ClickHouse identifiers")
		default:
			switch name {
			case "database":
				d.Database = v
			case "table":
				d.Table = v
			default:
				d.ClickHouseUser = v
			}
		}
	}
	strictBool("main_exposed", &d.MainExposed)
	if pw, ok := str("clickhouse_password", true); ok {
		if pyval.Strip(pw) == "" {
			fail("clickhouse_password is required")
		} else {
			d.ClickHousePassword = pw
		}
	}
	if raw, ok := obj.Get("bearer_tokens"); !ok {
		fail("bearer_tokens field required")
	} else if tokens, ok := raw.(*pyval.Dict); !ok {
		fail("bearer_tokens must be an object")
	} else if err := d.setTokens(tokens); err != nil {
		fail(err.Error())
	}
	d.MaxQueryRows = limit("max_query_rows")
	d.MaxSampleRows = limit("max_sample_rows")
	d.MaxExecutionSeconds = limit("max_execution_seconds")

	if len(errs) > 0 {
		return nil, invalid(strings.Join(errs, "\n"))
	}
	if !d.Enabled {
		return nil, invalid("disabled datasets must not be present in the MCP runtime registry")
	}
	return d, nil
}

func (d *Dataset) setTokens(tokens *pyval.Dict) error {
	for i := range tokens.Values {
		if _, ok := tokens.Values[i].(string); !ok {
			return errors.New("scoped bearer tokens must be strings")
		}
	}
	if tokens.Len() == 0 {
		return errors.New("at least one scoped bearer token is required")
	}
	seen := map[string]bool{}
	for i, k := range tokens.Keys {
		label := k.(string)
		value := tokens.Values[i].(string)
		if !tokenLabelRe.MatchString(label) {
			return errors.New("scoped token labels must use letters, digits, dots, underscores, or hyphens")
		}
		if pyval.Strip(value) == "" || value != pyval.Strip(value) {
			return errors.New("scoped token labels and values must be non-empty")
		}
		if seen[value] {
			return errors.New("scoped bearer token values must be unique")
		}
		seen[value] = true
		d.BearerTokens = append(d.BearerTokens, Token{Label: label, Value: value})
	}
	return nil
}

func (r *Registry) validateUniqueness() error {
	ids, paths, users, databases := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	targets, tokens := map[string]bool{}, map[string]bool{}
	var dupID, dupPath, dupUser, dupTarget, dupDB, dupToken bool
	for _, d := range r.Datasets {
		dupID = dupID || ids[d.ID]
		dupPath = dupPath || paths[d.EndpointPath]
		dupUser = dupUser || users[d.ClickHouseUser]
		dupTarget = dupTarget || targets[d.Database+"\x00"+d.Table]
		dupDB = dupDB || databases[d.Database]
		ids[d.ID], paths[d.EndpointPath], users[d.ClickHouseUser] = true, true, true
		targets[d.Database+"\x00"+d.Table], databases[d.Database] = true, true
		for _, t := range d.BearerTokens {
			dupToken = dupToken || tokens[t.Value]
			tokens[t.Value] = true
		}
	}
	switch {
	case dupID:
		return invalid("dataset ids must be unique")
	case dupPath:
		return invalid("dataset endpoint paths must be unique")
	case dupUser:
		return invalid("each dataset must use a distinct ClickHouse user")
	case dupTarget:
		return invalid("each dataset must target a distinct ClickHouse table")
	case dupDB:
		return invalid("each dataset must use a distinct ClickHouse database")
	case dupToken:
		return invalid("scoped bearer token values must be unique across datasets")
	}
	return nil
}

// MainPath is MCP_PATH without a trailing slash ("/mcp" when that is empty).
func MainPath(cfg *settings.Settings) string {
	p := strings.TrimRight(cfg.MCPPath, "/")
	if p == "" {
		return "/mcp"
	}
	return p
}

// ValidateForSettings checks the registry against the main endpoint settings.
func (r *Registry) ValidateForSettings(cfg *settings.Settings) error {
	mainPath := MainPath(cfg)
	mainTokens := map[string]bool{}
	for _, t := range cfg.APIKeysByUser() {
		mainTokens[t.Token] = true
	}
	seenPrefixes := map[string]bool{}
	for _, d := range r.Datasets {
		if d.ClickHouseUser == cfg.ClickHouseUser {
			return &ConfigurationError{"dataset and main endpoints must use distinct ClickHouse users"}
		}
		if d.Database == cfg.ClickHouseDatabase {
			return &ConfigurationError{"dataset and main endpoints must use distinct ClickHouse databases"}
		}
		if d.EndpointPath == mainPath {
			return &ConfigurationError{"dataset endpoint path collides with the main MCP path"}
		}
		if !strings.HasSuffix(d.EndpointPath, mainPath) {
			return &ConfigurationError{"dataset endpoint path must end with the configured MCP_PATH"}
		}
		prefix := strings.TrimRight(d.EndpointPath[:len(d.EndpointPath)-len(mainPath)], "/")
		if !strings.HasPrefix(prefix, "/datasets/") || strings.Count(prefix, "/") != 2 {
			return &ConfigurationError{"dataset endpoint path must use /datasets/<id><MCP_PATH>"}
		}
		if prefix[strings.LastIndex(prefix, "/")+1:] != d.ID {
			return &ConfigurationError{"dataset endpoint path must match its dataset id"}
		}
		if seenPrefixes[prefix] {
			return &ConfigurationError{"dataset endpoint path prefixes must be unique"}
		}
		seenPrefixes[prefix] = true
		for _, t := range d.BearerTokens {
			if mainTokens[t.Value] {
				return &ConfigurationError{"main and scoped bearer token values must be distinct"}
			}
		}
	}
	return nil
}

// MountPrefix is the dataset endpoint path without the MCP path suffix.
func (d *Dataset) MountPrefix(cfg *settings.Settings) string {
	suffix := strings.TrimRight(cfg.MCPPath, "/")
	if suffix == "" {
		suffix = "/mcp"
	}
	return strings.TrimRight(d.EndpointPath[:len(d.EndpointPath)-len(suffix)], "/")
}

const loadError = "MCP_DATASETS_FILE is missing or has an invalid registry"

// Load reads the protected registry file; failures never reveal the path,
// secrets or JSON content.
func Load(path string, cfg *settings.Settings) (*Registry, error) {
	if pyval.Strip(path) == "" {
		return Empty(), nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, &ConfigurationError{loadError}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, &ConfigurationError{"MCP_DATASETS_FILE must not be a symlink"}
	}
	if !info.Mode().IsRegular() {
		return nil, &ConfigurationError{"MCP_DATASETS_FILE must be a regular file"}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, &ConfigurationError{"MCP_DATASETS_FILE must not be accessible by group or other users"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ConfigurationError{loadError}
	}
	doc, err := pyval.DecodeJSON(data)
	if err != nil {
		return nil, &ConfigurationError{loadError}
	}
	reg, err := Parse(doc)
	if err != nil {
		return nil, &ConfigurationError{loadError}
	}
	if err := reg.ValidateForSettings(cfg); err != nil {
		return nil, err
	}
	return reg, nil
}

// String redacts credentials like pydantic's SecretStr.
func (d *Dataset) String() string {
	return "Dataset(id=" + d.ID + ", endpoint_path=" + d.EndpointPath + ", database=" + d.Database +
		", table=" + d.Table + ", clickhouse_user=" + d.ClickHouseUser + ", clickhouse_password=**********, bearer_tokens=**********)"
}

// GoString redacts credentials in %#v output.
func (d *Dataset) GoString() string { return d.String() }
