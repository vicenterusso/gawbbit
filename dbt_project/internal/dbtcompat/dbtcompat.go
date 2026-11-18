// Package dbtcompat is the minimal dbt-compatible CLI kept for operator and
// deployment workflows after dbt-core was removed: `dbt build`, `dbt debug`
// and `dbt parse` against the single ingestion model, accepting the former
// project vars through --vars. Exit codes follow dbt: 0 success, 1 node
// failure, 2 usage/compilation/runtime error.
package dbtcompat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/clickhouse"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/model"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/runner"
)

const usage = `Usage: dbt [--version] COMMAND [ARGS]...

  Rawbbit ingestion runner, dbt-compatible subset (dbt-core is not installed).

Commands:
  build  Build rawbbit_events_load into CLICKHOUSE_DATABASE.events and run its data tests.
  debug  Check the ClickHouse loader connection.
  parse  Validate the project and --vars without connecting.

Options for build/parse: --vars YAML, --selector rawbbit_ingestion, --fail-fast,
  --project-dir DIR, --profiles-dir DIR, --target production, --threads N,
  --no-partial-parse (accepted for compatibility).
`

type options struct {
	command string
	vars    map[string]any
}

func parse(args []string) (options, error) {
	var opts options
	valued := map[string]bool{"--project-dir": true, "--profiles-dir": true, "--selector": true,
		"--vars": true, "--target": true, "-t": true, "--threads": true}
	flags := map[string]bool{"--fail-fast": true, "-x": true, "--no-partial-parse": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch {
		case !strings.HasPrefix(arg, "-"):
			if opts.command != "" {
				return opts, fmt.Errorf("Got unexpected extra argument (%s)", arg)
			}
			opts.command = arg
		case flags[arg]:
		case valued[name]:
			if !hasValue {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("Option '%s' requires an argument.", name)
				}
				i++
				value = args[i]
			}
			if err := opts.apply(name, value); err != nil {
				return opts, err
			}
		default:
			return opts, fmt.Errorf("No such option: %s", name)
		}
	}
	return opts, nil
}

func (o *options) apply(name, value string) error {
	switch name {
	case "--selector":
		if value != "rawbbit_ingestion" {
			return fmt.Errorf("Could not find selector named %s, expected rawbbit_ingestion", value)
		}
	case "--target", "-t":
		if value != "production" {
			return fmt.Errorf("The profile 'rawbbit' does not have a target named '%s'", value)
		}
	case "--vars":
		vars := map[string]any{}
		if err := yaml.Unmarshal([]byte(value), &vars); err != nil {
			return errors.New("The --vars argument must be a YAML dictionary")
		}
		o.vars = vars
	}
	return nil
}

// Main runs the dbt-compatible CLI.
func Main(args []string, env config.Env, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(stdout, "rawbbit-dbt-runner: dbt-compatible Go runner (dbt-core and dbt-clickhouse are not installed)")
		return 0
	}
	opts, err := parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "%sError: %s\n", usage, err)
		return 2
	}
	switch opts.command {
	case "build":
		return build(opts, env, stdout)
	case "parse":
		return parseProject(opts, stdout)
	case "debug":
		return debug(env, stdout)
	case "":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "%sError: No such command '%s'.\n", usage, opts.command)
	return 2
}

func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s  %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func compile(opts options, w io.Writer) (*model.Plan, bool) {
	plan, err := model.Compile(opts.vars, time.Now())
	if err != nil {
		logf(w, "Encountered an error:\nCompilation Error in model rawbbit_events_load (models/ingestion/rawbbit_events_load.sql)\n  %s", err)
		return nil, false
	}
	return plan, true
}

func parseProject(opts options, w io.Writer) int {
	if _, ok := compile(opts, w); !ok {
		return 2
	}
	logf(w, "Found 1 model, %d data tests", len(model.Tests))
	return 0
}

func debug(env config.Env, w io.Writer) int {
	logf(w, "Connection:")
	logf(w, "  database: %s", env.Get("CLICKHOUSE_DATABASE", "analytics"))
	control, err := clickhouse.NewControl(env, 10, 30)
	if err == nil {
		_, err = control.Query("SELECT 1")
	}
	if err != nil {
		logf(w, "  Connection test: [ERROR]")
		logf(w, "1 check failed:")
		return 1
	}
	logf(w, "  Connection test: [OK connection ok]")
	logf(w, "All checks passed!")
	return 0
}

type logObserver struct {
	w        io.Writer
	database string
	index    int
}

func (o *logObserver) NodeStart(resource, node string) {
	o.index++
	logf(o.w, "%d of %d START %s %s ... [RUN]", o.index, 1+len(model.Tests), resource, o.name(resource, node))
}

func (o *logObserver) name(resource, node string) string {
	if resource == "model" {
		return "incremental model " + o.database + ".events"
	}
	return node
}

func (o *logObserver) NodeFinish(resource, node, status string, elapsed time.Duration) {
	logf(o.w, "%d of %d %s %s %s [%s in %.2fs]", o.index, 1+len(model.Tests), status, resource,
		o.name(resource, node), status, elapsed.Seconds())
}

func build(opts options, env config.Env, w io.Writer) int {
	plan, ok := compile(opts, w)
	if !ok {
		return 2
	}
	ep, err := clickhouse.ParseEndpoint(env)
	if err != nil {
		logf(w, "Encountered an error:\nRuntime Error\n  %s", err)
		return 2
	}
	database := env.Get("CLICKHOUSE_DATABASE", "analytics")
	if !config.IsIdentifier(database) {
		logf(w, "Encountered an error:\nRuntime Error\n  unsafe database or dataset identifier")
		return 2
	}
	exec := model.NewHTTPExecutor(ep, env.Get("RAWBBIT_DBT_ATTEMPT_TAG", ""), runner.MaxThreads(env), "")
	logf(w, "Running window %s to %s (%d hour paths)", plan.Window.Start, plan.Window.End, len(plan.HourPaths))
	err = model.Build(context.Background(), exec, database, plan, clickhouse.UUID4(), &logObserver{w: w, database: database})
	var buildErr *model.BuildError
	switch {
	case err == nil:
		logf(w, "Completed successfully")
		return 0
	case errors.As(err, &buildErr) && buildErr.Phase == model.PhaseSetup:
		logf(w, "Encountered an error:\nDatabase Error\n  ClickHouse setup request failed")
		return 2
	case errors.As(err, &buildErr) && buildErr.Fail:
		logf(w, "Failure in test %s", buildErr.Node)
	case buildErr != nil:
		logf(w, "Database Error in %s: ClickHouse request failed", buildErr.Node)
	}
	return 1
}
