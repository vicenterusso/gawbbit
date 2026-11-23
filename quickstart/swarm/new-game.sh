#!/usr/bin/env bash
# Add a game with its own database, then deploy it with ./deploy.sh.
#
#   ./new-game.sh <id> <app_id>              add the game and deploy
#   ./new-game.sh <id> <app_id> --no-deploy  only edit datasets.json
#
# <id> names the database (dataset_<id>) and the MCP endpoint
# (/datasets/<id>/mcp): lowercase letters, digits and _. <app_id> is the
# app_id the game's SDK sends, for example com.studio.game.
#
# Generates the game's collector API key, ClickHouse password and MCP token,
# adds them to datasets.json (created when missing) and prints them. Nothing
# in .env changes. The deploy creates the database before Vector routes the
# game to it, and replaces the services one task at a time, so the other
# games keep sending. Needs bash and docker (jq runs from its image when it
# is not installed).
set -euo pipefail

cd "$(dirname "$0")"

die() { echo "new-game: $*" >&2; exit 1; }
usage() { sed -n '4,5p' "$0" | sed 's/^# *//' >&2; exit 2; }

deploy=1
args=()
for arg in "$@"; do
  case "$arg" in
    --no-deploy) deploy=0 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    -*) usage ;;
    *) args+=("$arg") ;;
  esac
done
((${#args[@]} == 2)) || usage
id=${args[0]} app_id=${args[1]}

[[ "$id" =~ ^[a-z][a-z0-9_]{0,39}$ ]] || die "id must be lowercase letters, digits and _ (at most 40), starting with a letter"
[[ "$app_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "app_id must be letters, digits, ., _ and -, for example com.studio.game"

jq() {
  if command -v jq >/dev/null; then command jq "$@"
  else docker run --rm -i ghcr.io/jqlang/jq:1.7.1 "$@"; fi
}
secret() { od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'; }

env_file="${RAWBBIT_ENV_FILE:-.env}"
# A plain value from .env (quotes stripped, never evaluated).
env_value() { sed -n "s/^$1=//p" "$env_file" 2>/dev/null | tail -1 | sed "s/^[\"']//; s/[\"']\$//"; }
file="${RAWBBIT_DATASETS_FILE:-$(env_value RAWBBIT_DATASETS_FILE)}"
file="${file:-datasets.json}"
umask 077
current='{"datasets": []}'
[[ -f "$file" ]] && current=$(cat "$file")

clash=$(jq -r --arg id "$id" --arg app "$app_id" '
  .datasets[]? | select(.id == $id or any(.app_ids[]?; . == $app))
  | if .id == $id then "dataset \($id) already exists" else "app_id \($app) is already in dataset \(.id)" end' <<< "$current") || die "$file is not valid JSON"
[[ -z "$clash" ]] || die "$clash"

api_key=$(secret 32) clickhouse_password=$(secret 24) mcp_token=$(secret 32)
jq --arg id "$id" --arg app "$app_id" --arg key "$api_key" --arg pw "$clickhouse_password" --arg tok "$mcp_token" '
  .datasets = ((.datasets // []) + [{
    id: $id, app_ids: [$app], api_keys: [$key], main_exposed: false,
    clickhouse_password: $pw, mcp_tokens: {owner: $tok}}])' <<< "$current" > "$file.new"
mv "$file.new" "$file"
echo "new-game: added $id ($app_id) to $file"

if ((deploy)); then
  ./deploy.sh || die "deploy failed; $id stays in $file, fix the problem and run ./deploy.sh again"
fi

collector=$(env_value COLLECTOR_PUBLIC_HOSTNAME) mcp=$(env_value MCP_PUBLIC_HOSTNAME)
cat <<INFO

Game $id is ready$( ((deploy)) || echo " after ./deploy.sh").

  SDK
    endpoint   https://${collector:-<COLLECTOR_PUBLIC_HOSTNAME>}
    app_id     $app_id
    api key    $api_key

  ClickHouse database  dataset_$id
    Metabase: Admin > Databases > Add > ClickHouse, host clickhouse,
    port 8123, user metabase_reader, database dataset_$id.

  MCP
    url        https://${mcp:-<MCP_PUBLIC_HOSTNAME>}/datasets/$id/mcp
    token      $mcp_token  (Authorization: Bearer)

These values are also in $file.
INFO
