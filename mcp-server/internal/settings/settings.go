// Package settings loads the MCP server configuration from the environment
// (and an optional ./.env file) with the same names, defaults and validation
// as the original pydantic-settings model.
package settings

import (
	"math/big"
	"os"
	"strings"
	"unicode"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
)

// Auth modes.
const (
	AuthStaticTokens = "static_tokens"
	AuthJWT          = "jwt"
	AuthNone         = "none"
)

// LabeledToken is one MCP_API_KEYS_JSON entry, in JSON order.
type LabeledToken struct {
	Label string
	Token string
}

// Settings mirrors rawbbit_mcp.settings.Settings.
type Settings struct {
	Env      string
	LogLevel string

	MCPName string
	MCPHost string
	MCPPort int64
	MCPPath string

	ClickHouseHost     string
	ClickHousePort     int64
	ClickHouseUser     string
	ClickHousePassword string
	ClickHouseDatabase string
	ClickHouseSecure   bool
	ClickHouseVerify   bool
	ClickHouseTable    string

	MaxQueryRows        int64
	MaxSampleRows       int64
	MaxExecutionSeconds int64

	BotUserAgentRegex string

	MCPAPIKeysJSON  string
	MCPDatasetsFile string

	JWTPublicKey         string
	JWTJWKSURI           string
	JWTIssuer            string
	JWTAudience          string
	AllowUnauthenticated bool

	apiKeys []LabeledToken
}

type fieldKind int

const (
	kindString fieldKind = iota
	kindInt
	kindBool
)

type field struct {
	alias string
	kind  fieldKind
	def   string
	str   func(*Settings) *string
	num   func(*Settings) *int64
	flag  func(*Settings) *bool
}

// fields lists the settings in model-definition order (validation errors are
// reported in this order).
var fields = []field{
	{alias: "ENV", def: "local", str: func(s *Settings) *string { return &s.Env }},
	{alias: "LOG_LEVEL", def: "INFO", str: func(s *Settings) *string { return &s.LogLevel }},
	{alias: "MCP_NAME", def: "Rawbbit MCP server", str: func(s *Settings) *string { return &s.MCPName }},
	{alias: "MCP_HOST", def: "0.0.0.0", str: func(s *Settings) *string { return &s.MCPHost }},
	{alias: "MCP_PORT", kind: kindInt, def: "8000", num: func(s *Settings) *int64 { return &s.MCPPort }},
	{alias: "MCP_PATH", def: "/mcp", str: func(s *Settings) *string { return &s.MCPPath }},
	{alias: "CLICKHOUSE_HOST", def: "host.docker.internal", str: func(s *Settings) *string { return &s.ClickHouseHost }},
	{alias: "CLICKHOUSE_PORT", kind: kindInt, def: "8123", num: func(s *Settings) *int64 { return &s.ClickHousePort }},
	{alias: "CLICKHOUSE_USER", def: "default", str: func(s *Settings) *string { return &s.ClickHouseUser }},
	{alias: "CLICKHOUSE_PASSWORD", def: "", str: func(s *Settings) *string { return &s.ClickHousePassword }},
	{alias: "CLICKHOUSE_DATABASE", def: "analytics", str: func(s *Settings) *string { return &s.ClickHouseDatabase }},
	{alias: "CLICKHOUSE_SECURE", kind: kindBool, def: "0", flag: func(s *Settings) *bool { return &s.ClickHouseSecure }},
	{alias: "CLICKHOUSE_VERIFY", kind: kindBool, def: "1", flag: func(s *Settings) *bool { return &s.ClickHouseVerify }},
	{alias: "CLICKHOUSE_TABLE", def: "events", str: func(s *Settings) *string { return &s.ClickHouseTable }},
	{alias: "MCP_MAX_QUERY_ROWS", kind: kindInt, def: "500", num: func(s *Settings) *int64 { return &s.MaxQueryRows }},
	{alias: "MCP_MAX_SAMPLE_ROWS", kind: kindInt, def: "50", num: func(s *Settings) *int64 { return &s.MaxSampleRows }},
	{alias: "MCP_MAX_EXECUTION_SECONDS", kind: kindInt, def: "30", num: func(s *Settings) *int64 { return &s.MaxExecutionSeconds }},
	{alias: "MCP_BOT_USER_AGENT_REGEX", def: "bot|spider|crawler|headless|bytespider|ahrefs|googlebot", str: func(s *Settings) *string { return &s.BotUserAgentRegex }},
	{alias: "MCP_API_KEYS_JSON", def: "", str: func(s *Settings) *string { return &s.MCPAPIKeysJSON }},
	{alias: "MCP_DATASETS_FILE", def: "", str: func(s *Settings) *string { return &s.MCPDatasetsFile }},
	{alias: "MCP_JWT_PUBLIC_KEY", def: "", str: func(s *Settings) *string { return &s.JWTPublicKey }},
	{alias: "MCP_JWT_JWKS_URI", def: "", str: func(s *Settings) *string { return &s.JWTJWKSURI }},
	{alias: "MCP_JWT_ISSUER", def: "", str: func(s *Settings) *string { return &s.JWTIssuer }},
	{alias: "MCP_JWT_AUDIENCE", def: "", str: func(s *Settings) *string { return &s.JWTAudience }},
	{alias: "MCP_ALLOW_UNAUTHENTICATED", kind: kindBool, def: "0", flag: func(s *Settings) *bool { return &s.AllowUnauthenticated }},
}

