// Package deploy is the fail-closed host cutover for the dbt-runner
// service (formerly deploy-dbt-runner.py). All supported ingestion enters
// through dbt-job. No signals are sent to jobs; an ungated running runner is
// NEVER stopped here. The same pipeline.lock inode is held from drain
// through publication and verification.
package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/deployer/internal/pyjson"
)

// Error is a controlled, printable deployment failure.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(msg string) error { return &Error{Msg: msg} }

// errHost is any malformed staged input or host state; it is never
// stringified because parser/OS errors may include configuration.
var errHost = errors.New("Invalid staged deployment or host state")

// Contract is the required candidate deployment contract.
var Contract = pyjson.NewObject("version", int64(1), "drain_aware", true,
	"pipeline_lock", "/app/runtime/pipeline.lock", "deploy_gate", "/app/runtime/deploy.gate",
	"route_snapshot_version", int64(1))

var (
	identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	appIDRE      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	datasetRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	legacyModeRE = regexp.MustCompile(`(?m)^RAWBBIT_RAW_LOAD_MODE=['"]?legacy`)
	lockFileRE   = regexp.MustCompile(`(?m)^RAWBBIT_DBT_LOCK_FILE=(.*)$`)
)

// ComposeFiles is the supported overlay inventory.
var ComposeFiles = map[string]bool{"docker-compose.yml": true, "docker-compose.dozzle.yml": true,
	"docker-compose.datasets.yml": true}

const pausedOverlay = "docker-compose.dbt-paused.yml"

// RunFunc runs a host command and returns its stripped stdout.
type RunFunc func(argv []string, timeout time.Duration) (string, error)

// Run executes a command. Docker config, credentials and subprocess error
// bodies are never printed.
func Run(argv []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", fail("Docker/host command failed; gate retained if installed")
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Deployment is one validated host transaction.
type Deployment struct {
	Args      *Args
	Install   string
	Stage     string
	Runtime   string
	Files     []string
	Artifacts []artifact
	Config    *pyjson.Object
	Env       *pyjson.Object
	Database  string
	Enabled   bool
	Routes    []byte

	PreviousLoaderUsers map[string]bool

	Run RunFunc
	// Containers, AssertQuiescence and HTTP are replaceable in tests.
	Containers       func() ([]*pyjson.Object, error)
	AssertQuiescence func() error
	HTTP             func(req *http.Request) (*http.Response, error)
	Now              func() time.Time
	ProcRoot         string
	Stdout           io.Writer
}

type artifact struct {
	name string
	data []byte
}

// get mirrors dict[key] (KeyError -> host error).
func get(v any, key string) (any, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, errHost
	}
	value, ok := o.Get(key)
	if !ok {
		return nil, errHost
	}
	return value, nil
}

// getDefault mirrors dict.get(key, default).
func getDefault(v any, key string, def any) (any, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, errHost
	}
	if value, ok := o.Get(key); ok {
		return value, nil
	}
	return def, nil
}

