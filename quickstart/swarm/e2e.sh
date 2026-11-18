#!/usr/bin/env bash
# End-to-end test of the Swarm stack on a single-node swarm:
#
#   loadgen -> Caddy -> collector-api (2) -> NATS -> raw-writer (2) -> SeaweedFS
#     -> ingestion runner backfill -> ClickHouse -> MCP run_readonly_sql
#
# rawbbit-loadgen sends synthetic events (plus requests the collector must
# reject and re-sent duplicate batches), then checks through the public MCP
# endpoint that ClickHouse holds every accepted event exactly once.
#
#   ./e2e.sh [--build] [--keep]
#
#   --build  build the four Go images locally as rawbbit-<name>:e2e first
#   --keep   leave the stack running afterwards (default: remove it and its data)
#
# Without --build, set COLLECTOR_API_IMAGE, RAW_WRITER_IMAGE, MCP_SERVER_IMAGE
# and DBT_RUNNER_IMAGE. Every secret is random per run. The stack publishes
# ports 80/443 on this host, so run it on a CI runner, a disposable VM or in
# Docker-in-Docker, not next to another web server. Needs bash, docker, curl
# and either Go or Docker to build the load generator; uses sudo when not
# root.
#
# Tunables: E2E_EVENTS (3000), E2E_RATE (events/s, 500), E2E_BATCH (50),
# E2E_DATA_ROOT (/srv/rawbbit-e2e), E2E_STACK (rawbbit-e2e),
# E2E_VERIFY_TIMEOUT (10m), LOADGEN_BIN (prebuilt binary).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
cd "$here"

build=0 keep=0
for arg in "$@"; do
  case "$arg" in
    --build) build=1 ;;
    --keep) keep=1 ;;
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "usage: $0 [--build] [--keep]" >&2; exit 2 ;;
  esac
done

log() { printf 'e2e: %s\n' "$*"; }
die() { printf 'e2e: %s\n' "$*" >&2; exit 1; }
rand() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
as_root() { if [[ $EUID -eq 0 ]]; then "$@"; else sudo -E "$@"; fi; }

stack=${E2E_STACK:-rawbbit-e2e}
data_root=${E2E_DATA_ROOT:-/srv/rawbbit-e2e}
work=$(mktemp -d)

# --- swarm ------------------------------------------------------------------
if [[ "$(docker info --format '{{.Swarm.LocalNodeState}}')" != active ]]; then
  log "initialising a single-node swarm"
  docker swarm init --advertise-addr 127.0.0.1 >/dev/null
fi
node=$(docker info --format '{{.Swarm.NodeID}}')
docker node update --label-add rawbbit.data=true "$node" >/dev/null

# --- images -----------------------------------------------------------------
if ((build)); then
  for pair in collector-api:backend/collector-api raw-writer:backend/raw-writer \
              mcp-server:mcp-server dbt-runner:dbt_project; do
    name=${pair%%:*} dir=${pair#*:}
    log "building rawbbit-$name:e2e from $dir"
    DOCKER_BUILDKIT=1 docker build -q -t "rawbbit-$name:e2e" "$repo/$dir" >/dev/null
  done
  export COLLECTOR_API_IMAGE=rawbbit-collector-api:e2e RAW_WRITER_IMAGE=rawbbit-raw-writer:e2e \
         MCP_SERVER_IMAGE=rawbbit-mcp-server:e2e DBT_RUNNER_IMAGE=rawbbit-dbt-runner:e2e
fi
for v in COLLECTOR_API_IMAGE RAW_WRITER_IMAGE MCP_SERVER_IMAGE DBT_RUNNER_IMAGE; do
  [[ -n "${!v:-}" ]] || die "$v is not set (or pass --build)"
done

# --- load generator ---------------------------------------------------------
loadgen=${LOADGEN_BIN:-}
if [[ -z "$loadgen" ]]; then
  loadgen="$work/rawbbit-loadgen"
  if command -v go >/dev/null; then
    (cd "$repo/tools/loadgen" && CGO_ENABLED=0 go build -o "$loadgen" ./cmd/rawbbit-loadgen)
  else
    docker run --rm -v "$repo/tools/loadgen:/src:ro" -v "$work:/out" -w /src -e CGO_ENABLED=0 \
      golang:1.24-bookworm go build -buildvcs=false -o /out/rawbbit-loadgen ./cmd/rawbbit-loadgen
  fi
fi

# --- settings: .env.example, every change_me replaced by a random value -------
while IFS='=' read -r key val; do
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ && "$val" == *change_me* && "$key" != *_IMAGE ]] || continue
  export "$key=e2e$(rand)"
done < .env.example