// Load reads the process environment, falling back to ./.env like
// pydantic-settings (environment variables win, names are case-insensitive).
func Load() (*Settings, error) {
	values := map[string]string{}
	if data, err := os.ReadFile(".env"); err == nil {
		for k, v := range parseDotenv(string(data)) {
			values[strings.ToLower(k)] = v
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			values[strings.ToLower(k)] = v
		}
	}
	return FromValues(values)
}

// FromValues builds settings from explicit alias=value pairs (keys are
// matched case-insensitively). Unknown keys are ignored.
func FromValues(raw map[string]string) (*Settings, error) {
	values := make(map[string]string, len(raw))
	for k, v := range raw {
		values[strings.ToLower(k)] = v
	}
	s := &Settings{}
	provided := pyval.NewDict(len(values))
	var errs []pyval.LineError
	for _, f := range fields {
		value, ok := values[strings.ToLower(f.alias)]
		if ok {
			provided.Set(f.alias, value)
		} else {
			value = f.def
		}
		if le := s.assign(f, value, ok); le != nil {
			le.Loc = []string{f.alias}
			errs = append(errs, *le)
		}
	}
	if len(errs) == 0 && s.AuthMode() == AuthNone && !s.AllowUnauthenticated {
		errs = append(errs, pyval.LineError{
			Type: "value_error",
			Message: "Value error, MCP authentication is required. Set MCP_API_KEYS_JSON or JWT settings, " +
				"or explicitly set MCP_ALLOW_UNAUTHENTICATED=1 for local-only development.",
			Input: provided,
		})
	}
	if len(errs) > 0 {
		return nil, &pyval.ValidationError{Title: "Settings", Errors: errs}
	}
	return s, nil
}

func (s *Settings) assign(f field, value string, provided bool) *pyval.LineError {
	switch f.kind {
	case kindInt:
		n, le := pyval.ValidateLaxInt(value)
		if le != nil {
			return le
		}
		if !n.IsInt64() {
			return &pyval.LineError{Type: "int_parsing_size", Message: "Unable to parse input string as an integer, exceeded maximum size", Input: value}
		}
		*f.num(s) = n.Int64()
		return nil
	case kindBool:
		b, le := pyval.ValidateLaxBool(value)
		if le != nil {
			return le
		}
		*f.flag(s) = b
		return nil
	}
	checked, le := validateString(f.alias, value, provided)
	if le != nil {
		return le
	}
	*f.str(s) = checked
	if f.alias == "MCP_API_KEYS_JSON" && checked != "" {
		s.apiKeys = parseAPIKeys(checked)
	}
	return nil
}

func valueError(msg, input string) *pyval.LineError {
	return &pyval.LineError{Type: "value_error", Message: "Value error, " + msg, Input: input}
}