func path(v any, keys ...string) (any, error) {
	var err error
	for _, key := range keys {
		if v, err = get(v, key); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func decode(text string) (any, error) {
	v, err := pyjson.Decode(text, pyjson.DecodeOptions{})
	if err != nil {
		return nil, errHost
	}
	return v, nil
}

// New validates the staged deployment (formerly Deployment.__init__).
func New(args *Args, run RunFunc) (*Deployment, error) {
	d := &Deployment{Args: args, Run: run, Files: args.ComposeFiles, PreviousLoaderUsers: map[string]bool{},
		HTTP: noProxyClient.Do, Now: time.Now, ProcRoot: "/proc", Stdout: os.Stdout}
	d.Containers = d.containers
	d.AssertQuiescence = d.assertQuiescence
	var err error
	if d.Install, err = Resolve(args.InstallDir); err != nil {
		return nil, errHost
	}
	if d.Stage, err = Resolve(args.StageDir); err != nil {
		return nil, errHost
	}
	if d.Runtime, err = Resolve(args.RuntimeDir); err != nil {
		return nil, errHost
	}
	unique := map[string]bool{}
	supported := len(d.Files) > 0 && d.Files[0] == "docker-compose.yml"
	for _, f := range d.Files {
		supported = supported && !unique[f] && ComposeFiles[f]
		unique[f] = true
	}
	if !supported {
		return nil, fail("Explicit base and complete desired overlay list required")
	}
	for _, name := range append([]string{".env"}, d.Files...) {
		data, err := os.ReadFile(filepath.Join(d.Stage, name))
		if err != nil {
			return nil, errHost
		}
		d.Artifacts = append(d.Artifacts, artifact{name, data})
	}
	out, err := d.Run(append(d.compose(true), "config", "--format", "json"), 120*time.Second)
	if err != nil {
		return nil, err
	}
	config, err := decode(out)
	if err != nil {
		return nil, err
	}
	d.Config, _ = config.(*pyjson.Object)
	runner, err := path(config, "services", "dbt-runner")
	if err != nil {
		return nil, err
	}
	envValue, err := get(runner, "environment")
	if err != nil {
		return nil, err
	}
	env, ok := envValue.(*pyjson.Object)
	if !ok {
		return nil, errHost
	}
	d.Env = env
	database, _ := getDefault(env, "CLICKHOUSE_DATABASE", "analytics")
	if s, ok := database.(string); !ok || !identifierRE.MatchString(s) {
		return nil, fail("Invalid resolved default database")
	} else {
		d.Database = s
	}
	enabled, _ := getDefault(env, "RAWBBIT_DBT_ROUTING_ENABLED", "0")
	switch pyjson.Str(enabled) {
	case "0", "1":
		d.Enabled = pyjson.Str(enabled) == "1"
	default:
		return nil, fail("Routing switch must be 0 or 1")
	}
	mode, _ := getDefault(env, "RAWBBIT_RAW_LOAD_MODE", nil)
	if mode != "dbt" && mode != "legacy" {
		return nil, fail("Raw load mode must be dbt or legacy")
	}
	if d.Enabled && mode != "dbt" {
		return nil, fail("Enabled routing is incompatible with legacy loading")
	}
	if routes, _ := getDefault(env, "RAWBBIT_DBT_ROUTES_FILE", nil); routes != "/app/runtime/dbt-routes.json" {
		return nil, fail("Runner must use the shared runtime route snapshot")
	}
	shared, err := d.sharesRuntime(runner)
	if err != nil {
		return nil, err
	}
	if !shared {
		return nil, fail("Runner and host must share the exact writable runtime mount")
	}
	if d.Enabled {
		data, err := os.ReadFile(filepath.Join(d.Stage, "dbt-routes.json"))
		if err != nil {
			return nil, fail("Enabled route snapshot missing/unreadable; no publication or writes")
		}
		d.Routes = data
		if err := ValidateRoutes(data, d.Database); err != nil {
			return nil, err
		}
	}
	if args.PreRoutingRollback && d.Enabled {
		return nil, fail("Pre-routing rollback requires routing disabled")
	}
	if err := d.AssertWarehouseUnchanged(); err != nil {
		return nil, err
	}
	return d, nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x.Len() > 0
	}
	return true
}

func (d *Deployment) sharesRuntime(runner any) (bool, error) {
	mounts, err := getDefault(runner, "volumes", []any{})
	if err != nil {
		return false, err
	}
	list, ok := mounts.([]any)
	if !ok {
		return false, errHost
	}
	for _, v := range list {
		target, err := getDefault(v, "target", nil)
		if err != nil {
			return false, err
		}
		kind, _ := getDefault(v, "type", nil)
		if target != "/app/runtime" || kind != "bind" {
			continue
		}
		source, err := get(v, "source")
		if err != nil {
			return false, err
		}
		s, ok := source.(string)
		if !ok {
			return false, errHost
		}
		resolved, err := Resolve(s)
		if err != nil {
			return false, errHost
		}
		readOnly, _ := getDefault(v, "read_only", false)
		if resolved == d.Runtime && !truthy(readOnly) {
			return true, nil
		}
	}
	return false, nil
}

// ValidateRoutes accepts only a complete, safe version-1 route snapshot.
func ValidateRoutes(data []byte, database string) error {
	invalid := fail("Invalid enabled routing snapshot; no publication or writes")
	if len(data) > 1048576 {
		return invalid
	}
	text, ok := pyjson.DecodeBytes(data)
	if !ok {
		return invalid
	}
	value, err := pyjson.Decode(text, pyjson.DecodeOptions{RejectDuplicates: true})
	if err != nil {
		return invalid
	}
	doc, ok := value.(*pyjson.Object)
	if !ok || !doc.HasExactKeys("version", "routes") {
		return invalid
	}
	version, _ := doc.Get("version")
	routesValue, _ := doc.Get("routes")
	routes, ok := routesValue.([]any)
	if !pyjson.IsInt(version, 1) || !ok || len(routes) > 1000 {
		return invalid
	}
	type target struct{ db, table string }
	apps, targets := map[string]bool{}, map[target]bool{}
	for _, raw := range routes {
		route, ok := raw.(*pyjson.Object)
		if !ok || !route.HasExactKeys("app_id", "dataset_id", "database", "table") {
			return invalid
		}
		fields := map[string]string{}
		for _, key := range route.Keys() {
			v, _ := route.Get(key)
			s, ok := v.(string)
			if !ok {
				return invalid
			}
			fields[key] = s
		}
		if !appIDRE.MatchString(fields["app_id"]) || !datasetRE.MatchString(fields["dataset_id"]) ||
			!identifierRE.MatchString(fields["database"]) || fields["table"] != "events" {
			return invalid
		}
		t := target{fields["database"], fields["table"]}
		if t == (target{database, "events"}) || apps[fields["app_id"]] || targets[t] {
			return invalid
		}
		apps[fields["app_id"]], targets[t] = true, true
	}
	return nil
}

// SupportsGate reports whether argv prints a compatible capabilities contract.
func (d *Deployment) SupportsGate(argv []string) bool {
	out, err := d.Run(argv, 30*time.Second)
	if err != nil {
		return false
	}
	value, err := pyjson.Decode(out, pyjson.DecodeOptions{})
	if err != nil {
		return false
	}
	doc, ok := value.(*pyjson.Object)
	if !ok {
		return false
	}
	for _, key := range Contract.Keys() {
		want, _ := Contract.Get(key)
		got, _ := doc.Get(key)
		if !pyjson.Equal(got, want) {
			return false
		}
	}
	commands, ok := doc.Get("commands")
	if !ok {
		return false
	}
	return pyjson.Contains(commands, "validate-config")
}

func envMap(container any) (map[string]string, error) {
	cfg, err := getDefault(container, "Config", pyjson.NewObject())
	if err != nil {
		return nil, err
	}
	raw, err := getDefault(cfg, "Env", []any{})
	if err != nil {
		return nil, err
	}
	return splitEnv(raw)
}

func splitEnv(raw any) (map[string]string, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, errHost
	}
	env := map[string]string{}
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, errHost
		}
		k, v, found := strings.Cut(s, "=")
		if !found {
			return nil, errHost
		}
		env[k] = v
	}
	return env, nil
}

