#!/usr/bin/env bash
# Create the persistent host directories on the Swarm data node (the manager
# labeled rawbbit.data=true). Run once with sudo on that node.
#
# Collector replicas need GEOIP_HOST_DIR on every node that can run them; run
#   sudo ./bootstrap-data-node.sh --geoip-only
# on the other nodes (or create the directory yourself).
set -euo pipefail

ROOT_DIR="${RAWBBIT_DATA_ROOT:-/srv/rawbbit}"
GEOIP_DIR="${GEOIP_HOST_DIR:-/srv/rawbbit-geoip}"
# The ingestion runner image and SeaweedFS (user "seaweed") run as uid 1000.
DBT_UID="${DBT_RUNNER_UID:-1000}"
DBT_GID="${DBT_RUNNER_GID:-1000}"
SEAWEED_UID="${SEAWEED_UID:-1000}"
SEAWEED_GID="${SEAWEED_GID:-1000}"

if [[ "${EUID}" -ne 0 ]]; then
  echo "This script creates host directories under ${ROOT_DIR}; run it with sudo." >&2
  exit 1
fi

install -d -m 0755 "${GEOIP_DIR}"
echo "created_or_verified ${GEOIP_DIR}"
if [[ "${1:-}" == "--geoip-only" ]]; then
  exit 0
fi

# Containers that start as root and manage their own ownership.
for dir in \
  "${ROOT_DIR}/caddy/data" \
  "${ROOT_DIR}/caddy/config" \
  "${ROOT_DIR}/nats/jetstream" \
  "${ROOT_DIR}/clickhouse/data" \
  "${ROOT_DIR}/clickhouse/logs" \
  "${ROOT_DIR}/postgres"; do
  install -d -m 0755 "${dir}"
  echo "created_or_verified ${dir}"
done

for dir in "${ROOT_DIR}/seaweedfs/master" "${ROOT_DIR}/seaweedfs/volume" "${ROOT_DIR}/seaweedfs/filerldb2"; do
  install -d -m 0755 -o "${SEAWEED_UID}" -g "${SEAWEED_GID}" "${dir}"
  echo "created_or_verified ${dir} owner=${SEAWEED_UID}:${SEAWEED_GID}"
done

for dir in "${ROOT_DIR}/dbt" "${ROOT_DIR}/dbt/logs" "${ROOT_DIR}/dbt/target"; do
  install -d -m 0755 -o "${DBT_UID}" -g "${DBT_GID}" "${dir}"
  echo "created_or_verified ${dir} owner=${DBT_UID}:${DBT_GID}"
done

echo
echo "Next, on a manager: docker node update --label-add rawbbit.data=true $(hostname)"
