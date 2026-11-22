# Rawbbit on Docker Swarm

One stack for the whole pipeline, on one node or many:

```text
game SDK -> Caddy -> Vector (N replicas) -> ClickHouse -> MCP / Metabase
                                              \-> daily BACKUP (local, optional S3)
```

ClickHouse is the source of truth. Vector answers a request only after
ClickHouse stored its events, so it keeps no state of its own.

## Placement

| Service | Replicas | Where |
|---|---|---|
| `caddy` | 1 | data node (TLS certificates on disk, host-mode ports 80/443, or one port behind Cloudflare) |
| `clickhouse` | 1 | data node |
| `clickhouse-setup` | one-shot job | any node (talks to ClickHouse over the network) |
| `clickhouse-backup` | 1 | data node (prunes the local backup directory) |
| `postgres` | 1 | data node (Metabase state) |
| `vector` | `VECTOR_REPLICAS` (1) | any node |
| `mcp-server` | `MCP_SERVER_REPLICAS` (1) | any node |
| `metabase` | `METABASE_REPLICAS` (1) | any node |

The **data node** is the manager with the label `rawbbit.data=true`. Every
stateful service uses bind mounts under `RAWBBIT_DATA_ROOT` (`/srv/rawbbit`)
on that node, so the label must be on exactly one node; `deploy.sh` refuses to
deploy otherwise. If ClickHouse is down, Vector keeps the requests open and
retries; the SDKs time out, keep the events in their local queues and send
them again later. This stack scales the stateless tier; it does not make the
data tier highly available.