func (d *Deployment) inspectOne(id string) (any, error) {
	out, err := d.Run([]string{"docker", "inspect", id}, 120*time.Second)
	if err != nil {
		return nil, err
	}
	value, err := decode(out)
	if err != nil {
		return nil, err
	}
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		return nil, errHost
	}
	return list[0], nil
}

// AssertWarehouseUnchanged refuses a cutover while the running ClickHouse
// differs from the candidate configuration.
func (d *Deployment) AssertWarehouseUnchanged() error {
	out, err := d.Run([]string{"docker", "ps", "-q", "--filter", "label=com.docker.compose.project=rawbbit-two",
		"--filter", "label=com.docker.compose.service=clickhouse"}, 120*time.Second)
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil // First install may start ClickHouse from the candidate.
	}
	if len(ids) != 1 {
		return fail("Ambiguous running warehouse; separate maintenance required")
	}
	actual, err := d.inspectOne(ids[0])
	if err != nil {
		return err
	}
	expected, err := path(d.Config, "services", "clickhouse")
	if err != nil {
		return err
	}
	actualImage, err := path(actual, "Config", "Image")
	if err != nil {
		return err
	}
	rawEnv, err := path(actual, "Config", "Env")
	if err != nil {
		return err
	}
	actualEnv, err := splitEnv(rawEnv)
	if err != nil {
		return err
	}
	image, err := get(expected, "image")
	if err != nil {
		return err
	}
	expectedEnv, err := get(expected, "environment")
	if err != nil {
		return err
	}
	envObj, ok := expectedEnv.(*pyjson.Object)
	if !ok {
		return errHost
	}
	changed := !pyjson.Equal(actualImage, image)
	for _, key := range envObj.Keys() {
		v, _ := envObj.Get(key)
		if got, ok := actualEnv[key]; !ok || got != pyjson.Str(v) {
			changed = true
		}
	}
	if changed {
		return fail("Running ClickHouse image/environment differs; separately drain warehouse first")
	}
	mounts, err := get(actual, "Mounts")
	if err != nil {
		return err
	}
	actualMounts := map[string]string{}
	list, ok := mounts.([]any)
	if !ok {
		return errHost
	}
	for _, m := range list {
		dest, err := get(m, "Destination")
		if err != nil {
			return err
		}
		source, err := get(m, "Source")
		if err != nil {
			return err
		}
		ds, ok1 := dest.(string)
		ss, ok2 := source.(string)
		if !ok1 || !ok2 {
			return errHost
		}
		resolved, err := Resolve(ss)
		if err != nil {
			return errHost
		}
		actualMounts[ds] = resolved
	}
	volumes, err := getDefault(expected, "volumes", []any{})
	if err != nil {
		return err
	}
	vlist, ok := volumes.([]any)
	if !ok {
		return errHost
	}
	for _, mount := range vlist {
		if kind, err := getDefault(mount, "type", nil); err != nil {
			return err
		} else if kind != "bind" {
			continue
		}
		target, err := get(mount, "target")
		if err != nil {
			return err
		}
		source, err := get(mount, "source")
		if err != nil {
			return err
		}
		t, ok1 := target.(string)
		s, ok2 := source.(string)
		if !ok1 || !ok2 {
			return errHost
		}
		resolved, err := Resolve(s)
		if err != nil {
			return errHost
		}
		if got, ok := actualMounts[t]; !ok || got != resolved {
			return fail("Running ClickHouse bind configuration differs; separately drain warehouse first")
		}
	}
	return nil
}

