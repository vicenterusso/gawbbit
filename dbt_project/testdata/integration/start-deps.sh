#!/usr/bin/env bash
# Start disposable ClickHouse 24.8 and SeaweedFS containers for
# `go test -tags integration ./internal/runner/`. Never point the tests at
# production. Stop them with: start-deps.sh stop
set -euo pipefail

prefix=${RAWBBIT_DBT_IT_PREFIX:-rawbbit-dbt-it}
ch_port=${RAWBBIT_DBT_IT_CH_PORT:-48123}
s3_port=${RAWBBIT_DBT_IT_S3_PORT:-48333}
here=$(cd "$(dirname "$0")" && pwd)

if [[ ${1:-start} == stop ]]; then
  docker rm -f "$prefix-ch" "$prefix-s3" >/dev/null 2>&1 || true
  docker network rm "$prefix" >/dev/null 2>&1 || true
  exit 0
fi

docker network create "$prefix" >/dev/null
docker run -d --name "$prefix-s3" --network "$prefix" -p "127.0.0.1:$s3_port:8333" \
  chrislusf/seaweedfs:4.39 server -s3 -dir=/data >/dev/null
docker run -d --name "$prefix-ch" --network "$prefix" -p "127.0.0.1:$ch_port:8123" \
  --ulimit nofile=262144:262144 \
  -e CLICKHOUSE_SKIP_USER_SETUP=1 \
  -e CLICKHOUSE_RAW_S3_URL="http://$prefix-s3:8333/raw/raw/" \
  -e CLICKHOUSE_RAW_S3_ACCESS_KEY=any -e CLICKHOUSE_RAW_S3_SECRET_KEY=any \
  -v "$here/../../../quickstart/vm_rawbbit_two/clickhouse/config.d/raw-s3-named-collection.xml:/etc/clickhouse-server/config.d/raw-s3-named-collection.xml:ro" \
  -v "$here/users-access-management.xml:/etc/clickhouse-server/users.d/access-management.xml:ro" \
  clickhouse/clickhouse-server:24.8 >/dev/null

for _ in $(seq 60); do
  if curl -fsS "http://127.0.0.1:$ch_port/ping" >/dev/null 2>&1 \
    && curl -fsS -o /dev/null "http://127.0.0.1:$s3_port/" 2>/dev/null; then
    # system.query_log is created on the first flush; the runner fails closed
    # without it, so create it before the tests start.
    curl -fsS "http://127.0.0.1:$ch_port/" --data-binary "SELECT 1" >/dev/null
    curl -fsS "http://127.0.0.1:$ch_port/" --data-binary "SYSTEM FLUSH LOGS" >/dev/null
    curl -fsS -X PUT "http://127.0.0.1:$s3_port/raw" >/dev/null
    echo "RAWBBIT_DBT_IT_CLICKHOUSE=http://127.0.0.1:$ch_port"
    echo "RAWBBIT_DBT_IT_S3=http://127.0.0.1:$s3_port/raw"
    exit 0
  fi
  sleep 2
done
echo "integration dependencies did not become ready" >&2
exit 1
