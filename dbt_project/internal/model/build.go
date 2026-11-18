package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Executor runs one warehouse statement per call, never retrying.
type Executor interface {
	Exec(ctx context.Context, sql string) error
	Query(ctx context.Context, sql string) ([]map[string]any, error)
}

// Observer receives node lifecycle events (formerly dbt NodeStart and
// NodeFinished callbacks). Status values are the progress statuses: model
// OK/ERROR, test PASS/FAIL/ERROR.
type Observer interface {
	NodeStart(resource, node string)
	NodeFinish(resource, node, status string, elapsed time.Duration)
}

// ModelNode is the progress label of the ingestion model.
const ModelNode = "rawbbit_events_load"

// DataTest is one data test run after the model. Label is the approved
// progress label; tests without a label run silently.
type DataTest struct {
	Name   string
	Label  string
	Column string
}

// Tests run in dbt's deterministic order (sorted unique IDs).
var Tests = []DataTest{
	{Name: "not_null_rawbbit_events_load_app_id", Label: "not_null_app_id", Column: "app_id"},
	{Name: "not_null_rawbbit_events_load_event_id", Label: "not_null_event_id", Column: "event_id"},
	{Name: "not_null_rawbbit_events_load_event_time", Label: "not_null_event_time", Column: "event_time"},
	{Name: "rawbbit_events_load_unique"},
}

// Phase identifies where a build failed.
type Phase int

// Build failure phases.
const (
	PhaseSetup Phase = iota // connection, database and relation setup, before any node
	PhaseModel
	PhaseTest
)

// BuildError reports a failed build without any server detail.
type BuildError struct {
	Phase Phase
	Node  string // dbt node name for model/test failures
	Fail  bool   // a data test returned failing rows (as opposed to an error)
	Err   error
}

func (e *BuildError) Error() string {
	switch {
	case e.Phase == PhaseSetup:
		return "setup failed: " + e.Err.Error()
	case e.Fail:
		return "test " + e.Node + " failed"
	default:
		return e.Node + " errored: " + e.Err.Error()
	}
}

func (e *BuildError) Unwrap() error { return e.Err }

type nopObserver struct{}

func (nopObserver) NodeStart(string, string)                         {}
func (nopObserver) NodeFinish(string, string, string, time.Duration) {}

// Build materializes the model into database.events and runs its data tests
// with fail-fast semantics, like `dbt build --selector rawbbit_ingestion
// --fail-fast`. invocationID names the scratch new-data table.
func Build(ctx context.Context, ex Executor, database string, plan *Plan, invocationID string, obs Observer) error {
	if obs == nil {
		obs = nopObserver{}
	}
	relations, err := setup(ctx, ex, database)
	if err != nil {
		return &BuildError{Phase: PhaseSetup, Err: err}
	}
	obs.NodeStart("model", ModelNode)
	started := time.Now()
	err = materialize(ctx, ex, database, relations, plan, invocationID)
	if err != nil {
		obs.NodeFinish("model", ModelNode, "ERROR", time.Since(started))
		return &BuildError{Phase: PhaseModel, Node: ModelNode, Err: err}
	}
	obs.NodeFinish("model", ModelNode, "OK", time.Since(started))
	target := Relation(database, "events")
	for _, test := range Tests {
		if test.Label != "" {
			obs.NodeStart("test", test.Label)
		}
		started := time.Now()
		failures, err := runTest(ctx, ex, test, target)
		status := "PASS"
		switch {
		case err != nil:
			status = "ERROR"
		case failures != 0:
			status = "FAIL"
		}
		if test.Label != "" {
			obs.NodeFinish("test", test.Label, status, time.Since(started))
		}
		if status != "PASS" {
			return &BuildError{Phase: PhaseTest, Node: test.Name, Fail: err == nil, Err: err}
		}
	}
	return nil
}

