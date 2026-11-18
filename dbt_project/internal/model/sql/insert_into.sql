{{- /* dbt-clickhouse 1.9.8 clickhouse__insert_into (no contract). */ -}}
insert into {{.Relation}}
  ({{.Columns}})
{{.SQL}}
  SETTINGS s3_throw_on_zero_files_match=0
