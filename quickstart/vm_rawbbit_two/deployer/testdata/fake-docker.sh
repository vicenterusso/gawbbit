#!/bin/sh
# Fake docker CLI for deployer CLI tests. Responses come from files in
# $FAKE_DOCKER_DIR; every invocation is appended to calls.log.
d="$FAKE_DOCKER_DIR"
printf '%s\n' "$*" >> "$d/calls.log"
reply() { if [ -f "$d/$1" ]; then cat "$d/$1"; fi; if [ -f "$d/$1.exit" ]; then exit "$(cat "$d/$1.exit")"; fi; exit 0; }
case "$*" in
  *" config --format json") reply config.json ;;
  "compose "*" pull "*) reply pull ;;
  "compose "*" up "*) touch "$d/up.called"; reply up ;;
  "compose "*" ps -q dbt-runner") reply compose_ps ;;
  "compose "*" exec -T dbt-runner dbt debug "*) reply dbt_debug ;;
  "compose "*" exec -T dbt-runner dbt parse "*) reply dbt_parse ;;
  "ps "*"service=clickhouse") reply ps_clickhouse ;;
  "ps "*"service=dbt-runner") reply ps_runner ;;
  "ps "*"service=mcp-server") reply ps_mcp ;;
  "image inspect "*) reply image_id ;;
  "run "*"--capabilities") reply caps_candidate ;;
  "run "*"validate-config") reply validate_candidate ;;
  "exec "*" /app/bin/dbt-job --capabilities") reply "caps_$2" ;;
  "exec "*" /app/bin/dbt-job validate-config") reply validate_active ;;
  "exec "*" /app/bin/dbt-job verify-control") reply verify ;;
  "exec "*" cat /app/runtime/dbt-routes.json") cat "$FAKE_RUNTIME/dbt-routes.json" ;;
  "inspect "*) shift; reply "inspect_$*" ;;
esac
