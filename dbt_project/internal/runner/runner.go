// Package runner is the availability-first sequential ingestion job with
// persistent ownership and fencing (formerly runner/runner.py). It plans
// windows, holds the shared pipeline lock, journals every job/attempt/request,
// loads the default complement and each route in order, and proves server
// quiescence after every attempt before trusting its outcome.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/clickhouse"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/model"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/progress"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/storage"
)

// Control is the server observation used to gate and settle writes.
type Control interface {
	Ready(database string) bool
	Settle(tag string, databases []string, requestDir string) bool
}

// ControlFactory builds the control plane (validating endpoint config).
type ControlFactory func(env config.Env, timeoutSeconds, settleSeconds int) (Control, error)

// BuildFunc runs one model build attempt and returns its outcome:
// success, dbt_failed, timeout, interrupted or process_error.
type BuildFunc func(record *pyjson.Object, vars map[string]any, requestDir string) string

// StopGrace bounds the wait for a cancelled in-process build to return.
const StopGrace = 10 * time.Second

// LoaderFunc compiles and builds the model for one attempt.
type LoaderFunc func(ctx context.Context, env config.Env, database, tag, requestDir string, vars map[string]any, obs model.Observer) error

// Runner executes one invocation.
type Runner struct {
	Env     config.Env
	Root    string
	Lock    string
	Gate    string
	Fence   string
	JobID   string
	JobPath string
	Job     *pyjson.Object

	ControlFactory ControlFactory
	// Build is the attempt executor; tests replace it.
	Build  BuildFunc
	Stdout io.Writer
	Stderr io.Writer
	// NewEmitter creates the lazily started progress sink.
	NewEmitter func(io.Writer) Emitter
	// Loader and StopGrace drive the default in-process Build.
	Loader    LoaderFunc
	StopGrace time.Duration
	// afterLock runs once the pipeline lock is held (test seam).
	afterLock func()

	Snap    *config.Snapshot
	Control Control

	interruptOnce sync.Once
	interruptCh   chan struct{}
	emitter       Emitter
	emitterOnce   bool
	lastPhase     string
	lastNode      string
	loaderLive    bool
	deadline      time.Time
	attemptLimit  time.Duration
}

// Emitter is the progress sink.
type Emitter interface {
	Emit(line string)
	Close()
}

// New prepares a runner from the environment.
func New(env config.Env, stdout, stderr io.Writer) *Runner {
	root := env.Get("RAWBBIT_DBT_RUNTIME_DIR", "/app/runtime")
	r := &Runner{
		Env:         env,
		Root:        root,
		Lock:        env.Get("RAWBBIT_DBT_LOCK_FILE", filepath.Join(root, "pipeline.lock")),
		Gate:        filepath.Join(root, "deploy.gate"),
		Fence:       filepath.Join(root, "write-fence.json"),
		JobID:       clickhouse.UUID4(),
		Stdout:      stdout,
		Stderr:      stderr,
		interruptCh: make(chan struct{}),
		lastPhase:   "before_first_node",
	}
	r.JobPath = filepath.Join(root, "jobs", r.JobID+".json")
	r.Job = pyjson.NewObject("version", int64(1), "job_id", r.JobID, "status", "created",
		"created_at", ISO(time.Now()), "outcomes", []any{})
	r.ControlFactory = func(env config.Env, timeout, settle int) (Control, error) {
		return clickhouse.NewControl(env, timeout, settle)
	}
	r.NewEmitter = func(w io.Writer) Emitter { return progress.NewEmitter(w) }
	r.Loader, r.StopGrace = Load, StopGrace
	r.Build = r.build
	return r
}

// Signal records an interruption (SIGTERM/SIGINT) and cancels the attempt.
func (r *Runner) Signal() {
	r.interruptOnce.Do(func() { close(r.interruptCh) })
}

