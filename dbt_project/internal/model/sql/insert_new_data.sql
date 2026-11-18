{{- /* dbt-clickhouse 1.9.8 delete_insert: insert the window winners. */ -}}
insert into {{.Target}} select {{.Columns}} from {{.NewData}}
  SETTINGS s3_throw_on_zero_files_match=0
