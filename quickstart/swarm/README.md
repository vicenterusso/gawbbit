# Rawbbit on Docker Swarm

One stack for the whole pipeline, on one node or many:

```text
Producer -> Caddy -> collector-api (N replicas) -> NATS JetStream -> raw-writer
  -> SeaweedFS (local S3) -> ingestion runner -> ClickHouse -> MCP / Metabase
                         \-> optional rclone copy to AWS S3
```

## Placement

| Service | Replicas | Where |
|---|---|---|
| `caddy` | 1 | data node (TLS certificates on disk, host-mode ports 80/443) |
| `nats` | 1 | data node (JetStream store) |
| `seaweed-master`, `seaweed-volume`, `seaweed-filer`, `seaweed-s3` | 1 each | data node |
| `seaweed-bucket-init` | one-shot job | data node |
| `clickhouse` | 1 | data node |
| `postgres` | 1 | data node (Metabase state) |
| `dbt-runner` | 1 | data node (lock and state files; two runners must never overlap) |
| `collector-api` | `COLLECTOR_API_REPLICAS` (2) | any node |
| `raw-writer` | `RAW_WRITER_REPLICAS` (1) | any node |
| `mcp-server` | `MCP_SERVER_REPLICAS` (1) | any node |
| `metabase` | `METABASE_REPLICAS` (1) | any node |
| `aws-replica` (optional) | 1 | any node |

The **data node** is the manager with the label `rawbbit.data=true`. Every
stateful service uses bind mounts under `RAWBBIT_DATA_ROOT` (`/srv/rawbbit`)
on that node, so the label must be on exactly one node; `deploy.sh` refuses to
deploy otherwise. If the data node is down, ingestion stops: the collectors
return 5xx because NATS is unreachable, and the SDKs keep events in their local
queues and retry. This stack scales the stateless tier; it does not make the
data tier highly available.

Caddy publishes 80/443 in host mode on the data node, so point the public DNS
names there. Host mode preserves the client address (the routing mesh would
replace it), and the collector hashes that address into `ingest.ip_hash`.
Caddy forwards to the collector service VIP, which load-balances across all
replicas on all nodes.

## Requirements

- Docker Engine 24+ on every node (`replicated-job`, `ulimits` and
  `max_replicas_per_node` in stacks). Tested with 29.x.
- Between nodes: TCP 2377, TCP/UDP 7946, UDP 4789 and **ESP (IP protocol
  50)**, because the overlay networks are encrypted. On Oracle Cloud, add
  these to the VCN security list or NSG.
- Multi-arch (`linux/amd64`, `linux/arm64`) Go images for collector-api,
  raw-writer, mcp-server and the ingestion runner in a registry every node can
  pull from. Swarm cannot build images. The older
  `ghcr.io/mirlan-irokez/rawbbit-*` tags are the Python builds and
  `linux/amd64` only. Build, for example:

  ```bash
  docker buildx build --platform linux/amd64,linux/arm64 \
    -t registry.example.com/rawbbit-collector-api:1.0.0 --push backend/collector-api
  docker buildx build --platform linux/amd64,linux/arm64 \
    -t registry.example.com/rawbbit-raw-writer:1.0.0 --push backend/raw-writer
  docker buildx build --platform linux/amd64,linux/arm64 \
    -t registry.example.com/rawbbit-mcp-server:1.0.0 --push mcp-server
  docker buildx build --platform linux/amd64,linux/arm64 \
    -t registry.example.com/rawbbit-dbt-runner:1.0.0 --push dbt_project
  ```

- `bash` 4+ and `sha256sum` on the manager you deploy from.

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

   `deploy.sh` refuses to run while any `change_me` value is left. The
   SeaweedFS identities file is generated from the three `S3_*` credential
   pairs (writer, reader, admin), so there is no separate `s3.json` to edit.

4. Deploy (and later, update) from the master:

   ```bash
   ./deploy.sh
   ```

   It reads `.env` with Compose dotenv rules, checks the cluster, names every
   config and secret by a hash of its content (Swarm configs are immutable,
   so changing a file rolls only the services that use it), runs
   `docker stack deploy --prune --with-registry-auth` and removes unused older
   configs and secrets. `./deploy.sh --render` prints the interpolated stack
   without deploying.

5. Verify:

   ```bash
   docker stack services rawbbit
   curl -i https://collector.example.com/healthz
   ```

   Send a test event as in the
   [VM-one verification](../vm_rawbbit_one/README.md#verification) (use your
   `COLLECTOR_API_KEYS_JSON` key and `app_id`). Parquet lands after
   `RAW_FLUSH_INTERVAL_SECONDS`; check it on the data node with:

   ```bash
   c=$(docker ps -q -f name=rawbbit_seaweed-master)
   echo "fs.ls -l /buckets/rawbbit-raw/raw" |
     docker exec -i "$c" weed shell -master=seaweed-master:9333 -filer=seaweed-filer:8888
   ```

   The runner loads it at minute 12 of each hour. To load now:

   ```bash
   docker exec "$(docker ps -q -f name=rawbbit_dbt-runner)" /app/bin/dbt-job hourly
   ```

