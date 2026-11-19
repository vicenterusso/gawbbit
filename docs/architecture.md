# Architecture

Rawbbit is a self-hosted event pipeline for games. Game clients, web builds,
mobile apps and backend services send batched events over HTTP; ClickHouse
stores them; MCP clients, Metabase and SQL read them.

```text
game SDK -> Caddy -> Vector -> ClickHouse analytics.events -> MCP / Metabase / agents / users
                         \-> dataset_<id>.events (optional, per app_id)
```

Everything runs as one Docker Swarm stack:
[`quickstart/swarm`](../quickstart/swarm/README.md).

## Components

### Caddy

The public edge. For the collector hostname it:

- terminates TLS (automatic certificates);
- answers CORS preflights for the origins in `CORS_ALLOW_ORIGINS` and adds
  the CORS headers to responses;
- rejects bodies over `MAX_REQUEST_BYTES` with `413`, which makes the SDKs
  split the batch;
- passes the client address in `X-Forwarded-For`, replacing whatever the
  client sent;
- forwards `GET /healthz` to Vector's health endpoint.

It also proxies the MCP and Metabase hostnames.

### Vector

[Vector](https://vector.dev/) replaces the collector service. Its
configuration is [`quickstart/swarm/vector/vector.yaml`](../quickstart/swarm/vector/vector.yaml):

1. **Authenticate.** The `X-API-Key` must be a key of
   `COLLECTOR_API_KEYS_JSON`; otherwise `401`. The key's single allowed
   `app_id` travels with the request.
2. **Split.** `{"events": [...]}` becomes one Vector event per entry. The
   receive time, a request id, the `User-Agent` and the client address are
   copied to each.
3. **Validate and normalise.** Each event needs `event_id`, `app_id` (the
   key's app) and `event_name`, and an `event_timestamp` Vector can parse.
   Valid events become one `analytics.events` row: nested `user` and `device`
   fields become columns, `event_params`, `user_properties`, `traffic_source`,
   `consent` and `geo` become JSON strings, and the client address becomes
   `ingest_ip_hash = sha256(IP_HASH_SALT + ":" + ip)`. The raw address is not
   stored. An invalid event is dropped and logged with the reason; the other
   events of the batch are kept.
4. **GeoIP** (optional). Adds `country` and `country_code` from a
   MaxMind-format database to `geo_json`.
5. **Route.** An `app_id` listed in a dataset goes to `dataset_<id>.events`,
   everything else to `analytics.events`.
6. **Insert.** Rows are inserted in batches (up to 20 000 rows or one
   second) with `JSONEachRow`.

End-to-end acknowledgements are on: the HTTP response waits until ClickHouse
confirmed the insert of the request's events. A `200` therefore means the
events are stored. While ClickHouse is unreachable Vector retries and the
request stays open; the SDK times out, keeps the events in its durable queue
and sends them again. Vector itself holds no state, so its replicas can run
on any node.

### ClickHouse

The source of truth. [`schema.sql`](../quickstart/swarm/clickhouse/schema.sql)
defines `analytics.events` as a `ReplacingMergeTree(received_time)` ordered
by `(app_id, event_id)` and partitioned by month. A retried event (an SDK
resend after a timeout, a Vector retry) has the same key and collapses into
one row, the latest, when ClickHouse merges parts. Queries that must be exact
before a merge use `FINAL`.

The `clickhouse-setup` job creates the table, the service users (`rawbbit_vector`
can only insert; the MCP and Metabase users can only read) and the datasets.
The `clickhouse-backup` service runs a daily native `BACKUP` to the data node
and optionally to S3.

### Datasets

A dataset gives chosen `app_id`s their own database `dataset_<id>`, table and
read-only ClickHouse user, and the MCP server serves it at
`/datasets/<id>/mcp` for its own bearer tokens. Datasets provide logical
access separation, not resource isolation: they share the ClickHouse server.

### MCP server and Metabase

The [Rawbbit MCP server](../mcp-server/README.md) exposes read-only tools for
event discovery, JSON-key discovery, sampling, guarded SQL, DAU and funnel
checks to MCP clients and AI agents. Metabase is a BI interface over the same
ClickHouse data. Neither is part of ingestion.

## Design choices

- **ClickHouse as the source of truth.** Rawbbit kept a Parquet raw layer on
  object storage so the analytics database could change (ClickHouse or
  BigQuery) without touching ingestion. This deployment only uses ClickHouse,
  so the raw layer, NATS JetStream, the raw-writer, SeaweedFS and the hourly
  loader were removed. Durability comes from ClickHouse plus daily backups.
- **Off-the-shelf intake.** Validation and batching are Vector configuration,
  not service code to maintain. The trade-off is that Vector cannot return a
  per-request validation report: invalid events are dropped and logged
  instead of failing the request with `422`.
- **Deduplication in the table.** `ReplacingMergeTree` on `(app_id, event_id)`
  replaces the NATS duplicate window and the loader's per-window
  deduplication.
