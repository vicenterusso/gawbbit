# Configuration

Every setting lives in [`quickstart/swarm/.env.example`](../quickstart/swarm/.env.example);
copy it to `.env` and run `./deploy.sh`. This page groups the settings by
responsibility. Optional datasets are configured in `datasets.json` (see the
[Swarm guide](../quickstart/swarm/README.md#datasets)).

## Images and scaling

- `MCP_SERVER_IMAGE` — the only Rawbbit image; CI publishes it for every
  release tag
- `VECTOR_IMAGE`, `CLICKHOUSE_IMAGE`, `CADDY_IMAGE`, `POSTGRES_IMAGE`,
  `METABASE_IMAGE` — optional overrides of the pinned upstream images
- `VECTOR_REPLICAS`, `VECTOR_MAX_REPLICAS_PER_NODE`, `MCP_SERVER_REPLICAS`,
  `METABASE_REPLICAS`

## Public hostnames

- `COLLECTOR_PUBLIC_HOSTNAME` — where the SDKs send events
- `MCP_PUBLIC_HOSTNAME`
- `METABASE_PUBLIC_HOSTNAME`

Point their DNS records at the data node.

## TLS

- `TLS_MODE` — `auto` (default): Caddy obtains certificates itself on ports
  80 and 443. `cloudflare-flexible`: Cloudflare (SSL Flexible) talks plain
  HTTP to Caddy on `HTTP_PORT`. `cloudflare`: Cloudflare (SSL Full strict)
  talks HTTPS and Caddy serves the Cloudflare Origin Certificate; see
  [Behind Cloudflare](../quickstart/swarm/README.md#behind-cloudflare)
- `HTTP_PORT` — host port for HTTP in `cloudflare-flexible` mode (default 80)
- `HTTPS_PORT` — host port Caddy publishes for HTTPS (default 443)
- `ORIGIN_CERT_FILE`, `ORIGIN_KEY_FILE` — the origin certificate and key
  (`cloudflare` mode; default `cloudflare/origin.pem` and `cloudflare/origin.key`)
- `CADDY_TRUSTED_PROXIES` — peers allowed to set the client address; deploy.sh
  fills in Cloudflare's ranges in both cloudflare modes

## Event collector

- `COLLECTOR_API_KEYS_JSON` — `{"<api key>": "<app_id>", ...}`; each key may
  send events for exactly one `app_id`
- `IP_HASH_SALT` — salt of `ingest_ip_hash`; changing it breaks the link
  between old and new hashes of the same address
- `MAX_REQUEST_BYTES` — larger bodies get `413` (default 1 MiB)
- `CORS_ALLOW_ORIGINS` — comma-separated browser origins allowed to post
  events, `*` for any, empty for none
- `CORS_MAX_AGE_SECONDS` — how long browsers cache a preflight
- `GEOIP_ENABLED`, `GEOIP_HOST_DIR` — optional country lookup from
  `GEOIP_HOST_DIR/dbip-country-lite.mmdb`
- `RAWBBIT_DATASETS_FILE` — dataset file (default `datasets.json`)

## ClickHouse

- `CLICKHOUSE_ADMIN_USER`, `CLICKHOUSE_ADMIN_PASSWORD`
- `CLICKHOUSE_VECTOR_PASSWORD` — the insert-only user `rawbbit_vector`
- `CLICKHOUSE_MCP_USER`, `CLICKHOUSE_MCP_PASSWORD` — read-only, `analytics.*`
- `CLICKHOUSE_METABASE_USER`, `CLICKHOUSE_METABASE_PASSWORD` — read-only,
  `analytics.*` and every dataset
- `CLICKHOUSE_CONFIG_PROFILE_FILE`, `CLICKHOUSE_USERS_PROFILE_FILE` — memory
  profiles in `quickstart/swarm/clickhouse/` (`low-memory`, `production-small`
  for 8 vCPU / 32 GB, `production-medium` for 8 vCPU / 64 GB)

## Backups

- `BACKUP_AT` — daily UTC time, `HH:MM`
- `BACKUP_KEEP_LOCAL` — local copies kept per database
- `BACKUP_S3_URL`, `BACKUP_S3_ACCESS_KEY`, `BACKUP_S3_SECRET_KEY` — optional
  off-site copy

## MCP server

- `MCP_PATH`, `MCP_MAX_QUERY_ROWS`, `MCP_MAX_SAMPLE_ROWS`,
  `MCP_MAX_EXECUTION_SECONDS`, `MCP_BOT_USER_AGENT_REGEX`
- authentication: `MCP_API_KEYS_JSON` (static bearer tokens) or the
  `MCP_JWT_*` settings; `MCP_ALLOW_UNAUTHENTICATED=1` only for private
  networks

See [`mcp-server/README.md`](../mcp-server/README.md).

## Postgres and Metabase

- `POSTGRES_SUPERUSER`, `POSTGRES_SUPERUSER_PASSWORD`
- `METABASE_DB_NAME`, `METABASE_DB_USER`, `METABASE_DB_PASSWORD`
- `METABASE_TIMEZONE`

## Stack

- `RAWBBIT_STACK_NAME` — `docker stack deploy` name (default `rawbbit`)
- `RAWBBIT_DATA_ROOT` — host directory of all persistent state on the data
  node (default `/srv/rawbbit`)
- `LOG_LEVEL` — MCP server log level; `VECTOR_LOG` sets Vector's
