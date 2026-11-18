#!/usr/bin/env bash
# Deploy (or update) the Rawbbit Swarm stack from a manager node.
#
#   ./deploy.sh            validate .env and the cluster, then deploy
#   ./deploy.sh --render   print the fully interpolated stack and exit
#
# .env is read with Compose dotenv rules (no shell evaluation), so JSON
# values such as COLLECTOR_API_KEYS_JSON need no extra quoting.
set -euo pipefail

cd "$(dirname "$0")"

render=0
case "${1:-}" in
  "") ;;
  --render) render=1 ;;
  -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
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
aws_replica="${AWS_REPLICA_ENABLED:-0}"

# --- refuse placeholders ----------------------------------------------------
placeholders=()
for key in "${env_keys[@]}"; do
  [[ "$aws_replica" != 1 && "$key" == AWS_REPLICA_* ]] && continue
  [[ "${!key}" == *change_me* ]] && placeholders+=("$key")
done
((${#placeholders[@]} == 0)) || die "replace the change_me values of: ${placeholders[*]}"

# SeaweedFS identities are generated as JSON from these values.
for key in S3_WRITER_ACCESS_KEY S3_WRITER_SECRET_KEY S3_READER_ACCESS_KEY \
           S3_READER_SECRET_KEY S3_ADMIN_ACCESS_KEY S3_ADMIN_SECRET_KEY; do
  [[ -n "${!key:-}" ]] || die "$key is required"
  [[ "${!key}" =~ ^[A-Za-z0-9+/=_-]+$ ]] || die "$key may only contain letters, digits and + / = _ -"
done
[[ "$S3_WRITER_ACCESS_KEY" != "$S3_READER_ACCESS_KEY" && "$S3_WRITER_ACCESS_KEY" != "$S3_ADMIN_ACCESS_KEY" && "$S3_READER_ACCESS_KEY" != "$S3_ADMIN_ACCESS_KEY" ]] \
  || die "the writer, reader and admin S3 access keys must differ"

# --- generated files --------------------------------------------------------
umask 077
mkdir -p .generated
cat > .generated/seaweedfs-s3.json <<EOF
{
  "identities": [
    {
      "name": "rawbbit-writer",
      "credentials": [{"accessKey": "${S3_WRITER_ACCESS_KEY}", "secretKey": "${S3_WRITER_SECRET_KEY}"}],
      "actions": ["Read", "List", "Write"]
    },
    {
      "name": "rawbbit-reader",
      "credentials": [{"accessKey": "${S3_READER_ACCESS_KEY}", "secretKey": "${S3_READER_SECRET_KEY}"}],
      "actions": ["Read", "List"]
    },
    {
      "name": "rawbbit-admin",
      "credentials": [{"accessKey": "${S3_ADMIN_ACCESS_KEY}", "secretKey": "${S3_ADMIN_SECRET_KEY}"}],
      "actions": ["Admin", "Read", "List", "Tagging", "Write"]
    }
  ]
}
EOF

# --- content-addressed config and secret names -----------------------------
# Swarm configs/secrets are immutable; a new name per content lets
# `docker stack deploy` roll the services that use a changed file.
ch="../vm_rawbbit_two/clickhouse"
declare -A objects=(
  [CFG_CADDYFILE]=./Caddyfile
  [CFG_NATS_CONF]=../vm_rawbbit_one/nats-server.conf
  [CFG_SEAWEED_FILER]=../vm_rawbbit_one/seaweedfs/filer.toml
  [CFG_CLICKHOUSE_RESOURCE_PROFILE]="$ch/config.d/${CLICKHOUSE_CONFIG_PROFILE_FILE:-low-memory.xml}"
  [CFG_CLICKHOUSE_NAMED_COLLECTION]="$ch/config.d/raw-s3-named-collection.xml"
  [CFG_CLICKHOUSE_USERS_PROFILE]="$ch/users.d/${CLICKHOUSE_USERS_PROFILE_FILE:-low-memory-users.xml}"
  [CFG_CLICKHOUSE_SCHEMA]="$ch/schema_analytics_events.sql"
  [CFG_CLICKHOUSE_SERVICE_USERS]="$ch/initdb.d/02_service_users.sh"
  [CFG_POSTGRES_METABASE_INIT]=../vm_rawbbit_two/postgres/initdb.d/01_metabase_database.sh
  [SECRET_SEAWEED_S3_JSON]=./.generated/seaweedfs-s3.json
)
declare -a current_names=()
for var in "${!objects[@]}"; do
  file="${objects[$var]}"
  [[ -f "$file" ]] || die "$file not found (from $var)"
  base="${var#CFG_}"; base="${base#SECRET_}"; base="${base,,}"
  name="${stack}_${base}_$(sha256sum "$file" | cut -c1-12)"
  export "$var=$name"
  current_names+=("$name")
done

stack_files=(-c stack.yml)
[[ "$aws_replica" == 1 ]] && stack_files+=(-c stack.aws-replica.yml)

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

echo "deploy: stack=$stack aws_replica=$aws_replica"
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
