#!/usr/bin/env bash
# End-to-end test of the Swarm stack on a single-node swarm:
#
#   loadgen -> Caddy -> Vector (2 replicas) -> ClickHouse -> MCP run_readonly_sql
#
# rawbbit-loadgen sends synthetic events (plus requests the collector must
# reject, events it must drop and re-sent duplicate batches), then checks
# through the public MCP endpoint that ClickHouse holds every accepted event
# exactly once. A second run sends a dataset app and checks it through the
# dataset's own MCP endpoint. Then CORS, backups and MCP auth are checked.
#
#   ./e2e.sh [--build] [--keep]
#
#   --build  build the MCP server image locally as rawbbit-mcp-server:e2e first
#   --keep   leave the stack running afterwards (default: remove it and its data)
#
# Without --build, set MCP_SERVER_IMAGE. Every secret is random per run. The
# stack publishes ports 80/443 on this host, so run it on a CI runner, a
# disposable VM or in Docker-in-Docker, not next to another web server. Needs
# bash, docker, curl and either Go or Docker to build the load generator; uses
# sudo when not root.
#
# Tunables: E2E_EVENTS (3000), E2E_RATE (events/s, 500), E2E_BATCH (50),
# E2E_DATA_ROOT (/srv/rawbbit-e2e), E2E_STACK (rawbbit-e2e),
# E2E_VERIFY_TIMEOUT (5m), LOADGEN_BIN (prebuilt binary).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
cd "$here"

build=0 keep=0
for arg in "$@"; do
  case "$arg" in
    --build) build=1 ;;
    --keep) keep=1 ;;
    -h|--help) sed -n '2,26p' "$0"; exit 0 ;;
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

# --- image ------------------------------------------------------------------
if ((build)); then
  log "building rawbbit-mcp-server:e2e"
  DOCKER_BUILDKIT=1 docker build -q -t rawbbit-mcp-server:e2e "$repo/mcp-server" >/dev/null
  export MCP_SERVER_IMAGE=rawbbit-mcp-server:e2e
fi
[[ -n "${MCP_SERVER_IMAGE:-}" ]] || die "MCP_SERVER_IMAGE is not set (or pass --build)"

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
ds_key=e2e$(rand) ds_token=e2e$(rand) ds_app=com.example.loadgen.dataset ds_id=e2e
cat > "$work/datasets.json" <<EOF
{"datasets": [{"id": "$ds_id", "app_ids": ["$ds_app"], "main_exposed": false,
  "api_keys": ["$ds_key"], "clickhouse_password": "e2e$(rand)", "mcp_tokens": {"e2e": "$ds_token"}}]}
EOF
export RAWBBIT_ENV_FILE=.env.example RAWBBIT_STACK_NAME=$stack RAWBBIT_DATA_ROOT=$data_root \
  RAWBBIT_DATASETS_FILE=$work/datasets.json GEOIP_HOST_DIR=$data_root-geoip \
  COLLECTOR_PUBLIC_HOSTNAME=http://collector.e2e.test MCP_PUBLIC_HOSTNAME=http://mcp.e2e.test \
  METABASE_PUBLIC_HOSTNAME=http://metabase.e2e.test \
  COLLECTOR_API_KEYS_JSON="{\"$api_key\":\"com.example.*\"}" \
  MCP_API_KEYS_JSON="{\"e2e\":\"$mcp_token\"}" \
  CORS_ALLOW_ORIGINS=https://game.e2e.test \
  VECTOR_REPLICAS=2 MCP_SERVER_REPLICAS=1 METABASE_REPLICAS=${E2E_METABASE_REPLICAS:-0}

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

log "waiting for clickhouse-setup to finish"
for i in $(seq 120); do
  states=$(docker service ps --format '{{.CurrentState}}' "${stack}_clickhouse-setup" 2>/dev/null || true)
  grep -q '^Complete' <<< "$states" && break
  ((i < 120)) || die "clickhouse-setup did not complete: $states"
  sleep 5
