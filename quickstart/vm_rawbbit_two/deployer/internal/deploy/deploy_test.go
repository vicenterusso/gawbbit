package deploy

// Host transaction tests; no Docker daemon, network, credentials or databases.
// Ported from tests/test_deploy_dbt_runner.py.

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mirlan-irokez/rawbbit/quickstart/vm_rawbbit_two/deployer/internal/pyjson"
)

type fixture struct {
	t                       *testing.T
	install, stage, runtime string
	files                   []string
	args                    *Args
	env                     *pyjson.Object
	config                  *pyjson.Object
	mu                      sync.Mutex
	commands                [][]string
	containers              []*pyjson.Object
	gated                   bool
	verifiedLock            bool
	override                func(argv []string) (string, error, bool)
	document                string
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newFixture(t *testing.T) *fixture {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, install: filepath.Join(root, "install"), stage: filepath.Join(root, "stage"),
		runtime: filepath.Join(root, "runtime"), gated: true,
		files: []string{"docker-compose.yml", "docker-compose.dozzle.yml", "docker-compose.datasets.yml"}}
	for _, p := range []string{f.install, f.stage, f.runtime} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range f.files {
		write(t, filepath.Join(f.stage, name), "candidate "+name)
		write(t, filepath.Join(f.install, name), "active "+name)
	}
	write(t, filepath.Join(f.stage, ".env"), "RAWBBIT_RAW_LOAD_MODE=dbt\nCANDIDATE=1\n")
	write(t, filepath.Join(f.install, ".env"), "RAWBBIT_RAW_LOAD_MODE=dbt\nACTIVE=1\n")
	f.document = `{"version": 1, "routes": [{"app_id": "runner_rawbbit", "dataset_id": "runner_rawbbit", "database": "runner_data", "table": "events"}]}`
	write(t, filepath.Join(f.stage, "dbt-routes.json"), f.document)
	write(t, filepath.Join(f.runtime, "dbt-routes.json"), "old snapshot")
	f.args = &Args{InstallDir: f.install, StageDir: f.stage, RuntimeDir: f.runtime, ComposeFiles: f.files,
		DrainTimeout: time.Second, drainPositive: true}
	f.env = pyjson.NewObject("RAWBBIT_DBT_ROUTING_ENABLED", "1", "RAWBBIT_DBT_ROUTES_FILE", "/app/runtime/dbt-routes.json",
		"RAWBBIT_RAW_LOAD_MODE", "dbt", "CLICKHOUSE_DATABASE", "custom_default", "CLICKHOUSE_DBT_USER", "rawbbit_dbt")
	f.config = pyjson.NewObject("services", pyjson.NewObject(
		"dbt-runner", pyjson.NewObject("image", "test:candidate", "environment", f.env,
			"volumes", []any{pyjson.NewObject("type", "bind", "source", f.runtime, "target", "/app/runtime")}),
		"clickhouse", pyjson.NewObject(
			"ports", []any{pyjson.NewObject("target", int64(8123), "host_ip", "127.0.0.1", "published", "8123")},
			"environment", pyjson.NewObject("CLICKHOUSE_USER", "admin", "CLICKHOUSE_PASSWORD", "not-a-secret"))))
	return f
}

func envStr(o *pyjson.Object, key string) string {
	v, _ := o.Get(key)
	return pyjson.Str(v)
}

