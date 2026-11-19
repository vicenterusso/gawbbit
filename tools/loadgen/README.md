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
2. Sends requests the collector must reject, and checks the status of each:
   - missing API key and invalid API key (401);
   - invalid JSON (400);
   - a body over `-max-request-bytes` (413).
3. Sends requests the collector must accept (200) while dropping their
   events: an `app_id` the key may not send, a missing `event_name` and a bad
   `event_timestamp`. Their events carry the run tag, so verify proves none of
   them was stored.
4. Sends `-events` distinct events from `-players` synthetic players:
   - batches of `-batch` events, at `-rate` events per second, over `-concurrency` connections;
   - a weighted mix of `level_started`, `level_completed`, `level_failed`, `item_collected`, `item_purchased`, `settings_changed` and `ad_watched`;
   - a `-duplicates` fraction of the batches is sent a second time with the same `event_id`s, to exercise deduplication;
   - 5xx answers and network errors are retried with backoff.
5. Writes a manifest with the run tag, the hour window and the expected count for each `event_name`, and prints throughput and p50/p95/p99 latency.
6. **verify** runs one query per attempt until the counts match or `-verify-timeout` expires:
   - it goes through the MCP `run_readonly_sql` tool, or straight to ClickHouse over HTTP;
   - it reads the table with `FINAL`, so rows ClickHouse has not merged yet do not count as duplicates;
   - it reports events that are missing, events that should not be there (for example dropped ones) and duplicate rows;
   - `-load-cmd`, if set, runs before each check with `$LOADGEN_WINDOW_START` and `$LOADGEN_WINDOW_END` in its environment.

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
  -mcp-url https://mcp.example.com/mcp -mcp-token "$MCP_TOKEN"
```

A dataset app is checked through its own endpoint and table:

```bash
./rawbbit-loadgen run ... -app-id com.example.team_a_game \
  -mcp-url https://mcp.example.com/datasets/team_a/mcp -mcp-token "$TEAM_A_TOKEN" \
  -table dataset_team_a.events
```

Send now, verify later:

```bash
./rawbbit-loadgen send -collector ... -api-key ... -app-id ... -manifest run.json
./rawbbit-loadgen verify -manifest run.json -clickhouse-url http://127.0.0.1:8123 \
  -clickhouse-user admin -clickhouse-password "$PW"
```

Load only, with no rejection checks and no duplicates:

```bash
./rawbbit-loadgen send ... -negative=false -duplicates 0 -rate 0 -events 100000
```

Each request is answered after ClickHouse stored it, which takes about a
second (Vector's batch interval), so throughput grows with `-concurrency` and
`-batch`.

Any flag can also come from an environment variable named `LOADGEN_` plus
the flag name in upper case, with `-` replaced by `_`. For example
`LOADGEN_API_KEY` or `LOADGEN_MCP_TOKEN`. Run `rawbbit-loadgen <command> -h`
for every flag.

The exit status is 0 when every check passed and 1 otherwise. Each result is
printed as a single `loadgen_*` line, so the output is easy to grep in CI.
