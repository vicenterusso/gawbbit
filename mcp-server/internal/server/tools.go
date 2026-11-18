package server

import (
	"context"
	"math/big"
	"strings"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/clickhouse"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/datasets"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/settings"
)

// GatewayFactory creates the query backend for one ClickHouse identity.
type GatewayFactory func(*settings.Settings) clickhouse.Querier

// target is one dataset a tool call can be routed to.
type target struct {
	datasetID string
	cfg       *settings.Settings
	gateway   clickhouse.Querier
}

type toolFunc func(ctx context.Context, args map[string]pyval.Value) (pyval.Value, error)

// tool is one registered MCP tool.
type tool struct {
	name        string
	description string
	params      []param
	wrapResult  bool // list results are wrapped as {"result": [...]}
	run         toolFunc
}

// userError is a ValueError raised inside a tool body.
type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

func datasetSettings(cfg *settings.Settings, d *datasets.Dataset) *settings.Settings {
	c := cfg.Clone()
	c.ClickHouseUser = d.ClickHouseUser
	c.ClickHousePassword = d.ClickHousePassword
	c.ClickHouseDatabase = d.Database
	c.ClickHouseTable = d.Table
	lower := func(current int64, override *int64) int64 {
		if override != nil && *override < current {
			return *override
		}
		return current
	}
	c.MaxQueryRows = lower(c.MaxQueryRows, d.MaxQueryRows)
	c.MaxSampleRows = lower(c.MaxSampleRows, d.MaxSampleRows)
	c.MaxExecutionSeconds = lower(c.MaxExecutionSeconds, d.MaxExecutionSeconds)
	return c
}

func str(v pyval.Value) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// buildTools mirrors register_tools(): the tool set and descriptions depend
// on whether this is a scoped endpoint, the main endpoint with datasets, or
// the main endpoint without a registry.
func buildTools(cfg *settings.Settings, reg *datasets.Registry, factory GatewayFactory, scoped *datasets.Dataset) []*tool {
	defaultTarget := &target{datasetID: "default", cfg: cfg, gateway: factory(cfg)}
	byID := map[string]*target{}
	exposed := map[string]*target{}
	for _, d := range reg.Datasets {
		dc := datasetSettings(cfg, d)
		byID[d.ID] = &target{datasetID: d.ID, cfg: dc, gateway: factory(dc)}
		if d.MainExposed && d.Enabled {
			exposed[d.ID] = byID[d.ID]
		}
	}
	var fixed *target
	if scoped != nil {
		fixed = byID[scoped.ID]
		if fixed == nil {
			dc := datasetSettings(cfg, scoped)
			fixed = &target{datasetID: scoped.ID, cfg: dc, gateway: factory(dc)}
		}
	}
	resolve := func(args map[string]pyval.Value) (*target, error) {
		if fixed != nil {
			return fixed, nil
		}
		id, _ := args["dataset_id"].(string)
		if args["dataset_id"] == nil || id == "default" {
			return defaultTarget, nil
		}
		if t, ok := exposed[id]; ok {
			return t, nil
		}
		return nil, &userError{"dataset_id is unknown, disabled, or not exposed on the main MCP endpoint"}
	}

	tools := []*tool{{
		name:        "healthcheck",
		description: "Check that this endpoint can reach its configured ClickHouse table.",
		run: func(ctx context.Context, _ map[string]pyval.Value) (pyval.Value, error) {
			t := fixed
			if t == nil {
				t = defaultTarget
			}
			rows, err := t.gateway.QueryRows(ctx, "SELECT 1 AS ok")
			if err != nil {
				return nil, err
			}
			status := "unknown"
			if len(rows) > 0 {
				if v, ok := rows[0].Get("ok"); ok && equalsOne(v) {
					status = "ok"
				}
			}
			out := pyval.NewDict(2)
			out.Set("status", status)
			out.Set("clickhouse_table", t.cfg.TableRef())
			return out, nil
		},
	}}

	if fixed == nil && len(reg.Datasets) > 0 {
		tools = append(tools, &tool{
			name:        "list_datasets",
			description: "List the default dataset and datasets explicitly exposed on this main endpoint.",
			wrapResult:  true,
			run: func(context.Context, map[string]pyval.Value) (pyval.Value, error) {
				entry := func(id, path, db, table string, isDefault bool) *pyval.Dict {
					d := pyval.NewDict(5)
					d.Set("dataset_id", id)
					d.Set("endpoint_path", path)
					d.Set("database", db)
					d.Set("table", table)
					d.Set("is_default", isDefault)
					return d
				}
				values := pyval.List{entry("default", cfg.MCPPath, cfg.ClickHouseDatabase, cfg.ClickHouseTable, true)}
				for _, d := range reg.Datasets {
					if d.MainExposed {
						values = append(values, entry(d.ID, d.EndpointPath, d.Database, d.Table, false))
					}
				}
				return values, nil
			},
		})
	}

	descriptions := mainDescriptions
	withDatasetID := true
	switch {
	case fixed != nil:
		descriptions, withDatasetID = scopedDescriptions, false
	case len(reg.Datasets) == 0:
		descriptions, withDatasetID = defaultDescriptions, false
	}
	targetFor := func(args map[string]pyval.Value) (*target, error) {
		if fixed != nil {
			return fixed, nil
		}
		if !withDatasetID {
			return defaultTarget, nil
		}
		return resolve(args)
	}
	for _, spec := range analyticsTools() {
		params := spec.params
		if withDatasetID {
			params = append(append([]param(nil), params...), optional("dataset_id", kindOptStr, nil))
		}
		run := spec.run
		tools = append(tools, &tool{
			name:        spec.name,
			description: descriptions[spec.name],
			params:      params,
			wrapResult:  true,
			run: func(ctx context.Context, args map[string]pyval.Value) (pyval.Value, error) {
				t, err := targetFor(args)
				if err != nil {
					return nil, err
				}
				return run(ctx, t, args)
			},
		})
	}
	return tools
}