func (d *Deployment) compose(staged bool) []string {
	dir := d.Install
	if staged {
		dir = d.Stage
	}
	argv := []string{"docker", "compose", "--project-directory", d.Install, "--env-file", filepath.Join(dir, ".env")}
	for _, name := range d.Files {
		argv = append(argv, "-f", filepath.Join(dir, name))
	}
	if !staged && d.Args.PreRoutingRollback {
		argv = append(argv, "-f", filepath.Join(d.Install, pausedOverlay))
	}
	return argv
}

func (d *Deployment) containers() ([]*pyjson.Object, error) {
	out, err := d.Run([]string{"docker", "ps", "-a", "-q", "--filter", "label=com.docker.compose.project=rawbbit-two",
		"--filter", "label=com.docker.compose.service=dbt-runner"}, 120*time.Second)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = d.Run(append([]string{"docker", "inspect"}, ids...), 120*time.Second)
	if err != nil {
		return nil, err
	}
	value, err := decode(out)
	if err != nil {
		return nil, err
	}
	list, ok := value.([]any)
	if !ok {
		return nil, errHost
	}
	result := make([]*pyjson.Object, 0, len(list))
	for _, item := range list {
		o, ok := item.(*pyjson.Object)
		if !ok {
			return nil, errHost
		}
		result = append(result, o)
	}
	return result, nil
}

// MCPEnvironmentChanged reports whether the running MCP server's environment
// differs from the candidate (routing-only changes never force a restart).
func (d *Deployment) MCPEnvironmentChanged() (bool, error) {
	out, err := d.Run([]string{"docker", "ps", "-q", "--filter", "label=com.docker.compose.project=rawbbit-two",
		"--filter", "label=com.docker.compose.service=mcp-server"}, 120*time.Second)
	if err != nil {
		return false, err
	}
	ids := strings.Fields(out)
	if len(ids) != 1 {
		return true, nil
	}
	actual, err := d.inspectOne(ids[0])
	if err != nil {
		return false, err
	}
	raw, err := path(actual, "Config", "Env")
	if err != nil {
		return false, err
	}
	env, err := splitEnv(raw)
	if err != nil {
		return false, err
	}
	expected, err := path(d.Config, "services", "mcp-server", "environment")
	if err != nil {
		return false, err
	}
	obj, ok := expected.(*pyjson.Object)
	if !ok {
		return false, errHost
	}
	for _, key := range obj.Keys() {
		v, _ := obj.Get(key)
		if got, ok := env[key]; !ok || got != pyjson.Str(v) {
			return true, nil
		}
	}
	return false, nil
}

func labels(container any) (*pyjson.Object, error) {
	cfg, err := getDefault(container, "Config", pyjson.NewObject())
	if err != nil {
		return nil, err
	}
	l, err := getDefault(cfg, "Labels", pyjson.NewObject())
	if err != nil {
		return nil, err
	}
	if l == nil {
		return pyjson.NewObject(), nil
	}
	o, ok := l.(*pyjson.Object)
	if !ok {
		return nil, errHost
	}
	return o, nil
}

func labelString(l *pyjson.Object, key string) string {
	v, _ := l.Get(key)
	s, _ := v.(string)
	return s
}

// pathName mirrors pathlib.PurePath(name).name.
func pathName(p string) string {
	parts := strings.Split(p, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" && parts[i] != "." {
			return parts[i]
		}
	}
	return ""
}

// AssertOverlaysPreserved refuses to silently drop an active overlay.
func (d *Deployment) AssertOverlaysPreserved(containers []*pyjson.Object) error {
	desired := map[string]bool{}
	for _, f := range d.Files {
		desired[f] = true
	}
	for _, container := range containers {
		l, err := labels(container)
		if err != nil {
			return err
		}
		recorded := labelString(l, "com.docker.compose.project.config_files")
		if recorded == "" {
			return fail("Existing runner has no verifiable overlay inventory; operator review required")
		}
		names := map[string]bool{}
		for _, name := range strings.Split(recorded, ",") {
			names[pathName(name)] = true
		}
		missing := map[string]bool{}
		for name := range names {
			if !ComposeFiles[name] && name != pausedOverlay {
				return fail("An unsupported active overlay needs separate reviewed migration; no overlays omitted")
			}
			if ComposeFiles[name] && !desired[name] {
				missing[name] = true
			}
		}
		if len(missing) == 1 && missing["docker-compose.datasets.yml"] && d.Args.RemoveDatasetOverlay {
			continue // Explicit metadata-driven dataset endpoint removal.
		}
		if len(missing) > 0 {
			return fail("Active overlays would be omitted; supply the complete overlay set")
		}
	}
	return nil
}

