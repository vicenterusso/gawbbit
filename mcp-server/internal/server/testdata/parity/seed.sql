DROP DATABASE IF EXISTS analytics SYNC;

DROP DATABASE IF EXISTS team_a SYNC;

DROP DATABASE IF EXISTS team_b SYNC;

DROP DATABASE IF EXISTS team_c SYNC;

DROP USER IF EXISTS rawbbit_mcp;

DROP USER IF EXISTS rawbbit_mcp_team_a;

DROP USER IF EXISTS rawbbit_mcp_team_b;

DROP USER IF EXISTS rawbbit_mcp_team_c;

DROP ROLE IF EXISTS rawbbit_role_team_a;

DROP ROLE IF EXISTS rawbbit_role_team_b;

DROP ROLE IF EXISTS rawbbit_role_team_c;

CREATE DATABASE analytics;

CREATE TABLE IF NOT EXISTS analytics.events
(
    event_id Nullable(String),
    app_id LowCardinality(String),
    environment LowCardinality(String),
    event_name LowCardinality(String),

    event_time DateTime64(3, 'UTC'),
    event_date Date,

    received_time Nullable(DateTime64(3, 'UTC')),
    user_id Nullable(String),
    user_pseudo_id String,
    session_id Nullable(String),
    platform LowCardinality(Nullable(String)),
    app_version Nullable(String),
    os_version Nullable(String),
    device_model Nullable(String),
    locale Nullable(String),
    timezone Nullable(String),
    event_params_json Nullable(String),
    user_properties_json Nullable(String),
    traffic_source_json Nullable(String),
    geo_json Nullable(String),
    consent_json Nullable(String),

    ingest_request_id Nullable(String),
    ingest_user_agent Nullable(String),
    ingest_ip_hash Nullable(String),
    nats_stream Nullable(String),
    nats_sequence Nullable(Int64)
)
ENGINE=MergeTree
PARTITION BY toYYYYMM(event_date)
ORDER BY (app_id, environment, event_name, event_date, user_pseudo_id, event_time);

CREATE DATABASE team_a;

CREATE TABLE IF NOT EXISTS team_a.events
(
    event_id Nullable(String),
    app_id LowCardinality(String),
    environment LowCardinality(String),
    event_name LowCardinality(String),

    event_time DateTime64(3, 'UTC'),
    event_date Date,

    received_time Nullable(DateTime64(3, 'UTC')),
    user_id Nullable(String),
    user_pseudo_id String,
    session_id Nullable(String),
    platform LowCardinality(Nullable(String)),
    app_version Nullable(String),
    os_version Nullable(String),
    device_model Nullable(String),
    locale Nullable(String),
    timezone Nullable(String),
    event_params_json Nullable(String),
    user_properties_json Nullable(String),
    traffic_source_json Nullable(String),
    geo_json Nullable(String),
    consent_json Nullable(String),

    ingest_request_id Nullable(String),
    ingest_user_agent Nullable(String),
    ingest_ip_hash Nullable(String),
    nats_stream Nullable(String),
    nats_sequence Nullable(Int64)
)
ENGINE=MergeTree
PARTITION BY toYYYYMM(event_date)
ORDER BY (app_id, environment, event_name, event_date, user_pseudo_id, event_time);

CREATE DATABASE team_b;

CREATE TABLE IF NOT EXISTS team_b.events
(
    event_id Nullable(String),
    app_id LowCardinality(String),
    environment LowCardinality(String),
    event_name LowCardinality(String),

    event_time DateTime64(3, 'UTC'),
    event_date Date,

    received_time Nullable(DateTime64(3, 'UTC')),
    user_id Nullable(String),
    user_pseudo_id String,
    session_id Nullable(String),
    platform LowCardinality(Nullable(String)),
    app_version Nullable(String),
    os_version Nullable(String),
    device_model Nullable(String),
    locale Nullable(String),
    timezone Nullable(String),
    event_params_json Nullable(String),
    user_properties_json Nullable(String),
    traffic_source_json Nullable(String),
    geo_json Nullable(String),
    consent_json Nullable(String),

    ingest_request_id Nullable(String),
    ingest_user_agent Nullable(String),
    ingest_ip_hash Nullable(String),
    nats_stream Nullable(String),
    nats_sequence Nullable(Int64)
)
ENGINE=MergeTree
PARTITION BY toYYYYMM(event_date)
ORDER BY (app_id, environment, event_name, event_date, user_pseudo_id, event_time);