Caddy publishes 80/443 in host mode on the data node (or one port behind Cloudflare,
see [Behind Cloudflare](#behind-cloudflare)), so point the public DNS names
there. Host mode preserves the client address (the routing mesh would
replace it); Vector hashes it into `ingest_ip_hash` and uses it for GeoIP.

## Requirements

- Docker Engine 24+ on every node (`replicated-job`, `ulimits` and
  `max_replicas_per_node` in stacks). Tested with 27.x and 29.x.
- Between nodes: TCP 2377, TCP/UDP 7946, UDP 4789 and **ESP (IP protocol
  50)**, because the overlay networks are encrypted. On Oracle Cloud, add
  these to the VCN security list or NSG.
- The MCP server image for `linux/amd64` and/or `linux/arm64` in a registry
  every node can pull from (CI publishes `ghcr.io/<owner>/rawbbit-mcp-server`
  for each release tag). The other images are upstream: Caddy, Vector,
  ClickHouse, Postgres and Metabase.
- `bash` 4+ and `sha256sum` on the manager you deploy from, plus `jq` (or
  Docker, which runs the jq image) when you use datasets.

## Deploy

1. Create the swarm and label the data node (on the master):

   ```bash
   docker swarm init --advertise-addr <private-ip>
   docker node update --label-add rawbbit.data=true "$(hostname)"
   ```

   Join the other machines with the `docker swarm join` command it prints.
   Join them as workers; additional managers are fine but must not carry the
   `rawbbit.data` label.

2. Create the host directories on the data node, and the GeoIP directory on
   every other node:

   ```bash
   sudo ./bootstrap-data-node.sh               # data node
   sudo ./bootstrap-data-node.sh --geoip-only  # every other node
   ```

3. Configure:

   ```bash
   cp .env.example .env
   chmod 600 .env
   $EDITOR .env
   ```

   `deploy.sh` refuses to run while any `change_me` value is left.
   `COLLECTOR_API_KEYS_JSON` maps each API key to the `app_id`s it may send:
   an exact `app_id` (`{"key-1":"com.example.mygame"}`), a prefix ending in
   `*` (`{"key-1":"com.example.*"}`) or `*` for any. With a prefix or `*`, a
   new game needs no change on the server: it just sends its own `app_id`.
   Browser games need their origin in `CORS_ALLOW_ORIGINS`; native and Unity
   games do not.

4. Deploy (and later, update) from the master:

   ```bash
   ./deploy.sh
   ```

   It reads `.env` with Compose dotenv rules, checks the cluster, renders the
   dataset files, names every config and secret by a hash of its content
   (Swarm configs are immutable, so changing a file rolls only the services
   that use it), runs `docker stack deploy --prune --with-registry-auth` and
   removes unused older configs and secrets. `./deploy.sh --render` prints the
   interpolated stack without deploying.

   On every deploy where its inputs changed, the `clickhouse-setup` job
   creates `analytics.events`, the service users and every dataset. It is
   idempotent. Follow it with
   `docker service logs rawbbit_clickhouse-setup`.

5. Verify:

   ```bash
   docker stack services rawbbit
   curl -i https://collector.example.com/healthz
   curl -i https://collector.example.com/v1/events:batch \
     -H 'Content-Type: application/json' -H 'X-API-Key: <key>' \
     -d '{"events":[{"event_id":"'"$(uuidgen)"'","app_id":"com.example.mygame",
          "event_name":"test_event","event_timestamp":"'"$(date -u +%FT%TZ)"'"}]}'
   ```

   A `200` means the event is in ClickHouse:

   ```bash
   docker exec "$(docker ps -q -f name=rawbbit_clickhouse)" clickhouse-client \
     -u admin --password "$CLICKHOUSE_ADMIN_PASSWORD" \
     -q "SELECT event_name, event_time FROM analytics.events FINAL ORDER BY received_time DESC LIMIT 5"
   ```

   For a fuller check, run [`rawbbit-loadgen`](../../tools/loadgen/README.md)
   against the deployment.

## Behind Cloudflare

Two modes put Cloudflare in front, for example when ports 80 and 443 are
taken on the host by Docker Compose services that already run there. In
both, players connect to Cloudflare over HTTPS, Caddy refuses connections
that do not come from [Cloudflare's ranges](https://www.cloudflare.com/ips/),
and the player's address comes from `CF-Connecting-IP`, so
`ingest_ip_hash` and GeoIP see the player, not Cloudflare.

| `TLS_MODE` | Cloudflare SSL/TLS mode | Cloudflare → server | On the server |
|---|---|---|---|
| `cloudflare-flexible` | Flexible | plain HTTP to `HTTP_PORT` | nothing |
| `cloudflare` | Full (strict) | HTTPS to `HTTPS_PORT` | a Cloudflare Origin Certificate |

With Flexible, the traffic between Cloudflare and the server (API keys, MCP
tokens, Metabase logins) is not encrypted. `cloudflare` closes that gap at
the cost of one certificate file.

1. In Cloudflare, for the zone:
   - DNS: `A` records for the three hostnames to the data node's public IP,
     **proxied** (orange cloud).
   - SSL/TLS → Overview: **Flexible** or **Full (strict)**, matching
     `TLS_MODE`.
   - Full (strict) only: SSL/TLS → Origin Server → **Create Certificate**
     for the three hostnames (or `*.yourdomain.com`). Save the certificate as
     `cloudflare/origin.pem` and the private key as `cloudflare/origin.key`
     next to `deploy.sh` (`chmod 600`; the directory is git-ignored).
   - Rules → **Origin Rules**: when the hostname is one of the three, rewrite
     the destination port to `HTTP_PORT` (Flexible) or `HTTPS_PORT` (Full).
     Players keep using `https://collector.yourdomain.com`. Without the rule
     the port must be one Cloudflare proxies (HTTP: 8080, 8880, 2052, 2082,
     2086, 2095; HTTPS: 8443, 2053, 2083, 2087, 2096) and appear in the URLs.
   - Security: the SDKs are not browsers and cannot solve challenges. Keep
     **Bot Fight Mode** off, or add a WAF custom rule that skips the managed
     challenges for the collector hostname.
2. In `.env`, for example:

   ```ini
   TLS_MODE=cloudflare-flexible
   HTTP_PORT=8080
   ```

   or `TLS_MODE=cloudflare` with `HTTPS_PORT=8443`.
3. Open that port in the host firewall and the cloud security list (on OCI,
   an ingress rule in the VCN security list or NSG). Caddy already refuses
   other peers, so you may open it to `0.0.0.0/0` or only to Cloudflare's
   ranges.
4. `./deploy.sh`, then check through Cloudflare:
   `curl -i https://collector.yourdomain.com/healthz`. A `curl` straight to
   the server's IP and port is refused on purpose.

Cloudflare accepts bodies up to 100 MB, well above `MAX_REQUEST_BYTES`. When
Cloudflare publishes new ranges, set `CADDY_TRUSTED_PROXIES` in `.env` to the
full new list.

## What the collector accepts

`POST /v1/events:batch` with `X-API-Key` and a JSON body
`{"events": [ ... ]}`, the same request the Rawbbit SDKs send:

| Request | Answer |
|---|---|
| valid | `200` with an empty body, after ClickHouse stored the events |
| missing or unknown `X-API-Key` | `401` |
| body is not JSON | `400` |
| body over `MAX_REQUEST_BYTES` | `413` (the SDKs split the batch) |
| an event without `event_id`, `app_id` or `event_name`, with an unparseable `event_timestamp`, or with an `app_id` the key may not send | that event is dropped, the rest is stored, `200` |

Dropped events are logged with the reason:

```bash
docker service logs rawbbit_vector 2>&1 | grep event_dropped
```

`event_timestamp` may be RFC 3339 (`2026-11-19T10:00:00Z`, with or without
fraction and offset), `YYYY-MM-DD HH:MM:SS` (UTC) or a number of seconds or
milliseconds since the epoch.

A resent event (same `app_id` and `event_id`) collapses into one row when
ClickHouse merges parts; add `FINAL` to a query that must be exact before
that. MCP, Metabase and the load generator see the table as it is.

## Datasets

A dataset sends chosen `app_id`s to their own database and gives them their
own read-only MCP endpoint, for example to hand a game's data to someone
without the rest:

```bash
cp datasets.example.json datasets.json   # git-ignored
$EDITOR datasets.json
./deploy.sh
```

```json
{
  "datasets": [
    {
      "id": "team_a",
      "app_ids": ["com.example.team_a_game"],
      "main_exposed": false,
      "clickhouse_password": "long-random-password",
      "mcp_tokens": { "agent": "long-random-token" }
    }
  ]
}
```

For each dataset, `clickhouse-setup` creates the database `dataset_<id>`, the
table `dataset_<id>.events` (same structure as `analytics.events`) and the
read-only ClickHouse user `mcp_<id>`. Vector inserts events of the listed
`app_id`s there instead of `analytics.events`. The MCP server serves the
dataset at `https://<MCP_PUBLIC_HOSTNAME>/datasets/<id>/mcp` for the
`mcp_tokens` bearer tokens. `main_exposed: true` also offers the dataset to
the main `/mcp` endpoint's tools. Metabase's ClickHouse user can read every
dataset.

- Use another `RAWBBIT_DATASETS_FILE` path if you keep the file elsewhere.
- Removing a dataset (or an `app_id`) from the file stops routing and removes
  its MCP endpoint; its database, data and user stay. Drop them by hand.
- Moving an `app_id` into a dataset does not move its existing rows.

## Backups

`clickhouse-backup` runs ClickHouse's native `BACKUP DATABASE` for
`analytics` and every `dataset_*` database each day at `BACKUP_AT` (UTC):

- locally to `${RAWBBIT_DATA_ROOT}/clickhouse/backups/<database>/<timestamp>.zip`,
  keeping the newest `BACKUP_KEEP_LOCAL` copies;
- when `BACKUP_S3_URL` is set, also to `<BACKUP_S3_URL>/<database>/<timestamp>.zip`
  (any S3-compatible service; the key needs `s3:PutObject`, `s3:GetObject` and
  `s3:ListBucket`). Expire old copies with a bucket lifecycle rule.

Run one now:

```bash
docker exec "$(docker ps -q -f name=rawbbit_clickhouse-backup)" /bin/bash /etc/rawbbit/backup.sh --now
docker service logs rawbbit_clickhouse-backup
```

Restore a database (drop or rename the current one first):

```bash
docker exec -it "$(docker ps -q -f name=rawbbit_clickhouse)" clickhouse-client -u admin --password ... \
  -q "RESTORE DATABASE analytics FROM File('analytics/20261119T033000Z.zip')"
# or from S3:
#  -q "RESTORE DATABASE analytics FROM S3('https://bucket.s3.region.amazonaws.com/rawbbit/analytics/<stamp>.zip', '<key>', '<secret>')"
```

Users and grants are not part of the backup; `clickhouse-setup` recreates
them on the next deploy (`docker service update --force rawbbit_clickhouse-setup`
runs it again right away).

## Scaling

```bash
docker service scale rawbbit_vector=3
```

or set `VECTOR_REPLICAS` in `.env` and run `./deploy.sh` again (a plain
`docker service scale` is reverted by the next deploy).

- **vector**: stateless; scale freely. The same `GEOIP_HOST_DIR` (with
  `dbip-country-lite.mmdb` when `GEOIP_ENABLED=1`) must exist on every node
  that can run it.
- **mcp-server**: stateless; scale freely.
- **metabase**: keep 1 unless you have tested Metabase's multi-instance mode.

## GeoIP

With `GEOIP_ENABLED=1`, Vector looks up the client address in
`GEOIP_HOST_DIR/dbip-country-lite.mmdb` (any MaxMind-format country database,
for example [DB-IP Country Lite](https://db-ip.com/db/download/ip-to-country-lite))
and adds `country` and `country_code` to the event's `geo_json`. The file
must exist before Vector starts.

## End-to-end test

`./e2e.sh --build` builds the MCP image, deploys this stack on a single-node
swarm with random secrets, two Vector replicas and a dataset, and runs
[`rawbbit-loadgen`](../../tools/loadgen/README.md) twice: once for a main app
(events, rejected requests, dropped events and duplicate batches, checked
through the main MCP endpoint) and once for the dataset app (checked through
the dataset's own MCP endpoint). It also checks that the main MCP endpoint
cannot read the dataset, CORS, a backup and MCP auth. The stack and its data
are removed afterwards (`--keep` leaves them). It publishes ports 80/443, so
run it on a CI runner, a throwaway VM or in Docker-in-Docker:

```bash
docker run -d --privileged --name rawbbit-e2e -v "$PWD/../..:/repo" docker:27-dind
docker exec rawbbit-e2e sh -c 'apk add -q bash curl coreutils sudo jq'
docker exec rawbbit-e2e bash -c 'cd /repo/quickstart/swarm && ./e2e.sh --build'
```

The CI workflow runs it on every push and pull request.

## Notes

- On a host that already runs Docker Compose projects, `docker swarm init`
  leaves them alone. Swarm's overlay networks default to `10.0.0.0/8`; if
  the host's own network is in that range (an OCI VCN is `10.0.0.0/16` by
  default), init with another pool:
  `docker swarm init --advertise-addr <private-ip> --default-addr-pool 10.250.0.0/16`.
- ClickHouse and Postgres are reachable only on the encrypted overlay
  network. No `127.0.0.1` ports are published (Swarm cannot bind to
  loopback). For ClickHouse IDE access, add a Caddy block (see
  [`Caddyfile`](Caddyfile)) or use `docker exec` into the ClickHouse task.
- Vector 0.57+ does not expand `${VAR}` in its configuration, so
  [`vector/vector.yaml`](vector/vector.yaml) is static: its programs read
  `COLLECTOR_API_KEYS_JSON`, `IP_HASH_SALT` and `RAWBBIT_APP_DATASETS` from the
  environment, and the ClickHouse password from a Swarm secret.
- When you change a ClickHouse password in `.env`, `clickhouse-setup` updates
  it on the next deploy while the services restart with the new value; inserts
  can fail for a few seconds in between, and the SDKs resend.
- There is no `depends_on` in Swarm. On a first deploy, Vector, MCP and the
  setup job may retry for a while until ClickHouse is up; that is expected.
- Secrets reach containers as environment variables (except the Vector
  password and the dataset files, which are Swarm secrets).
  `docker service inspect` shows the variables to anyone with manager access.
- An existing install from the NATS/Parquet pipeline (tag
  `go-collector-final`) has a `MergeTree` `analytics.events` with two extra
  columns. `clickhouse-setup` leaves an existing table alone; to switch,
  rename it, deploy, and copy the rows:
  `INSERT INTO analytics.events SELECT * EXCEPT (nats_stream, nats_sequence) REPLACE (ifNull(received_time, event_time) AS received_time) FROM analytics.events_old WHERE event_id IS NOT NULL`.

## Files

| File | Purpose |
|---|---|
| `stack.yml` | the stack |
| `stack.tls-*.yml` | Caddy's ports (and the origin certificate) for each `TLS_MODE` |
| `.env.example` | all settings |
| `datasets.example.json` | dataset template (copy to `datasets.json`) |
| `deploy.sh` | validate, render datasets, name configs by hash, deploy, clean up |
| `bootstrap-data-node.sh` | host directories |
| `e2e.sh` | end-to-end test on a single-node swarm (used by CI) |
| `Caddyfile` | public routes: collector (CORS, size limit), MCP, Metabase |
| `vector/vector.yaml` | event intake: auth, validation, normalisation, routing, ClickHouse sinks |
| `vector/geoip-0.yaml`, `vector/geoip-1.yaml` | GeoIP off / on |
| `clickhouse/schema.sql` | `analytics.events` |
| `clickhouse/setup.sh` | schema, users, grants and datasets (the `clickhouse-setup` job) |
| `clickhouse/backup.sh` | daily backups (the `clickhouse-backup` service) |
| `clickhouse/config.d/`, `clickhouse/users.d/` | memory profiles and the backup path |
| `postgres/01_metabase_database.sh` | Metabase database and user |