// Interrupted reports whether a signal was received.
func (r *Runner) Interrupted() bool {
	select {
	case <-r.interruptCh:
		return true
	default:
		return false
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (r *Runner) outcomes() []any {
	v, _ := r.Job.Get("outcomes")
	return v.([]any)
}

func (r *Runner) progress(record *pyjson.Object, body string) {
	if !r.emitterOnce {
		r.emitterOnce = true
		r.emitter = r.NewEmitter(r.Stderr)
	}
	prefix, err := progress.Prefix(time.Now(), getString(record, "job_id"), getString(record, "attempt_id"),
		getString(record, "kind"), getString(record, "actual_database"))
	if err != nil || r.emitter == nil {
		return
	}
	r.emitter.Emit(prefix + body)
}

// CloseProgress flushes the progress sink with a bounded wait.
func (r *Runner) CloseProgress() {
	if r.emitter != nil {
		r.emitter.Close()
	}
}

func getString(o *pyjson.Object, key string) string {
	v, _ := o.Get(key)
	s, _ := v.(string)
	return s
}

func (r *Runner) record(status string) error {
	if status != "" {
		r.Job.Set("status", status)
	}
	return storage.Save(r.JobPath, r.Job)
}

// Finish persists the terminal status and prints the public summary.
func (r *Runner) Finish(status string, code int) (int, error) {
	r.Job.Set("exit_code", int64(code))
	r.Job.Set("finished_at", ISO(time.Now()))
	if err := r.record(status); err != nil {
		return 4, err
	}
	revision, _ := r.Job.Get("revision")
	summary := pyjson.NewObject("job_id", r.JobID, "status", status, "exit_code", int64(code),
		"revision", revision, "outcomes", r.outcomes())
	fmt.Fprintln(r.Stdout, pyjson.Encode(summary, pyjson.EncodeOptions{}))
	return code, nil
}

func (r *Runner) setFence(attempt any, reason string) error {
	// Conservative global fence: even a different app shares scratch objects.
	return storage.Save(r.Fence, pyjson.NewObject("version", int64(1), "job_id", r.JobID,
		"attempt_id", attempt, "reason", reason, "operator_review_required", true))
}

func (r *Runner) recover() (bool, error) {
	for _, path := range storage.GlobJSON(filepath.Join(r.Root, "jobs")) {
		if path == r.JobPath {
			continue
		}
		previous, err := storage.LoadObject(path)
		if err == nil {
			status, ok := previous.Get("status")
			if !ok {
				err = os.ErrInvalid
			} else if s, _ := status.(string); s == "running" {
				previous.Set("status", "unknown")
				previous.Set("exit_code", int64(4))
				previous.Set("error_class", "interrupted_job_unloaded_windows_require_review")
				err = storage.Save(path, previous)
			}
		}
		if err != nil {
			if err := r.setFence(nil, "unreadable_job_record"); err != nil {
				return false, err
			}
		}
	}
	for _, path := range storage.GlobJSON(filepath.Join(r.Root, "attempts")) {
		record, err := storage.LoadObject(path)
		if err == nil {
			status, ok := record.Get("status")
			if !ok {
				err = os.ErrInvalid
			} else if s, _ := status.(string); s == "intent" || s == "running" {
				// Do not auto-cancel/replay a crashed predecessor: preserve its evidence.
				record.Set("status", "unknown")
				record.Set("error_class", "interrupted_unknown_partial_write")
				attempt, _ := record.Get("attempt_id")
				if err = r.setFence(attempt, "interrupted_intent"); err == nil {
					err = storage.Save(path, record)
				}
			}
		}
		if err != nil {
			if err := r.setFence(nil, "unreadable_attempt_record"); err != nil {
				return false, err
			}
		}
	}
	return exists(r.Fence), nil
}

// observer forwards approved node events to progress and remembers the
// last observed phase/node for the terminal failure diagnostic.
type observer struct {
	r      *Runner
	record *pyjson.Object
	mu     sync.Mutex
	// detached is set when build returns. A loader abandoned after its stop
	// grace may still report progress; it must not touch the attempt record
	// or runner state the caller now owns.
	detached bool
}

func (o *observer) node(resource, node, status string, elapsed float64) {
	body := progress.NodeBody(resource, node, status, elapsed)
	if body == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.detached {
		return
	}
	o.r.lastPhase, o.r.lastNode = resource, node
	o.r.progress(o.record, body)
}

func (o *observer) NodeStart(resource, node string) { o.node(resource, node, "START", -1) }

func (o *observer) NodeFinish(resource, node, status string, elapsed time.Duration) {
	o.node(resource, node, status, elapsed.Seconds())
}

// build runs the model in-process. The attempt is bounded by the attempt and
// job deadlines and cancelled by signals. Cancellation only stops the client;
// the caller always proves server quiescence afterwards.
func (r *Runner) build(record *pyjson.Object, vars map[string]any, requestDir string) string {
	deadline := time.Now().Add(r.attemptLimit)
	if r.deadline.Before(deadline) {
		deadline = r.deadline
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	go func() {
		select {
		case <-r.interruptCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	env := config.Env{}
	for k, v := range r.Env {
		env[k] = v
	}
	database := getString(record, "actual_database")
	env["CLICKHOUSE_DATABASE"] = database
	tag := getString(record, "tag")
	obs := &observer{r: r, record: record}
	defer func() {
		obs.mu.Lock()
		obs.detached = true
		obs.mu.Unlock()
	}()
	done := make(chan string, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- "process_error"
			}
		}()
		err := r.Loader(ctx, env, database, tag, requestDir, vars, obs)
		if err != nil {
			done <- "dbt_failed"
			return
		}
		done <- "success"
	}()
	select {
	case result := <-done:
		if ctx.Err() == nil || result == "success" {
			return result
		}
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(r.StopGrace):
			r.loaderLive = true
		}
	}
	if r.Interrupted() {
		return "interrupted"
	}
	return "timeout"
}

// Load compiles and builds the model for one database, like the former
// `dbt build --selector rawbbit_ingestion --fail-fast --vars ...` child.
func Load(ctx context.Context, env config.Env, database, tag, requestDir string, vars map[string]any, obs model.Observer) error {
	plan, err := model.Compile(vars, time.Now())
	if err != nil {
		return err
	}
	ep, err := clickhouse.ParseEndpoint(env)
	if err != nil {
		return err
	}
	exec := model.NewHTTPExecutor(ep, tag, MaxThreads(env), requestDir)
	return model.Build(ctx, exec, database, plan, clickhouse.UUID4(), obs)
}

// MaxThreads is the per-query max_threads (DBT_CLICKHOUSE_MAX_THREADS).
func MaxThreads(env config.Env) int64 {
	return model.JinjaInt(env.Get("DBT_CLICKHOUSE_MAX_THREADS", "2"))
}

func (r *Runner) attempt(kind string, app any, intended, actual string, window [2]string, excluded []string, readiness bool) (string, error) {
	attemptID := clickhouse.UUID4()
	record := pyjson.NewObject("version", int64(1), "job_id", r.JobID, "attempt_id", attemptID,
		"tag", "rawbbit-dbt:"+attemptID, "revision", r.Snap.Revision, "kind", kind, "app_id", app,
		"intended_database", intended, "actual_database", actual, "table", "events",
		"window_start", window[0], "window_end", window[1], "status", "intent",
		"partial_write_possible", false)
	path := filepath.Join(r.Root, "attempts", attemptID+".json")
	requestDir := filepath.Join(r.Root, "requests", attemptID)
	if err := mkdirNew(requestDir); err != nil {
		return "", err
	}
	// Intent must reach persistent storage BEFORE any warehouse call.
	if err := storage.Save(path, record); err != nil {
		return "", err
	}
	outcome := pyjson.NewObject("attempt_id", attemptID, "kind", kind, "app_id", app, "database", actual,
		"window_start", window[0], "window_end", window[1], "status", "intent")
	r.Job.Set("outcomes", append(r.outcomes(), outcome))
	if err := r.record(""); err != nil {
		return "", err
	}
	r.lastPhase, r.lastNode = "before_first_node", ""
	r.loaderLive = false
	r.progress(record, "START attempt")
	result := "readiness_failed"
	dbtFailed := false
	if !readiness || r.Control.Ready(actual) {
		record.Set("status", "running")
		record.Set("partial_write_possible", true)
		if err := storage.Save(path, record); err != nil {
			return "", err
		}
		excludedList := make([]any, len(excluded))
		for i, a := range excluded {
			excludedList[i] = a
		}
		result = r.safeBuild(record, map[string]any{
			model.VarWindowStart:    window[0],
			model.VarWindowEnd:      window[1],
			model.VarAppID:          app,
			model.VarExcludedAppIDs: excludedList,
		}, requestDir)
		dbtFailed = result == "dbt_failed"
		// Always confirm server quiescence, even on success or test failure.
		r.progress(record, "VERIFY attempt phase=server_settlement")
		if r.loaderLive || !r.Control.Settle(getString(record, "tag"), []string{actual}, requestDir) {
			if err := r.setFence(attemptID, "unknown_server_work"); err != nil {
				return "", err
			}
			result = "unknown"
		}
	}
	switch result {
	case "success":
		record.Set("status", "completed")
		record.Set("error_class", nil)
	case "unknown":
		record.Set("status", "unknown")
		record.Set("error_class", result)
	default:
		record.Set("status", "failed")
		record.Set("error_class", result)
	}
	record.Set("finished_at", ISO(time.Now()))
	if err := storage.Save(path, record); err != nil {
		return "", err
	}
	status, _ := record.Get("status")
	outcome.Set("status", status)
	if err := r.record(""); err != nil {
		return "", err
	}
	if dbtFailed {
		body := "FAIL dbt category=execution_failed last_phase=" + r.lastPhase
		if r.lastNode != "" {
			body += " last_node=" + r.lastNode
		}
		r.progress(record, body)
	}
	r.progress(record, "END attempt status="+status.(string)+" outcome="+result)
	return result, nil
}

func (r *Runner) safeBuild(record *pyjson.Object, vars map[string]any, requestDir string) (result string) {
	defer func() {
		if recover() != nil {
			result = "process_error"
		}
	}()
	return r.Build(record, vars, requestDir)
}

func mkdirNew(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return os.Mkdir(path, 0o777)
}

func (r *Runner) runLocked(args []string) (int, error) {
	if exists(r.Gate) {
		return r.Finish("gated", 4)
	}
	fenced, err := r.recover()
	if err != nil {
		return 0, err
	}
	if fenced {
		return r.Finish("fenced", 4)
	}
	if r.Snap, err = config.Load(r.Env); err != nil {
		return 0, err
	}
	chunks, err := Windows(args, r.Env, time.Now())
	if err != nil {
		return 0, err
	}
	if r.Env.Get("RAWBBIT_RAW_LOAD_MODE", "legacy") == "legacy" {
		if args[0] == "backfill" {
			return 0, config.Errorf("dbt backfill requires dbt mode")
		}
		return r.Finish("mode_disabled", 4)
	}
	if r.Env.Get("RAWBBIT_RAW_LOAD_MODE", "") != "dbt" {
		return 0, config.Errorf("RAWBBIT_RAW_LOAD_MODE must be dbt or legacy")
	}
	attemptSeconds, err := config.Positive(r.Env, "RAWBBIT_DBT_ATTEMPT_TIMEOUT_SECONDS", 900, 7200)
	if err != nil {
		return 0, err
	}
	budget, err := config.Positive(r.Env, "RAWBBIT_DBT_JOB_TIMEOUT_SECONDS", 21600, 172800)
	if err != nil {
		return 0, err
	}
	httpTimeout, err := config.Positive(r.Env, "RAWBBIT_DBT_CONTROL_TIMEOUT_SECONDS", 10, 60)
	if err != nil {
		return 0, err
	}
	settle, err := config.Positive(r.Env, "RAWBBIT_DBT_SETTLE_TIMEOUT_SECONDS", 30, 300)
	if err != nil {
		return 0, err
	}
	r.attemptLimit = time.Duration(attemptSeconds) * time.Second
	if r.Control, err = r.ControlFactory(r.Env, httpTimeout, settle); err != nil {
		return 0, err
	}
	r.deadline = time.Now().Add(time.Duration(budget) * time.Second)
	windows := make([]any, len(chunks))
	for i, w := range chunks {
		windows[i] = []any{w[0], w[1]}
	}
	r.Job.Set("revision", r.Snap.Revision)
	r.Job.Set("snapshot", r.Snap.Document)
	r.Job.Set("command", args[0])
	r.Job.Set("windows", windows)
	if err := r.record("running"); err != nil {
		return 0, err
	}
	// Read-only preflight catches unowned loader activity/mutations before
	// the first new write; a single fenced writer is safer than overlap.
	preflight := filepath.Join(r.Root, "requests", "preflight-"+r.JobID)
	if err := mkdirNew(preflight); err != nil {
		return 0, err
	}
	if !r.Control.Settle("rawbbit-dbt:preflight-"+r.JobID, r.Snap.Databases(), preflight) {
		if err := r.setFence(nil, "preexisting_loader_work"); err != nil {
			return 0, err
		}
		return r.Finish("fenced", 4)
	}
	failed, fallback := false, false
	excluded := r.Snap.ExcludedApps()
	stop := func() bool { return r.Interrupted() || !time.Now().Before(r.deadline) }
	for _, window := range chunks {
		if stop() {
			return r.Finish("unknown", 4)
		}
		result, err := r.attempt("default", nil, r.Snap.Default, r.Snap.Default, window, excluded, false)
		if err != nil {
			return 0, err
		}
		if result == "unknown" || r.Interrupted() {
			return r.Finish("unknown", 4)
		}
		failed = failed || result != "success"
		for _, route := range r.Snap.Routes {
			if stop() {
				return r.Finish("unknown", 4)
			}
			result, err := r.attempt("primary", route.AppID, route.Database, route.Database, window, nil, true)
			if err != nil {
				return 0, err
			}
			if result == "unknown" || r.Interrupted() {
				return r.Finish("unknown", 4)
			}
			if result != "success" {
				if !time.Now().Before(r.deadline) {
					return r.Finish("unknown", 4)
				}
				result, err = r.attempt("fallback", route.AppID, route.Database, r.Snap.Default, window, nil, false)
				if err != nil {
					return 0, err
				}
				if result == "unknown" || r.Interrupted() {
					return r.Finish("unknown", 4)
				}
				fallback = fallback || result == "success"
				failed = failed || result != "success"
			}
		}
	}
	switch {
	case failed:
		return r.Finish("failed", 1)
	case fallback:
		return r.Finish("completed_with_fallback", 2)
	}
	return r.Finish("completed", 0)
}

// Run executes the invocation and always closes the progress sink.
func (r *Runner) Run(args []string) (int, error) {
	defer r.CloseProgress()
	return r.run(args)
}

func (r *Runner) run(args []string) (int, error) {
	if err := os.MkdirAll(r.Root, 0o777); err != nil {
		return 0, err
	}
	if err := r.record(""); err != nil {
		return 0, err
	}
	if exists(r.Gate) {
		return r.Finish("gated", 4)
	}
	if err := os.MkdirAll(filepath.Dir(r.Lock), 0o777); err != nil {
		return 0, err
	}
	wait, err := config.Positive(r.Env, "RAWBBIT_DBT_LOCK_WAIT_SECONDS", 300, 7200)
	if err != nil {
		return 0, err
	}
	backfill := len(args) > 0 && args[0] == "backfill"
	// Append, never truncate or rename this inode (shared with host and deployer).
	lock, err := os.OpenFile(r.Lock, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	deadline := time.Now()
	if backfill {
		deadline = deadline.Add(time.Duration(wait) * time.Second)
	}
	for {
		if exists(r.Gate) {
			return r.Finish("gated", 4)
		}
		if r.Interrupted() {
			return r.Finish("unknown", 4)
		}
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return 0, err
		}
		if !time.Now().Before(deadline) {
			if backfill {
				return r.Finish("lock_wait_expired", 4)
			}
			return r.Finish("lock_skipped", 4)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r.afterLock != nil {
		r.afterLock()
	}
	code, err := r.runLocked(args)
	if err == nil {
		return code, nil
	}
	if cfg, ok := config.AsError(err); ok {
		r.Job.Set("error_class", cfg.Msg) // Only controlled public validation messages.
		return r.Finish("invalid_enabled_config", 3)
	}
	// Keep nonterminal intent and fence on unexpected ledger/control errors.
	if err := r.setFence(nil, "runtime_error"); err != nil {
		return 0, err
	}
	return r.Finish("unknown", 4)
}
