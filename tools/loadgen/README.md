# rawbbit-loadgen

Sends synthetic game events to a Rawbbit collector, then checks that
ClickHouse holds every accepted event exactly once. It drives the CI
end-to-end test ([`quickstart/swarm/e2e.sh`](../../quickstart/swarm/e2e.sh)),
and you can also run it against your own deployment as a load test or a
smoke test after a deploy.

```bash
cd tools/loadgen
go build -o rawbbit-loadgen ./cmd/rawbbit-loadgen
```

## What a run does

1. Waits until `GET /healthz` answers.
2. Sends requests that must be rejected and checks the status of each one:
   - missing API key and invalid API key (401);
   - `app_id` not allowed for the key (403);
   - empty `events` (400);
   - invalid JSON, a missing `event_name` and a bad `event_timestamp` (422);
   - a batch over `MAX_EVENTS_PER_REQUEST` (413).
3. Sends `-events` distinct events from `-players` synthetic players:
   - batches of `-batch` events, at `-rate` events per second, over `-concurrency` connections;
   - a weighted mix of `level_started`, `level_completed`, `level_failed`, `item_collected`, `item_purchased`, `settings_changed` and `ad_watched`;
   - a `-duplicates` fraction of the batches is sent a second time with the same `event_id`s, to exercise NATS deduplication;
   - 5xx answers and network errors are retried with backoff.
4. Writes a manifest with the run tag, the hour window and the expected count for each `event_name`, and prints throughput and p50/p95/p99 latency.
5. **verify** runs one query per attempt until the counts match or `-verify-timeout` expires:
   - it goes through the MCP `run_readonly_sql` tool, or straight to ClickHouse over HTTP;
   - it reports events that are missing, events that should not be there (for example from rejected requests) and duplicate rows;
   - `-load-cmd` runs before each check. Use it to trigger a runner backfill instead of waiting for the hourly cron. It gets `$LOADGEN_WINDOW_START` and `$LOADGEN_WINDOW_END`.

Every event carries `event_params.loadgen_run=<run id>` and
`environment=loadgen` (set with `-environment`), so test data is easy to
filter out of dashboards and to delete:

```sql
ALTER TABLE analytics.events DELETE WHERE environment = 'loadgen';
```

## Examples

Against a deployment, with verification through MCP (the API key must be
allowed for `-app-id` in `COLLECTOR_API_KEYS_JSON`):

```bash
./rawbbit-loadgen run \
  -collector https://collector.example.com -api-key "$KEY" -app-id com.example.game \
  -events 20000 -rate 1000 -concurrency 16 \
  -mcp-url https://mcp.example.com/mcp -mcp-token "$MCP_TOKEN" \
  -load-cmd 'ssh master docker exec $(docker ps -q -f name=rawbbit_dbt-runner) /app/bin/dbt-job backfill $LOADGEN_WINDOW_START $LOADGEN_WINDOW_END'
```

Without `-load-cmd`, verify waits for the runner's hourly job. That job covers
only whole past hours, so raise `-verify-timeout` to about 75m.

Send now, verify later:

```bash
./rawbbit-loadgen send -collector ... -api-key ... -app-id ... -manifest run.json
./rawbbit-loadgen verify -manifest run.json -clickhouse-url http://127.0.0.1:8123 \
  -clickhouse-user admin -clickhouse-password "$PW" -verify-timeout 90m
```

Load only, with no rejection checks and no duplicates:

```bash
./rawbbit-loadgen send ... -negative=false -duplicates 0 -rate 0 -events 100000
```

Any flag can also come from an environment variable named `LOADGEN_` plus
the flag name in upper case, with `-` replaced by `_`. For example
`LOADGEN_API_KEY` or `LOADGEN_MCP_TOKEN`. Run `rawbbit-loadgen <command> -h`
for every flag.

The exit status is 0 when every check passed and 1 otherwise. Each result is
printed as a single `loadgen_*` line, so the output is easy to grep in CI.
