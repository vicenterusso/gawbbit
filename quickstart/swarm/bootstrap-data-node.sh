#!/usr/bin/env bash
# Create the persistent host directories on the Swarm data node (the manager
# labeled rawbbit.data=true). Run once with sudo on that node.
#
# Vector replicas need GEOIP_HOST_DIR on every node that can run them; run
#   sudo ./bootstrap-data-node.sh --geoip-only
# on the other nodes (or create the directory yourself).
set -euo pipefail

ROOT_DIR="${RAWBBIT_DATA_ROOT:-/srv/rawbbit}"
GEOIP_DIR="${GEOIP_HOST_DIR:-/srv/rawbbit-geoip}"
# The ClickHouse image runs as uid/gid 101; backup files are written by the
# server and pruned by the backup service as root.
CLICKHOUSE_UID="${CLICKHOUSE_UID:-101}"
CLICKHOUSE_GID="${CLICKHOUSE_GID:-101}"

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
  "${ROOT_DIR}/clickhouse/data" \
  "${ROOT_DIR}/clickhouse/logs" \
  "${ROOT_DIR}/postgres"; do
  install -d -m 0755 "${dir}"
  echo "created_or_verified ${dir}"
done

install -d -m 0750 -o "${CLICKHOUSE_UID}" -g "${CLICKHOUSE_GID}" "${ROOT_DIR}/clickhouse/backups"
echo "created_or_verified ${ROOT_DIR}/clickhouse/backups owner=${CLICKHOUSE_UID}:${CLICKHOUSE_GID}"

echo
echo "Next, on a manager: docker node update --label-add rawbbit.data=true $(hostname)"
