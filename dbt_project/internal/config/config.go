// Package config validates the entire public route snapshot before any write
// is permitted. Error messages are part of the persisted job contract
// (job.error_class) and match the original runner exactly.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
)

// Error is a controlled public validation message (exit code 3).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Errorf builds a configuration error.
func Errorf(msg string) error { return &Error{Msg: msg} }

// AsError extracts a configuration error.
func AsError(err error) (*Error, bool) {
	var cfg *Error
	ok := errors.As(err, &cfg)
	return cfg, ok
}

var (
	identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	appRE        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	positiveRE   = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// Env is the process environment as a map (os.environ in Python).
type Env map[string]string

// Get returns env[key] or def when absent.
func (e Env) Get(key, def string) string {
	if v, ok := e[key]; ok {
		return v
	}
	return def
}

// IsIdentifier reports a safe database/dataset identifier.
func IsIdentifier(v string) bool { return identifierRE.MatchString(v) }

// IsAppID reports a safe raw partition app ID.
func IsAppID(v string) bool { return appRE.MatchString(v) && v != "." && v != ".." }

func identifier(v any) (string, error) {
	s, ok := v.(string)
	if !ok || !IsIdentifier(s) {
		return "", Errorf("unsafe database or dataset identifier")
	}
	return s, nil
}

func appID(v any) (string, error) {
	s, ok := v.(string)
	if !ok || !IsAppID(s) {
		return "", Errorf("unsafe app partition identifier")
	}
	return s, nil
}

// Positive reads a bounded positive integer setting.
func Positive(env Env, key string, def, maximum int) (int, error) {
	value := env.Get(key, strconv.Itoa(def))
	if !positiveRE.MatchString(value) {
		return 0, Errorf("invalid bounded positive integer: " + key)
	}
	n, err := strconv.Atoi(value)
	if err != nil || n > maximum {
		return 0, Errorf("invalid bounded positive integer: " + key)
	}
	return n, nil
}

// Route is one resolved public route.
type Route struct {
	AppID     string
	DatasetID string
	Database  string
	Table     string
	document  *pyjson.Object
}

// Snapshot is the frozen, validated routing configuration of one invocation.
type Snapshot struct {
	Enabled  bool
	Default  string
	Routes   []Route
	Revision string
	Document *pyjson.Object
}

// Databases returns the default database followed by every route database.
func (s *Snapshot) Databases() []string {
	out := []string{s.Default}
	for _, r := range s.Routes {
		out = append(out, r.Database)
	}
	return out
}

// ExcludedApps returns every routed app ID (the default complement filter).
func (s *Snapshot) ExcludedApps() []string {
	out := []string{}
	for _, r := range s.Routes {
		out = append(out, r.AppID)
	}
	return out
}

const maxSnapshotBytes = 1048576

// Load builds the route snapshot from the environment.
func Load(env Env) (*Snapshot, error) {
	switch env.Get("RAWBBIT_DBT_ROUTING_ENABLED", "0") {
	case "0", "1":
	default:
		return nil, Errorf("RAWBBIT_DBT_ROUTING_ENABLED must be 0 or 1")
	}
	enabled := env.Get("RAWBBIT_DBT_ROUTING_ENABLED", "0") == "1"
	def, err := identifier(env.Get("CLICKHOUSE_DATABASE", "analytics"))
	if err != nil {
		return nil, err
	}
	routes := []Route{}
	if enabled {
		if env.Get("RAWBBIT_RAW_LOAD_MODE", "legacy") != "dbt" {
			return nil, Errorf("routing requires RAWBBIT_RAW_LOAD_MODE=dbt")
		}
		value, err := readSnapshot(env.Get("RAWBBIT_DBT_ROUTES_FILE", "/app/runtime/dbt-routes.json"))
		if err != nil {
			return nil, err
		}
		routes, err = validate(value, def)
		if err != nil {
			return nil, err
		}
	}
	docRoutes := make([]any, 0, len(routes))
	for _, r := range routes {
		docRoutes = append(docRoutes, r.document)
	}
	document := pyjson.NewObject("version", int64(1), "enabled", enabled, "default_database", def,
		"default_table", "events", "routes", docRoutes)
	sum := sha256.Sum256([]byte(pyjson.Compact(document)))
	return &Snapshot{Enabled: enabled, Default: def, Routes: routes,
		Revision: hex.EncodeToString(sum[:]), Document: document}, nil
}

func readSnapshot(path string) (any, error) {
	malformed := Errorf("enabled route snapshot unreadable or malformed")
	info, err := os.Stat(path)
	if err != nil {
		return nil, malformed
	}
	if info.Size() > maxSnapshotBytes {
		return nil, Errorf("route snapshot too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, malformed
	}
	value, err := pyjson.Decode(string(data), pyjson.DecodeOptions{RejectDuplicates: true})
	if errors.Is(err, pyjson.ErrDuplicateKey) {
		return nil, Errorf("duplicate JSON field")
	}
	if err != nil {
		return nil, malformed
	}
	return value, nil
}

func validate(value any, def string) ([]Route, error) {
	unsupported := Errorf("unsupported route snapshot")
	doc, ok := value.(*pyjson.Object)
	if !ok || !doc.HasExactKeys("version", "routes") {
		return nil, unsupported
	}
	version, _ := doc.Get("version")
	rows, isList := mustGet(doc, "routes").([]any)
	if !pyjson.IsInt(version, 1) || !isList || len(rows) > 1000 {
		return nil, unsupported
	}
	type target struct{ db, table string }
	seenApps, seenTargets := map[string]bool{}, map[target]bool{}
	routes := []Route{}
	for _, raw := range rows {
		row, ok := raw.(*pyjson.Object)
		if !ok || !row.HasExactKeys("app_id", "dataset_id", "database", "table") {
			return nil, Errorf("route must contain only resolved public fields")
		}
		app, err := appID(mustGet(row, "app_id"))
		if err != nil {
			return nil, err
		}
		dataset, err := identifier(mustGet(row, "dataset_id"))
		if err != nil {
			return nil, err
		}
		database, err := identifier(mustGet(row, "database"))
		if err != nil {
			return nil, err
		}
		if table, ok := mustGet(row, "table").(string); !ok || table != "events" {
			return nil, Errorf("only events table is supported")
		}
		t := target{database, "events"}
		if seenApps[app] || seenTargets[t] || t == (target{def, "events"}) {
			return nil, Errorf("duplicate or conflicting route")
		}
		seenApps[app], seenTargets[t] = true, true
		routes = append(routes, Route{AppID: app, DatasetID: dataset, Database: database,
			Table: "events", document: row.Clone()})
	}
	sort.SliceStable(routes, func(i, j int) bool { return routes[i].AppID < routes[j].AppID })
	return routes, nil
}

func mustGet(o *pyjson.Object, key string) any {
	v, _ := o.Get(key)
	return v
}