done
docker service logs --raw "${stack}_clickhouse-setup" 2>&1 | grep '^setup:' || true

collector=(-collector http://127.0.0.1 -collector-host collector.e2e.test)
mcp=(-mcp-url http://127.0.0.1/mcp -mcp-host mcp.e2e.test)
verify=(-verify-timeout "${E2E_VERIFY_TIMEOUT:-5m}" -verify-interval 5s)

# --- main app: load, rejections, drops, duplicates ---------------------------
"$loadgen" run "${collector[@]}" -api-key "$api_key" -app-id "$app_id" \
  -events "${E2E_EVENTS:-3000}" -rate "${E2E_RATE:-500}" -batch "${E2E_BATCH:-50}" \
  -concurrency 8 -players 100 -duplicates 0.1 -max-request-bytes 1048576 \
  "${mcp[@]}" -mcp-token "$mcp_token" "${verify[@]}" -manifest "$work/main.json"

# --- dataset app: its own database, read through its own MCP endpoint --------
"$loadgen" run "${collector[@]}" -api-key "$ds_key" -app-id "$ds_app" \
  -events 500 -rate 0 -batch 50 -concurrency 4 -duplicates 0.1 -negative=false \
  -mcp-url "http://127.0.0.1/datasets/$ds_id/mcp" -mcp-host mcp.e2e.test -mcp-token "$ds_token" \
  -table "dataset_$ds_id.events" "${verify[@]}" -manifest "$work/dataset.json"

# The main MCP user must not read a dataset that is not main_exposed.
if "$loadgen" verify -manifest "$work/dataset.json" "${mcp[@]}" -mcp-token "$mcp_token" \
     -table "dataset_$ds_id.events" -verify-timeout 1s -verify-interval 1s >/dev/null 2>&1; then
  die "the main MCP endpoint can read dataset_$ds_id"
fi
log "main MCP endpoint cannot read dataset_$ds_id"

# --- move a live app into a new dataset ---------------------------------------
# deploy.sh creates the dataset before Vector routes to it, so an app with
# traffic moves without a failed request or a lost event.
mover_app=com.example.mover mover_total=12000
"$loadgen" send "${collector[@]}" -api-key "$api_key" -app-id "$mover_app" \
  -events "$mover_total" -rate 50 -batch 20 -concurrency 4 -duplicates 0 -negative=false \
  -manifest "$work/mover.json" > "$work/mover.log" 2>&1 &
mover_pid=$!
sleep 10
jq --arg app "$mover_app" --arg pw "e2e$(rand)" --arg tok "e2e$(rand)" \
  '.datasets += [{"id": "mover", "app_ids": [$app], "main_exposed": false,
    "clickhouse_password": $pw, "mcp_tokens": {"e2e": $tok}}]' \
  "$work/datasets.json" > "$work/datasets.next" && mv "$work/datasets.next" "$work/datasets.json"