func (d *Deployment) validateCandidate(image string) (any, error) {
	argv := []string{"docker", "run", "--rm", "--network", "none", "--entrypoint", "/app/bin/dbt-job"}
	for _, key := range []string{"RAWBBIT_DBT_ROUTING_ENABLED", "RAWBBIT_DBT_ROUTES_FILE",
		"RAWBBIT_RAW_LOAD_MODE", "CLICKHOUSE_DATABASE"} {
		v, err := get(d.Env, key)
		if err != nil {
			return nil, err
		}
		argv = append(argv, "-e", key+"="+pyjson.Str(v))
	}
	if d.Enabled {
		argv = append(argv, "--mount", "type=bind,source="+filepath.Join(d.Stage, "dbt-routes.json")+
			",target=/app/runtime/dbt-routes.json,readonly")
	}
	out, err := d.Run(append(argv, image, "validate-config"), 30*time.Second)
	if err != nil {
		return nil, err
	}
	value, err := decode(out)
	if err != nil {
		return nil, err
	}
	if status, err := getDefault(value, "status", nil); err != nil {
		return nil, err
	} else if status != "valid" {
		return nil, fail("Candidate rejected staged configuration; no publication")
	}
	return get(value, "revision")
}

var noProxyClient = &http.Client{
	// Never route privileged localhost fence requests through a proxy.
	Transport: &http.Transport{Proxy: nil},
	Timeout:   10 * time.Second,
}

func (d *Deployment) assertQuiescence() error {
	// Also fence outstanding async mutations: lock release alone is not
	// proof that ClickHouse finished a previous delete_insert request.
	clickhouse, err := path(d.Config, "services", "clickhouse")
	if err != nil {
		return err
	}
	portsValue, err := get(clickhouse, "ports")
	if err != nil {
		return err
	}
	ports, ok := portsValue.([]any)
	if !ok {
		return errHost
	}
	var http8123 []any
	for _, p := range ports {
		target, err := get(p, "target")
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(strings.TrimSpace(pyjson.Str(target)))
		if err != nil {
			return errHost
		}
		if n == 8123 {
			http8123 = append(http8123, p)
		}
	}
	if len(http8123) != 1 {
		return fail("A localhost-only ClickHouse HTTP port is required for the cutover fence")
	}
	if hostIP, err := getDefault(http8123[0], "host_ip", nil); err != nil {
		return err
	} else if hostIP != "127.0.0.1" {
		return fail("A localhost-only ClickHouse HTTP port is required for the cutover fence")
	}
	env, err := get(clickhouse, "environment")
	if err != nil {
		return err
	}
	dbtUser, _ := getDefault(d.Env, "CLICKHOUSE_DBT_USER", "rawbbit_dbt")
	loaderUser, err := getDefault(env, "CLICKHOUSE_LOADER_USER", "rawbbit_loader")
	if err != nil {
		return err
	}
	users := []any{dbtUser, loaderUser}
	previous := make([]string, 0, len(d.PreviousLoaderUsers))
	for u := range d.PreviousLoaderUsers {
		previous = append(previous, u)
	}
	sort.Strings(previous)
	for _, u := range previous {
		users = append(users, u)
	}
	quoted := make([]string, len(users))
	for i, u := range users {
		s, ok := u.(string)
		if !ok || !identifierRE.MatchString(s) {
			return fail("Unsafe loader identity")
		}
		quoted[i] = "'" + s + "'"
	}
	sql := "SELECT (SELECT count() FROM system.processes WHERE user IN (" + strings.Join(quoted, ",") + ")) + " +
		"(SELECT count() FROM system.mutations WHERE NOT is_done) FORMAT TabSeparated"
	user, err := get(env, "CLICKHOUSE_USER")
	if err != nil {
		return err
	}
	password, err := get(env, "CLICKHOUSE_PASSWORD")
	if err != nil {
		return err
	}
	published, err := getDefault(http8123[0], "published", nil)
	if err != nil {
		return err
	}
	token := base64.StdEncoding.EncodeToString([]byte(pyjson.Str(user) + ":" + pyjson.Str(password)))
	fenceFailed := fail("Read-only ClickHouse cancellation fence failed; gate retained")
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+pyjson.Str(published), strings.NewReader(sql))
	if err != nil {
		return fenceFailed
	}
	request.Header.Set("Authorization", "Basic "+token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := d.HTTP(request)
	if err != nil {
		return fenceFailed
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return fenceFailed
	}
	pending, ok := pyInt(string(body))
	if !ok {
		return fenceFailed
	}
	if pending.Sign() != 0 {
		return fail("ClickHouse still has loader queries or unfinished mutations; gate retained")
	}
	return nil
}

