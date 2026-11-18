{{- /* Singular data test (formerly tests/rawbbit_events_load_unique.sql). */ -}}
select
  app_id,
  event_id,
  count() as row_count
from {{.Relation}}
group by app_id, event_id
having row_count > 1