func (f *fixture) run(argv []string, _ time.Duration) (string, error) {
	f.mu.Lock()
	f.commands = append(f.commands, argv)
	f.mu.Unlock()
	if f.override != nil {
		if out, err, ok := f.override(argv); ok {
			return out, err
		}
	}
	has := func(s string) bool { return slices.Contains(argv, s) }
	n := len(argv)
	switch {
	case n >= 3 && slices.Equal(argv[n-3:], []string{"config", "--format", "json"}):
		return pyjson.Encode(f.config, pyjson.EncodeOptions{}), nil
	case n >= 3 && argv[1] == "image" && argv[2] == "inspect":
		return "sha256:" + strings.Repeat("a", 64), nil
	case has("--capabilities"):
		if !f.gated || (has("exec") && has("old-ungated")) {
			return "", fail("ungated")
		}
		caps := Contract.Clone()
		caps.Set("commands", []any{"--capabilities", "validate-config"})
		return pyjson.Encode(caps, pyjson.EncodeOptions{}), nil
	case has("validate-config"):
		return `{"status": "valid", "revision": "candidate-revision"}`, nil
	case has("verify-control"):
		return `{"status": "control_access_verified"}`, nil
	case has("up"):
		if !exists(filepath.Join(f.runtime, "deploy.gate")) {
			f.t.Error("gate missing during recreation")
		}
		stream, err := os.OpenFile(filepath.Join(f.runtime, "pipeline.lock"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
		if err != nil {
			f.t.Fatal(err)
		}
		defer stream.Close()
		if err := syscall.Flock(int(stream.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			f.t.Error("pipeline lock not held during recreation")
		}
		f.verifiedLock = true
		return "", nil
	case argv[1] == "inspect":
		files := make([]any, 0)
		for _, name := range f.files {
			files = append(files, filepath.Join(f.install, name))
		}
		if f.args.PreRoutingRollback {
			files = append(files, filepath.Join(f.install, "docker-compose.dbt-paused.yml"))
		}
		joined := make([]string, len(files))
		for i, s := range files {
			joined[i] = s.(string)
		}
		var entrypoint any
		if f.args.PreRoutingRollback {
			entrypoint = []any{"/bin/sleep", "infinity"}
		}
		var env []any
		for _, k := range f.env.Keys() {
			env = append(env, k+"="+envStr(f.env, k))
		}
		return pyjson.Encode([]any{pyjson.NewObject("State", pyjson.NewObject("Running", true),
			"Image", "sha256:"+strings.Repeat("a", 64),
			"Mounts", []any{pyjson.NewObject("Destination", "/app/runtime", "Source", f.runtime, "RW", true)},
			"Config", pyjson.NewObject("Image", "test:candidate", "Env", env, "Entrypoint", entrypoint,
				"Labels", pyjson.NewObject("com.docker.compose.project.config_files", strings.Join(joined, ","))))},
			pyjson.EncodeOptions{}), nil
	case has("ps") && has("dbt-runner"):
		return "new-runner", nil
	case n >= 2 && argv[n-2] == "cat" && argv[n-1] == "/app/runtime/dbt-routes.json":
		return read(f.t, filepath.Join(f.runtime, "dbt-routes.json")), nil
	}
	return "", nil
}

func (f *fixture) deployment() (*Deployment, error) {
	d, err := New(f.args, f.run)
	if err != nil {
		return nil, err
	}
	d.Containers = func() ([]*pyjson.Object, error) { return f.containers, nil }
	d.AssertQuiescence = func() error { return nil }
	d.Stdout = io.Discard
	return d, nil
}

func (f *fixture) mustDeployment() *Deployment {
	f.t.Helper()
	d, err := f.deployment()
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *fixture) oldContainer(id string, running, oneoff bool) *pyjson.Object {
	files := make([]string, len(f.files))
	for i, name := range f.files {
		files[i] = filepath.Join(f.install, name)
	}
	label := "False"
	if oneoff {
		label = "True"
	}
	return pyjson.NewObject("Id", id, "State", pyjson.NewObject("Running", running), "Config", pyjson.NewObject(
		"Labels", pyjson.NewObject("com.docker.compose.project.config_files", strings.Join(files, ","),
			"com.docker.compose.oneoff", label)))
}

func (f *fixture) assertNotPublished() {
	f.t.Helper()
	if !strings.Contains(read(f.t, filepath.Join(f.install, ".env")), "ACTIVE=1") {
		f.t.Error(".env was published")
	}
	if read(f.t, filepath.Join(f.runtime, "dbt-routes.json")) != "old snapshot" {
		f.t.Error("route snapshot was published")
	}
	if !exists(filepath.Join(f.runtime, "deploy.gate")) {
		f.t.Error("gate was released")
	}
	for _, cmd := range f.commands {
		if slices.Contains(cmd, "up") {
			f.t.Error("runner was recreated")
		}
	}
}

func expectError(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil || !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("error %v does not match %q", err, pattern)
	}
}

func TestSingleLockTransactionPreservesAllOverlaysAndTargetsOnlyRunner(t *testing.T) {
	f := newFixture(t)
	lock := filepath.Join(f.runtime, "pipeline.lock")
	write(t, lock, "")
	before, _ := os.Stat(lock)
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(lock)
	if !os.SameFile(before, after) {
		t.Error("pipeline.lock inode changed")
	}
	if !f.verifiedLock || exists(filepath.Join(f.runtime, "deploy.gate")) {
		t.Error("lock not verified or gate retained")
	}
	for _, cmd := range f.commands {
		if slices.Contains(cmd, "up") {
			if cmd[len(cmd)-1] != "dbt-runner" || !slices.Contains(cmd, "--no-deps") {
				t.Errorf("unscoped up: %v", cmd)
			}
			for _, name := range f.files {
				if !slices.Contains(cmd, filepath.Join(f.install, name)) {
					t.Errorf("overlay %s missing", name)
				}
			}
		}
	}
	fence, err := pyjson.Decode(read(t, filepath.Join(f.runtime, "deploy-fence.json")), pyjson.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	obj := fence.(*pyjson.Object)
	if v, _ := obj.Get("route_revision"); v != "candidate-revision" {
		t.Errorf("route_revision %v", v)
	}
	want := []any{}
	for _, name := range []string{"docker-compose.datasets.yml", "docker-compose.dozzle.yml", "docker-compose.yml"} {
		want = append(want, filepath.Join(f.install, name))
	}
	if v, _ := obj.Get("compose_files"); !pyjson.Equal(v, want) {
		t.Errorf("compose_files %v", v)
	}
}

// activeJob holds pipeline.lock on its own open file description, like a
// running dbt-job in the container sharing the bind mount.
func (f *fixture) activeJob(duration time.Duration) chan struct{} {
	ready, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		stream, err := os.OpenFile(filepath.Join(f.runtime, "pipeline.lock"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
		if err != nil {
			close(ready)
			return
		}
		defer stream.Close()
		_ = syscall.Flock(int(stream.Fd()), syscall.LOCK_EX)
		close(ready)
		time.Sleep(duration)
	}()
	<-ready
	return done
}

func TestTimeoutRetainsGateAndOldArtifactsWithoutKillingActiveJob(t *testing.T) {
	f := newFixture(t)
	done := f.activeJob(600 * time.Millisecond)
	f.args.DrainTimeout = 100 * time.Millisecond
	expectError(t, f.mustDeployment().Execute(), "Drain timed out")
	select {
	case <-done:
		t.Error("active job was interrupted")
	default:
	}
	f.assertNotPublished()
	<-done
}

func TestSuccessWaitsForActiveJobToComplete(t *testing.T) {
	f := newFixture(t)
	done := f.activeJob(200 * time.Millisecond)
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("active job did not finish")
	}
	if !f.verifiedLock {
		t.Error("lock not verified")
	}
}

func TestWaitingBackfillDefersOnGateBeforeDeploymentLockIsReleased(t *testing.T) {
	f := newFixture(t)
	f.activeJob(300 * time.Millisecond)
	result := make(chan string, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if exists(filepath.Join(f.runtime, "deploy.gate")) {
				result <- "gated"
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		result <- "timeout"
	}()
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
	if got := <-result; got != "gated" {
		t.Fatal(got)
	}
}

func TestFirstUpgradeRefusesRunningUngatedRunnerEvenWithBootstrapFlag(t *testing.T) {
	f := newFixture(t)
	f.args.BootstrapStopped = true
	f.containers = []*pyjson.Object{f.oldContainer("old-ungated", true, false)}
	expectError(t, f.mustDeployment().Execute(), "Ungated runner is running")
	f.assertNotPublished()
}

func TestQueuedOneOffContainerMustDeferOrFinishBeforeCutover(t *testing.T) {
	f := newFixture(t)
	f.containers = []*pyjson.Object{f.oldContainer("one-off", true, true)}
	expectError(t, f.mustDeployment().Execute(), "one-off runner remains")
	f.assertNotPublished()
}

func TestGatedExistingRunnerCanBeDrained(t *testing.T) {
	f := newFixture(t)
	f.containers = []*pyjson.Object{f.oldContainer("gate-compatible", true, false)}
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
	if !f.verifiedLock {
		t.Error("lock not verified")
	}
}

func (f *fixture) upCommand() []string {
	for _, cmd := range f.commands {
		if slices.Contains(cmd, "up") {
			return cmd
		}
	}
	f.t.Fatal("no up command")
	return nil
}

func TestRemovingDatasetOverlayPreservesDozzleAndRunnerTargeting(t *testing.T) {
	f := newFixture(t)
	f.containers = []*pyjson.Object{f.oldContainer("gate-compatible", true, false)}
	f.files = f.files[:2]
	f.args.ComposeFiles = f.files
	f.args.RemoveDatasetOverlay = true
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
	up := f.upCommand()
	if !slices.Contains(up, filepath.Join(f.install, "docker-compose.dozzle.yml")) ||
		slices.Contains(up, filepath.Join(f.install, "docker-compose.datasets.yml")) || up[len(up)-1] != "dbt-runner" {
		t.Errorf("unexpected up: %v", up)
	}
}

func TestAccidentallyOmittingActiveDozzleOverlayFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.containers = []*pyjson.Object{f.oldContainer("gate-compatible", true, false)}
	f.args.ComposeFiles = []string{"docker-compose.yml", "docker-compose.datasets.yml"}
	expectError(t, f.mustDeployment().Execute(), "Active overlays would be omitted")
	f.assertNotPublished()
}

func TestDatasetOverlayRemovalRequiresExplicitMetadataDecision(t *testing.T) {
	f := newFixture(t)
	f.containers = []*pyjson.Object{f.oldContainer("gate-compatible", true, false)}
	f.args.ComposeFiles = f.files[:2]
	expectError(t, f.mustDeployment().Execute(), "Active overlays would be omitted")
	f.assertNotPublished()
}

func TestRoutingOnlyEnvironmentChangeDoesNotForceMCPRestart(t *testing.T) {
	f := newFixture(t)
	d := f.mustDeployment()
	services, _ := d.Config.Get("services")
	services.(*pyjson.Object).Set("mcp-server", pyjson.NewObject("environment", pyjson.NewObject(
		"CLICKHOUSE_DATABASE", "custom_default", "MCP_DATASETS_CONFIG_PATH", "/app/datasets.json")))
	actualEnv := []any{"CLICKHOUSE_DATABASE=custom_default", "MCP_DATASETS_CONFIG_PATH=/app/datasets.json"}
	d.Run = func(argv []string, _ time.Duration) (string, error) {
		if argv[1] == "ps" {
			return "mcp", nil
		}
		return pyjson.Encode([]any{pyjson.NewObject("Config", pyjson.NewObject("Env", actualEnv))}, pyjson.EncodeOptions{}), nil
	}
	if changed, err := d.MCPEnvironmentChanged(); err != nil || changed {
		t.Fatal(changed, err)
	}
	f.env.Set("RAWBBIT_DBT_ROUTING_ENABLED", "0")
	if changed, err := d.MCPEnvironmentChanged(); err != nil || changed {
		t.Fatal(changed, err)
	}
	actualEnv[0] = "CLICKHOUSE_DATABASE=other"
	if changed, err := d.MCPEnvironmentChanged(); err != nil || !changed {
		t.Fatal(changed, err)
	}
}

func TestChangedRunningWarehouseIsRefusedBeforePublication(t *testing.T) {
	f := newFixture(t)
	d := f.mustDeployment()
	ch, _ := path(d.Config, "services", "clickhouse")
	ch.(*pyjson.Object).Set("image", "clickhouse:24.8")
	d.Run = func(argv []string, _ time.Duration) (string, error) {
		if argv[1] == "ps" {
			return "warehouse", nil
		}
		return pyjson.Encode([]any{pyjson.NewObject("Config", pyjson.NewObject("Image", "clickhouse:24.8",
			"Env", []any{"CLICKHOUSE_USER=admin", "CLICKHOUSE_PASSWORD=different"}))}, pyjson.EncodeOptions{}), nil
	}
	expectError(t, d.AssertWarehouseUnchanged(), "separately drain warehouse")
	if !strings.Contains(read(t, filepath.Join(f.install, ".env")), "ACTIVE=1") {
		t.Error(".env changed")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func TestReadOnlyCancellationFenceRefusesUnknownServerWork(t *testing.T) {
	f := newFixture(t)
	d := f.mustDeployment()
	var body string
	var auth string
	d.HTTP = func(req *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(req.Body)
		body, auth = string(data), req.Header.Get("Authorization")
		if req.URL.String() != "http://127.0.0.1:8123" {
			t.Errorf("url %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("1\n"))}, nil
	}
	expectError(t, d.assertQuiescence(), "unfinished mutations")
	if !strings.Contains(body, "system.processes") || !strings.Contains(body, "system.mutations") || strings.Contains(body, "KILL") {
		t.Errorf("unexpected fence SQL %q", body)
	}
	if !strings.Contains(body, "'rawbbit_dbt','rawbbit_loader'") || auth != "Basic YWRtaW46bm90LWEtc2VjcmV0" {
		t.Errorf("unexpected users/auth %q %q", body, auth)
	}
	d.HTTP = func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("0\n"))}, nil
	}
	if err := d.assertQuiescence(); err != nil {
		t.Fatal(err)
	}
	d.HTTP = func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") }
	expectError(t, d.assertQuiescence(), "cancellation fence failed")
}

func TestStoppedOldRunnerRequiresExplicitBootstrapAndCancellationFence(t *testing.T) {
	f := newFixture(t)
	f.containers = []*pyjson.Object{f.oldContainer("old-ungated", false, false)}
	expectError(t, f.mustDeployment().Execute(), "bootstrap-stopped")
	f.assertNotPublished()
	f.args.BootstrapStopped, f.args.RecoverGate = true, true
	d := f.mustDeployment()
	d.AssertQuiescence = func() error { return fail("unfinished mutations") }
	expectError(t, d.Execute(), "unfinished mutations")
	f.assertNotPublished()
}

func TestExistingGateNeedsExplicitRecovery(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.runtime, "deploy.gate"), "")
	expectError(t, f.mustDeployment().Execute(), "inspected recovery")
	f.assertNotPublished()
}

func TestDurableWriteFenceIsNotClearedByGateRecovery(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.runtime, "write-fence.json"), "{}")
	f.args.RecoverGate = true
	expectError(t, f.mustDeployment().Execute(), "operator reconciliation")
	f.assertNotPublished()
	if !exists(filepath.Join(f.runtime, "write-fence.json")) {
		t.Error("write fence removed")
	}
}