api_key=e2e$(rand) mcp_token=e2e$(rand) app_id=com.example.loadgen
export RAWBBIT_ENV_FILE=.env.example RAWBBIT_STACK_NAME=$stack RAWBBIT_DATA_ROOT=$data_root \
  GEOIP_HOST_DIR=$data_root-geoip \
  COLLECTOR_PUBLIC_HOSTNAME=http://collector.e2e.test MCP_PUBLIC_HOSTNAME=http://mcp.e2e.test \
  METABASE_PUBLIC_HOSTNAME=http://metabase.e2e.test \
  COLLECTOR_API_KEYS_JSON="{\"$api_key\":\"$app_id\"}" MCP_API_KEYS_JSON="{\"e2e\":\"$mcp_token\"}" \
  COLLECTOR_API_REPLICAS=2 RAW_WRITER_REPLICAS=2 RAW_MAX_BUFFERED_EVENTS=4000 \
  MCP_SERVER_REPLICAS=1 METABASE_REPLICAS=${E2E_METABASE_REPLICAS:-0} \
  SEAWEED_VOLUME_SIZE_LIMIT_MB=64 SEAWEED_VOLUME_MAX=30 SEAWEED_VOLUME_MIN_FREE_SPACE_GB=1 \
  RAW_FLUSH_INTERVAL_SECONDS=10 AWS_REPLICA_ENABLED=0

dump() {
  log "--- services"
  docker stack services "$stack" || true
  for s in $(docker stack services "$stack" --format '{{.Name}}' 2>/dev/null); do
    log "--- tasks and logs: $s"
    docker service ps --no-trunc "$s" 2>/dev/null | head -6 || true
    docker service logs --raw --tail 40 "$s" 2>&1 || true
  done
}

cleanup() {
  status=$?
  ((status == 0)) || dump
  if ((keep)); then
    log "stack $stack kept (data in $data_root); remove with: docker stack rm $stack"
  else
    log "removing stack $stack"
    docker stack rm "$stack" >/dev/null 2>&1 || true
    for _ in $(seq 60); do
      [[ -z "$(docker ps -q -f "label=com.docker.stack.namespace=$stack")" ]] && break
      sleep 2
    done
    for _ in $(seq 30); do
      docker network ls -q -f "label=com.docker.stack.namespace=$stack" | grep -q . || break
      sleep 2
    done
    as_root rm -rf "$data_root" "$data_root-geoip"
    rm -rf "$here/.generated"
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

# --- deploy -----------------------------------------------------------------
as_root env RAWBBIT_DATA_ROOT="$data_root" GEOIP_HOST_DIR="$data_root-geoip" \
  "$here/bootstrap-data-node.sh" >/dev/null
log "deploying stack $stack"
timeout 900 ./deploy.sh

# --- load and verify --------------------------------------------------------
runner_exec="docker exec \$(docker ps -q -f name=${stack}_dbt-runner | head -1) /app/bin/dbt-job"
# The runner fences itself (operator review) when its preflight cannot reach
# ClickHouse, so wait until its control checks pass before the first backfill.
log "waiting for the runner to reach ClickHouse"
for i in $(seq 60); do
  sh -c "$runner_exec verify-control" >/dev/null 2>&1 && break
  ((i < 60)) || die "the runner cannot reach ClickHouse"
  sleep 5
done

"$loadgen" run \
  -collector http://127.0.0.1 -collector-host collector.e2e.test \
  -api-key "$api_key" -app-id "$app_id" \
  -events "${E2E_EVENTS:-3000}" -rate "${E2E_RATE:-500}" -batch "${E2E_BATCH:-50}" \
  -concurrency 8 -players 100 -duplicates 0.1 -max-events-per-request 500 \
  -mcp-url http://127.0.0.1/mcp -mcp-host mcp.e2e.test -mcp-token "$mcp_token" \
  -load-cmd "$runner_exec backfill \"\$LOADGEN_WINDOW_START\" \"\$LOADGEN_WINDOW_END\"" \
  -verify-timeout "${E2E_VERIFY_TIMEOUT:-10m}" -verify-interval 20s \
  -manifest "$work/manifest.json"

# A second backfill of the same window must not duplicate rows.
log "re-running the backfill to check idempotency"
window=$(sed -n 's/.*"window_\(start\|end\)": "\(.*\)".*/\2/p' "$work/manifest.json" | tr '\n' ' ')
for attempt in 1 2 3; do
  sh -c "$runner_exec backfill $window" >/dev/null && break
  ((attempt < 3)) || die "second backfill failed"
  sleep 20
done
"$loadgen" verify -manifest "$work/manifest.json" \
  -mcp-url http://127.0.0.1/mcp -mcp-host mcp.e2e.test -mcp-token "$mcp_token" \
  -verify-timeout 1m -verify-interval 10s

# The MCP endpoint must refuse a request without a token.
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: mcp.e2e.test' -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' http://127.0.0.1/mcp \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}')
[[ "$code" == 401 ]] || die "MCP without a token returned $code, want 401"
log "MCP without a token -> 401"

log "PASSED"
