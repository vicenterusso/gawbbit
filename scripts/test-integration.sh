#!/usr/bin/env bash
# Run every Go integration suite (build tag "integration") against disposable
# NATS, SeaweedFS and ClickHouse containers, then remove them.
#
#   scripts/test-integration.sh            all suites
#   scripts/test-integration.sh collector  only the named suites
#
# Suites: collector, raw-writer, runner, mcp, provisioner. Never point these
# tests at a real deployment: they drop and recreate databases and users.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
prefix=${IT_PREFIX:-rawbbit-it}
suites=("$@")
((${#suites[@]})) || suites=(collector raw-writer runner mcp provisioner)

nats_port=${IT_NATS_PORT:-24222}
s3_port=${IT_S3_PORT:-28333}
ch_port=${IT_CH_PORT:-38123}

log() { printf 'it: %s\n' "$*"; }

cleanup() {
  status=$?
  if ((status != 0)); then
    for c in nats s3 ch; do
      log "--- logs $prefix-$c"
      docker logs --tail 30 "$prefix-$c" 2>&1 || true
    done
  fi
  docker rm -f "$prefix-nats" "$prefix-s3" "$prefix-ch" >/dev/null 2>&1 || true
  "$repo/dbt_project/testdata/integration/start-deps.sh" stop >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT

wait_for() {
  local what=$1; shift
  for _ in $(seq 90); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 2
  done
  echo "it: $what did not become ready" >&2
  return 1
}

want() { local s; for s in "${suites[@]}"; do [[ "$s" == "$1" ]] && return 0; done; return 1; }

if want collector || want raw-writer; then
  docker run -d --name "$prefix-nats" -p "127.0.0.1:$nats_port:4222" -p "127.0.0.1:$((nats_port + 1)):8222" \
    nats:2.10-alpine -js -m 8222 >/dev/null
  wait_for nats curl -fsS "http://127.0.0.1:$((nats_port + 1))/healthz?js-enabled-only=true"
fi
if want raw-writer; then
  docker run -d --name "$prefix-s3" -p "127.0.0.1:$s3_port:8333" \
    chrislusf/seaweedfs:4.39 server -s3 -dir=/data >/dev/null
  wait_for seaweedfs curl -fsS -o /dev/null "http://127.0.0.1:$s3_port/"
  wait_for bucket curl -fsS -X PUT "http://127.0.0.1:$s3_port/rawwriter-it"
fi
if want mcp || want provisioner; then
  docker run -d --name "$prefix-ch" -p "127.0.0.1:$ch_port:8123" --ulimit nofile=262144:262144 \
    -e CLICKHOUSE_USER=admin -e CLICKHOUSE_PASSWORD=adminpw -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 \
    clickhouse/clickhouse-server:24.8 >/dev/null
  wait_for clickhouse curl -fsS -u admin:adminpw "http://127.0.0.1:$ch_port/" --data-binary "SELECT 1"
  curl -fsS -u admin:adminpw "http://127.0.0.1:$ch_port/" --data-binary "SYSTEM FLUSH LOGS" >/dev/null
fi

run() {
  local dir=$1; shift
  log "go test -tags integration in $dir $*"
  (cd "$repo/$dir" && go test -count=1 -tags integration "$@")
}

for s in "${suites[@]}"; do
  case "$s" in
    collector)
      COLLECTOR_TEST_NATS_URL=nats://127.0.0.1:$nats_port \
        run backend/collector-api ./internal/publisher/ -v -run . ;;
    raw-writer)
      RAWWRITER_IT_NATS_URL=nats://127.0.0.1:$nats_port RAWWRITER_IT_S3_ENDPOINT=http://127.0.0.1:$s3_port \
      RAWWRITER_IT_S3_ACCESS_KEY=any RAWWRITER_IT_S3_SECRET_KEY=any RAWWRITER_IT_S3_BUCKET=rawwriter-it \
        run backend/raw-writer ./internal/natsconsumer/ -v ;;
    runner)
      eval "$("$repo/dbt_project/testdata/integration/start-deps.sh" | sed 's/^/export /')"
      run dbt_project ./internal/runner/ -timeout 30m ;;
    mcp)
      MCP_PARITY_CLICKHOUSE=http://admin:adminpw@127.0.0.1:$ch_port \
        run mcp-server ./internal/server -run TestMCPParity -timeout 20m ;;
    provisioner)
      PROVISION_PARITY_CLICKHOUSE=http://admin:adminpw@127.0.0.1:$ch_port \
        run quickstart/vm_rawbbit_two/clickhouse/provisioner ./cmd/rawbbit-provision-datasets -timeout 20m ;;
    *) echo "it: unknown suite $s" >&2; exit 2 ;;
  esac
done
log "all integration suites passed: ${suites[*]}"