CREATE DATABASE team_c;

CREATE TABLE IF NOT EXISTS team_c.events
(
    event_id Nullable(String),
    app_id LowCardinality(String),
    environment LowCardinality(String),
    event_name LowCardinality(String),

    event_time DateTime64(3, 'UTC'),
    event_date Date,

    received_time Nullable(DateTime64(3, 'UTC')),
    user_id Nullable(String),
    user_pseudo_id String,
    session_id Nullable(String),
    platform LowCardinality(Nullable(String)),
    app_version Nullable(String),
    os_version Nullable(String),
    device_model Nullable(String),
    locale Nullable(String),
    timezone Nullable(String),
    event_params_json Nullable(String),
    user_properties_json Nullable(String),
    traffic_source_json Nullable(String),
    geo_json Nullable(String),
    consent_json Nullable(String),

    ingest_request_id Nullable(String),
    ingest_user_agent Nullable(String),
    ingest_ip_hash Nullable(String),
    nats_stream Nullable(String),
    nats_sequence Nullable(Int64)
)
ENGINE=MergeTree
PARTITION BY toYYYYMM(event_date)
ORDER BY (app_id, environment, event_name, event_date, user_pseudo_id, event_time);

CREATE USER rawbbit_mcp IDENTIFIED WITH sha256_password BY 'mainpw';

GRANT SELECT, SHOW ON analytics.* TO rawbbit_mcp;

CREATE ROLE rawbbit_role_team_a;

CREATE USER rawbbit_mcp_team_a IDENTIFIED WITH sha256_password BY 'team_a-db-secret' SETTINGS readonly = 1, max_execution_time = 30, max_result_rows = 7, result_overflow_mode = 'break';

GRANT rawbbit_role_team_a TO rawbbit_mcp_team_a;

SET DEFAULT ROLE rawbbit_role_team_a TO rawbbit_mcp_team_a;

GRANT SELECT ON team_a.events TO rawbbit_role_team_a;

CREATE ROLE rawbbit_role_team_b;

CREATE USER rawbbit_mcp_team_b IDENTIFIED WITH sha256_password BY 'team_b-db-secret' SETTINGS readonly = 1, max_execution_time = 5, max_result_rows = 50, result_overflow_mode = 'break';

GRANT rawbbit_role_team_b TO rawbbit_mcp_team_b;

SET DEFAULT ROLE rawbbit_role_team_b TO rawbbit_mcp_team_b;

GRANT SELECT ON team_b.events TO rawbbit_role_team_b;

CREATE ROLE rawbbit_role_team_c;

CREATE USER rawbbit_mcp_team_c IDENTIFIED WITH sha256_password BY 'team_c-db-secret' SETTINGS readonly = 1;

GRANT rawbbit_role_team_c TO rawbbit_mcp_team_c;

SET DEFAULT ROLE rawbbit_role_team_c TO rawbbit_mcp_team_c;

GRANT SELECT ON team_c.events TO rawbbit_role_team_c;

