# Raw Writer

`raw-writer` is the worker that turns accepted events into durable raw data.

## Responsibilities

- consume events from NATS JetStream
- buffer them into micro-batches
- assign output partitions
- write Snappy Parquet files
- upload them to object storage
- acknowledge messages only after successful durable landing

## Boundary

This component is the storage boundary of the ingestion pipeline.

It preserves raw events in a portable format that downstream query engines and modeling layers can read without changing the ingestion contract.

## Published image

Published tag:

```text
ghcr.io/mirlan-irokez/rawbbit-raw-writer:0.1.11
```

Raw-writer `0.1.11` is published to GHCR and includes malformed-message
isolation.

If event transformation fails, the worker logs a warning with the error class and stream sequence, never the payload, and sends TERM for that message. TERM stops that message's consumer redelivery; it does not delete the stream. This adds no schema layer and does not change other ACK/NAK handling, buffering, flushing, or valid-event output.

The worker needs NATS connectivity and raw-storage configuration through environment variables. It can write to GCS or an S3-compatible object store such as SeaweedFS.

Deploy a pinned version only after confirming it has been published; `latest` may still refer to the previous release.

## Behavior

- Pulls from the durable consumer `NATS_CONSUMER` on `NATS_STREAM`, filter subject `<NATS_SUBJECT_PREFIX>.>`. The consumer is created (explicit ACK, `NATS_ACK_WAIT_SECONDS`, `NATS_MAX_DELIVER`, `NATS_MAX_ACK_PENDING`) only when it does not exist; an existing consumer keeps its configuration.
- Each fetch takes up to `NATS_FETCH_BATCH` messages that are available right away, otherwise waits up to `NATS_FETCH_TIMEOUT_SECONDS`.
- Events are buffered per `(app_id, event_date, hour)`. A partition is flushed when it reaches `RAW_MAX_EVENTS_PER_FILE` events or `RAW_MAX_BYTES_PER_FILE` payload bytes, every `RAW_FLUSH_INTERVAL_SECONDS` for all partitions, and under pressure: when the buffer across partitions reaches 80% of the consumer's server-side `max_ack_pending` (or `RAW_MAX_BUFFERED_EVENTS` if lower), the largest partitions are flushed until fewer than half the threshold remain.
- A flushed partition becomes one Parquet file at `<prefix>/app_id=<app_id>/event_date=YYYY-MM-DD/hour=HH/part-<UTC yyyymmddThhmmss>-<12 hex>.parquet`. Its messages are ACKed after the upload succeeds and NAKed if writing or uploading fails (uploads are tried 3 times, 2 s then 4 s apart).
- Messages that are not JSON, or whose JSON cannot become a row, are TERMed with a warning that names only the error class and stream sequence.
- SIGTERM/SIGINT stop fetching, flush and ACK the buffer, then exit. Give the container a stop grace period (the VM-one quickstart uses 30 seconds).

### Parquet schema

All columns are optional. Strings are `BYTE_ARRAY` with the `String` logical type, `nats_sequence` is a plain `INT64`; files are Snappy-compressed, Parquet format 2.6, one row group, and carry the `ARROW:schema` metadata:

`event_id`, `app_id`, `environment`, `event_name`, `event_timestamp`, `received_at`, `user_id`, `user_pseudo_id`, `session_id`, `platform`, `app_version`, `os_version`, `device_model`, `locale`, `timezone`, `event_params_json`, `user_properties_json`, `traffic_source_json`, `geo_json`, `consent_json`, `ingest_request_id`, `ingest_user_agent`, `ingest_ip_hash`, `nats_stream`, `nats_sequence`.

Scalar columns hold the value as text exactly as the original Python writer rendered it with `str()` (for example `True`, `1.5`, `{'a': 1}`); `*_json` columns hold compact ASCII-only JSON (`json.dumps(..., separators=(",", ":"))`), `{}` when the field is missing or null. `event_date`/`hour` come from `event_timestamp`, else `received_at` (top level, then `ingest.received_at`), else the current UTC time; timestamps are parsed with CPython 3.13 `datetime.fromisoformat` rules and naive values count as UTC.

## Configuration

Environment variables (names are case-insensitive; a `.env` file in the working directory is read for names not set in the environment). See `../../docs/configuration.md` for the full list.

