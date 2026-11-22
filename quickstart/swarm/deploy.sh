#!/usr/bin/env bash
# Deploy (or update) the Rawbbit Swarm stack from a manager node.
#
#   ./deploy.sh            validate .env and the cluster, then deploy
#   ./deploy.sh --render   print the fully interpolated stack and exit
#
# .env is read with Compose dotenv rules (no shell evaluation), so JSON
# values such as COLLECTOR_API_KEYS_JSON need no extra quoting. Needs bash and
# docker; datasets.json is read with jq (the jq image is used when jq is not
# installed).
set -euo pipefail

cd "$(dirname "$0")"

render=0
case "${1:-}" in
  "") ;;
  --render) render=1 ;;
  -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
  *) echo "usage: $0 [--render]" >&2; exit 2 ;;
esac

die() { echo "deploy: $*" >&2; exit 1; }

env_file="${RAWBBIT_ENV_FILE:-.env}"
[[ -f "$env_file" ]] || die "$env_file not found; copy .env.example to .env and edit it"

# --- load .env (Compose dotenv semantics, values are never evaluated) -----
declare -a env_keys=()
lineno=0
while IFS= read -r line || [[ -n "$line" ]]; do
  lineno=$((lineno + 1))
  line="${line%$'\r'}"
  [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
  if [[ ! "$line" =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
    die "$env_file:$lineno: expected KEY=VALUE"
  fi
  key="${BASH_REMATCH[2]}"
  val="${BASH_REMATCH[3]}"
  if [[ "$val" =~ ^\"(.*)\"[[:space:]]*(#.*)?$ ]]; then
    val="${BASH_REMATCH[1]}"
  elif [[ "$val" =~ ^\'(.*)\'[[:space:]]*(#.*)?$ ]]; then
    val="${BASH_REMATCH[1]}"
  else
    # Unquoted: " #" starts a comment; trailing blanks are dropped.
    val="${val%%[[:space:]]#*}"
    val="${val%"${val##*[![:space:]]}"}"
  fi
  # Variables already set in the caller's environment win, as with Compose.
  if [[ -z "${!key+x}" ]]; then
    export "$key=$val"
  fi
  env_keys+=("$key")
done < "$env_file"

stack="${RAWBBIT_STACK_NAME:-rawbbit}"
[[ "$stack" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "RAWBBIT_STACK_NAME must be lowercase letters, digits, _ or -"

# --- refuse placeholders ----------------------------------------------------
placeholders=()
for key in "${env_keys[@]}"; do
  [[ "${!key}" == *change_me* ]] && placeholders+=("$key")
done
((${#placeholders[@]} == 0)) || die "replace the change_me values of: ${placeholders[*]}"
[[ -n "${CLICKHOUSE_VECTOR_PASSWORD:-}" ]] || die "CLICKHOUSE_VECTOR_PASSWORD is required"
[[ "${GEOIP_ENABLED:-0}" =~ ^[01]$ ]] || die "GEOIP_ENABLED must be 0 or 1"
[[ "${MAX_REQUEST_BYTES:-1048576}" =~ ^[1-9][0-9]*$ ]] || die "MAX_REQUEST_BYTES must be a number of bytes"
export HTTPS_PORT="${HTTPS_PORT:-443}"
if [[ ! "$HTTPS_PORT" =~ ^[1-9][0-9]{0,4}$ ]] || ((HTTPS_PORT > 65535)); then die "HTTPS_PORT must be a TCP port"; fi
export HTTP_PORT="${HTTP_PORT:-80}"
if [[ ! "$HTTP_PORT" =~ ^[1-9][0-9]{0,4}$ ]] || ((HTTP_PORT > 65535)); then die "HTTP_PORT must be a TCP port"; fi

# --- TLS: Caddy's own certificates, or the Cloudflare origin certificate ----
# Cloudflare's published ranges (https://www.cloudflare.com/ips/). Only they
# may set the client address, and in cloudflare mode only they may connect.
cloudflare_ranges="173.245.48.0/20 103.21.244.0/22 103.22.200.0/22 103.31.4.0/22
141.101.64.0/18 108.162.192.0/18 190.93.240.0/20 188.114.96.0/20 197.234.240.0/22
198.41.128.0/17 162.158.0.0/15 104.16.0.0/13 104.24.0.0/14 172.64.0.0/13
131.0.72.0/22 2400:cb00::/32 2606:4700::/32 2803:f800::/32 2405:b500::/32
2405:8100::/32 2a06:98c0::/29 2c0f:f248::/32"
export TLS_MODE="${TLS_MODE:-auto}"
case "$TLS_MODE" in
  auto)
    CADDY_TRUSTED_PROXIES="${CADDY_TRUSTED_PROXIES:-127.0.0.1/32}"
    ;;
  cloudflare-flexible)
    CADDY_TRUSTED_PROXIES="${CADDY_TRUSTED_PROXIES:-$cloudflare_ranges}"
    export CADDY_SITE_SCHEME="http://"
    ;;
  cloudflare)
    CADDY_TRUSTED_PROXIES="${CADDY_TRUSTED_PROXIES:-$cloudflare_ranges}"
    export ORIGIN_CERT_FILE="${ORIGIN_CERT_FILE:-./cloudflare/origin.pem}"
    export ORIGIN_KEY_FILE="${ORIGIN_KEY_FILE:-./cloudflare/origin.key}"
    [[ -s "$ORIGIN_CERT_FILE" ]] || die "TLS_MODE=cloudflare needs the Cloudflare origin certificate at $ORIGIN_CERT_FILE (ORIGIN_CERT_FILE)"
    [[ -s "$ORIGIN_KEY_FILE" ]] || die "TLS_MODE=cloudflare needs the origin certificate's private key at $ORIGIN_KEY_FILE (ORIGIN_KEY_FILE)"
    grep -q 'BEGIN CERTIFICATE' "$ORIGIN_CERT_FILE" || die "$ORIGIN_CERT_FILE is not a PEM certificate"
    grep -q 'PRIVATE KEY' "$ORIGIN_KEY_FILE" || die "$ORIGIN_KEY_FILE is not a PEM private key"
    ;;
  *) die "TLS_MODE must be auto, cloudflare or cloudflare-flexible" ;;
esac
CADDY_TRUSTED_PROXIES="$(tr -s '[:space:]' ' ' <<< "$CADDY_TRUSTED_PROXIES" | sed 's/^ //; s/ $//')"
export CADDY_TRUSTED_PROXIES

# --- CORS: comma-separated origins -> one anchored regex for Caddy ----------
origins="${CORS_ALLOW_ORIGINS:-}"
if [[ "$origins" == "*" ]]; then
  CORS_ORIGIN_REGEX='^.+$'
elif [[ -z "${origins//[[:space:],]/}" ]]; then
  CORS_ORIGIN_REGEX='^$'
else
  alternatives=()
  IFS=',' read -ra list <<< "$origins"
  for origin in "${list[@]}"; do
    origin="${origin//[[:space:]]/}"
    [[ -n "$origin" ]] || continue
    [[ "$origin" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || die "CORS_ALLOW_ORIGINS: $origin is not an origin like https://game.example.com"
    alternatives+=("$(printf '%s' "$origin" | sed 's/[.]/\\./g')")
  done
  CORS_ORIGIN_REGEX="^($(IFS='|'; echo "${alternatives[*]}"))\$"
fi
export CORS_ORIGIN_REGEX

# --- generated secrets: Vector password and the datasets ---------------------
umask 077
mkdir -p .generated
printf '%s' "$CLICKHOUSE_VECTOR_PASSWORD" > .generated/clickhouse_vector_password

datasets_file="${RAWBBIT_DATASETS_FILE:-datasets.json}"
if [[ -f "$datasets_file" ]]; then
  jq() {
    if command -v jq >/dev/null; then command jq "$@"
    else docker run --rm -i ghcr.io/jqlang/jq:1.7.1 "$@"; fi
  }
  datasets_json=$(cat "$datasets_file")
  problems=$(jq -r '
    def bad($m): "  " + $m;
    if (.datasets | type) != "array" then bad("\"datasets\" must be a list") else
      (.datasets | to_entries[] | .key as $i | .value |
        (if (.id | type) != "string" or (.id | test("^[a-z][a-z0-9_]{0,39}$") | not)
           then bad("datasets[\($i)].id must match ^[a-z][a-z0-9_]{0,39}$") else empty end),
        (if (.app_ids | type) != "array" or (.app_ids | length) == 0 or any(.app_ids[]; type != "string" or . == "")
           then bad("datasets[\($i)].app_ids must be a non-empty list of app_ids") else empty end),
        (if (.clickhouse_password | type) != "string" or (.clickhouse_password | length) < 12
           then bad("datasets[\($i)].clickhouse_password must be a string of 12+ characters") else empty end),
        (if (.clickhouse_password // "" | tostring | contains("change_me"))
           then bad("datasets[\($i)].clickhouse_password is still change_me") else empty end),
        (if (.mcp_tokens | type) != "object" or (.mcp_tokens | length) == 0 or any(.mcp_tokens[]; type != "string" or length < 16)
           then bad("datasets[\($i)].mcp_tokens must map labels to tokens of 16+ characters") else empty end),
        (if any(.mcp_tokens[]?; tostring | contains("change_me"))
           then bad("datasets[\($i)].mcp_tokens still has change_me values") else empty end),
        (if has("main_exposed") and (.main_exposed | type) != "boolean"
           then bad("datasets[\($i)].main_exposed must be true or false") else empty end)
      ),
      ([.datasets[].id] | group_by(.) | map(select(length > 1)[0]) | .[] | bad("dataset id \(.) is used twice")),
      ([.datasets[].app_ids[]?] | group_by(.) | map(select(length > 1)[0]) | .[] | bad("app_id \(.) is in more than one dataset"))
    end' <<< "$datasets_json") || die "$datasets_file is not valid JSON"
  [[ -z "$problems" ]] || die "$datasets_file:"$'\n'"$problems"

  RAWBBIT_APP_DATASETS=$(jq -c '[.datasets[] | .id as $id | .app_ids[] | {key: ., value: $id}] | from_entries' <<< "$datasets_json")
  jq -r '.datasets[] | [.id, .clickhouse_password] | @tsv' <<< "$datasets_json" > .generated/datasets.tsv
  jq '{version: 1, datasets: [.datasets[] | {
        id, endpoint_path: "/datasets/\(.id)/mcp", database: "dataset_\(.id)", table: "events",
        main_exposed: (.main_exposed // false), clickhouse_user: "mcp_\(.id)",
        clickhouse_password, bearer_tokens: .mcp_tokens}]}' <<< "$datasets_json" > .generated/mcp-datasets.json
  echo "deploy: datasets: $(jq -r '[.datasets[].id] | join(", ")' <<< "$datasets_json")" >&2
else
  RAWBBIT_APP_DATASETS='{}'
  # Swarm rejects an empty secret; setup.sh skips comment lines.
  echo "# no datasets" > .generated/datasets.tsv
  printf '{"version": 1, "datasets": []}\n' > .generated/mcp-datasets.json
fi
export RAWBBIT_APP_DATASETS

# --- content-addressed config and secret names -----------------------------
# Swarm configs/secrets are immutable; a new name per content lets
# `docker stack deploy` roll the services that use a changed file.
declare -A objects=(
  [CFG_CADDYFILE]=./Caddyfile
  [CFG_VECTOR_CONFIG]=./vector/vector.yaml
  [CFG_VECTOR_GEOIP]="./vector/geoip-${GEOIP_ENABLED:-0}.yaml"
  [CFG_CLICKHOUSE_RESOURCE_PROFILE]="./clickhouse/config.d/${CLICKHOUSE_CONFIG_PROFILE_FILE:-low-memory.xml}"
  [CFG_CLICKHOUSE_BACKUPS]=./clickhouse/config.d/backups.xml
  [CFG_CLICKHOUSE_USERS_PROFILE]="./clickhouse/users.d/${CLICKHOUSE_USERS_PROFILE_FILE:-low-memory-users.xml}"
  [CFG_CLICKHOUSE_SCHEMA]=./clickhouse/schema.sql
  [CFG_CLICKHOUSE_SETUP]=./clickhouse/setup.sh
  [CFG_CLICKHOUSE_BACKUP]=./clickhouse/backup.sh
  [CFG_POSTGRES_METABASE_INIT]=./postgres/01_metabase_database.sh
  [SECRET_CLICKHOUSE_VECTOR_PASSWORD]=./.generated/clickhouse_vector_password
  [SECRET_DATASETS_TSV]=./.generated/datasets.tsv
  [SECRET_MCP_DATASETS_JSON]=./.generated/mcp-datasets.json
)
if [[ "$TLS_MODE" == cloudflare ]]; then
  objects[SECRET_ORIGIN_CERT]="$ORIGIN_CERT_FILE"
  objects[SECRET_ORIGIN_KEY]="$ORIGIN_KEY_FILE"
fi
declare -a current_names=()
for var in "${!objects[@]}"; do
  file="${objects[$var]}"
  [[ -f "$file" ]] || die "$file not found (from $var)"
  base="${var#CFG_}"; base="${base#SECRET_}"; base="${base,,}"
  name="${stack}_${base}_$(sha256sum "$file" | cut -c1-12)"
  export "$var=$name"
  current_names+=("$name")
done

stack_files=(-c stack.yml -c "stack.tls-$TLS_MODE.yml")

if ((render)); then
  exec docker stack config "${stack_files[@]}"
fi

# --- cluster checks ---------------------------------------------------------
[[ "$(docker info --format '{{.Swarm.LocalNodeState}}')" == active ]] || die "this node is not part of a swarm (docker swarm init)"
[[ "$(docker info --format '{{.Swarm.ControlAvailable}}')" == true ]] || die "run this on a swarm manager"

data_nodes=()
while IFS= read -r node; do
  [[ -n "$node" ]] || continue
  data_nodes+=("$node")
done < <(docker node ls -q | xargs -r docker node inspect \
  --format '{{if and (eq .Spec.Role "manager") (eq (index .Spec.Labels "rawbbit.data") "true")}}{{.Description.Hostname}}{{end}}')
case ${#data_nodes[@]} in
  1) echo "deploy: data node: ${data_nodes[0]}" ;;
  0) die "no manager has the label rawbbit.data=true; run: docker node update --label-add rawbbit.data=true <master-node>" ;;
  *) die "more than one manager has rawbbit.data=true (${data_nodes[*]}); stateful services must have exactly one home" ;;
esac

echo "deploy: stack=$stack"
docker stack deploy --detach=false --prune --with-registry-auth "${stack_files[@]}" "$stack"

# --- drop configs/secrets from earlier deploys that nothing uses anymore -----
is_current() { local n; for n in "${current_names[@]}"; do [[ "$n" == "$1" ]] && return 0; done; return 1; }
for kind in config secret; do
  docker "$kind" ls --filter "label=com.docker.stack.namespace=$stack" --format '{{.Name}}' |
    while IFS= read -r name; do
      is_current "$name" && continue
      docker "$kind" rm "$name" >/dev/null 2>&1 && echo "deploy: removed unused $kind $name" || true
    done
done