func TestDisabledRoutingNeedsNoSnapshotAndLastRouteRemovalIsDefaultOnly(t *testing.T) {
	f := newFixture(t)
	f.env.Set("RAWBBIT_DBT_ROUTING_ENABLED", "0")
	os.Remove(filepath.Join(f.stage, "dbt-routes.json"))
	os.Remove(filepath.Join(f.runtime, "dbt-routes.json"))
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.runtime, "dbt-routes.json")) {
		t.Error("route snapshot created")
	}
	fence, _ := pyjson.Decode(read(t, filepath.Join(f.runtime, "deploy-fence.json")), pyjson.DecodeOptions{})
	if v, _ := fence.(*pyjson.Object).Get("routing_enabled"); v != false {
		t.Errorf("routing_enabled %v", v)
	}
}

func TestEnabledZeroRoutesStillRequiresValidSnapshot(t *testing.T) {
	f := newFixture(t)
	os.Remove(filepath.Join(f.stage, "dbt-routes.json"))
	_, err := f.deployment()
	expectError(t, err, "snapshot missing/unreadable")
	if exists(filepath.Join(f.runtime, "deploy.gate")) {
		t.Error("gate installed")
	}
	write(t, filepath.Join(f.stage, "dbt-routes.json"), `{"version":1,"routes":[]}`)
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidEnabledSnapshotNeverInstallsGateOrPublishes(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.stage, "dbt-routes.json"), `{"version":2,"routes":[]}`)
	if _, err := f.deployment(); err == nil {
		t.Fatal("accepted invalid snapshot")
	}
	if exists(filepath.Join(f.runtime, "deploy.gate")) || !strings.Contains(read(t, filepath.Join(f.install, ".env")), "ACTIVE=1") {
		t.Error("published")
	}
}