// setup ensures the target database exists and lists its relations.
func setup(ctx context.Context, ex Executor, database string) (map[string]string, error) {
	rows, err := ex.Query(ctx, "EXISTS DATABASE `"+database+"`")
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || fmt.Sprint(rows[0]["result"]) != "1" {
		if err := ex.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+database+"`"); err != nil {
			return nil, err
		}
	}
	rows, err = ex.Query(ctx, RelationsQuery(database))
	if err != nil {
		return nil, err
	}
	relations := map[string]string{}
	for _, row := range rows {
		name, _ := row["name"].(string)
		switch engine, _ := row["engine"].(string); engine {
		case "MaterializedView", "View":
			relations[name] = "view"
		case "Dictionary":
			relations[name] = "dictionary"
		default:
			relations[name] = "table"
		}
	}
	return relations, nil
}

func columns(ctx context.Context, ex Executor, database, identifier string) ([]string, error) {
	rows, err := ex.Query(ctx, ColumnsQuery(database, identifier))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		name, _ := row["name"].(string)
		names = append(names, name)
	}
	return names, nil
}

// materialize reproduces dbt-clickhouse 1.9.8's incremental materialization
// for this model's configuration (delete_insert, unique_key app_id/event_id,
// full_refresh=false, on_schema_change=ignore, lightweight deletes).
func materialize(ctx context.Context, ex Executor, database string, relations map[string]string, plan *Plan, invocationID string) error {
	target := Relation(database, "events")
	// Preexisting intermediate/backup relations are dropped as tables, like
	// dbt (whose relation objects carry the target's type).
	for _, name := range []string{"events__dbt_tmp", "events__dbt_backup"} {
		if _, ok := relations[name]; ok {
			if err := ex.Exec(ctx, DropIfExists("table", Relation(database, name))); err != nil {
				return err
			}
		}
	}
	kind, exists := relations["events"]
	if !exists {
		// No existing table: create it from the model, then load it.
		return createAndLoad(ctx, ex, database, "events", plan)
	}
	if kind == "view" {
		// An existing view always forces dbt's full-refresh path: build the
		// intermediate table, then swap it in by renames (views cannot be
		// exchanged atomically) and drop the backup.
		intermediate, backup := Relation(database, "events__dbt_tmp"), Relation(database, "events__dbt_backup")
		if err := createAndLoad(ctx, ex, database, "events__dbt_tmp", plan); err != nil {
			return err
		}
		for _, sql := range []string{
			DropIfExists("table", backup), Rename(target, backup),
			DropIfExists("table", target), Rename(intermediate, target),
			DropIfExists("table", backup),
		} {
			if err := ex.Exec(ctx, sql); err != nil {
				return err
			}
		}
		return nil
	}
	scratch := "events__dbt_new_data_" + strings.ReplaceAll(invocationID, "-", "_")
	newData := Relation(database, scratch)
	steps := []func() error{
		func() error { return ex.Exec(ctx, DropIfExists("table", newData)) },
		func() error { return createAndLoad(ctx, ex, database, scratch, plan) },
		func() error { return ex.Exec(ctx, DeleteExisting(target, newData)) },
		func() error {
			cols, err := columns(ctx, ex, database, "events")
			if err != nil {
				return err
			}
			return ex.Exec(ctx, InsertNewData(target, newData, cols))
		},
		func() error { return ex.Exec(ctx, DropIfExists("table", newData)) },
		func() error {
			return ex.Exec(ctx, DropIfExists("table", Relation(database, "events__dbt_distributed_new_data")))
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// createAndLoad creates identifier empty from the model, then inserts it.
func createAndLoad(ctx context.Context, ex Executor, database, identifier string, plan *Plan) error {
	relation := Relation(database, identifier)
	if err := ex.Exec(ctx, CreateTableAs(relation, plan.SQL)); err != nil {
		return err
	}
	cols, err := columns(ctx, ex, database, identifier)
	if err != nil {
		return err
	}
	return ex.Exec(ctx, InsertInto(relation, cols, plan.SQL))
}

func runTest(ctx context.Context, ex Executor, test DataTest, target string) (int64, error) {
	rows, err := ex.Query(ctx, TestSQL(test, target))
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 {
		return 0, errors.New("unexpected test result shape")
	}
	failures, err := strconv.ParseInt(fmt.Sprint(rows[0]["failures"]), 10, 64)
	if err != nil {
		return 0, errors.New("unexpected test result type")
	}
	return failures, nil
}