func (d *Deployment) assertLegacyDisabled() error {
	active := filepath.Join(d.Install, ".env")
	data, err := os.ReadFile(active)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errHost
	}
	if !legacyModeRE.Match(data) {
		return nil
	}
	mode, _ := getDefault(d.Env, "RAWBBIT_RAW_LOAD_MODE", nil)
	if !d.Enabled && mode == "legacy" && bytes.Equal(data, d.Artifacts[0].data) {
		// An unchanged legacy loader already skips on the same flock. Permit
		// repeat deployments, not a mode/credential cutover with an entrant
		// that could have parsed stale settings before the lock.
		lock := "/srv/rawbbit-two/dbt/pipeline.lock"
		if m := lockFileRE.FindSubmatch(data); m != nil {
			lock = strings.Trim(strings.TrimSpace(string(m[1])), `'"`)
		}
		resolved, err := Resolve(lock)
		if err != nil {
			return errHost
		}
		if resolved != filepath.Join(d.Runtime, "pipeline.lock") {
			return fail("Legacy loader does not share the deployment pipeline lock")
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(d.Install, "clickhouse/load_events_hourly.sh")); err == nil {
		return fail("Disable/rename the ungated host loader before cutover; no automatic stop")
	}
	// A launch which parsed the old mode before the rename is still unsafe.
	entries, err := os.ReadDir(d.ProcRoot)
	if err != nil {
		return errHost
	}
	for _, entry := range entries {
		if name := entry.Name(); name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(d.ProcRoot, entry.Name(), "cmdline"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			return errHost
		}
		if bytes.Contains(cmdline, []byte("load_events_hourly.sh")) {
			return fail("An old host loader process remains; wait for completion")
		}
	}
	return nil
}

func (d *Deployment) previousUsers(containers []*pyjson.Object) error {
	for _, c := range containers {
		env, err := envMap(c)
		if err != nil {
			return err
		}
		user, ok := env["CLICKHOUSE_DBT_USER"]
		if !ok {
			user = "rawbbit_dbt"
		}
		d.PreviousLoaderUsers[user] = true
	}
	return nil
}

// CheckQuiescence is the read-only server check for an operator-held lock.
func (d *Deployment) CheckQuiescence() error {
	containers, err := d.Containers()
	if err != nil {
		return err
	}
	if err := d.previousUsers(containers); err != nil {
		return err
	}
	return d.AssertQuiescence()
}

