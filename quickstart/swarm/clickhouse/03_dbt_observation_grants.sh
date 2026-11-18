#!/usr/bin/env bash
# First-boot grants that let the ingestion runner observe its own ClickHouse
# work. The runner's safety checks read these system tables before every
# write and fence themselves (operator review, no loads) when they cannot.
#
# The two-VM guides leave these grants to an operator
# (../vm_rawbbit_two/README.md#dbt-observation-access); this stack applies
# them on first boot. Like every initdb script, it runs only when the
# ClickHouse data directory is empty. For an existing install, run the same
# GRANT statements once as the admin user.
set -euo pipefail

: "${CLICKHOUSE_USER:?CLICKHOUSE_USER is required}"
: "${CLICKHOUSE_PASSWORD:?CLICKHOUSE_PASSWORD is required}"
: "${CLICKHOUSE_DBT_USER:?CLICKHOUSE_DBT_USER is required}"

if [[ ! "$CLICKHOUSE_DBT_USER" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
  echo "Invalid CLICKHOUSE_DBT_USER: ${CLICKHOUSE_DBT_USER}" >&2
  exit 1
fi

user="\`${CLICKHOUSE_DBT_USER}\`"
clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" --multiquery --query "
  GRANT SELECT ON system.processes TO ${user};
  GRANT SELECT ON system.mutations TO ${user};
  GRANT SELECT ON system.query_log TO ${user};
  GRANT SELECT ON system.tables TO ${user};
  GRANT SELECT ON system.columns TO ${user};
"
echo "granted runner observation access to ${CLICKHOUSE_DBT_USER}"