INSERT INTO analytics.events SELECT
  if(number % 7 = 0, NULL, concat('evt-', toString(number))) AS event_id,
  ['com.example.game','com.example.other','bob''s app'][1 + number % 3] AS app_id,
  ['prod','dev','staging'][1 + number % 3 + (number % 5 = 0)] AS environment,
  ['session_start','tutorial_started','tutorial_completed','purchase_completed','level_up'][1 + (number % 11) % 5] AS event_name,
  toDateTime64('2026-06-01 00:00:00', 3, 'UTC') + toIntervalMillisecond(number * 3600123) AS event_time,
  toDate(event_time) AS event_date,
  if(number % 4 = 0, NULL, event_time + toIntervalMillisecond(250)) AS received_time,
  multiIf(number % 6 = 0, NULL, number % 6 = 1, '', concat('user-', toString(number % 13))) AS user_id,
  concat('pseudo-', toString(number % 17)) AS user_pseudo_id,
  if(number % 9 = 0, NULL, concat('sess-', toString(intDiv(number, 3)))) AS session_id,
  if(number % 8 = 0, NULL, ['ios','android','web'][1 + number % 3]) AS platform,
  if(number % 5 = 0, NULL, '1.2.3') AS app_version,
  if(number % 5 = 1, NULL, '17.4') AS os_version,
  if(number % 5 = 2, NULL, 'Pixel "8" ''pro''') AS device_model,
  if(number % 5 = 3, NULL, 'pt-BR') AS locale,
  if(number % 5 = 4, NULL, 'America/Sao_Paulo') AS timezone,
  multiIf(number % 10 = 0, NULL, number % 10 = 1, '', number % 10 = 2, 'not json', number % 10 = 3, '[1,2]',
    concat('{"level":', toString(number % 4), ',"score":', toString(number * 1.5), ',"nested":{"a":[1,2,{"b":null}]},"unicode":"caf\\u00e9 \\ud83d\\ude00","k', toString(number % 3), '":true}')) AS event_params_json,
  if(number % 3 = 0, NULL, '{"vip":false,"tier":"gold"}') AS user_properties_json,
  if(number % 2 = 0, '', '{"source":"ads","medium":"cpc"}') AS traffic_source_json,
  multiIf(number % 4 = 0, NULL, number % 4 = 1, '{}', '{"country":"BR","city":"São Paulo"}') AS geo_json,
  if(number % 5 = 0, NULL, '{"analytics":true}') AS consent_json,
  concat('req-', toString(number)) AS ingest_request_id,
  multiIf(number % 12 = 0, 'Mozilla/5.0 (compatible; Googlebot/2.1)', number % 12 = 1, NULL, number % 12 = 2, 'HeadlessChrome/120', 'Mozilla/5.0 (iPhone)') AS ingest_user_agent,
  if(number % 3 = 0, NULL, lower(hex(SHA256(toString(number))))) AS ingest_ip_hash,
  'EVENTS' AS nats_stream,
  if(number % 6 = 5, NULL, toInt64(number) * 1000000007 - 9000000000000000000) AS nats_sequence
FROM numbers(240);