func running(c *pyjson.Object) (bool, error) {
	v, err := path(c, "State", "Running")
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

func (d *Deployment) runnerCapable(c *pyjson.Object) (bool, error) {
	id, err := get(c, "Id")
	if err != nil {
		return false, err
	}
	return d.SupportsGate([]string{"docker", "exec", pyjson.Str(id), "/app/bin/dbt-job", "--capabilities"}), nil
}

// Execute performs the locked drain, publication, recreation and verification.
func (d *Deployment) Execute() error {
	if err := os.MkdirAll(d.Runtime, 0o777); err != nil {
		return errHost
	}
	gate := filepath.Join(d.Runtime, "deploy.gate")
	err := Locked(filepath.Join(d.Runtime, "deployment.lock"), d.Args.DrainTimeout, func() error {
		if exists(gate) && !d.Args.RecoverGate {
			return fail("An existing deploy.gate needs explicit inspected recovery")
		}
		if !exists(gate) {
			if err := AtomicWrite(gate, []byte("dbt deployment in progress; do not remove without verification\n"), 0o600); err != nil {
				return errHost
			}
		}
		return d.drainAndPublish(gate)
	})
	if err != nil {
		return err
	}
	if d.Args.PreRoutingRollback {
		fmt.Fprintln(d.Stdout, "Runner cutover verified; ingestion paused, gate retained")
	} else {
		fmt.Fprintln(d.Stdout, "Runner cutover verified; gate released")
	}
	return nil
}

func (d *Deployment) drainAndPublish(gate string) error {
	if err := d.assertLegacyDisabled(); err != nil {
		return err
	}
	containers, err := d.Containers()
	if err != nil {
		return err
	}
	anyRunning := false
	for _, c := range containers {
		isRunning, err := running(c)
		if err != nil {
			return err
		}
		if isRunning {
			anyRunning = true
			ok, err := d.runnerCapable(c)
			if err != nil {
				return err
			}
			if !ok {
				return fail("Ungated runner is running; operator shared-lock drain/stop required")
			}
		}
	}
	if len(containers) > 0 && !anyRunning && !d.Args.BootstrapStopped {
		return fail("Stopped existing runner requires explicit bootstrap-stopped fence")
	}
	if err := d.AssertOverlaysPreserved(containers); err != nil {
		return err
	}
	// Pull and inspect the immutable candidate without mounting live runtime.
	if _, err := d.Run(append(d.compose(true), "pull", "--policy", "missing", "dbt-runner"), 600*time.Second); err != nil {
		return err
	}
	imageValue, err := path(d.Config, "services", "dbt-runner", "image")
	if err != nil {
		return err
	}
	image := pyjson.Str(imageValue)
	imageID, err := d.Run([]string{"docker", "image", "inspect", "--format", "{{.Id}}", image}, 120*time.Second)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(imageID) {
		return fail("Candidate image ID could not be frozen")
	}
	candidateGated := d.SupportsGate([]string{"docker", "run", "--rm", "--network", "none",
		"--entrypoint", "/app/bin/dbt-job", imageID, "--capabilities"})
	if !candidateGated && !d.Args.PreRoutingRollback {
		return fail("Candidate image lacks the deployment gate/lock contract")
	}
	var revision any
	if candidateGated && !d.Args.PreRoutingRollback {
		if revision, err = d.validateCandidate(imageID); err != nil {
			return err
		}
	}
	return Locked(filepath.Join(d.Runtime, "pipeline.lock"), d.Args.DrainTimeout, func() error {
		return d.publish(gate, image, imageID, revision)
	})
}

func (d *Deployment) publish(gate, image, imageID string, revision any) error {
	if err := d.assertLegacyDisabled(); err != nil {
		return err
	}
	if err := d.AssertWarehouseUnchanged(); err != nil {
		return err
	}
	// Reinspect under lock: do not trust an earlier stopped observation.
	current, err := d.Containers()
	if err != nil {
		return err
	}
	if err := d.AssertOverlaysPreserved(current); err != nil {
		return err
	}
	for _, c := range current {
		isRunning, err := running(c)
		if err != nil {
			return err
		}
		l, err := labels(c)
		if err != nil {
			return err
		}
		if isRunning && strings.ToLower(labelString(l, "com.docker.compose.oneoff")) == "true" {
			return fail("A one-off runner remains; let queued/manual work defer or finish before retry")
		}
		if isRunning {
			ok, err := d.runnerCapable(c)
			if err != nil {
				return err
			}
			if !ok {
				return fail("Ungated entrant appeared during drain; gate retained")
			}
		}
		if err := d.previousUsers([]*pyjson.Object{c}); err != nil {
			return err
		}
	}
	if err := d.AssertQuiescence(); err != nil {
		return err
	}
	if exists(filepath.Join(d.Runtime, "write-fence.json")) {
		return fail("Runtime write-fence requires operator reconciliation; gate retained")
	}
	// Refuse mutable staging inputs (Ansible uses a unique stage dir).
	for _, a := range d.Artifacts {
		data, err := os.ReadFile(filepath.Join(d.Stage, a.name))
		if err != nil {
			return errHost
		}
		if !bytes.Equal(data, a.data) {
			return fail("Staged artifact changed during drain")
		}
	}
	if d.Routes != nil {
		data, err := os.ReadFile(filepath.Join(d.Stage, "dbt-routes.json"))
		if err != nil {
			return errHost
		}
		if !bytes.Equal(data, d.Routes) {
			return fail("Staged route changed during drain")
		}
		if err := AtomicWrite(filepath.Join(d.Runtime, "dbt-routes.json"), d.Routes, 0o644); err != nil {
			return errHost
		}
	}
	// Disabled routing retains any prior snapshot as recovery evidence; a new
	// disabled install requires no route file at all.
	for _, a := range d.Artifacts {
		mode := os.FileMode(0o644)
		if a.name == ".env" {
			mode = 0o600
		}
		if err := AtomicWrite(filepath.Join(d.Install, a.name), a.data, mode); err != nil {
			return errHost
		}
	}
	if d.Args.PreRoutingRollback {
		if err := AtomicWrite(filepath.Join(d.Install, pausedOverlay),
			[]byte("services:\n  dbt-runner:\n    entrypoint: [/bin/sleep, infinity]\n"), 0o644); err != nil {
			return errHost
		}
	}
	if _, err := d.Run(append(d.compose(false), "up", "-d", "--no-deps", "--no-build", "--pull", "missing",
		"--force-recreate", "--wait", "--wait-timeout", "120", "dbt-runner"), 180*time.Second); err != nil {
		return err
	}
	out, err := d.Run(append(d.compose(false), "ps", "-q", "dbt-runner"), 120*time.Second)
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	if len(ids) != 1 {
		return fail("Expected exactly one activated runner")
	}
	if err := d.verifyActivated(ids[0], image, imageID, revision); err != nil {
		return err
	}
	expected := d.expectedFiles()
	var routesSHA any
	if d.Routes != nil {
		sum := sha256.Sum256(d.Routes)
		routesSHA = hex.EncodeToString(sum[:])
	}
	files := make([]any, len(expected))
	for i, f := range expected {
		files[i] = f
	}
	now := d.Now()
	record := pyjson.NewObject("version", int64(1), "image", image, "image_id", imageID,
		"compose_files", files, "routing_enabled", d.Enabled, "ingestion_paused", d.Args.PreRoutingRollback,
		"route_revision", revision, "routes_sha256", routesSHA,
		"verified_at", float64(now.UnixMicro())/1e6)
	if err := AtomicWrite(filepath.Join(d.Runtime, "deploy-fence.json"),
		[]byte(pyjson.Encode(record, pyjson.EncodeOptions{})), 0o644); err != nil {
		return errHost
	}
	if !d.Args.PreRoutingRollback {
		if err := os.Remove(gate); err != nil {
			return errHost
		}
		if err := SyncDir(d.Runtime); err != nil {
			return errHost
		}
	}
	return nil
}

func (d *Deployment) expectedFiles() []string {
	set := map[string]bool{}
	for _, f := range d.Files {
		set[filepath.Join(d.Install, f)] = true
	}
	if d.Args.PreRoutingRollback {
		set[filepath.Join(d.Install, pausedOverlay)] = true
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func (d *Deployment) verifyActivated(id, image, imageID string, revision any) error {
	actual, err := d.inspectOne(id)
	if err != nil {
		return err
	}
	isRunning, err := path(actual, "State", "Running")
	if err != nil {
		return err
	}
	actualImageID, err := getDefault(actual, "Image", nil)
	if err != nil {
		return err
	}
	configImage, err := path(actual, "Config", "Image")
	if err != nil {
		return err
	}
	rawEnv, err := path(actual, "Config", "Env")
	if err != nil {
		return err
	}
	env, err := splitEnv(rawEnv)
	if err != nil {
		return err
	}
	enabled := "0"
	if d.Enabled {
		enabled = "1"
	}
	routing, hasRouting := env["RAWBBIT_DBT_ROUTING_ENABLED"]
	database, hasDatabase := env["CLICKHOUSE_DATABASE"]
	if !truthy(isRunning) || actualImageID != imageID || configImage != image ||
		!hasRouting || routing != enabled || !hasDatabase || database != d.Database {
		return fail("Runner image/environment verification failed")
	}
	mounts, err := getDefault(actual, "Mounts", []any{})
	if err != nil {
		return err
	}
	list, ok := mounts.([]any)
	if !ok {
		return errHost
	}
	shared := false
	for _, m := range list {
		dest, err := get(m, "Destination")
		if err != nil {
			return err
		}
		if dest != "/app/runtime" {
			continue
		}
		source, err := get(m, "Source")
		if err != nil {
			return err
		}
		s, _ := source.(string)
		resolved, err := Resolve(s)
		if err != nil {
			return errHost
		}
		rw, _ := getDefault(m, "RW", nil)
		if resolved == d.Runtime && truthy(rw) {
			shared = true
			break
		}
	}
	if !shared {
		return fail("Activated runner no longer shares the host runtime/lock")
	}
	cfg, _ := get(actual, "Config")
	l, err := getDefault(cfg, "Labels", pyjson.NewObject())
	if err != nil {
		return err
	}
	recorded := ""
	if lo, ok := l.(*pyjson.Object); ok {
		recorded = labelString(lo, "com.docker.compose.project.config_files")
	} else if l != nil {
		return errHost
	}
	got := map[string]bool{}
	for _, f := range strings.Split(recorded, ",") {
		got[f] = true
	}
	want := d.expectedFiles()
	same := len(got) == len(want)
	for _, f := range want {
		same = same && got[f]
	}
	if !same {
		return fail("Activated runner overlay set does not match cutover")
	}
	if d.Args.PreRoutingRollback {
		entrypoint, err := getDefault(cfg, "Entrypoint", nil)
		if err != nil {
			return err
		}
		if !pyjson.Equal(entrypoint, []any{"/bin/sleep", "infinity"}) {
			return fail("Pre-routing rollback must remain inert")
		}
		return nil
	}
	if !d.SupportsGate([]string{"docker", "exec", id, "/app/bin/dbt-job", "--capabilities"}) {
		return fail("Activated runner lost gate compatibility")
	}
	if d.Routes != nil {
		mounted, err := d.Run([]string{"docker", "exec", id, "cat", "/app/runtime/dbt-routes.json"}, 120*time.Second)
		if err != nil {
			return err
		}
		a, err := decode(mounted)
		if err != nil {
			return err
		}
		text, ok := pyjson.DecodeBytes(d.Routes)
		if !ok {
			return errHost
		}
		b, err := decode(text)
		if err != nil {
			return err
		}
		if !pyjson.Equal(a, b) {
			return fail("Mounted route snapshot verification failed")
		}
	}
	out, err := d.Run([]string{"docker", "exec", id, "/app/bin/dbt-job", "validate-config"}, 120*time.Second)
	if err != nil {
		return err
	}
	snapshot, err := decode(out)
	if err != nil {
		return err
	}
	status, err := getDefault(snapshot, "status", nil)
	if err != nil {
		return err
	}
	actualRevision, _ := getDefault(snapshot, "revision", nil)
	if status != "valid" || !pyjson.Equal(actualRevision, revision) {
		return fail("Activated route revision verification failed")
	}
	if mode, _ := getDefault(d.Env, "RAWBBIT_RAW_LOAD_MODE", nil); mode == "dbt" {
		out, err := d.Run([]string{"docker", "exec", id, "/app/bin/dbt-job", "verify-control"}, 120*time.Second)
		if err != nil {
			return err
		}
		verified, err := decode(out)
		if err != nil {
			return err
		}
		if status, err := getDefault(verified, "status", nil); err != nil {
			return err
		} else if status != "control_access_verified" {
			return fail("Loader observation grants unverified; gate retained")
		}
	}
	// Default-only checks; route physical readiness is NOT a global
	// deployment gate and no grants are mutated here.
	for _, command := range []string{"debug", "parse"} {
		if _, err := d.Run(append(d.compose(false), "exec", "-T", "dbt-runner", "dbt", command,
			"--project-dir", "/app", "--profiles-dir", "/app"), 120*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