func TestPreRoutingRollbackIsExplicitInertAndRetainsGate(t *testing.T) {
	f := newFixture(t)
	f.args.PreRoutingRollback = true
	f.env.Set("RAWBBIT_DBT_ROUTING_ENABLED", "0")
	f.gated = false
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(f.runtime, "deploy.gate")) {
		t.Error("gate released")
	}
	if !strings.Contains(read(t, filepath.Join(f.install, "docker-compose.dbt-paused.yml")), "/bin/sleep") {
		t.Error("paused overlay missing")
	}
	fence, _ := pyjson.Decode(read(t, filepath.Join(f.runtime, "deploy-fence.json")), pyjson.DecodeOptions{})
	if v, _ := fence.(*pyjson.Object).Get("ingestion_paused"); v != true {
		t.Error("ingestion_paused not recorded")
	}
}

func TestFailureDuringVerificationKeepsGate(t *testing.T) {
	f := newFixture(t)
	d := f.mustDeployment()
	f.override = func(argv []string) (string, error, bool) {
		if slices.Contains(argv, "parse") {
			return "", fail("parse failed"), true
		}
		return "", nil, false
	}
	expectError(t, d.Execute(), "parse failed")
	if !exists(filepath.Join(f.runtime, "deploy.gate")) {
		t.Error("gate released")
	}
}