./deploy.sh | tee "$work/redeploy.log"
grep -q 'switching Vector to the new routes' "$work/redeploy.log" || die "deploy.sh did not create the dataset first"
wait "$mover_pid" || { cat "$work/mover.log" >&2; die "events failed while the app moved to a dataset"; }
ch_task=$(docker ps -q -f "name=${stack}_clickhouse.1" | head -1)
ch() { docker exec "$ch_task" clickhouse-client -u "${CLICKHOUSE_ADMIN_USER:-admin}" --password "$CLICKHOUSE_ADMIN_PASSWORD" -q "$1"; }
for _ in $(seq 30); do
  counts=$(ch "SELECT
    (SELECT count() FROM analytics.events FINAL WHERE app_id = '$mover_app'),
    (SELECT count() FROM dataset_mover.events FINAL WHERE app_id = '$mover_app')")
  read -r in_main in_dataset <<< "$counts"
  ((in_main + in_dataset == mover_total)) && break
  sleep 2
done
((in_main + in_dataset == mover_total)) || die "moved app: $in_main + $in_dataset rows, want $mover_total"
((in_main > 0 && in_dataset > 0)) || die "moved app: the move did not happen during the traffic ($in_main / $in_dataset)"
log "app moved to a dataset under traffic: $in_main rows before, $in_dataset after, none lost"

# --- new-game.sh: a game with its own database and its own API key -----------
new_app=com.example.newgame new_total=300
./new-game.sh newgame "$new_app" > "$work/new-game.log" 2>&1 || { cat "$work/new-game.log" >&2; die "new-game.sh failed"; }
new_key=$(jq -r '.datasets[] | select(.id == "newgame") | .api_keys[0]' "$work/datasets.json")
"$loadgen" send "${collector[@]}" -api-key "$new_key" -app-id "$new_app" \
  -events "$new_total" -rate 0 -batch 50 -concurrency 2 -duplicates 0 -negative=false \
  -manifest "$work/newgame.json" > "$work/newgame-send.log" 2>&1 || { cat "$work/newgame-send.log" >&2; die "sending with the new game's key failed"; }
# Its key may not send for another app.
"$loadgen" send "${collector[@]}" -api-key "$new_key" -app-id "$mover_app" \
  -events 10 -rate 0 -batch 10 -concurrency 1 -duplicates 0 -negative=false \
  -manifest "$work/newgame-other.json" > /dev/null 2>&1 || true
for _ in $(seq 30); do
  read -r in_new in_main_new other <<< "$(ch "SELECT
    (SELECT count() FROM dataset_newgame.events FINAL WHERE app_id = '$new_app'),
    (SELECT count() FROM analytics.events FINAL WHERE app_id = '$new_app'),
    (SELECT count() FROM dataset_mover.events FINAL WHERE app_id = '$mover_app') - $in_dataset")"
  ((in_new == new_total)) && break
  sleep 2
done
((in_new == new_total && in_main_new == 0)) || die "new game: $in_new rows in dataset_newgame, $in_main_new in analytics; want $new_total and 0"
((other == 0)) || die "the new game's key sent $other events for $mover_app"
log "new-game.sh: $new_total rows in dataset_newgame, none in analytics, key limited to its app"

# --- CORS -------------------------------------------------------------------
preflight() {
  curl -s -o /dev/null -D - -X OPTIONS -H 'Host: collector.e2e.test' -H "Origin: $1" \
    -H 'Access-Control-Request-Method: POST' -H 'Access-Control-Request-Headers: content-type,x-api-key' \
    http://127.0.0.1/v1/events:batch
}
preflight https://game.e2e.test | grep -qi '^access-control-allow-origin: https://game.e2e.test' \
  || die "CORS preflight from an allowed origin was not allowed"
if preflight https://other.e2e.test | grep -qi '^access-control-allow-origin'; then
  die "CORS preflight from another origin was allowed"
fi
log "CORS allows https://game.e2e.test only"

# --- backups ----------------------------------------------------------------
backup_task=$(docker ps -q -f "name=${stack}_clickhouse-backup" | head -1)
[[ -n "$backup_task" ]] || die "clickhouse-backup is not running"
docker exec "$backup_task" /bin/bash /etc/rawbbit/backup.sh --now
for db in analytics "dataset_$ds_id"; do
  as_root sh -c "ls '$data_root/clickhouse/backups/$db/'*.zip" >/dev/null 2>&1 || die "no backup of $db"
done
log "backups of analytics and dataset_$ds_id written"

# --- MCP auth ---------------------------------------------------------------
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Host: mcp.e2e.test' -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' http://127.0.0.1/mcp \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}')
[[ "$code" == 401 ]] || die "MCP without a token returned $code, want 401"
log "MCP without a token -> 401"

log "PASSED"