func validateString(alias, value string, provided bool) (string, *pyval.LineError) {
	if !provided {
		// pydantic does not run field validators on defaults.
		return value, nil
	}
	switch alias {
	case "MCP_PATH":
		v := pyval.Strip(value)
		if v == "" {
			v = "/mcp"
		}
		if !strings.HasPrefix(v, "/") {
			v = "/" + v
		}
		return v, nil
	case "CLICKHOUSE_DATABASE", "CLICKHOUSE_TABLE":
		v := pyval.Strip(value)
		if !isAlnum(strings.ReplaceAll(v, "_", "")) {
			return "", valueError("ClickHouse database and table names may only contain letters, numbers, and underscores", value)
		}
		return v, nil
	case "MCP_API_KEYS_JSON":
		return validateAPIKeysJSON(value)
	}
	return value, nil
}

// isAlnum mirrors str.isalnum(): non-empty and only letters or numbers.
func isAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return false
		}
	}
	return true
}

func validateAPIKeysJSON(value string) (string, *pyval.LineError) {
	v := pyval.Strip(value)
	if v == "" {
		return "", nil
	}
	parsed, err := pyval.DecodeJSON([]byte(v))
	if err != nil {
		return "", valueError("MCP_API_KEYS_JSON must be valid JSON", value)
	}
	obj, ok := parsed.(*pyval.Dict)
	if !ok || obj.Len() == 0 {
		return "", valueError("MCP_API_KEYS_JSON must be a non-empty JSON object mapping user labels to bearer tokens", value)
	}
	seen := map[string]bool{}
	for i, k := range obj.Keys {
		label, _ := k.(string)
		if pyval.Strip(label) == "" {
			return "", valueError("MCP_API_KEYS_JSON keys must be non-empty strings", value)
		}
		token, ok := obj.Values[i].(string)
		if !ok || pyval.Strip(token) == "" {
			return "", valueError("MCP_API_KEYS_JSON values must be non-empty strings", value)
		}
		normalized := pyval.Strip(token)
		if seen[normalized] {
			return "", valueError("MCP_API_KEYS_JSON token values must be unique", value)
		}
		seen[normalized] = true
	}
	return v, nil
}

func parseAPIKeys(value string) []LabeledToken {
	parsed, err := pyval.DecodeJSON([]byte(value))
	if err != nil {
		return nil
	}
	obj := parsed.(*pyval.Dict)
	out := make([]LabeledToken, 0, obj.Len())
	for i, k := range obj.Keys {
		out = append(out, LabeledToken{
			Label: pyval.Strip(k.(string)),
			Token: pyval.Strip(obj.Values[i].(string)),
		})
	}
	return out
}

// APIKeysByUser returns the static bearer tokens in configuration order.
func (s *Settings) APIKeysByUser() []LabeledToken { return s.apiKeys }

// AuthMode reports static_tokens, jwt or none.
func (s *Settings) AuthMode() string {
	switch {
	case len(s.apiKeys) > 0:
		return AuthStaticTokens
	case s.JWTPublicKey != "" || s.JWTJWKSURI != "":
		return AuthJWT
	}
	return AuthNone
}

// TableRef is database.table.
func (s *Settings) TableRef() string { return s.ClickHouseDatabase + "." + s.ClickHouseTable }

// QuerySetting is one ClickHouse setting sent with every analytics query.
type QuerySetting struct {
	Name  string
	Value string
}

// QuerySettings mirrors Settings.query_settings (in dict order).
func (s *Settings) QuerySettings() []QuerySetting {
	return []QuerySetting{
		{"readonly", "1"},
		{"max_execution_time", big.NewInt(s.MaxExecutionSeconds).String()},
		{"max_result_rows", big.NewInt(s.MaxQueryRows).String()},
		{"result_overflow_mode", "break"},
	}
}

// Clone returns a shallow copy that can be modified independently.
func (s *Settings) Clone() *Settings {
	c := *s
	c.apiKeys = append([]LabeledToken(nil), s.apiKeys...)
	return &c
}