INSERT INTO team_a.events SELECT
  if(number % 7 = 0, NULL, concat('evt-', toString(number))) AS event_id,
  ['com.example.game','com.example.other','bob''s app'][1 + number % 3] AS app_id,
  ['prod','dev','staging'][1 + number % 3 + (number % 5 = 0)] AS environment,
  ['session_start','tutorial_started','tutorial_completed','purchase_completed','level_up'][1 + (number % 11) % 5] AS event_name,
  toDateTime64('2026-06-01 00:00:00', 3, 'UTC') + toIntervalMillisecond(number * 3600123) AS event_time,
  toDate(event_time) AS event_date,
  if(number % 4 = 0, NULL, event_time + toIntervalMillisecond(250)) AS received_time,
  multiIf(number % 6 = 0, NULL, number % 6 = 1, '', concat('user-', toString(number % 13))) AS user_id,
  concat('pseudo-', toString(number % 17)) AS user_pseudo_id,
  if(number % 9 = 0, NULL, concat('sess-', toString(intDiv(number, 3)))) AS session_id,
  if(number % 8 = 0, NULL, ['ios','android','web'][1 + number % 3]) AS platform,
  if(number % 5 = 0, NULL, '1.2.3') AS app_version,
  if(number % 5 = 1, NULL, '17.4') AS os_version,
  if(number % 5 = 2, NULL, 'Pixel "8" ''pro''') AS device_model,
  if(number % 5 = 3, NULL, 'pt-BR') AS locale,
  if(number % 5 = 4, NULL, 'America/Sao_Paulo') AS timezone,
  multiIf(number % 10 = 0, NULL, number % 10 = 1, '', number % 10 = 2, 'not json', number % 10 = 3, '[1,2]',
    concat('{"level":', toString(number % 4), ',"score":', toString(number * 1.5), ',"nested":{"a":[1,2,{"b":null}]},"unicode":"caf\\u00e9 \\ud83d\\ude00","k', toString(number % 3), '":true}')) AS event_params_json,
  if(number % 3 = 0, NULL, '{"vip":false,"tier":"gold"}') AS user_properties_json,
  if(number % 2 = 0, '', '{"source":"ads","medium":"cpc"}') AS traffic_source_json,
  multiIf(number % 4 = 0, NULL, number % 4 = 1, '{}', '{"country":"BR","city":"São Paulo"}') AS geo_json,
  if(number % 5 = 0, NULL, '{"analytics":true}') AS consent_json,
  concat('req-', toString(number)) AS ingest_request_id,
  multiIf(number % 12 = 0, 'Mozilla/5.0 (compatible; Googlebot/2.1)', number % 12 = 1, NULL, number % 12 = 2, 'HeadlessChrome/120', 'Mozilla/5.0 (iPhone)') AS ingest_user_agent,
  if(number % 3 = 0, NULL, lower(hex(SHA256(toString(number))))) AS ingest_ip_hash,
  'EVENTS' AS nats_stream,
  if(number % 6 = 5, NULL, toInt64(number) * 1000000007 - 9000000000000000000) AS nats_sequence
FROM numbers(240);

INSERT INTO team_b.events SELECT
  if(number % 7 = 0, NULL, concat('evt-', toString(number))) AS event_id,
  ['com.example.game','com.example.other','bob''s app'][1 + number % 3] AS app_id,
  ['prod','dev','staging'][1 + number % 3 + (number % 5 = 0)] AS environment,
  ['session_start','tutorial_started','tutorial_completed','purchase_completed','level_up'][1 + (number % 11) % 5] AS event_name,
  toDateTime64('2026-06-01 00:00:00', 3, 'UTC') + toIntervalMillisecond(number * 3600123) AS event_time,
  toDate(event_time) AS event_date,
  if(number % 4 = 0, NULL, event_time + toIntervalMillisecond(250)) AS received_time,
  multiIf(number % 6 = 0, NULL, number % 6 = 1, '', concat('user-', toString(number % 13))) AS user_id,
  concat('pseudo-', toString(number % 17)) AS user_pseudo_id,
  if(number % 9 = 0, NULL, concat('sess-', toString(intDiv(number, 3)))) AS session_id,
  if(number % 8 = 0, NULL, ['ios','android','web'][1 + number % 3]) AS platform,
  if(number % 5 = 0, NULL, '1.2.3') AS app_version,
  if(number % 5 = 1, NULL, '17.4') AS os_version,
  if(number % 5 = 2, NULL, 'Pixel "8" ''pro''') AS device_model,
  if(number % 5 = 3, NULL, 'pt-BR') AS locale,
  if(number % 5 = 4, NULL, 'America/Sao_Paulo') AS timezone,
  multiIf(number % 10 = 0, NULL, number % 10 = 1, '', number % 10 = 2, 'not json', number % 10 = 3, '[1,2]',
    concat('{"level":', toString(number % 4), ',"score":', toString(number * 1.5), ',"nested":{"a":[1,2,{"b":null}]},"unicode":"caf\\u00e9 \\ud83d\\ude00","k', toString(number % 3), '":true}')) AS event_params_json,
  if(number % 3 = 0, NULL, '{"vip":false,"tier":"gold"}') AS user_properties_json,
  if(number % 2 = 0, '', '{"source":"ads","medium":"cpc"}') AS traffic_source_json,
  multiIf(number % 4 = 0, NULL, number % 4 = 1, '{}', '{"country":"BR","city":"São Paulo"}') AS geo_json,
  if(number % 5 = 0, NULL, '{"analytics":true}') AS consent_json,
  concat('req-', toString(number)) AS ingest_request_id,
  multiIf(number % 12 = 0, 'Mozilla/5.0 (compatible; Googlebot/2.1)', number % 12 = 1, NULL, number % 12 = 2, 'HeadlessChrome/120', 'Mozilla/5.0 (iPhone)') AS ingest_user_agent,
  if(number % 3 = 0, NULL, lower(hex(SHA256(toString(number))))) AS ingest_ip_hash,
  'EVENTS' AS nats_stream,
  if(number % 6 = 5, NULL, toInt64(number) * 1000000007 - 9000000000000000000) AS nats_sequence
