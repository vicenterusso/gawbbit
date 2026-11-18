# Collector API

`collector-api` is the public ingestion edge for Rawbbit.

## Responsibilities

- accept `POST /v1/events:batch`
- validate request payloads
- map API keys to `app_id`
- attach ingest-time metadata
- publish accepted events into NATS JetStream
- handle browser-facing ingress concerns such as CORS

## Boundary

The collector is a stateless HTTP service. Its contract ends when an accepted event has been handed off to the durable message layer.

That separation keeps request handling narrow and leaves durable storage and downstream modeling to later stages in the pipeline.

## Build, run and test

The service is a single static Go binary (Go 1.24, no cgo).

```bash
go build -o collector-api ./cmd/collector-api
COLLECTOR_API_KEYS_JSON='{"my-key":"com.example.game"}' \
NATS_URL=nats://localhost:4222 ./collector-api            # listens on 0.0.0.0:${PORT:-8080}
./collector-api --host 127.0.0.1                          # bind a specific address
```

Tests:

```bash
go test ./...                       # unit, golden and parity tests; no network needed
go test -race ./...
COLLECTOR_TEST_NATS_URL=nats://127.0.0.1:4222 \
  go test -tags integration ./internal/publisher/   # against a real NATS JetStream
go test -run XXX -fuzz FuzzParseBatch ./internal/schema
```

Container image (multi-stage, `linux/amd64` and `linux/arm64`, distroless
`nonroot` runtime):

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t rawbbit-collector-api .
```

Layout: `cmd/collector-api` (process lifecycle), `internal/config`
(settings), `internal/schema` (request validation), `internal/pyjson` (JSON
parsing/encoding), `internal/geoip`, `internal/publisher` (JetStream),
`internal/server` (HTTP, CORS, enrichment).

## HTTP API

- `POST /v1/events:batch` with `X-API-Key`: `200 {"request_id": "...", "accepted_events": N}`.
  Errors: `413 {"detail":"Request too large"}` (body over `MAX_REQUEST_BYTES`),
  `422 {"detail":[{"type","loc","msg"}, ...]}` (invalid JSON or schema),
  `400 {"detail":"events cannot be empty"}`,
  `413 {"detail":"events limit exceeded: <MAX_EVENTS_PER_REQUEST>"}`,
  `401 {"detail":"Missing X-API-Key"}` / `401 {"detail":"Invalid API key"}`,
  `403 {"detail":"API key is not allowed for this app_id"}`,
  `503 {"detail":"Failed to publish events"}` (safe to retry).
- `GET /healthz`: `{"status":"ok"}`.
- `GET /openapi.json`, `/docs`, `/redoc`: the API description and docs pages.

Every accepted event is published to `<NATS_SUBJECT_PREFIX>.<app_id>` (app_id
stripped, spaces replaced by `_`) with header `Nats-Msg-Id: <app_id>:<event_id>`.
Server-side values always replace client-supplied `received_at`,
`ingest.received_at`, `ingest.request_id`, `ingest.user_agent` and
`ingest.ip_hash` (salted SHA-256 of the client IP); `ingest.ip` is kept only
with `STORE_RAW_IP`; GeoIP overrides `geo.country`/`geo.country_code`.

## Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `ENV` | `local` | logged at startup |
| `PORT` | `8080` | listening port |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARNING`, `ERROR`, `CRITICAL` |
| `COLLECTOR_API_KEYS_JSON` | `{}` | JSON object `{"api_key": "app_id"}` |
| `NATS_URL` | `nats://localhost:4222` | |
| `NATS_STREAM` | `EVENTS` | created if missing; must cover `<prefix>.>` with file storage |
| `NATS_SUBJECT_PREFIX` | `events` | |
| `NATS_STREAM_MAX_AGE_SECONDS` | `604800` | used when creating the stream |
| `NATS_DUPLICATE_WINDOW_SECONDS` | `600` | JetStream dedupe window, used when creating the stream |
| `NATS_MAX_INFLIGHT_PUBLISHES` | `512` | publishes awaiting an ACK, process-wide (>= 1) |
| `MAX_EVENTS_PER_REQUEST` | `500` | |
| `MAX_REQUEST_BYTES` | `1048576` | |
| `IP_HASH_SALT` | `change-me` | |
| `STORE_RAW_IP` | `false` | |
| `GEOIP_ENABLED` | `false` | |
| `GEOIP_MMDB_PATH` | | MaxMind-format country database (GeoIP2, DB-IP lite) |
| `CORS_ALLOW_ORIGINS` | | comma separated; CORS is off when empty |
| `CORS_ALLOW_METHODS` | `POST,OPTIONS` | |
| `CORS_ALLOW_HEADERS` | `Content-Type,X-API-Key` | |
| `CORS_ALLOW_CREDENTIALS` | `false` | |
| `CORS_MAX_AGE_SECONDS` | `600` | only `Retry-After` is exposed to browsers |
| `FORWARDED_ALLOW_IPS` | `127.0.0.1` | peers trusted for `X-Forwarded-For`/`X-Forwarded-Proto` as the connection's client address (`*` trusts all) |
| `UVICORN_WORKERS` | | accepted and ignored (see notes) |

Names are case-insensitive. A `.env` file in the working directory is read
too; real environment variables take precedence. Invalid values stop the
process with exit code 1 and a pydantic-style message; failing to reach NATS
(61 attempts, 2 s apart) or an incompatible stream exits with code 3. On
SIGTERM/SIGINT the server stops accepting connections, finishes in-flight
requests, drains NATS and terminates with that signal.

## Published image

Currently published public GHCR image (owner-confirmed 2026-10-24):

```text
ghcr.io/mirlan-irokez/rawbbit-collector-api:0.1.10
```