func equalsOne(v pyval.Value) bool {
	switch t := v.(type) {
	case *big.Int:
		return t.IsInt64() && t.Int64() == 1
	case bool:
		return t
	case float64:
		return t == 1
	case pyval.Decimal:
		f, ok := new(big.Float).SetString(string(t))
		return ok && f.Cmp(big.NewFloat(1)) == 0
	}
	return false
}

var defaultDescriptions = map[string]string{
	"table_overview":     "Summarize the configured Rawbbit ClickHouse events table.",
	"list_event_names":   "List event names with counts and observed time ranges.",
	"discover_json_keys": "Discover top-level JSON keys in one of the Rawbbit JSON string columns.",
	"sample_events":      "Return recent raw event rows from the configured ClickHouse table.",
	"run_readonly_sql":   "Run a guarded read-only ClickHouse query against Rawbbit analytics data.",
	"calculate_dau":      "Calculate daily active users by actor_id for the configured events table.",
	"calculate_funnel":   "Calculate ordered user counts for an event-name funnel.",
}

var scopedDescriptions = map[string]string{
	"table_overview":     "Summarize the dataset bound to this scoped MCP endpoint.",
	"list_event_names":   "List event names with counts and observed time ranges.",
	"discover_json_keys": "Discover top-level JSON keys in one of the dataset's JSON string columns.",
	"sample_events":      "Return recent event rows from the dataset bound to this endpoint.",
	"run_readonly_sql":   "Run a guarded read-only query using this endpoint's restricted ClickHouse identity.",
	"calculate_dau":      "Calculate daily active users for the dataset bound to this endpoint.",
	"calculate_funnel":   "Calculate ordered user counts for an event-name funnel in this dataset.",
}

var mainDescriptions = map[string]string{
	"table_overview":     "Summarize the selected Rawbbit ClickHouse events table.",
	"list_event_names":   "List event names with counts and observed time ranges.",
	"discover_json_keys": "Discover top-level JSON keys in a Rawbbit JSON string column.",
	"sample_events":      "Return recent event rows from the selected dataset.",
	"run_readonly_sql":   "Run a guarded read-only query against the selected authorized dataset.",
	"calculate_dau":      "Calculate daily active users for the selected dataset.",
	"calculate_funnel":   "Calculate ordered user counts for an event-name funnel in the selected dataset.",
}

