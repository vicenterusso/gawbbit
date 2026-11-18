{{- /* dbt generic not_null test. */ -}}
select {{.Column}}
from {{.Relation}}
where {{.Column}} is null
