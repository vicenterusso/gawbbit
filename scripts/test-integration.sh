#!/usr/bin/env bash
# Run the Go integration suites (build tag "integration") against a
# disposable ClickHouse container, then remove it.
#
#   scripts/test-integration.sh        every suite
#   scripts/test-integration.sh mcp    only the named suites
#
# Suites: mcp (MCP server parity cases). Never point these tests at a real
# deployment: they drop and recreate databases and users.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
prefix=${IT_PREFIX:-rawbbit-it}
suites=("$@")
((${#suites[@]})) || suites=(mcp)

ch_port=${IT_CH_PORT:-38123}
ch_image=${IT_CLICKHOUSE_IMAGE:-clickhouse/clickhouse-server:24.8}

log() { printf 'it: %s\n' "$*"; }

cleanup() {
  status=$?
  if ((status != 0)); then
    log "--- logs $prefix-ch"
    docker logs --tail 30 "$prefix-ch" 2>&1 || true
  fi
  docker rm -f "$prefix-ch" >/dev/null 2>&1 || true
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

docker run -d --name "$prefix-ch" -p "127.0.0.1:$ch_port:8123" --ulimit nofile=262144:262144 \
  -e CLICKHOUSE_USER=admin -e CLICKHOUSE_PASSWORD=adminpw -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 \
  "$ch_image" >/dev/null
wait_for clickhouse curl -fsS -u admin:adminpw "http://127.0.0.1:$ch_port/" --data-binary "SELECT 1"
curl -fsS -u admin:adminpw "http://127.0.0.1:$ch_port/" --data-binary "SYSTEM FLUSH LOGS" >/dev/null

run() {
  local dir=$1; shift
  log "go test -tags integration in $dir $*"
  (cd "$repo/$dir" && go test -count=1 -tags integration "$@")
}

for s in "${suites[@]}"; do
  case "$s" in
    mcp)
      MCP_PARITY_CLICKHOUSE=http://admin:adminpw@127.0.0.1:$ch_port \
        run mcp-server ./internal/server -run TestMCPParity -timeout 20m ;;
    *) echo "it: unknown suite $s" >&2; exit 2 ;;
  esac
done
log "all integration suites passed: ${suites[*]}"