type analyticsSpec struct {
	name   string
	params []param
	run    func(ctx context.Context, t *target, args map[string]pyval.Value) (pyval.Value, error)
}

func rowsValue(rows []*pyval.Dict, err error) (pyval.Value, error) {
	if err != nil {
		return nil, err
	}
	out := make(pyval.List, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out, nil
}

func filters(args map[string]pyval.Value, eventKey string) clickhouse.OptionalFilters {
	f := clickhouse.OptionalFilters{AppID: str(args["app_id"]), Environment: str(args["environment"])}
	if eventKey != "" {
		f.EventName = str(args[eventKey])
	}
	return f
}

func limitSQL(args map[string]pyval.Value, maximum int64) string {
	return clickhouse.CheckedLimit(args["limit"].(*big.Int), maximum).String()
}

func analyticsTools() []analyticsSpec {
	excludeBots := optional("exclude_bots", kindBool, true)
	appID := optional("app_id", kindOptStr, nil)
	environment := optional("environment", kindOptStr, "prod")
	eventName := optional("event_name", kindOptStr, nil)
	bots := func(t *target, args map[string]pyval.Value) string {
		return clickhouse.BotFilterSQL(t.cfg, args["exclude_bots"].(bool))
	}
	return []analyticsSpec{
		{
			name:   "table_overview",
			params: []param{excludeBots},
			run: func(ctx context.Context, t *target, args map[string]pyval.Value) (pyval.Value, error) {
				sql := `
        SELECT
          count() AS events,
          uniqExact(app_id) AS apps,
          uniqExact(event_name) AS event_names,
          uniqExact(coalesce(nullIf(user_id, ''), user_pseudo_id)) AS actors,
          min(event_time) AS first_event_time,
          max(event_time) AS last_event_time
        FROM ` + t.cfg.TableRef() + `
        WHERE event_time IS NOT NULL
        ` + bots(t, args) + `
        `
				return rowsValue(t.gateway.QueryRows(ctx, sql))
			},
		},
		{
			name:   "list_event_names",
			params: []param{appID, environment, optional("limit", kindInt, pyval.Int(100)), excludeBots},
			run: func(ctx context.Context, t *target, args map[string]pyval.Value) (pyval.Value, error) {
				sql := `
        SELECT
          event_name,
          count() AS events,
          uniqExact(coalesce(nullIf(user_id, ''), user_pseudo_id)) AS actors,
          min(event_time) AS first_event_time,
          max(event_time) AS last_event_time
        FROM ` + t.cfg.TableRef() + `
        WHERE event_time IS NOT NULL
        ` + filters(args, "").SQL() + `
        ` + bots(t, args) + `
        GROUP BY event_name
        ORDER BY events DESC
        LIMIT ` + limitSQL(args, t.cfg.MaxQueryRows) + `
        `
				return rowsValue(t.gateway.QueryRows(ctx, sql))
			},
		},
		{
			name: "discover_json_keys",
			params: []param{
				optional("json_column", kindStr, "event_params_json"), eventName, appID, environment,
				optional("limit", kindInt, pyval.Int(100)), excludeBots,
			},
			run: func(ctx context.Context, t *target, args map[string]pyval.Value) (pyval.Value, error) {
				column := args["json_column"].(string)
				if !clickhouse.IsJSONColumn(column) {
					errRow := pyval.NewDict(1)
					errRow.Set("error", "json_column must be one of: "+strings.Join(clickhouse.SortedJSONColumns(), ", "))
					return pyval.List{errRow}, nil
				}
				sql := `
        SELECT
          key,
          count() AS rows_with_key
        FROM
        (
          SELECT arrayJoin(JSONExtractKeys(ifNull(nullIf(` + column + `, ''), '{}'))) AS key
          FROM ` + t.cfg.TableRef() + `
          WHERE event_time IS NOT NULL
          ` + filters(args, "event_name").SQL() + `
          ` + bots(t, args) + `
        )
        GROUP BY key
        ORDER BY rows_with_key DESC, key
        LIMIT ` + limitSQL(args, t.cfg.MaxQueryRows) + `
        `
				return rowsValue(t.gateway.QueryRows(ctx, sql))
			},
		},
		{
			name:   "sample_events",
			params: []param{eventName, appID, environment, optional("limit", kindInt, pyval.Int(20)), excludeBots},
			run: func(ctx context.Context, t *target, args map[string]pyval.Value) (pyval.Value, error) {
				sql := `
        SELECT
          event_id,
          app_id,
          environment,
          event_name,
          event_time,
          coalesce(nullIf(user_id, ''), user_pseudo_id) AS actor_id,
          session_id,
          platform,
          event_params_json,
          geo_json,
          ingest_user_agent
        FROM ` + t.cfg.TableRef() + `
        WHERE event_time IS NOT NULL
        ` + filters(args, "event_name").SQL() + `
        ` + bots(t, args) + `
        ORDER BY event_time DESC
        LIMIT ` + limitSQL(args, t.cfg.MaxSampleRows) + `
        `
				return rowsValue(t.gateway.QueryRows(ctx, sql))
			},
		},
		{
			name:   "run_readonly_sql",
			params: []param{required("sql", kindStr), optional("limit", kindInt, pyval.Int(100))},
			run: func(ctx context.Context, t *target, args map[string]pyval.Value) (pyval.Value, error) {
				checked, err := clickhouse.ValidateReadonlySQL(args["sql"].(string))
				if err != nil {
					return nil, err
				}
				lowered := pyval.Lower(checked)
				if (strings.HasPrefix(lowered, "select") || strings.HasPrefix(lowered, "with")) &&
					!strings.Contains(" "+lowered+" ", " limit ") {
					checked = "SELECT * FROM (" + checked + ") LIMIT " + limitSQL(args, t.cfg.MaxQueryRows)
				}
				return rowsValue(t.gateway.QueryRows(ctx, checked))
			},
		},
		{
			name: "calculate_dau",
			params: []param{
				required("start_date", kindStr), required("end_date", kindStr), appID, environment,
				optional("active_event_name", kindOptStr, nil), excludeBots,
			},
			run: func(ctx context.Context, t *target, args map[string]pyval.Value) (pyval.Value, error) {
				sql := `
        SELECT
          event_date,
          uniqExact(coalesce(nullIf(user_id, ''), user_pseudo_id)) AS dau
        FROM ` + t.cfg.TableRef() + `
        WHERE event_date BETWEEN toDate(` + clickhouse.QuoteString(args["start_date"].(string)) +
					`) AND toDate(` + clickhouse.QuoteString(args["end_date"].(string)) + `)
          AND event_time IS NOT NULL
        ` + filters(args, "active_event_name").SQL() + `
        ` + bots(t, args) + `
        GROUP BY event_date
        ORDER BY event_date
        `
				return rowsValue(t.gateway.QueryRows(ctx, sql))
			},
		},
		{
			name: "calculate_funnel",
			params: []param{
				required("steps", kindStrList), required("start_date", kindStr), required("end_date", kindStr),
				appID, environment, optional("window_hours", kindInt, pyval.Int(24)), excludeBots,
			},
			run: func(ctx context.Context, t *target, args map[string]pyval.Value) (pyval.Value, error) {
				var steps []string
				for _, s := range args["steps"].([]string) {
					if trimmed := pyval.Strip(s); trimmed != "" {
						steps = append(steps, trimmed)
					}
				}
				if len(steps) < 2 || len(steps) > 10 {
					errRow := pyval.NewDict(1)
					errRow.Set("error", "steps must contain between 2 and 10 event names")
					return pyval.List{errRow}, nil
				}
				sql := clickhouse.BuildFunnelSQL(t.cfg, clickhouse.FunnelParams{
					Steps:       steps,
					StartDate:   args["start_date"].(string),
					EndDate:     args["end_date"].(string),
					AppID:       str(args["app_id"]),
					Environment: str(args["environment"]),
					WindowHours: args["window_hours"].(*big.Int),
					ExcludeBots: args["exclude_bots"].(bool),
				})
				return rowsValue(t.gateway.QueryRows(ctx, sql))
			},
		},
	}
}
