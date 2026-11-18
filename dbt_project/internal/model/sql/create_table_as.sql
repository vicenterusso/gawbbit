{{- /* dbt-clickhouse 1.9.8 create_table_or_empty (MergeTree, no contract). */ -}}
create table {{.Relation}}
  engine = MergeTree()
  order by ((app_id, environment, event_name, event_date, user_pseudo_id, event_time))
  partition by (toYYYYMM(event_date))
  SETTINGS replicated_deduplication_window='0'
  empty
  as (
{{.SQL}}
  )
  SETTINGS s3_throw_on_zero_files_match=0