func TestLegacyLoaderEntrypointMustBeDisabledBeforeCutover(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.install, ".env"), "RAWBBIT_RAW_LOAD_MODE=legacy\n")
	write(t, filepath.Join(f.install, "clickhouse/load_events_hourly.sh"), "old launcher")
	expectError(t, f.mustDeployment().Execute(), "Disable/rename")
	for _, cmd := range f.commands {
		if slices.Contains(cmd, "up") {
			t.Error("recreated")
		}
	}
}

func TestOldHostLoaderProcessBlocksCutover(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.install, ".env"), "RAWBBIT_RAW_LOAD_MODE=legacy\n")
	d := f.mustDeployment()
	d.ProcRoot = t.TempDir()
	write(t, filepath.Join(d.ProcRoot, "123", "cmdline"), "/bin/sh\x00clickhouse/load_events_hourly.sh\x00")
	write(t, filepath.Join(d.ProcRoot, "self", "cmdline"), "load_events_hourly.sh")
	expectError(t, d.Execute(), "old host loader process remains")
}

func TestUnverifiedLoaderObservationGrantsKeepGateAfterRecreation(t *testing.T) {
	f := newFixture(t)
	f.override = func(argv []string) (string, error, bool) {
		if slices.Contains(argv, "verify-control") {
			return `{"status": "control_access_unverified"}`, nil, true
		}
		return "", nil, false
	}
	expectError(t, f.mustDeployment().Execute(), "observation grants")
	if !f.verifiedLock || !exists(filepath.Join(f.runtime, "deploy.gate")) {
		t.Error("lock not verified or gate released")
	}
}

