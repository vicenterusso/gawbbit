package runner

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/clickhouse"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
)

// Capabilities is the deployment contract advertised to the deploy helper.
func Capabilities(env config.Env) *pyjson.Object {
	root := env.Get("RAWBBIT_DBT_RUNTIME_DIR", "/app/runtime")
	lock := env.Get("RAWBBIT_DBT_LOCK_FILE", filepath.Join(root, "pipeline.lock"))
	gate := filepath.Join(root, "deploy.gate")
	fence := filepath.Join(root, "write-fence.json")
	return pyjson.NewObject(
		"version", int64(1), "drain_aware", true, "route_snapshot_version", int64(1),
		"deploy_gate", gate, "pipeline_lock", lock, "write_fence", fence,
		"lock_file", lock, "gate_file", gate, "write_fence_file", fence,
		"commands", []string{"hourly", "daily", "backfill", "deployment-contract", "capabilities",
			"--capabilities", "validate-config", "verify-control"},
		"exit_codes", pyjson.NewObject("completed", int64(0), "failed", int64(1),
			"completed_with_fallback", int64(2), "invalid_enabled_config", int64(3),
			"interrupted_unknown_or_deferred", int64(4)))
}

// Main is the dbt-job entrypoint. It returns the process exit code.
func Main(args []string, env config.Env, stdout, stderr io.Writer) int {
	sorted := pyjson.EncodeOptions{SortKeys: true}
	if len(args) == 1 {
		switch args[0] {
		case "--capabilities", "capabilities", "deployment-contract":
			fmt.Fprintln(stdout, pyjson.Encode(Capabilities(env), sorted))
			return 0
		case "validate-config":
			snap, err := config.Load(env)
			mode := env.Get("RAWBBIT_RAW_LOAD_MODE", "legacy")
			if err != nil || (mode != "legacy" && mode != "dbt") {
				fmt.Fprintln(stdout, `{"version":1,"status":"invalid_enabled_config"}`)
				return 3
			}
			fmt.Fprintln(stdout, pyjson.Encode(pyjson.NewObject("version", int64(1), "status", "valid",
				"revision", snap.Revision, "snapshot", snap.Document), sorted))
			return 0
		case "verify-control":
			if verifyControl(env) {
				fmt.Fprintln(stdout, `{"version":1,"status":"control_access_verified"}`)
				return 0
			}
			fmt.Fprintln(stdout, `{"version":1,"status":"control_access_unverified"}`)
			return 4
		}
	}
	r := New(env, stdout, stderr)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	go func() {
		for range signals {
			r.Signal()
		}
	}()
	code, err := r.Run(args)
	if err == nil {
		return code
	}
	if _, ok := config.AsError(err); ok {
		if code, err := r.Finish("invalid_enabled_config", 3); err == nil {
			return code
		}
	}
	// If the persistent volume is unavailable, emit only fixed public text.
	fmt.Fprintln(stderr, "rawbbit-dbt: runtime unavailable; operator review required")
	return 4
}

func verifyControl(env config.Env) bool {
	control, err := clickhouse.NewControl(env, 10, 30)
	if err != nil {
		return false
	}
	for _, table := range []string{"processes", "mutations", "query_log", "tables", "columns"} {
		if _, err := control.Query("SELECT * FROM system." + table + " LIMIT 0"); err != nil {
			return false
		}
	}
	return true
}
