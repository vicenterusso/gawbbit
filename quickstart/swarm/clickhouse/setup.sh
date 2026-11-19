#!/usr/bin/env bash
# clickhouse-setup: runs once per deploy as a Swarm job. Everything here is
# idempotent, so it is safe to run again on every deploy:
#
#   1. wait until ClickHouse answers;
#   2. create analytics.events (schema.sql) if it is missing;
#   3. create or update the service users and their grants:
#        vector    INSERT into analytics.events and every dataset table
#        mcp       SELECT on analytics.*
#        metabase  SELECT on analytics.* and every dataset database
#   4. for every dataset in /run/secrets/datasets_tsv ("<id>\t<password>"):
#      database dataset_<id>, table dataset_<id>.events (same structure as
#      analytics.events) and a read-only MCP user mcp_<id>.
#
# Removing a dataset from datasets.json does not drop its database or user.
set -euo pipefail

: "${CLICKHOUSE_ADMIN_USER:?}" "${CLICKHOUSE_ADMIN_PASSWORD:?}"
: "${CLICKHOUSE_MCP_USER:?}" "${CLICKHOUSE_MCP_PASSWORD:?}"
: "${CLICKHOUSE_METABASE_USER:?}" "${CLICKHOUSE_METABASE_PASSWORD:?}"
vector_password_file=${VECTOR_PASSWORD_FILE:-/run/secrets/clickhouse_vector_password}
datasets_file=${DATASETS_FILE:-/run/secrets/datasets_tsv}
schema_file=${SCHEMA_FILE:-/etc/rawbbit/schema.sql}
host=${CLICKHOUSE_HOST:-clickhouse}
vector_user=rawbbit_vector

ch() {
  clickhouse-client --host "$host" --user "$CLICKHOUSE_ADMIN_USER" \
    --password "$CLICKHOUSE_ADMIN_PASSWORD" --multiquery "$@"
}
ident() {
  [[ "$1" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || { echo "setup: invalid identifier: $1" >&2; exit 1; }
  printf '`%s`' "$1"
}
literal() { printf "'%s'" "$(printf '%s' "$1" | sed -e "s/\\\\/\\\\\\\\/g" -e "s/'/\\\\'/g")"; }
user_sql() {
  local u p
  u=$(ident "$1") p=$(literal "$2")
  printf 'CREATE USER IF NOT EXISTS %s IDENTIFIED WITH sha256_password BY %s;\n' "$u" "$p"
  printf 'ALTER USER %s IDENTIFIED WITH sha256_password BY %s;\n' "$u" "$p"
}

for i in $(seq 120); do
  ch --query "SELECT 1" >/dev/null 2>&1 && break
  ((i < 120)) || { echo "setup: ClickHouse did not answer" >&2; exit 1; }
  echo "setup: waiting for ClickHouse"
  sleep 5
done

ch < "$schema_file"
echo "setup: analytics.events ready"

vector_password=$(cat "$vector_password_file")
mcp=$(ident "$CLICKHOUSE_MCP_USER")
metabase=$(ident "$CLICKHOUSE_METABASE_USER")
vector=$(ident "$vector_user")
{
  user_sql "$vector_user" "$vector_password"
  user_sql "$CLICKHOUSE_MCP_USER" "$CLICKHOUSE_MCP_PASSWORD"
  user_sql "$CLICKHOUSE_METABASE_USER" "$CLICKHOUSE_METABASE_PASSWORD"
  echo "GRANT INSERT ON analytics.events TO $vector;"
  echo "GRANT SELECT, SHOW ON analytics.* TO $mcp;"
  echo "GRANT SELECT, SHOW ON analytics.* TO $metabase;"
} | ch
echo "setup: service users ready"

count=0
if [[ -s "$datasets_file" ]]; then
  while IFS=$'\t' read -r id password; do
    [[ -n "$id" ]] || continue
    [[ "$id" =~ ^[a-z][a-z0-9_]{0,39}$ ]] || { echo "setup: invalid dataset id: $id" >&2; exit 1; }
    db=$(ident "dataset_$id")
    user=$(ident "mcp_$id")
    {
      echo "CREATE DATABASE IF NOT EXISTS $db;"
      echo "CREATE TABLE IF NOT EXISTS $db.events AS analytics.events;"
      user_sql "mcp_$id" "$password"
      echo "ALTER USER $user SETTINGS readonly = 1;"
      echo "GRANT SELECT, SHOW ON $db.* TO $user;"
      echo "GRANT INSERT ON $db.events TO $vector;"
      echo "GRANT SELECT, SHOW ON $db.* TO $metabase;"
    } | ch
    echo "setup: dataset $id ready (dataset_$id.events, user mcp_$id)"
    count=$((count + 1))
  done < "$datasets_file"
fi
echo "setup: done, $count dataset(s)"
