{{- /* dbt-clickhouse 1.9.8 delete_insert: lightweight delete of replayed keys. */ -}}
delete from {{.Target}} where (app_id, event_id) in (select app_id, event_id
  from {{.NewData}})
  SETTINGS s3_throw_on_zero_files_match=0
