package clickhouse

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/config"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/storage"
)

// SortingKey and PartitionKey are the required events table layout.
const (
	PartitionKey = "toYYYYMM(event_date)"
	SortingKey   = "app_id, environment, event_name, event_date, user_pseudo_id, event_time"
)

// StringColumns lists the string-typed events columns in table order.
var StringColumns = []string{"event_id", "app_id", "environment", "event_name", "user_id",
	"user_pseudo_id", "session_id", "platform", "app_version", "os_version",
	"device_model", "locale", "timezone", "event_params_json", "user_properties_json",
	"traffic_source_json", "geo_json", "consent_json", "ingest_request_id",
	"ingest_user_agent", "ingest_ip_hash", "nats_stream"}

// ExpectedColumns is the compatible events schema (LowCardinality stripped).
var ExpectedColumns = func() map[string]string {
	columns := map[string]string{}
	for _, name := range StringColumns {
		columns[name] = "Nullable(String)"
	}
	for _, name := range []string{"app_id", "environment", "event_name", "user_pseudo_id"} {
		columns[name] = "String"
	}
	columns["event_time"] = "DateTime64(3, 'UTC')"
	columns["event_date"] = "Date"
	columns["received_time"] = "Nullable(DateTime64(3, 'UTC'))"
	columns["nats_sequence"] = "Nullable(Int64)"
	return columns
}()

// Control is the bounded control plane using the loader identity, never MCP
// credentials.
type Control struct {
	client        *Client
	Timeout       time.Duration
	SettleTimeout time.Duration
	// Query is replaceable in tests; it runs one FORMAT JSON statement.
	Query func(sql string) ([]map[string]any, error)
	sleep func(time.Duration)
}

// NewControl validates the endpoint and builds a control client.
func NewControl(env config.Env, timeoutSeconds, settleSeconds int) (*Control, error) {
	ep, err := ParseEndpoint(env)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	c := &Control{client: NewClient(ep, timeout, timeout), Timeout: timeout,
		SettleTimeout: time.Duration(settleSeconds) * time.Second, sleep: time.Sleep}
	c.Query = c.query
	return c, nil
}

// User is the dedicated loader identity.
func (c *Control) User() string { return c.client.Endpoint.User }

func (c *Control) query(sql string) ([]map[string]any, error) {
	queryID := UUID4()
	params := url.Values{}
	params.Set("query_id", queryID)
	params.Set("param_control_query_id", queryID)
	params.Set("log_comment", "rawbbit-dbt-control")
	params.Set("max_execution_time", strconv.Itoa(int(c.Timeout/time.Second)))
	params.Set("wait_end_of_query", "1")
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	body, err := c.client.Do(ctx, sql+" FORMAT JSON", params, 4*1024*1024)
	if err != nil {
		return nil, ErrRequest
	}
	return Rows(body)
}

func str(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}

// Ready reports whether database.events exists with the compatible engine,
// keys and columns, and the loader can SELECT from it.
func (c *Control) Ready(database string) bool {
	table, err := c.Query("SELECT engine, partition_key, sorting_key FROM system.tables WHERE database = " +
		Literal(database) + " AND name = 'events'")
	if err != nil || len(table) != 1 || str(table[0], "engine") != "MergeTree" ||
		str(table[0], "partition_key") != PartitionKey || str(table[0], "sorting_key") != SortingKey {
		return false
	}
	columns, err := c.Query("SELECT name, type FROM system.columns WHERE database = " +
		Literal(database) + " AND table = 'events'")
	if err != nil {
		return false
	}
	actual := map[string]string{}
	for _, row := range columns {
		kind := str(row, "type")
		if strings.HasPrefix(kind, "LowCardinality(") {
			kind = strings.Replace(kind, "LowCardinality(", "", 1)
			kind = kind[:len(kind)-1]
		}
		actual[str(row, "name")] = kind
	}
	if len(actual) != len(ExpectedColumns) {
		return false
	}
	for name, kind := range ExpectedColumns {
		if got, ok := actual[name]; !ok || got != kind {
			return false
		}
	}
	// Metadata visibility alone does not establish SELECT permission.
	_, err = c.Query("SELECT event_id FROM `" + database + "`.events LIMIT 0")
	return err == nil
}

// Settle never equates client timeout, loader exit or a KILL response with
// stopped writes. It checks ALL loader/tagged processes (metadata included).
// Mutations have no reliable ownership field and are never killed; any
// pending mutation in these databases' events/scratch namespace keeps the
// fence closed. Requests without an acknowledged response additionally
// require a terminal query_log event, even when the process list is empty.
// Missing log visibility or dropped logs fail closed.
func (c *Control) Settle(tag string, databases []string, requestDir string) bool {
	deadline := time.Now().Add(c.SettleTimeout)
	var pending []string
	for _, path := range storage.GlobJSON(requestDir) {
		record, err := storage.LoadObject(path)
		if err != nil {
			return false
		}
		status, ok1 := record.Get("status")
		queryID, ok2 := record.Get("query_id")
		if !ok1 || !ok2 {
			return false
		}
		if s, ok := status.(string); !ok || s != "confirmed" {
			id, ok := queryID.(string)
			if !ok {
				return false
			}
			pending = append(pending, id)
		}
	}
	owned := "user = " + Literal(c.User()) + " AND Settings['log_comment'] = " + Literal(tag)
	quoted := make([]string, len(databases))
	for i, db := range databases {
		quoted[i] = Literal(db)
	}
	for {
		active, err := c.Query("SELECT query_id FROM system.processes WHERE " + owned)
		if err != nil {
			return false
		}
		if len(active) > 0 {
			// No broad-user or database-only cancellation, ever.
			if _, err := c.Query("KILL QUERY WHERE " + owned + " SYNC"); err != nil {
				return false
			}
			if active, err = c.Query("SELECT query_id FROM system.processes WHERE " + owned); err != nil {
				return false
			}
		}
		// A dedicated loader identity is required. Never kill unowned work,
		// but do not overlap writes with any other loader query.
		other, err := c.Query("SELECT query_id FROM system.processes WHERE user = " +
			Literal(c.User()) + " AND query_id != {control_query_id:String}")
		if err != nil {
			return false
		}
		mutations, err := c.Query("SELECT mutation_id FROM system.mutations WHERE database IN (" +
			strings.Join(quoted, ",") + ")" +
			" AND (table = 'events' OR startsWith(table, 'events__dbt')) AND NOT is_done")
		if err != nil {
			return false
		}
		terminal := map[string]bool{}
		if len(pending) > 0 {
			ids := make([]string, len(pending))
			for i, id := range pending {
				ids[i] = Literal(id)
			}
			rows, err := c.Query("SELECT query_id FROM system.query_log WHERE user = " +
				Literal(c.User()) + " AND log_comment = " + Literal(tag) +
				" AND query_id IN (" + strings.Join(ids, ",") + ")" +
				" AND type IN ('QueryFinish', 'ExceptionBeforeStart', 'ExceptionWhileProcessing')")
			if err != nil {
				return false
			}
			for _, row := range rows {
				terminal[str(row, "query_id")] = true
			}
		}
		allTerminal := true
		for _, id := range pending {
			allTerminal = allTerminal && terminal[id]
		}
		if len(active) == 0 && len(other) == 0 && len(mutations) == 0 && allTerminal {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		c.sleep(250 * time.Millisecond)
	}
}