func TestUnchangedLegacyRedeployKeepsSameSharedLock(t *testing.T) {
	f := newFixture(t)
	data := "RAWBBIT_RAW_LOAD_MODE=legacy\nRAWBBIT_DBT_LOCK_FILE=" + filepath.Join(f.runtime, "pipeline.lock") + "\n"
	write(t, filepath.Join(f.install, ".env"), data)
	write(t, filepath.Join(f.stage, ".env"), data)
	f.env.Set("RAWBBIT_RAW_LOAD_MODE", "legacy")
	f.env.Set("RAWBBIT_DBT_ROUTING_ENABLED", "0")
	write(t, filepath.Join(f.install, "clickhouse/load_events_hourly.sh"), "unchanged loader")
	if err := f.mustDeployment().Execute(); err != nil {
		t.Fatal(err)
	}
	if !f.verifiedLock {
		t.Error("lock not verified")
	}
}

func TestLegacyLoaderWithDifferentLockIsRefused(t *testing.T) {
	f := newFixture(t)
	data := "RAWBBIT_RAW_LOAD_MODE=legacy\nRAWBBIT_DBT_LOCK_FILE='/elsewhere/pipeline.lock'\n"
	write(t, filepath.Join(f.install, ".env"), data)
	write(t, filepath.Join(f.stage, ".env"), data)
	f.env.Set("RAWBBIT_RAW_LOAD_MODE", "legacy")
	f.env.Set("RAWBBIT_DBT_ROUTING_ENABLED", "0")
	expectError(t, f.mustDeployment().Execute(), "does not share the deployment pipeline lock")
}