Platform: `linux/amd64`; OCI index digest:
`sha256:710e4bcd5347b15d0f1cac819bc5d26320a39026012264c4ba76a0ca98095fce`.
The `linux/amd64` platform manifest is
`sha256:48014fa043cc8235251d97e6702e0418d92e5f014a19ff724a271a5117faac7a`;
the index also includes an attestation manifest.

That image is the Python implementation; the Go rewrite in this directory
has not been published yet.

The service listens on `PORT`, defaulting to `8080`. It needs NATS connectivity and collector API-key configuration through environment variables.

Use the pinned version tag for deployments. The 0.1.10 publication did not move
the `latest` tag.

## Release notes

### 0.1.9 — published 2026-10-22

- Browser JavaScript may read only the `Retry-After` response header through the
  existing CORS policy; allowed origins and other exposed headers are unchanged.
- Body-supplied `received_at`, `ingest.request_id`, `ingest.user_agent`,
  `ingest.ip_hash`, and `ingest.ip` are always replaced with server-derived
  values. `ingest.ip` is omitted when raw-IP storage is disabled or no valid
  request IP is available. Top-level `received_at` is always populated.
- The VM quickstart pins the published 0.1.9 image.

### 0.1.10 — published 2026-10-24

Validation failures remain HTTP 422 with the existing `{"detail":[...]}`
envelope and return only `type`, `loc`, and `msg`. Up to 20 errors are returned
in order. Only when there are more than 20, the response contains the first 19
errors followed by this fixed marker in slot 20:

```json
{"type":"errors_truncated","loc":[],"msg":"Additional validation errors omitted"}
```

This intentionally removes Pydantic's prior `input`, `ctx`, and `url` fields.
Malformed JSON that previously could fail response serialization as HTTP 500
now returns the sanitized 422 response. SDKs quarantine that response rather
than retrying it as a server error.

Stream startup checks for an existing compatible JetStream stream before adding
one. A compatible stream confirmed after initial `NotFoundError` and a successful
compatible add logs `stream_created` regardless of `did_create` (including when
that attribute is absent); other compatible confirmations log
`stream_exists_and_compatible`. This log describes an observed absent-to-present
transition, not exclusive creation by a particular worker.

The VM-one quickstart now pins published collector-api `0.1.10`.

## Go rewrite notes

The service was rewritten from Python (FastAPI, pydantic, nats-py) to Go with
the HTTP contract, NATS subjects, headers and payloads, logs, exit codes and
configuration kept identical. Validation reproduces pydantic 2.12's lax JSON
mode exactly (jiter parsing including `NaN`/`Infinity` and big integers,
speedate datetime parsing with the date-only and unix-timestamp fallbacks,
lax booleans, duplicate keys, error types and order), and payloads are
encoded byte-for-byte like `json.dumps(..., separators=(",", ":"))`.

Parity was verified by running the Python and Go services side by side
against the same NATS JetStream on about 5,200 generated requests (valid,
invalid and byte-fuzzed bodies, header and CORS variants, four configuration
profiles), comparing responses, stored JetStream messages (raw headers and
payloads), application logs and access logs. The Python results are kept as
golden files under `internal/*/testdata` and replayed by `go test`.

Deliberate differences:

- An `event_id`/`app_id` containing CR or LF is written into `Nats-Msg-Id`
  with those characters replaced by spaces (nats.go); nats-py wrote them raw,
  which allowed header injection. JetStream deduplication keys differ from
  the Python service only for such IDs.
- Go's HTTP server trims trailing spaces/tabs from header values (uvicorn
  kept them) and rejects malformed requests that uvicorn tolerated, such as a
  non-ASCII `Host` header or invalid `%` escapes in the path (400). Behind
  the quickstart's Caddy proxy neither is observable.
- Reason phrases are Go's (`413 Request Entity Too Large`,
  `422 Unprocessable Entity`); there is no `server: uvicorn` header. Status
  codes, bodies and other headers are unchanged.
- `UVICORN_WORKERS` is ignored: one process handles requests concurrently, so
  `NATS_MAX_INFLIGHT_PUBLISHES` is now the cap for the whole service rather
  than per worker.
- After answering `413 Request too large`, at most 64 MiB of the remaining
  body is read and discarded (uvicorn consumed all of it). The
  `request_bytes` logged for that rejection counts what had arrived when the
  limit was crossed, which depends on network chunking as before.
- Integer settings beyond the 64-bit range saturate instead of being
  unbounded; `NATS_MAX_INFLIGHT_PUBLISHES` above 1,048,576 behaves as that
  value. `PORT` is parsed once for both logging and binding (the old image let
  uvicorn parse `$PORT` separately).
- `COLLECTOR_API_KEYS_JSON` rejects lone UTF-16 surrogate escapes and nesting
  deeper than 200 levels (Python's `json.loads` accepted them).
- `FORWARDED_ALLOW_IPS` networks must use `address/prefix-length` (netmask
  notation is treated as a literal host name).
- GeoIP `name`/`iso_code` values that are maps, lists or bytes are ignored
  (Python would have used their `repr`).
- The image runs as the distroless `nonroot` user, so a mounted GeoIP
  database must be readable by uid 65532 (world-readable files are fine).
- The startup line `Uvicorn running on ...` is now `Listening on ...`; the
  `/docs`, `/redoc` and `/openapi.json` pages are the FastAPI output captured
  from the Python service.
- The Python unit tests asserted `uses storage 'StorageType.MEMORY'` with a
  fake enum; the real service (and the Go one) reports the server's value,
  `uses storage 'memory'`.

## Related docs

- `../raw-writer/README.md`
- `../../docs/architecture.md`
- `../../docs/configuration.md`
