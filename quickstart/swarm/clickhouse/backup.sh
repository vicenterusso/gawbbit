#!/usr/bin/env bash
# clickhouse-backup: a long-running service that backs up every Rawbbit
# database (analytics and dataset_*) once a day with ClickHouse's native
# BACKUP command.
#
#   local copy   /backups/<database>/<UTC timestamp>.zip on the data node
#                (${RAWBBIT_DATA_ROOT}/clickhouse/backups); the newest
#                BACKUP_KEEP_LOCAL copies are kept
#   S3 copy      when BACKUP_S3_URL is set, the same backup is also written
#                to <BACKUP_S3_URL>/<database>/<timestamp>.zip; expire old
#                copies with a bucket lifecycle rule
#
# BACKUP_AT is the daily UTC time (HH:MM). Run one backup now with:
#   docker exec $(docker ps -q -f name=rawbbit_clickhouse-backup) /etc/rawbbit/backup.sh --now
# Restore: see README.md, "Backups".
set -euo pipefail

: "${CLICKHOUSE_ADMIN_USER:?}" "${CLICKHOUSE_ADMIN_PASSWORD:?}"
host=${CLICKHOUSE_HOST:-clickhouse}
at=${BACKUP_AT:-03:30}
keep=${BACKUP_KEEP_LOCAL:-7}
s3_url=${BACKUP_S3_URL:-}
s3_key=${BACKUP_S3_ACCESS_KEY:-}
s3_secret=${BACKUP_S3_SECRET_KEY:-}

[[ "$at" =~ ^([01][0-9]|2[0-3]):[0-5][0-9]$ ]] || { echo "backup: BACKUP_AT must be HH:MM" >&2; exit 1; }
[[ "$keep" =~ ^[1-9][0-9]*$ ]] || { echo "backup: BACKUP_KEEP_LOCAL must be a positive integer" >&2; exit 1; }

ch() {
  clickhouse-client --host "$host" --user "$CLICKHOUSE_ADMIN_USER" \
    --password "$CLICKHOUSE_ADMIN_PASSWORD" "$@"
}
literal() { printf "'%s'" "$(printf '%s' "$1" | sed -e "s/\\\\/\\\\\\\\/g" -e "s/'/\\\\'/g")"; }

run_backup() {
  local stamp db status=0
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  for db in $(ch --query "SELECT name FROM system.databases WHERE name = 'analytics' OR name LIKE 'dataset\\_%' ORDER BY name"); do
    if ch --query "BACKUP DATABASE \`$db\` TO File('$db/$stamp.zip')" >/dev/null; then
      echo "backup: $db -> local $db/$stamp.zip"
    else
      echo "backup: $db local backup FAILED" >&2
      status=1
    fi
    if [[ -n "$s3_url" ]]; then
      if ch --query "BACKUP DATABASE \`$db\` TO S3($(literal "${s3_url%/}/$db/$stamp.zip"), $(literal "$s3_key"), $(literal "$s3_secret"))" >/dev/null; then
        echo "backup: $db -> ${s3_url%/}/$db/$stamp.zip"
      else
        echo "backup: $db S3 backup FAILED" >&2
        status=1
      fi
    fi
    # Keep the newest $keep local copies of this database.
    ls -1 "/backups/$db"/*.zip 2>/dev/null | sort -r | tail -n +"$((keep + 1))" | xargs -r rm -f
  done
  return "$status"
}

if [[ "${1:-}" == "--now" ]]; then
  run_backup
  exit
fi

echo "backup: daily at $at UTC, keeping $keep local copies${s3_url:+, also to $s3_url}"
while true; do
  now=$(date -u +%s)
  next=$(date -u -d "today $at" +%s)
  ((next > now)) || next=$(date -u -d "tomorrow $at" +%s)
  sleep $((next - now))
  run_backup || echo "backup: finished with errors" >&2
done