| Variable | Default |
| --- | --- |
| `LOG_LEVEL` | `INFO` (`DEBUG`, `INFO`, `WARNING`/`WARN`, `ERROR`, `CRITICAL`/`FATAL`, `NOTSET`) |
| `NATS_URL` | `nats://localhost:4222` |
| `NATS_STREAM` | `EVENTS` |
| `NATS_SUBJECT_PREFIX` | `events` |
| `NATS_CONSUMER` | `raw-writer` |
| `NATS_FETCH_BATCH` | `500` |
| `NATS_FETCH_TIMEOUT_SECONDS` | `1.0` |
| `NATS_ACK_WAIT_SECONDS` | `120` |
| `NATS_MAX_DELIVER` | `10` |
| `NATS_MAX_ACK_PENDING` | `10000` (>= 1, used only when creating the consumer) |
| `RAW_FLUSH_INTERVAL_SECONDS` | `60` (must be lower than `NATS_ACK_WAIT_SECONDS`) |
| `RAW_MAX_EVENTS_PER_FILE` | `1000` |
| `RAW_MAX_BYTES_PER_FILE` | `4194304` |
| `RAW_MAX_BUFFERED_EVENTS` | unset (>= 1; empty means unset) |
| `RAW_STORAGE_BACKEND` | `gcs` (`gcs` or `s3`) |
| `GCS_RAW_BUCKET`, `GCS_RAW_PREFIX` | required for `gcs`; prefix `raw` |
| `GOOGLE_APPLICATION_CREDENTIALS` | Application Default Credentials; `STORAGE_EMULATOR_HOST` targets an emulator |
| `S3_ENDPOINT_URL`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` | required for `s3` |
| `S3_PREFIX` | `raw` |
| `S3_REGION` | `us-east-1` |
| `S3_FORCE_PATH_STYLE` | `1` |
| `S3_USE_SSL` | `1` (the endpoint URL scheme decides, as with boto3) |
| `S3_VERIFY_SSL` | `1` |

Invalid settings stop the process with exit code 1 and a message that never echoes the values.

## Build, run and test

Go 1.24, no cgo:

```sh
go build -o raw-writer ./cmd/raw-writer
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...   # OCI Ampere
go test ./...
go vet ./...
```

`go test ./...` runs offline. Parity with the original Python implementation is pinned by recorded goldens in `testdata/`:

- `transform_golden.jsonl.gz`: 6,759 messages (contract-shaped events, boundary cases, randomized payloads) with the outcome, error class, partition key and row the Python writer produced.
- `isoformat_golden.jsonl.gz`: 50,000 timestamp strings and their Python `parse_iso_datetime` result.
- `python_raw_sample.parquet`: a file written by pyarrow, compared column by column with the Go writer's output.
- `python_unicode_tables.json`, `redacted_url_golden.json`: CPython `str.isprintable`/`str.isspace` tables and `urlsplit`-based log redaction.

The image (`Dockerfile`) is a multi-stage build onto `gcr.io/distroless/static-debian12:nonroot`; set `TARGETOS`/`TARGETARCH` (buildx does) for multi-arch. It runs as UID 65532, so a mounted `GOOGLE_APPLICATION_CREDENTIALS` file must be readable by that user.

## Go rewrite notes

The service was rewritten from Python to Go. Output (object keys, Parquet schema and contents) and ACK/NAK/TERM behavior match the Python version; the deliberate differences are:

- A row whose text columns would hold a lone UTF-16 surrogate (from a JSON escape such as `"\ud800"`) is TERMed at transform time with `error_class=UnicodeEncodeError`. The Python writer accepted it, then failed the whole partition's Parquet encoding and NAKed every event in it until `NATS_MAX_DELIVER`.
- Parquet files are encoded in memory and uploaded directly instead of through a temporary file.
- S3 uploads use a single `PutObject` (boto3 switched to multipart above 8 MiB); GCS uploads use the JSON API media upload. SDK-level retries differ from boto3/google-cloud-storage; the outer 3-attempt loop is unchanged.
- `error_class` values in `upload_retry`, `flush_failed`, `consumer_info_failed` and `connect_failed` logs are Go error type names or S3 error codes (GCS HTTP errors keep the `google.api_core` names such as `Forbidden`). Transform failures keep the Python names (`AttributeError`, `OverflowError`).
- Integer settings above the 64-bit range are rejected as invalid integers (pydantic accepted them).
- The initial NATS connection is retried like nats-py (61 attempts, 2 s apart) and a stop signal during that phase exits cleanly. Fetch errors other than a closed connection are logged as `fetch_failed` and treated as an empty fetch; a closed connection stops the service with exit code 1.
- Third-party library debug logging (boto3, nats-py) is gone; only the service's own events are logged, in the same `<time> <LEVEL> service=raw-writer event=...` format on stderr.

## Related docs

- `../collector-api/README.md`
- `../../docs/architecture.md`
- `../../docs/configuration.md`
