// Package config loads collector settings from the environment (and an
// optional .env file) with the names, defaults and validation of the original
// pydantic-settings model.
package config

import (
	"fmt"
	"strings"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pytext"
)

// Settings holds the collector configuration.
type Settings struct {
	Env      string
	Port     int64
	LogLevel string

	CollectorAPIKeysJSON string
	// APIKeyMap maps an API key to the app_id it may publish for.
	APIKeyMap map[string]string

	NATSURL                    string
	NATSStream                 string
	NATSSubjectPrefix          string
	NATSStreamMaxAgeSeconds    int64
	NATSDuplicateWindowSeconds int64
	NATSMaxInflightPublishes   int64

	MaxEventsPerRequest int64
	MaxRequestBytes     int64
	IPHashSalt          string
	StoreRawIP          bool
	GeoIPEnabled        bool
	GeoIPMMDBPath       string

	CORSAllowOrigins     string
	CORSAllowMethods     string
	CORSAllowHeaders     string
	CORSAllowCredentials bool
	CORSMaxAgeSeconds    int64
}

// CORSOriginsList returns the configured origins (comma separated, trimmed).
func (s *Settings) CORSOriginsList() []string { return csvToList(s.CORSAllowOrigins) }

// CORSMethodsList returns the configured methods, upper-cased.
func (s *Settings) CORSMethodsList() []string {
	items := csvToList(s.CORSAllowMethods)
	for i, m := range items {
		items[i] = strings.ToUpper(m)
	}
	return items
}

// CORSHeadersList returns the configured request headers.
func (s *Settings) CORSHeadersList() []string { return csvToList(s.CORSAllowHeaders) }

