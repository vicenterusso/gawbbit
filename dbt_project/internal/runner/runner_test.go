package runner

// Ported from tests/test_runtime.py RuntimeTests (stdlib harness, no warehouse).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/model"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/storage"
)

const secret = "SECRET-NOT-IN-LEDGER"

type fakeControl struct {
	mu      sync.Mutex
	missing map[string]bool
	safe    bool
	settled [][2]string
}

func (f *fakeControl) Ready(database string) bool { return !f.missing[database] }

func (f *fakeControl) Settle(tag string, databases []string, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = append(f.settled, [2]string{tag, strings.Join(databases, ",")})
	return f.safe
}

type call struct {
	record *pyjson.Object
	vars   map[string]any
}

type harness struct {
	*Runner
	fake    *fakeControl
	calls   []call
	results []string
	onBuild func(*harness)
	stdout  *bytes.Buffer
	lines   *lines
}

type lines struct {
	mu     sync.Mutex
	items  []string
	closed int
	onEmit func(string)
}

func (l *lines) Emit(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.onEmit != nil {
		l.onEmit(line)
	}
	l.items = append(l.items, line)
}

func (l *lines) Close() { l.closed++ }

func (l *lines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.items, "\n")
}

type fixture struct {
	t      *testing.T
	root   string
	routes string
	env    config.Env
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	f := &fixture{t: t, root: root, routes: filepath.Join(root, "dbt-routes.json")}
	f.env = config.Env{"RAWBBIT_DBT_RUNTIME_DIR": root, "RAWBBIT_RAW_LOAD_MODE": "dbt",
		"RAWBBIT_DBT_ROUTES_FILE": f.routes, "CLICKHOUSE_DATABASE": "main_custom",
		"RAWBBIT_DBT_ROUTING_ENABLED": "1", "CLICKHOUSE_DBT_USER": "loader", "CLICKHOUSE_DBT_PASSWORD": secret}
	f.writeRoutes([2]string{"a", "team_a"}, [2]string{"b", "team_b"})
	return f
}

