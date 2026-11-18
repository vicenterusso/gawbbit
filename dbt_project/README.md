# Rawbbit ingestion runner (`dbt_project`)

This component loads bounded raw Parquet windows into the configured
ClickHouse `events` table in the Rawbbit two-VM deployment. The default target
is `CLICKHOUSE_DATABASE.events` (normally `analytics.events`); optional app-ID
routing can send selected apps to other existing `events` tables.

> dbt is no longer used. The directory name `dbt_project`, the image name
> `rawbbit-dbt-runner`, the `/app/bin/dbt-job` entrypoint, the `CLICKHOUSE_DBT_*`
> and `RAWBBIT_DBT_*` settings, the crontab, journals, exit codes and progress
> lines are kept for deployment compatibility. The former dbt-core 1.9.10 +
> dbt-clickhouse 1.9.8 project (model, macros, selector, profile and data
> tests) is reimplemented in Go and sends the same SQL statements to
> ClickHouse; see [Go rewrite notes](#go-rewrite-notes).

`RAWBBIT_RAW_LOAD_MODE` controls which implementation owns raw ingestion:

- `dbt`: the `rawbbit_events_load` model reads bounded Parquet paths,
  incrementally updates the configured default target, and runs its data
  tests. With routing disabled or unconfigured, this retains the all-app
  behavior;
- `legacy`: the host cron shell loader owns the default events table (normally
  `analytics.events`), while scheduled jobs log a skip.

The two paths share `/srv/rawbbit-two/dbt/pipeline.lock` and must not ingest
simultaneously.

## Layout

```text
cmd/dbt-job/            entrypoint (/app/bin/dbt-job; "dbt" when invoked via that name)
internal/runner/        windows, lock, journals, routing order, settlement, fencing
internal/model/         rawbbit_events_load model, window/app macros, data tests
internal/model/sql/     embedded SQL templates (former model, tests and adapter DDL/DML)
internal/clickhouse/    HTTP client and control plane (readiness, settlement)
internal/config/        route snapshot validation and revision
internal/progress/      bounded stderr progress lines
internal/dbtcompat/     `dbt build|debug|parse` compatibility CLI
internal/pyjson/        byte-compatible JSON for journals and summaries
testdata/               goldens recorded from the original Python + dbt runner
```

## Model

```text
rawbbit_events_load (alias events)
```

The model keeps dbt-clickhouse's `delete_insert` incremental semantics with
`(app_id, event_id)` as its stable key. It selects one winner for each key in
the current window (latest `received_time`, then highest `nats_sequence`,
latest `event_time`, highest `ingest_request_id`, nulls last), deletes
matching target keys, and inserts the winners. This makes overlapping hourly
and daily windows safe to replay at the event-key level.

Per attempt the runner sends, in order: `EXISTS DATABASE` (and
`CREATE DATABASE IF NOT EXISTS` when missing), a relation listing, then:

- first run (no `events` table): `create table ... empty as (<model>)` with
  `ENGINE MergeTree()`, `ORDER BY (app_id, environment, event_name,
  event_date, user_pseudo_id, event_time)`, `PARTITION BY
  toYYYYMM(event_date)` and `replicated_deduplication_window='0'`, followed by
  `insert into events (...) <model>`;
- incremental: drop/create/insert the scratch table
  `events__dbt_new_data_<invocation>`, lightweight
  `delete from events where (app_id, event_id) in (select ... from scratch)`,
  `insert into events select <target columns> from scratch`, then drop the
  scratch tables;
- an existing `events` *view* is replaced through `events__dbt_tmp` and
  `events__dbt_backup` renames, exactly like dbt's full-refresh path.

Model statements carry `SETTINGS s3_throw_on_zero_files_match=0`, so empty
hours are not errors. Every request carries `mutations_sync=2`,
`lightweight_deletes_sync=2`, `max_execution_time=600`,
`max_threads=DBT_CLICKHOUSE_MAX_THREADS` (default `2`) and the attempt tag as
`log_comment`. Raw files are read through the `rawbbit_raw_s3` named
collection from `app_id=<app|*>/event_date=YYYY-MM-DD/hour=HH/*.parquet`, one
path per hour of the window.

The model rejects empty event IDs and rows whose event timestamp cannot be
parsed. It preserves the configured default table's physical schema and never
full-refreshes a table; do not change that because the table is also the
legacy loader target and an operational boundary. Optional routing reuses
this one model and adds no per-dataset model or cron.

### Model variables

The former dbt vars keep their names, defaults and compile-error messages.
The runner always passes an explicit window; the `dbt` compatibility CLI
accepts them through `--vars` (YAML or JSON):

| Variable | Default | Meaning |
| --- | --- | --- |
| `rawbbit_window_start` / `rawbbit_window_end` | last `rawbbit_default_lookback_hours` whole hours | Hour-aligned UTC `YYYY-MM-DDTHH:00:00Z`, end after start. |
| `rawbbit_default_lookback_hours` | `3` | Default window length. |
| `rawbbit_max_model_window_hours` | `168` | Larger windows fail: use the chunked backfill. |
| `rawbbit_app_id` | none | Exact include: one raw partition plus `app_id = ...`. |
| `rawbbit_excluded_app_ids` | `[]` | Default complement: `app_id not in (...)`; exclusive with include. |

## Tests

The build runs the model and then its data tests with fail-fast semantics:
non-null `app_id`, `event_id` and `event_time`, plus global uniqueness of
`(app_id, event_id)` in the configured default events table. A failing test
fails the attempt (`dbt_failed`).

The tests inspect the complete target table, not only the current input window.
Audit data written by the legacy loader before switching ingestion ownership to
dbt mode.

## Progress and diagnostics

Each attempt emits bounded, best-effort progress lines on stderr for starts
and finishes of the ingestion model and its non-null tests:

- model `rawbbit_events_load`
- tests `not_null_event_id`, `not_null_app_id`, and `not_null_event_time`

For example:

```text
2026-10-05T12:00:00Z [dbt-progress job=<uuid> attempt=<uuid> kind=default db=analytics] START model rawbbit_events_load
2026-10-05T12:00:02Z [dbt-progress job=<uuid> attempt=<uuid> kind=default db=analytics] OK model rawbbit_events_load elapsed_seconds=2.400
```

The runner supplies the job, attempt, attempt kind (`default`, `primary`, or
`fallback`), and destination database. Progress statuses and labels are fixed
and allowlisted; model finishes map to `OK` or `ERROR`, test finishes to
`PASS`, `FAIL` or `ERROR`. Attempt-level lines also mark `START attempt`, the
`VERIFY attempt phase=server_settlement` check, and the terminal `END attempt`
outcome. An attempt that fails destination-readiness checks has attempt lines
without model/test lines. The stderr stream is informational, not a
job-failure signal. The final JSON summary is on stdout; relative order
between stdout and stderr is not guaranteed, so use the final summary, exit
code, and persistent records for the authoritative outcome.

Raw exceptions, SQL, credentials, paths, vars, server responses, and event or
user data are never forwarded. A failed build always gets the fixed terminal
line `FAIL dbt category=execution_failed last_phase=<phase>` with the last
observed node when available (`before_first_node` covers database setup and
connection failures). `FAIL test ...` lines add `category=test_failed`;
`ERROR` lines add `category=node_error`. The uniqueness test has no progress
label. Progress is bounded (64 queued lines) and can be dropped under sink
failure; a success line is not proof that ClickHouse completed the write: the
runner still performs its server-settlement verification before confirming an
attempt.

## Scheduling and recovery

The persistent `dbt-runner` container runs Supercronic in the foreground. Its
UTC schedule is versioned with the project:

```text
12 * * * * hourly build
30 3 * * * daily reconciliation
```

The hourly job uses a short configurable lookback. The daily job reconciles a
longer range for late files. Supercronic does not replay missed ticks; these
overlapping, idempotent windows are the recovery path.

The image's `DBT_UID` and `DBT_GID` build arguments must match the owner of
the host-mounted `/srv/rawbbit-two/dbt` directory. The Compose defaults are
`1000:1000`, matching the usual first non-root Linux user.

Scheduled jobs skip if another pipeline job holds the shared lock. Manual
backfills wait for the lock, require hour-aligned UTC timestamps, and process
the requested range in configurable chunks:

```bash
docker compose exec -T \
  -e RAWBBIT_DBT_MIRROR_PID1=1 \
  dbt-runner \
  /app/bin/dbt-job backfill \
  2026-07-01T00:00:00Z \
  2026-07-05T00:00:00Z
```

`RAWBBIT_DBT_MIRROR_PID1=1` mirrors the manual job output into the persistent
container's Docker log stream so it remains visible through Dozzle.
Manual backfill requires `RAWBBIT_RAW_LOAD_MODE=dbt`; in `legacy` mode,
scheduled jobs skip and manual backfill is rejected.

## Optional app-ID routing

Routing is an opt-in dbt-runner feature, independent of MCP endpoint
enablement. VM-two Ansible sources it from the non-secret
`rawbbit_two_dbt_routing_enabled` and `rawbbit_two_dbt_routes` variables and
renders one version-1 route snapshot for the runner. A route's `dataset_id`
references `rawbbit_two_datasets[].id`; Ansible resolves its ClickHouse target
from that metadata even when the dataset's MCP `enabled` flag is false. The MCP
runtime registry and MCP credentials are not used by ingestion.

The runner reads:

```env
RAWBBIT_DBT_ROUTING_ENABLED=0
RAWBBIT_DBT_ROUTES_FILE=/app/runtime/dbt-routes.json
```

Enabled routing also requires `RAWBBIT_RAW_LOAD_MODE=dbt`; the legacy shell
loader is not app-routed.

The checked-in Compose and Ansible role defaults remain
`ghcr.io/mirlan-irokez/rawbbit-dbt-runner:0.1.1`. That image predates the drain
and routing contract, and the deployment helper rejects it as a
routing-capable candidate; this README does not change either default. A tested
local build is an alternative to selecting a tested, compatible released image:
VM-two Ansible can select `rawbbit-dbt-runner:local` and build from this checked-in
project before candidate validation and safe activation. A local build does not
require a GHCR upload, and source copying alone does not update the running
container. For remote image selections, use a tested compatible release rather
than an untested/unpublished registry tag or registry `latest`. See the [VM-two
routing and deployment guide](../quickstart/vm_rawbbit_two/README.md#dbt-app-id-routing)
for image selection and cutover instructions.

With routing disabled (`0`), or with no routing configuration, ingestion keeps
loading all apps into `CLICKHOUSE_DATABASE.events`. Set the switch to `1` only
with a valid version-1 snapshot, for example:

```json
{
  "version": 1,
  "routes": [
    {
      "app_id": "example_app",
      "dataset_id": "example_dataset",
      "database": "example_events",
      "table": "events"
    }
  ]
}
```

Enabled routing requires a readable, supported snapshot before the first write,
even when its route list is empty. Missing, unreadable, malformed, unsupported,
duplicate, unsafe, unresolved, conflicting, or default-colliding route
configuration fails closed with exit code `3` and no writes. A valid destination
that is missing, inaccessible, or incompatible is instead a route-load failure
and falls back for that same app and window to the configured default table.
The runner validates and freezes one route snapshot under the shared lock per
invocation; every window/chunk uses that same recorded revision. A later
configuration change applies only to a later invocation.

For each window, the runner uses the single ingestion model sequentially:

1. Load the default-table complement, excluding every configured routed
   `app_id`.
2. Attempt each route using both an exact raw Parquet app path and an exact SQL
   `app_id` include filter.
3. If a routed attempt fails, try only that app and window against the default
   table with the same include filter, then continue to the next route.

The default complement's exclusion filter is never used for fallback. A known
default, route, or fallback failure does not prevent later route attempts or
remaining bounded windows from running. If a ClickHouse query or mutation may
still be running and its outcome is unknown, the runner records a durable fence
and stops conflicting writes until an operator resolves it. The runner tags
attempts and checks ClickHouse process and mutation state; a client
exit or cancellation alone is not proof that ClickHouse stopped the work.
Do not clear an unknown-work fence as a retry shortcut; verify the tagged work
is terminal, reconcile any partial output, and have an operator authorize
recovery first.

The existing hourly and daily schedules are unchanged. There is no per-route
cron, automatic replay queue, automatic cross-database delete/move, or full
refresh. Fallback rows are visible in the main/default dataset. A later
successful route does not remove those copies. `(app_id, event_id)` is a
per-table replay key; it does not guarantee uniqueness across databases, so
partial writes or fallback can leave copies in both places.
Removing the last route returns that app to default-table loading on later
lookback/backfill windows, but does not move or delete existing routed or
fallback rows. Rolling back to a pre-routing image while routes are enabled
requires the same drain procedure and explicit approval of all-app default
loading; otherwise stop ingestion rather than silently changing destinations.

Every attempt and terminal outcome is persisted under `/app/runtime` on the
existing `/srv/rawbbit-two/dbt` volume, together with the route revision used by
the invocation. Interpret job exit codes as follows:

| Exit | Meaning |
| --- | --- |
| `0` | All planned work completed without fallback. |
| `2` | Work completed using one or more default-table fallbacks; monitor as degraded. |
| `1` | One or more app-windows were not confirmed loaded. |
| `3` | Configuration or invocation input was invalid; no writes were made. |
| `4` | Deferred/skipped work or an unconfirmed invocation; inspect the recorded status and fence. |

Runner state is on the persistent `/srv/rawbbit-two/dbt` volume mounted at
`/app/runtime`. Inspect these atomic, mode-`0600` JSON records before replay:

| Path under `/app/runtime` | Contents |
| --- | --- |
| `jobs/<job_id>.json` | Invocation status, route revision, windows, and per-attempt outcomes. |
| `attempts/<attempt_id>.json` | Intended and actual table, window, route revision, and write status. |
| `requests/<attempt_id>/<query_id>.json` | Dispatched/confirmed ClickHouse request IDs and transport status; preflight uses `requests/preflight-<job_id>/`. |
| `write-fence.json` | Durable operator-review fence when ClickHouse work cannot be proved terminal. |

Exit `4` is shared by deferrals and unresolved invocations. The persisted
statuses `gated`, `lock_skipped`, `lock_wait_expired`, and `mode_disabled` are
non-success skips and do not themselves create a fence. An unconfirmed
ClickHouse server outcome writes `write-fence.json`; no later job may write
until an operator verifies tagged work is terminal and reconciles possible
partial output. Use the job, attempt, request, and fence records together; exit
code `4` alone does not distinguish these cases or prove ingestion completed.

Runner timeout environment settings accept positive integer seconds, bounded as
follows:

| Variable | Default | Maximum | Applies to |
| --- | ---: | ---: | --- |
| `RAWBBIT_DBT_ATTEMPT_TIMEOUT_SECONDS` | `900` | `7200` | One model build attempt. |
| `RAWBBIT_DBT_JOB_TIMEOUT_SECONDS` | `21600` | `172800` | Total invocation budget. |
| `RAWBBIT_DBT_CONTROL_TIMEOUT_SECONDS` | `10` | `60` | One ClickHouse control HTTP request. |
| `RAWBBIT_DBT_SETTLE_TIMEOUT_SECONDS` | `30` | `300` | Proving tagged requests, loader queries, and mutations are terminal. |
| `RAWBBIT_DBT_LOCK_WAIT_SECONDS` | `300` | `7200` | Manual backfill lock wait; scheduled jobs skip immediately. |

These optional timeout variables are read by the runner but are not currently
exported by the checked-in Compose or Ansible service configuration, so those
deployments use the defaults. Adding overrides requires an explicit reviewed
service-environment wiring change; putting a value in `.env` alone does not
pass it through.

There is no automatic retry. Before bounded manual replay, inspect the
persistent outcome and compare its route revision with the active snapshot. If
the revision differs, choose and review the intended mapping rather than
silently replaying to another destination. Replay only the required UTC window
range, before its source Parquet expires. Back up and retain outcome records and
route revisions long enough for investigation and that manual replay.

## Credentials

The runner container receives only ClickHouse loader (`CLICKHOUSE_DBT_*`) credentials. S3 credentials
remain inside the ClickHouse container and are exposed to SQL through the
`rawbbit_raw_s3` named collection. Consequently they do not appear in the
runner's SQL, journals or logs.

The ClickHouse configuration marks every S3 named-collection value as
non-overridable. The dbt user receives permission to use only the
`rawbbit_raw_s3` named collection, without named-collection administration or
secret-inspection privileges. The service-user initialization script creates a
secret-free `users.d` overlay that enables the configured ClickHouse admin to
delegate this privilege. Keep loader data privileges scoped to the configured
default and explicit route `events` tables. Before enabling a route, an operator
must verify the effective grants against the runner's `delete_insert`
statements (see [Model](#model)) on the deployed ClickHouse version and grant only the
verified operations. Do not grant MCP write access or broad `*.*` data
privileges. Routing does not create databases, tables, or grants, and the
first-boot user script is not a migration mechanism for persistent
installations. See
`quickstart/vm_rawbbit_two/clickhouse/initdb.d/02_service_users.sh` for the
current service-user baseline. Do not use the ClickHouse admin, MCP, Metabase,
or legacy loader account for dbt.

The safety control plane also requires explicit narrow `SELECT` grants to the
dedicated loader, even when routing is disabled. First-boot baseline grants do
not include these and do not migrate existing installations. In an authorized
ClickHouse admin session, review and apply only:

```sql
GRANT SELECT ON system.processes TO rawbbit_dbt;
GRANT SELECT ON system.mutations TO rawbbit_dbt;
GRANT SELECT ON system.query_log TO rawbbit_dbt;
GRANT SELECT ON system.tables TO rawbbit_dbt;
GRANT SELECT ON system.columns TO rawbbit_dbt;
```

The deployment runs `dbt-job verify-control` before releasing its gate; missing
observation access fails closed. Separately verify permission to cancel only the
loader's own tagged query on the deployed ClickHouse version. Do not grant
global query cancellation to bypass a fence.

## Building and testing

```bash
cd dbt_project
go test ./...                                   # offline: unit + golden tests
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
docker compose -f ../quickstart/vm_rawbbit_two/docker-compose.yml build dbt-runner
```

Offline tests include statement-for-statement comparison with the SQL
dbt-clickhouse 1.9.8 sent (`testdata/sql`), and window, route-snapshot and
JSON goldens recorded from the original runner (`testdata/unit`).

Integration tests (build tag `integration`) need a disposable ClickHouse 24.8
whose `rawbbit_raw_s3` named collection points at `<bucket URL>/raw/` and an
anonymous S3 bucket (for example SeaweedFS). They replay
`testdata/differential/scenarios.json` (first run, reruns, overlapping and
late windows, dedupe ranking, parsing rules, routing and fallback, invalid
configs, lock contention, gates, fences, test failures, randomized corpora)
and compare tables, DDL, journals, stdout, stderr and exit codes with the
goldens recorded from Python + dbt, then run the fault-injection cases:

```bash
RAWBBIT_DBT_IT_CLICKHOUSE=http://127.0.0.1:48123 \
RAWBBIT_DBT_IT_S3=http://127.0.0.1:48333/raw \
go test -tags integration ./internal/runner/
```

Never point them at production: they drop and recreate the `default_custom`,
`team_a`, `team_b`, `fresh_db` and `missing_target` databases and empty the bucket.

Inside a running runner container, `dbt debug` checks the loader connection
and `dbt parse` validates the project; the deployment helper runs both.

## Go rewrite notes

- The model runs in-process instead of in a dbt child process. Timeouts and
  signals cancel in-flight HTTP requests; the runner then still proves server
  quiescence (owned-query KILL, `system.query_log` terminal proof, mutations)
  before trusting the outcome, and fences when a build does not stop within
  10 seconds. The attempt outcome `signal_exit` (child killed by a signal)
  therefore no longer occurs; a crash of the runner itself is recovered as an
  interrupted intent by the next job, as before.
- Statement texts for writes and data tests equal dbt-clickhouse 1.9.8's
  (modulo whitespace and comments). Metadata reads differ: one
  `system.tables` listing replaces dbt's database/relation queries, and
  `allow_nondeterministic_mutations=1` is a request setting instead of a
  `SET` statement, so a request journal holds fewer entries per attempt with
  unchanged record fields. Requests use `wait_end_of_query=1` instead of HTTP
  progress headers.
- Fail-fast stops at the first failing test. dbt (one thread) could still run
  the next already-queued test, so its progress could show one more test and
  a later `last_node`; outcomes, exit codes and data are unchanged.
- Data tests run sequentially; `DBT_THREADS`, `DBT_PROFILES_DIR`,
  `DBT_PROJECT_DIR`, `DBT_LOG_PATH` and `DBT_TARGET_PATH` are ignored, and no
  `logs/<attempt>` or `target/<attempt>` directories are created.
- `dbt` in the image is a small compatibility CLI (`build`, `debug`, `parse`,
  `--version`), not dbt-core. It never prints server error bodies.
- A lone UTF-16 surrogate escape in a journal record is rewritten as U+FFFD
  when the record is re-saved during recovery.
- If even the configuration-error summary cannot be persisted, the runner
  prints its fixed "runtime unavailable" line and exits 4 (Python exited 1
  with a traceback).
- The image is `alpine` with Supercronic and the static binary (busybox `sh`
  is kept because Supercronic runs crontab lines through `/bin/sh`, and the
  deployment helper reads the mounted route snapshot with `cat`).
