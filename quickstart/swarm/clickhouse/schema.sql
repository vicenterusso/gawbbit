-- analytics.events is the source of truth for every accepted game event.
-- Vector inserts one row per event; clickhouse-setup applies this file on
-- every deploy, so it must stay idempotent.
--
-- Duplicates (an SDK retry, a Vector retry after a timeout) share
-- (app_id, event_id) and collapse to the row with the latest received_time
-- when ClickHouse merges parts. Queries that must be exact before the merge
-- use FINAL, for example:
--
--   SELECT count() FROM analytics.events FINAL WHERE app_id = 'com.example.game'

CREATE DATABASE IF NOT EXISTS analytics;

CREATE TABLE IF NOT EXISTS analytics.events
(
    event_id String,
    app_id LowCardinality(String),
    environment LowCardinality(String),
    event_name LowCardinality(String),

    event_time DateTime64(3, 'UTC'),
    event_date Date,

    received_time DateTime64(3, 'UTC'),
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

    INDEX idx_event_name event_name TYPE set(1000) GRANULARITY 4,
    INDEX idx_event_time event_time TYPE minmax GRANULARITY 4
)
ENGINE = ReplacingMergeTree(received_time)
PARTITION BY toYYYYMM(event_date)
ORDER BY (app_id, event_id);