func (f *fixture) writeRoutes(routes ...[2]string) {
	rows := []any{}
	for _, r := range routes {
		rows = append(rows, pyjson.NewObject("app_id", r[0], "dataset_id", r[1], "database", r[1], "table", "events"))
	}
	if err := storage.Save(f.routes, pyjson.NewObject("version", int64(1), "routes", rows)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) harness() *harness {
	stdout := &bytes.Buffer{}
	h := &harness{Runner: New(f.env, stdout, io.Discard), fake: &fakeControl{safe: true, missing: map[string]bool{}},
		stdout: stdout, lines: &lines{}}
	h.ControlFactory = func(config.Env, int, int) (Control, error) { return h.fake, nil }
	h.NewEmitter = func(io.Writer) Emitter { return h.lines }
	h.Build = func(record *pyjson.Object, vars map[string]any, _ string) string {
		h.calls = append(h.calls, call{record.Clone(), vars})
		if h.onBuild != nil {
			h.onBuild(h)
		}
		if len(h.results) > 0 {
			r := h.results[0]
			h.results = h.results[1:]
			return r
		}
		return "success"
	}
	return h
}

func (f *fixture) run(h *harness, args ...string) int {
	f.t.Helper()
	if h == nil {
		h = f.harness()
	}
	if len(args) == 0 {
		args = []string{"hourly"}
	}
	code, err := h.Run(args)
	if err != nil {
		f.t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(h.JobPath)
	if err != nil || strings.Contains(string(data), secret) {
		f.t.Fatalf("job ledger unreadable or leaks secret: %v", err)
	}
	return code
}

func str(o *pyjson.Object, key string) string {
	v, _ := o.Get(key)
	s, _ := v.(string)
	return s
}

func kinds(calls []call) []string {
	var out []string
	for _, c := range calls {
		out = append(out, str(c.record, "kind"))
	}
	return out
}

func excludedOf(c call) []any { return c.vars[model.VarExcludedAppIDs].([]any) }

func load(t *testing.T, path string) *pyjson.Object {
	t.Helper()
	o, err := storage.LoadObject(path)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func outcomes(h *harness) []any {
	v, _ := h.Job.Get("outcomes")
	return v.([]any)
}

func TestDisabledIgnoresMissingOrBadSnapshot(t *testing.T) {
	f := newFixture(t)
	f.env["RAWBBIT_DBT_ROUTING_ENABLED"] = "0"
	os.Remove(f.routes)
	h := f.harness()
	if code := f.run(h); code != 0 || len(h.calls) != 1 || len(excludedOf(h.calls[0])) != 0 {
		t.Fatal(code, h.calls)
	}
}

func TestSuccessDefaultThenRoutes(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	if code := f.run(h); code != 0 {
		t.Fatal(code)
	}
	if !slices.Equal(kinds(h.calls), []string{"default", "primary", "primary"}) {
		t.Fatal(kinds(h.calls))
	}
	if !pyjson.Equal(excludedOf(h.calls[0]), []any{"a", "b"}) || h.calls[1].vars[model.VarAppID] != "a" ||
		len(excludedOf(h.calls[1])) != 0 || h.calls[0].vars[model.VarAppID] != nil {
		t.Fatal(h.calls[0].vars, h.calls[1].vars)
	}
}

func TestReadinessFallbackIncludesOnlySameAppAndWindow(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.fake.missing["team_a"] = true
	if code := f.run(h); code != 2 {
		t.Fatal(code)
	}
	if !slices.Equal(kinds(h.calls), []string{"default", "fallback", "primary"}) {
		t.Fatal(kinds(h.calls))
	}
	fb := h.calls[1]
	if str(fb.record, "actual_database") != "main_custom" || str(fb.record, "intended_database") != "team_a" ||
		fb.vars[model.VarAppID] != "a" || len(excludedOf(fb)) != 0 ||
		fb.vars[model.VarWindowStart] != h.calls[0].vars[model.VarWindowStart] {
		t.Fatal(fb.vars)
	}
	if str(outcomes(h)[1].(*pyjson.Object), "status") != "failed" {
		t.Fatal("readiness failure not recorded")
	}
}

func TestDefaultPrimaryFallbackFailureDoNotStopHealthyRoutes(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.results = []string{"dbt_failed", "dbt_failed", "dbt_failed", "success"}
	if code := f.run(h); code != 1 {
		t.Fatal(code)
	}
	if !slices.Equal(kinds(h.calls), []string{"default", "primary", "fallback", "primary"}) || len(h.fake.settled) != 5 {
		t.Fatal(kinds(h.calls), len(h.fake.settled))
	}
}

func TestPartialPrimaryFailureCanFallbackAfterServerConfirmation(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.results = []string{"success", "dbt_failed", "success", "success"}
	if code := f.run(h); code != 2 {
		t.Fatal(code)
	}
	primary := load(t, filepath.Join(f.root, "attempts", str(outcomes(h)[1].(*pyjson.Object), "attempt_id")+".json"))
	if v, _ := primary.Get("partial_write_possible"); v != true {
		t.Fatal("partial write not recorded")
	}
}

func TestTimeoutWithProvenCompletionCanFallback(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.results = []string{"success", "timeout", "success", "success"}
	if code := f.run(h); code != 2 || exists(h.Fence) {
		t.Fatal(code)
	}
}

func TestUnknownServerWorkFencesBeforeFallbackAndFutureJobs(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.onBuild = func(h *harness) { h.fake.safe = false }
	if code := f.run(h); code != 4 || len(h.calls) != 1 || !exists(h.Fence) {
		t.Fatal(code, len(h.calls))
	}
	next := f.harness()
	if code := f.run(next); code != 4 || len(next.calls) != 0 {
		t.Fatal(code, len(next.calls))
	}
}

func TestUnownedWorkPreflightFencesWithoutWrites(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.fake.safe = false
	if code := f.run(h); code != 4 || len(h.calls) != 0 {
		t.Fatal(code)
	}
	fence := load(t, h.Fence)
	if str(fence, "reason") != "preexisting_loader_work" {
		t.Fatal(pyjson.Compact(fence))
	}
}

func TestBuildPanicIsProcessErrorAndConfirmsServer(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.Build = func(*pyjson.Object, map[string]any, string) string { panic("boom") }
	if code := f.run(h); code != 1 || len(h.fake.settled) != 6 {
		t.Fatal(code, len(h.fake.settled))
	}
	attempt := load(t, filepath.Join(f.root, "attempts", str(outcomes(h)[0].(*pyjson.Object), "attempt_id")+".json"))
	if str(attempt, "error_class") != "process_error" {
		t.Fatal(pyjson.Compact(attempt))
	}
}

func TestSignaledParentConfirmsServerWork(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.onBuild = func(h *harness) { h.Signal() }
	if code := f.run(h); code != 4 || len(h.fake.settled) != 2 {
		t.Fatal(code, len(h.fake.settled))
	}
}

func TestCrashNonterminalIntentSurvivesAndFences(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.root, "attempts", "interrupted.json")
	if err := storage.Save(path, pyjson.NewObject("status", "running", "attempt_id", "old")); err != nil {
		t.Fatal(err)
	}
	h := f.harness()
	if code := f.run(h); code != 4 || len(h.calls) != 0 || !exists(h.Fence) {
		t.Fatal(code)
	}
	if str(load(t, path), "status") != "unknown" || str(load(t, h.Fence), "attempt_id") != "old" {
		t.Fatal("intent not preserved/fenced")
	}
}

func TestInterruptedJobWithTerminalWritesIsDurablyReported(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.root, "jobs", "old.json")
	if err := storage.Save(path, pyjson.NewObject("job_id", "old", "status", "running")); err != nil {
		t.Fatal(err)
	}
	if code := f.run(nil); code != 0 {
		t.Fatal(code)
	}
	old := load(t, path)
	if code, _ := old.Get("exit_code"); str(old, "status") != "unknown" || !pyjson.IsInt(code, 4) {
		t.Fatal(pyjson.Compact(old))
	}
}

func prepare(f *fixture) *harness {
	h := f.harness()
	snap, err := config.Load(f.env)
	if err != nil {
		f.t.Fatal(err)
	}
	h.Snap, h.Control = snap, h.fake
	h.deadline = time.Now().Add(5 * time.Second)
	h.attemptLimit = 2 * time.Second
	h.Build = h.build
	return h
}

func TestInProcessTimeoutTracksIntentAndConfirmsServer(t *testing.T) {
	f := newFixture(t)
	h := prepare(f)
	h.attemptLimit = 100 * time.Millisecond
	h.Loader = func(ctx context.Context, _ config.Env, _, _, _ string, _ map[string]any, _ model.Observer) error {
		records := storage.GlobJSON(filepath.Join(f.root, "attempts"))
		if len(records) != 1 || str(load(t, records[0]), "status") != "running" {
			t.Error("intent not durable before the loader ran")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	result, err := h.attempt("default", nil, "main_custom", "main_custom", [2]string{"start", "end"}, nil, false)
	if err != nil || result != "timeout" || len(h.fake.settled) != 1 || exists(h.Fence) {
		t.Fatal(result, err, len(h.fake.settled))
	}
}

func TestUnstoppableLoaderMustFenceEvenWhenServerLooksQuiet(t *testing.T) {
	f := newFixture(t)
	h := prepare(f)
	h.attemptLimit, h.StopGrace = 50*time.Millisecond, 50*time.Millisecond
	release := make(chan struct{})
	defer close(release)
	h.Loader = func(context.Context, config.Env, string, string, string, map[string]any, model.Observer) error {
		<-release // ignores cancellation
		return nil
	}
	result, err := h.attempt("default", nil, "main_custom", "main_custom", [2]string{"start", "end"}, nil, false)
	if err != nil || result != "unknown" || !exists(h.Fence) || len(h.fake.settled) != 0 {
		t.Fatal(result, err, len(h.fake.settled))
	}
}

func TestGenericFailureAfterDurableOutcomeRetainsObservedNode(t *testing.T) {
	f := newFixture(t)
	h := prepare(f)
	h.Loader = func(_ context.Context, _ config.Env, _, _, _ string, _ map[string]any, obs model.Observer) error {
		obs.NodeStart("model", model.ModelNode)
		return errors.New(secret + " password sql")
	}
	h.lines.onEmit = func(line string) {
		if strings.Contains(line, "END attempt") || strings.Contains(line, "FAIL dbt") {
			if str(load(t, storage.GlobJSON(filepath.Join(f.root, "attempts"))[0]), "status") != "failed" {
				t.Error("terminal outcome not durable before diagnostic")
			}
		}
	}
	result, err := h.attempt("default", nil, "main_custom", "main_custom", [2]string{"start", "end"}, nil, false)
	if err != nil || result != "dbt_failed" {
		t.Fatal(result, err)
	}
	text := h.lines.text()
	if !strings.Contains(text, "START model rawbbit_events_load") ||
		!strings.Contains(text, "category=execution_failed last_phase=model last_node=rawbbit_events_load") ||
		strings.Contains(text, "SECRET") || strings.Index(text, "VERIFY attempt") > strings.Index(text, "END attempt") {
		t.Fatal(text)
	}
	record := load(t, storage.GlobJSON(filepath.Join(f.root, "attempts"))[0])
	if _, ok := record.Get("last_phase"); ok {
		t.Fatal("progress leaked into the ledger")
	}
}

func TestSilentFailureIsBeforeFirstNode(t *testing.T) {
	f := newFixture(t)
	h := prepare(f)
	h.Loader = func(context.Context, config.Env, string, string, string, map[string]any, model.Observer) error {
		return errors.New("parse")
	}
	if result, _ := h.attempt("default", nil, "main_custom", "main_custom", [2]string{"start", "end"}, nil, false); result != "dbt_failed" {
		t.Fatal(result)
	}
	if text := h.lines.text(); !strings.Contains(text, "category=execution_failed last_phase=before_first_node") ||
		strings.Contains(text, "last_node") || strings.Contains(text, "parse") {
		t.Fatal(text)
	}
}

func TestStdoutSummaryAndLedgerKeysUnchanged(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	if code := f.run(h); code != 0 {
		t.Fatal(code)
	}
	value, err := pyjson.Decode(strings.TrimSpace(h.stdout.String()), pyjson.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	summary := value.(*pyjson.Object)
	if !slices.Equal(summary.Keys(), []string{"job_id", "status", "exit_code", "revision", "outcomes"}) {
		t.Fatal(summary.Keys())
	}
	for _, o := range outcomes(h) {
		if !slices.Equal(o.(*pyjson.Object).Keys(), []string{"attempt_id", "kind", "app_id", "database", "window_start", "window_end", "status"}) {
			t.Fatal(o.(*pyjson.Object).Keys())
		}
	}
	_ = filepath.Walk(f.root, func(path string, info os.FileInfo, _ error) error {
		if strings.HasSuffix(path, ".json") {
			if data, _ := os.ReadFile(path); strings.Contains(string(data), secret) {
				t.Errorf("%s leaks credentials", path)
			}
		}
		return nil
	})
}

func TestSingleLazyEmitterPerInvocationAndBoundedCleanup(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	created := 0
	h.NewEmitter = func(io.Writer) Emitter { created++; return h.lines }
	if code := f.run(h); code != 0 || created != 1 || h.lines.closed != 1 {
		t.Fatal(code, created, h.lines.closed)
	}
}

func TestGateBeforeLock(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(filepath.Join(f.root, "deploy.gate"), nil, 0o600)
	h := f.harness()
	if code := f.run(h); code != 4 || str(h.Job, "status") != "gated" || exists(h.Lock) {
		t.Fatal(code)
	}
}

func TestGateAfterLock(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.afterLock = func() { os.WriteFile(filepath.Join(f.root, "deploy.gate"), nil, 0o600) }
	if code := f.run(h); code != 4 || len(h.calls) != 0 || str(h.Job, "status") != "gated" {
		t.Fatal(code)
	}
}

func holdLock(t *testing.T, path string) func() {
	held, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() { held.Close() }
}

func TestScheduledLockSkipIsObservableNonzero(t *testing.T) {
	f := newFixture(t)
	defer holdLock(t, filepath.Join(f.root, "pipeline.lock"))()
	h := f.harness()
	if code := f.run(h); code != 4 || str(h.Job, "status") != "lock_skipped" {
		t.Fatal(code)
	}
}

func TestBackfillWaitsForLockThenExpires(t *testing.T) {
	f := newFixture(t)
	f.env["RAWBBIT_DBT_LOCK_WAIT_SECONDS"] = "1"
	release := holdLock(t, filepath.Join(f.root, "pipeline.lock"))
	h := f.harness()
	started := time.Now()
	if code := f.run(h, "backfill", "2026-01-01T00:00:00Z", "2026-01-01T01:00:00Z"); code != 4 ||
		str(h.Job, "status") != "lock_wait_expired" || time.Since(started) < time.Second {
		t.Fatal(code, str(h.Job, "status"))
	}
	time.AfterFunc(200*time.Millisecond, release)
	h = f.harness()
	if code := f.run(h, "backfill", "2026-01-01T00:00:00Z", "2026-01-01T01:00:00Z"); code != 0 {
		t.Fatal(code)
	}
}

func TestFrozenSnapshotAcrossBackfillChunks(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.onBuild = func(*harness) { f.writeRoutes([2]string{"c", "team_c"}) }
	if code := f.run(h, "backfill", "2026-01-01T00:00:00Z", "2026-01-03T00:00:00Z"); code != 0 || len(h.calls) != 6 {
		t.Fatal(code, len(h.calls))
	}
	var apps []any
	for _, c := range h.calls {
		if str(c.record, "revision") != h.Snap.Revision {
			t.Fatal("revision changed between chunks")
		}
		v, _ := c.record.Get("app_id")
		apps = append(apps, v)
	}
	if !pyjson.Equal(apps, []any{nil, "a", "b", nil, "a", "b"}) {
		t.Fatal(apps)
	}
}

func TestContinueBackfillAfterFailure(t *testing.T) {
	f := newFixture(t)
	h := f.harness()
	h.results = []string{"dbt_failed"}
	if code := f.run(h, "backfill", "2026-01-01T00:00:00Z", "2026-01-03T00:00:00Z"); code != 1 || len(h.calls) != 6 {
		t.Fatal(code, len(h.calls))
	}
}

func TestLastRouteRemovalRestoresDefault(t *testing.T) {
	f := newFixture(t)
	f.writeRoutes()
	h := f.harness()
	if code := f.run(h); code != 0 || len(h.calls) != 1 || len(excludedOf(h.calls[0])) != 0 {
		t.Fatal(code)
	}
}

func TestInvalidEnabledConfigsNoWrites(t *testing.T) {
	for _, raw := range []*string{nil, ptr("{"), ptr(`{"version":2,"routes":[]}`), ptr(`{"version":true,"routes":[]}`),
		ptr(`{"version":1,"version":1,"routes":[]}`)} {
		f := newFixture(t)
		os.Remove(f.routes)
		if raw != nil {
			os.WriteFile(f.routes, []byte(*raw), 0o600)
		}
		h := f.harness()
		if code := f.run(h); code != 3 || len(h.calls) != 0 {
			t.Fatal(code)
		}
	}
}

func ptr(s string) *string { return &s }

func TestConflictsAndUnsafeFields(t *testing.T) {
	for _, routes := range [][][2]string{{{"a", "x"}, {"a", "y"}}, {{"a", "x"}, {"b", "x"}}, {{"a", "main_custom"}},
		{{"../a", "x"}}, {{"a*", "x"}}, {{"a'", "x"}}, {{"a", "x.y"}}} {
		f := newFixture(t)
		f.writeRoutes(routes...)
		h := f.harness()
		if code := f.run(h); code != 3 || len(h.calls) != 0 {
			t.Fatal(routes, code)
		}
	}
}

func TestInvalidSnapshotEncodingIsConfigErrorWithoutFence(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(f.routes, []byte{0xff, 0xfe}, 0o600)
	h := f.harness()
	if code := f.run(h); code != 3 || len(h.calls) != 0 || exists(h.Fence) {
		t.Fatal(code)
	}
	if str(h.Job, "error_class") != "enabled route snapshot unreadable or malformed" {
		t.Fatal(str(h.Job, "error_class"))
	}
}

func TestRoutingLegacyIncompatibleAndDisabledModeObservable(t *testing.T) {
	f := newFixture(t)
	f.env["RAWBBIT_RAW_LOAD_MODE"] = "legacy"
	if code := f.run(nil); code != 3 {
		t.Fatal(code)
	}
	f.env["RAWBBIT_DBT_ROUTING_ENABLED"] = "0"
	h := f.harness()
	if code := f.run(h); code != 4 || str(h.Job, "status") != "mode_disabled" {
		t.Fatal(code)
	}
}

func TestCapabilitiesAndValidateHaveNoLockLedgerOrCredentialsRequirement(t *testing.T) {
	f := newFixture(t)
	delete(f.env, "CLICKHOUSE_DBT_PASSWORD")
	before, _ := os.ReadDir(f.root)
	for _, args := range [][]string{{"deployment-contract"}, {"capabilities"}, {"--capabilities"}, {"validate-config"}} {
		var out bytes.Buffer
		if code := Main(args, f.env, &out, io.Discard); code != 0 {
			t.Fatal(args, code)
		}
		value, err := pyjson.Decode(strings.TrimSpace(out.String()), pyjson.DecodeOptions{})
		if v, _ := value.(*pyjson.Object).Get("version"); err != nil || !pyjson.IsInt(v, 1) {
			t.Fatal(out.String())
		}
	}
	after, _ := os.ReadDir(f.root)
	if len(before) != len(after) {
		t.Fatal("capability/validation commands touched the runtime directory")
	}
	var out bytes.Buffer
	f.env["RAWBBIT_RAW_LOAD_MODE"] = "other"
	if code := Main([]string{"validate-config"}, f.env, &out, io.Discard); code != 3 ||
		out.String() != "{\"version\":1,\"status\":\"invalid_enabled_config\"}\n" {
		t.Fatal(code, out.String())
	}
}

func TestRuntimeUnavailableEmitsFixedText(t *testing.T) {
	f := newFixture(t)
	blocker := filepath.Join(f.root, "file")
	os.WriteFile(blocker, nil, 0o600)
	f.env["RAWBBIT_DBT_RUNTIME_DIR"] = filepath.Join(blocker, "runtime")
	var stderr bytes.Buffer
	if code := Main([]string{"hourly"}, f.env, io.Discard, &stderr); code != 4 ||
		stderr.String() != "rawbbit-dbt: runtime unavailable; operator review required\n" {
		t.Fatal(code, stderr.String())
	}
}