func csvToList(value string) []string {
	items := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = pytext.Strip(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// fieldError is one pydantic-style settings validation error.
type fieldError struct {
	alias, errType, msg, input string
}

// ValidationError reports invalid settings in pydantic's text format.
type ValidationError struct{ errors []fieldError }

func (e *ValidationError) Error() string {
	var b strings.Builder
	noun := "errors"
	if len(e.errors) == 1 {
		noun = "error"
	}
	fmt.Fprintf(&b, "Invalid collector settings: %d validation %s for Settings", len(e.errors), noun)
	for _, fe := range e.errors {
		fmt.Fprintf(&b, "\n%s\n  %s [type=%s, input_value=%s, input_type=str]\n    For further information visit https://errors.pydantic.dev/2.12/v/%s",
			fe.alias, fe.msg, fe.errType, pyjson.Repr(fe.input), fe.errType)
	}
	return b.String()
}

type loader struct {
	env    map[string]string // lower-cased names
	dotenv map[string]string // lower-cased names
	errors []fieldError
}

func (l *loader) lookup(alias string) (string, bool) {
	key := strings.ToLower(alias)
	if v, ok := l.env[key]; ok {
		return v, true
	}
	v, ok := l.dotenv[key]
	return v, ok
}

func (l *loader) str(alias, def string) string {
	if v, ok := l.lookup(alias); ok {
		return v
	}
	return def
}

func (l *loader) int(alias string, def int64, min *int64) int64 {
	raw, ok := l.lookup(alias)
	if !ok {
		return def
	}
	n, res := parsePyInt(raw)
	switch res {
	case intParsing:
		l.errors = append(l.errors, fieldError{alias, "int_parsing", "Input should be a valid integer, unable to parse string as an integer", raw})
		return def
	case intParsingSize:
		l.errors = append(l.errors, fieldError{alias, "int_parsing_size", "Unable to parse input string as an integer, exceeded maximum size", raw})
		return def
	}
	if min != nil && n < *min {
		l.errors = append(l.errors, fieldError{alias, "greater_than_equal", fmt.Sprintf("Input should be greater than or equal to %d", *min), raw})
		return def
	}
	return n
}

func (l *loader) bool(alias string, def bool) bool {
	raw, ok := l.lookup(alias)
	if !ok {
		return def
	}
	b, ok := parsePyBool(raw)
	if !ok {
		l.errors = append(l.errors, fieldError{alias, "bool_parsing", "Input should be a valid boolean, unable to interpret input", raw})
		return def
	}
	return b
}

// apiKeys validates COLLECTOR_API_KEYS_JSON and builds {str(k): str(v)}.
func (l *loader) apiKeys(raw string) map[string]string {
	keys := map[string]string{}
	v, err := pyjson.Parse([]byte(raw))
	if err != nil {
		l.errors = append(l.errors, fieldError{"COLLECTOR_API_KEYS_JSON", "value_error", "Value error, COLLECTOR_API_KEYS_JSON must be valid JSON", raw})
		return keys
	}
	if v.Kind != pyjson.Object {
		l.errors = append(l.errors, fieldError{"COLLECTOR_API_KEYS_JSON", "value_error", "Value error, COLLECTOR_API_KEYS_JSON must be a JSON object", raw})
		return keys
	}
	for _, m := range v.Members { // later duplicates overwrite, like a dict
		keys[m.Key] = pyjson.PyStr(m.Value)
	}
	return keys
}

// Load reads settings from environ ("KEY=value" entries, as os.Environ
// returns) and the dotenv file at dotenvPath (ignored when absent).
// Environment variables take precedence; names are case-insensitive.
func Load(environ []string, dotenvPath string) (*Settings, error) {
	env := make(map[string]string, len(environ))
	exact := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		env[strings.ToLower(k)] = v
		exact[k] = v
	}
	dotenv := map[string]string{}
	for k, v := range readDotenv(dotenvPath, exact) {
		dotenv[strings.ToLower(k)] = v
	}
	l := &loader{env: env, dotenv: dotenv}
	one := int64(1)

	s := &Settings{}
	s.Env = l.str("ENV", "local")
	s.Port = l.int("PORT", 8080, nil)
	s.LogLevel = l.str("LOG_LEVEL", "INFO")
	s.CollectorAPIKeysJSON = l.str("COLLECTOR_API_KEYS_JSON", "{}")
	s.APIKeyMap = l.apiKeys(s.CollectorAPIKeysJSON)
	s.NATSURL = l.str("NATS_URL", "nats://localhost:4222")
	s.NATSStream = l.str("NATS_STREAM", "EVENTS")
	s.NATSSubjectPrefix = l.str("NATS_SUBJECT_PREFIX", "events")
	s.NATSStreamMaxAgeSeconds = l.int("NATS_STREAM_MAX_AGE_SECONDS", 604800, nil)
	s.NATSDuplicateWindowSeconds = l.int("NATS_DUPLICATE_WINDOW_SECONDS", 600, nil)
	s.NATSMaxInflightPublishes = l.int("NATS_MAX_INFLIGHT_PUBLISHES", 512, &one)
	s.MaxEventsPerRequest = l.int("MAX_EVENTS_PER_REQUEST", 500, nil)
	s.MaxRequestBytes = l.int("MAX_REQUEST_BYTES", 1_048_576, nil)
	s.IPHashSalt = l.str("IP_HASH_SALT", "change-me")
	s.StoreRawIP = l.bool("STORE_RAW_IP", false)
	s.GeoIPEnabled = l.bool("GEOIP_ENABLED", false)
	s.GeoIPMMDBPath = l.str("GEOIP_MMDB_PATH", "")
	s.CORSAllowOrigins = l.str("CORS_ALLOW_ORIGINS", "")
	s.CORSAllowMethods = l.str("CORS_ALLOW_METHODS", "POST,OPTIONS")
	s.CORSAllowHeaders = l.str("CORS_ALLOW_HEADERS", "Content-Type,X-API-Key")
	s.CORSAllowCredentials = l.bool("CORS_ALLOW_CREDENTIALS", false)
	s.CORSMaxAgeSeconds = l.int("CORS_MAX_AGE_SECONDS", 600, nil)

	if len(l.errors) > 0 {
		return nil, &ValidationError{errors: l.errors}
	}
	return s, nil
}