FROM numbers(30);

INSERT INTO team_c.events SELECT
  if(number % 7 = 0, NULL, concat('evt-', toString(number))) AS event_id,
  ['com.example.game','com.example.other','bob''s app'][1 + number % 3] AS app_id,
  ['prod','dev','staging'][1 + number % 3 + (number % 5 = 0)] AS environment,
  ['session_start','tutorial_started','tutorial_completed','purchase_completed','level_up'][1 + (number % 11) % 5] AS event_name,
  toDateTime64('2026-06-01 00:00:00', 3, 'UTC') + toIntervalMillisecond(number * 3600123) AS event_time,
  toDate(event_time) AS event_date,
  if(number % 4 = 0, NULL, event_time + toIntervalMillisecond(250)) AS received_time,
  multiIf(number % 6 = 0, NULL, number % 6 = 1, '', concat('user-', toString(number % 13))) AS user_id,
  concat('pseudo-', toString(number % 17)) AS user_pseudo_id,
  if(number % 9 = 0, NULL, concat('sess-', toString(intDiv(number, 3)))) AS session_id,
  if(number % 8 = 0, NULL, ['ios','android','web'][1 + number % 3]) AS platform,
  if(number % 5 = 0, NULL, '1.2.3') AS app_version,
  if(number % 5 = 1, NULL, '17.4') AS os_version,
  if(number % 5 = 2, NULL, 'Pixel "8" ''pro''') AS device_model,
  if(number % 5 = 3, NULL, 'pt-BR') AS locale,
  if(number % 5 = 4, NULL, 'America/Sao_Paulo') AS timezone,
  multiIf(number % 10 = 0, NULL, number % 10 = 1, '', number % 10 = 2, 'not json', number % 10 = 3, '[1,2]',
    concat('{"level":', toString(number % 4), ',"score":', toString(number * 1.5), ',"nested":{"a":[1,2,{"b":null}]},"unicode":"caf\\u00e9 \\ud83d\\ude00","k', toString(number % 3), '":true}')) AS event_params_json,
  if(number % 3 = 0, NULL, '{"vip":false,"tier":"gold"}') AS user_properties_json,
  if(number % 2 = 0, '', '{"source":"ads","medium":"cpc"}') AS traffic_source_json,
  multiIf(number % 4 = 0, NULL, number % 4 = 1, '{}', '{"country":"BR","city":"São Paulo"}') AS geo_json,
  if(number % 5 = 0, NULL, '{"analytics":true}') AS consent_json,
  concat('req-', toString(number)) AS ingest_request_id,
  multiIf(number % 12 = 0, 'Mozilla/5.0 (compatible; Googlebot/2.1)', number % 12 = 1, NULL, number % 12 = 2, 'HeadlessChrome/120', 'Mozilla/5.0 (iPhone)') AS ingest_user_agent,
  if(number % 3 = 0, NULL, lower(hex(SHA256(toString(number))))) AS ingest_ip_hash,
  'EVENTS' AS nats_stream,
  if(number % 6 = 5, NULL, toInt64(number) * 1000000007 - 9000000000000000000) AS nats_sequence
FROM numbers(12);
