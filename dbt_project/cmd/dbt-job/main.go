// Command dbt-job is the Rawbbit ingestion runner (cron target
// /app/bin/dbt-job). When invoked as "dbt" it provides the small
// dbt-compatible maintenance surface (build, debug, parse) that replaces the
// removed dbt-core installation.
package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/dbtcompat"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/runner"
)

func environ() config.Env {
	env := config.Env{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// mirror duplicates output to the persistent container's PID 1 streams so a
// manual `docker compose exec` job stays visible in Docker logs/Dozzle.
func mirror(env config.Env) (stdout, stderr io.Writer) {
	stdout, stderr = os.Stdout, os.Stderr
	if env.Get("RAWBBIT_DBT_MIRROR_PID1", "0") != "1" {
		return stdout, stderr
	}
	if out, err := os.OpenFile("/proc/1/fd/1", os.O_WRONLY|os.O_APPEND, 0); err == nil {
		stdout = io.MultiWriter(os.Stdout, bestEffort{out})
	}
	if errOut, err := os.OpenFile("/proc/1/fd/2", os.O_WRONLY|os.O_APPEND, 0); err == nil {
		stderr = io.MultiWriter(os.Stderr, bestEffort{errOut})
	}
	return stdout, stderr
}

// bestEffort never fails the primary stream because the mirror failed.
type bestEffort struct{ w io.Writer }

func (b bestEffort) Write(p []byte) (int, error) {
	_, _ = b.w.Write(p)
	return len(p), nil
}

func main() {
	syscall.Umask(0o077)
	env := environ()
	if filepath.Base(os.Args[0]) == "dbt" {
		os.Exit(dbtcompat.Main(os.Args[1:], env, os.Stdout, os.Stderr))
	}
	stdout, stderr := mirror(env)
	os.Exit(runner.Main(os.Args[1:], env, stdout, stderr))
}
