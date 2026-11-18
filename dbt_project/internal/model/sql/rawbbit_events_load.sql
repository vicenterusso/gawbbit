{{- /*
  Ingestion model (formerly models/ingestion/rawbbit_events_load.sql).
  Selects one winner per (app_id, event_id) from the bounded window of
  hour-partitioned raw Parquet files read through the rawbbit_raw_s3 named
  collection. Inputs are validated before rendering (see vars.go).
*/ -}}
with parquet_rows as (
{{- range $i, $path := .HourPaths}}
  {{- if $i}}
  union all
{{- end}}
  select *
  from s3(
    rawbbit_raw_s3,
    filename = '{{$path}}',
    format = 'Parquet',
    structure = '{{$.Structure}}'
  )
{{- end}}
),

parsed as (
  select
    nullIf(event_id, '') as event_id,
    ifNull(nullIf(app_id, ''), 'unknown_app') as app_id,
    ifNull(environment, '') as environment,
    ifNull(event_name, '') as event_name,
    parseDateTime64BestEffortOrNull(event_timestamp, 3, 'UTC') as event_time,
    parseDateTime64BestEffortOrNull(received_at, 3, 'UTC') as received_time,
    user_id,
    ifNull(user_pseudo_id, '') as user_pseudo_id,
    session_id,
    platform,
    app_version,
    os_version,
    device_model,
    locale,
    timezone,
    event_params_json,
    user_properties_json,
    traffic_source_json,
    geo_json,
    consent_json,
    ingest_request_id,
    ingest_user_agent,
    ingest_ip_hash,
    nats_stream,
    nats_sequence
  from parquet_rows
),

ranked as (
  select
    *,
    row_number() over (
      partition by app_id, event_id
      order by
        received_time desc nulls last,
        nats_sequence desc nulls last,
        event_time desc nulls last,
        ingest_request_id desc nulls last
    ) as _dedupe_rank
  from parsed
  where event_id is not null
    and event_time is not null
{{- if .Predicate}}
    {{.Predicate}}
{{- end}}
)

select
  event_id,
  app_id,
  environment,
  event_name,
  assumeNotNull(event_time) as event_time,
  toDate(assumeNotNull(event_time)) as event_date,
  received_time,
  user_id,
  user_pseudo_id,
  session_id,
  platform,
  app_version,
  os_version,
  device_model,
  locale,
  timezone,
  event_params_json,
  user_properties_json,
  traffic_source_json,
  geo_json,
  consent_json,
  ingest_request_id,
  ingest_user_agent,
  ingest_ip_hash,
  nats_stream,
  nats_sequence
from ranked
where _dedupe_rank = 1