func TestStagedMutationAbortsBeforePublication(t *testing.T) {
	f := newFixture(t)
	d := f.mustDeployment()
	write(t, filepath.Join(f.stage, ".env"), "changed staging content")
	expectError(t, d.Execute(), "Staged artifact changed")
	f.assertNotPublished()
}

func TestValidRoutesAndEmptyRoutes(t *testing.T) {
	value := `{"version": 1, "routes": [{"app_id": "runner.rawbbit", "dataset_id": "runner_rawbbit", "database": "runner_data", "table": "events"}]}`
	for _, data := range []string{value, `{"version":1,"routes":[]}`} {
		if err := ValidateRoutes([]byte(data), "custom_default"); err != nil {
			t.Errorf("%s: %v", data, err)
		}
	}
	// json.loads(bytes) also accepts a UTF-8 BOM and UTF-16 input.
	if err := ValidateRoutes([]byte("\xef\xbb\xbf"+value), "custom_default"); err != nil {
		t.Error(err)
	}
	utf16 := []byte{0xff, 0xfe}
	for _, r := range `{"version":1,"routes":[]}` {
		utf16 = append(utf16, byte(r), 0)
	}
	if err := ValidateRoutes(utf16, "custom_default"); err != nil {
		t.Error(err)
	}
}

func TestDuplicateUnsafeAndCustomDefaultCollisions(t *testing.T) {
	route := func(app, dataset, database, table string) string {
		return `{"app_id": "` + app + `", "dataset_id": "` + dataset + `", "database": "` + database + `", "table": "` + table + `"}`
	}
	good := route("runner.rawbbit", "runner_rawbbit", "runner_data", "events")
	cases := []string{
		route("../runner", "runner_rawbbit", "runner_data", "events"),
		route("runner.rawbbit", "runner_rawbbit", "runner_data", "other"),
		route("runner.rawbbit", "runner_rawbbit", "custom_default", "events"),
		route("runner.rawbbit", "runner_rawbbit", "bad-name", "events"),
		route("runner.rawbbit", "../metadata", "runner_data", "events"),
		good + ", " + good,
		good + ", " + route("other", "runner_rawbbit", "runner_data", "events"),
	}
	for _, c := range cases {
		if err := ValidateRoutes([]byte(`{"version": 1, "routes": [`+c+`]}`), "custom_default"); err == nil {
			t.Errorf("accepted %s", c)
		}
	}
}

func TestDuplicateJSONFieldsAndBooleanVersion(t *testing.T) {
	for _, data := range []string{`{"version":1,"version":1,"routes":[]}`, `{"version":true,"routes":[]}`,
		`{"version":1.0,"routes":[]}`, `{"version":1,"routes":[{"app_id":1,"dataset_id":"a","database":"b","table":"events"}]}`} {
		if err := ValidateRoutes([]byte(data), "analytics"); err == nil {
			t.Errorf("accepted %s", data)
		}
	}
}