## Scaling

```bash
docker service scale rawbbit_collector-api=4
```

or set `COLLECTOR_API_REPLICAS` in `.env` and run `./deploy.sh` again (a
plain `docker service scale` is reverted by the next deploy).

- **collector-api**: stateless; scale freely. The same `GEOIP_HOST_DIR`
  (with the `.mmdb` when `GEOIP_ENABLED=1`) must exist on every node that can
  run it.
- **raw-writer**: replicas share one durable JetStream pull consumer, and each
  writes uniquely named Parquet files. Each replica buffers up to
  `RAW_MAX_BUFFERED_EVENTS` (default 80 % of `NATS_MAX_ACK_PENDING`), so set
  it to about `0.8 * NATS_MAX_ACK_PENDING / replicas` when scaling.
- **mcp-server**: stateless; scale freely.
- **metabase**: keep 1 unless you have tested Metabase's multi-instance mode.

## Optional: off-site copy to AWS S3

With one data node, the raw Parquet and ClickHouse share a disk. Set
`AWS_REPLICA_ENABLED=1` and the `AWS_REPLICA_*` values, then run
`./deploy.sh`: it adds [`stack.aws-replica.yml`](stack.aws-replica.yml), an
rclone loop that copies new raw objects from SeaweedFS to an AWS bucket every
`AWS_REPLICA_INTERVAL_SECONDS`.

- It uses `rclone copy --ignore-existing`, never `sync`: nothing is deleted in
  AWS, and existing objects are not rewritten (raw-writer objects are
  immutable). The IAM user needs `s3:PutObject` and `s3:ListBucket` only.
- Each pass skips objects older than `AWS_REPLICA_MAX_AGE` (72h). After a longer
  outage, raise it once to cover the gap.
- ClickHouse keeps reading from the local SeaweedFS, so ingestion causes no AWS
  egress. AWS charges storage plus PUT/LIST requests.
- AWS bucket names cannot contain underscores.
- Another S3-compatible destination works too: set `AWS_REPLICA_ENDPOINT` and
  an rclone provider name in `AWS_REPLICA_PROVIDER` (`Other`, `Cloudflare`,
  `Wasabi`, `Minio`, ...).
- To restore, copy the objects back into SeaweedFS (or point
  `CLICKHOUSE_RAW_S3_URL` at AWS) and run a runner backfill.

Setting `AWS_REPLICA_ENABLED=0` and deploying again removes the service
(`--prune`).

## Differences from the two-VM guides

- ClickHouse, SeaweedFS S3, NATS and Postgres are reachable only on the
  encrypted overlay network. No `127.0.0.1` ports are published (Swarm cannot
  bind to loopback). For ClickHouse IDE access, add a Caddy block (see
  [`Caddyfile`](Caddyfile)) or use `docker exec` into the ClickHouse task.
- ClickHouse reads raw Parquet from `http://seaweed-s3:8333/<bucket>/<prefix>/`
  inside the cluster; the S3 endpoint is not published.
- `RAWBBIT_RAW_LOAD_MODE` defaults to `dbt`: the runner owns raw ingestion and
  the host cron shell loader is not used.
- Optional MCP datasets (`datasets-runtime.json`) and the
  `rawbbit-provision-datasets` / `rawbbit-deploy-dbt-runner` host helpers are
  not wired into this stack; they assume the two-VM Compose layout. For a
  runner upgrade that changes routing, drain it first with
  `docker service scale rawbbit_dbt-runner=0`, wait until no job is running,
  then deploy.
- SeaweedFS runs as uid 1000 (`seaweed`) and the runner as uid 1000; the
  bootstrap script creates their directories with that owner.
- There is no `depends_on` in Swarm. On a first deploy, raw-writer and the
  collectors may restart a few times until NATS and the stream are up; that
  is expected.
- Secrets reach containers as environment variables, as in the Compose guides.
  `docker service inspect` shows them to anyone with manager access.

## Files

| File | Purpose |
|---|---|
| `stack.yml` | the stack |
| `stack.aws-replica.yml` | optional rclone copy to AWS S3 |
| `.env.example` | all settings |
| `deploy.sh` | validate, name configs by hash, deploy, clean up |
| `bootstrap-data-node.sh` | host directories |
| `Caddyfile` | public routes: collector, MCP, Metabase |

The NATS, SeaweedFS, ClickHouse and Postgres config files are shared with
[`../vm_rawbbit_one`](../vm_rawbbit_one) and
[`../vm_rawbbit_two`](../vm_rawbbit_two).
