# Gawbbit

**Gawbbit is a simplified fork of [Rawbbit](https://github.com/mirlan-irokez/rawbbit),
the self-hosted event pipeline for games.** Game clients send batched events
over HTTP; [Vector](https://vector.dev/) validates them and inserts them into
[ClickHouse](https://clickhouse.com/), which is the source of truth; the
Rawbbit MCP server and Metabase read from ClickHouse. Everything runs as one
Docker Swarm stack, on a single node or a small cluster, `linux/amd64` or
`linux/arm64` (for example OCI Ampere A1).

```text
game SDK -> Caddy -> Vector -> ClickHouse (analytics.events) -> MCP / Metabase / SQL
```

The project keeps the Rawbbit names (`rawbbit-mcp-server`, `analytics.events`,
the SDK packages) so the SDKs, plugins and MCP clients keep working. See
[Relationship to Rawbbit](#relationship-to-rawbbit) for what changed.

![Rawbbit](./assets/Rawbbit.png)

## Table of contents

- [What is included](#what-is-included)
- [Architecture](#architecture)
- [Quickstart](#quickstart)
- [Relationship to Rawbbit](#relationship-to-rawbbit)
- [Published image](#published-image)
- [Testing and CI](#testing-and-ci)
- [Repository layout](#repository-layout)
- [Project status](#project-status)
- [Documentation](#documentation)
- [License](#license)

## What is included

- `quickstart/swarm/` — the deployment: the Swarm stack, the Caddy and Vector
  configuration, the ClickHouse schema, setup and backup scripts, `deploy.sh`
  and an end-to-end test
- `mcp-server/` — the Rawbbit MCP server (Go): read-only tools for event
  discovery, sampling, guarded SQL, DAU and funnels over ClickHouse, for AI
  agents and MCP clients such as Claude, Codex, OpenCode and OpenClaw
- `sdk/rawbbit-unity/` — copy-in Unity C# SDK source with a durable queue,
  consent handling, a game-adapter template and Editor/PlayMode tests
- `sdk/rawbbit-browser-game/` — browser-game JavaScript SDK source with a
  durable queue, consent integration guide, tests and a game-adapter template
- `plugins/rawbbit-game-events/` — coding-agent plugins for Claude and Codex
  that inspect a Unity or browser-JavaScript game, propose a reviewable event
  map and integrate approved events with the bundled SDKs. See
  [`docs/agent-plugins.md`](docs/agent-plugins.md)
- `tools/loadgen/` — `rawbbit-loadgen`, a synthetic event generator that also
  checks that ClickHouse holds every accepted event exactly once

## Architecture

- **Caddy** terminates TLS, answers CORS preflights for browser games, limits
  the request size (413) and passes the client address to Vector.
- **Vector** checks the `X-API-Key` (each key may send one `app_id`), splits
  each batch, validates every event, hashes the client IP, optionally adds the
  country from a GeoIP database and inserts rows into ClickHouse in batches.
  A request is answered `200` only after ClickHouse stored its events. Invalid
  events are dropped and logged with the reason; the rest of the batch is kept.
- **ClickHouse** holds `analytics.events`, a `ReplacingMergeTree` keyed by
  `(app_id, event_id)`, so SDK and Vector retries collapse into one row. A
  daily native `BACKUP` keeps local copies and can also write to S3.
- **Datasets** (optional) route chosen `app_id`s to their own database
  `dataset_<id>.events` with their own read-only MCP endpoint.
- **MCP server and Metabase** read from ClickHouse.

See [`docs/architecture.md`](docs/architecture.md).

## Quickstart

Follow [`quickstart/swarm/README.md`](quickstart/swarm/README.md). In short:

```bash
cd quickstart/swarm
cp .env.example .env              # replace every change_me value
sudo ./bootstrap-data-node.sh     # on the data node
docker node update --label-add rawbbit.data=true <data-node>
./deploy.sh
```

Then point a game SDK at `https://<COLLECTOR_PUBLIC_HOSTNAME>` or send a
test batch with [`rawbbit-loadgen`](tools/loadgen/README.md).

## Relationship to Rawbbit

Gawbbit started from the Rawbbit repository and keeps its history. Rawbbit's
pipeline was designed for teams that want a warehouse-neutral raw layer:

```text
Rawbbit:  collector -> NATS JetStream -> raw-writer -> Parquet on S3/SeaweedFS
            -> hourly ingestion runner -> ClickHouse (or BigQuery)
Gawbbit:  Caddy -> Vector -> ClickHouse
```

Gawbbit drops the Parquet raw layer and the BigQuery/SQLMesh path and makes
ClickHouse the source of truth. That removes the collector, NATS, the
raw-writer, SeaweedFS, the hourly runner and its cron, and the two-VM
Compose/Ansible guides. Events reach ClickHouse in about a second instead of
up to an hour. What remains is configuration plus the MCP server, the SDKs,
the plugins and the load generator.

Differences a client can see:

| | Rawbbit collector | Gawbbit (Caddy + Vector) |
|---|---|---|
| Success response | `200 {"request_id", "accepted_events"}` | `200` with an empty body, after ClickHouse stored the events |
| Missing or invalid API key | 401 | 401 |
| `app_id` not allowed for the key | 403 for the whole request | the event is dropped and logged |
| Invalid JSON | 422 | 400 |
| Invalid event (missing field, bad timestamp) | 422 for the whole request | the event is dropped and logged |
| Empty `events` | 400 | 200 |
| Too large | 413 (bytes or events count) | 413 (bytes, `MAX_REQUEST_BYTES`) |

Both SDKs accept the empty `200` as an acknowledgment and still check
`accepted_events` when a body reports it, so they work with either backend.

The last state with the Go collector, NATS, raw-writer and runner is the git
tag `go-collector-final`.

## Published image

The [CI workflow](.github/workflows/ci.yml) builds the MCP server image for
`linux/amd64` and `linux/arm64` and pushes it to the GitHub Container
Registry of the repository owner when a release tag (`v*`) is pushed, after
every test job passed: `ghcr.io/<owner>/rawbbit-mcp-server`. A `v1.2.3` tag
publishes `1.2.3`, `1.2` and `latest`; pin a version in deployments. Ordinary
pushes only run the tests. To release:

```bash
git tag v1.0.0 && git push gawbbit v1.0.0
```

Packages of a private repository are private: log in on every node
(`docker login ghcr.io` with a token that has `read:packages`); `deploy.sh`
deploys with `--with-registry-auth`. Caddy, Vector, ClickHouse, Postgres and
Metabase use their upstream images.

To build the image yourself:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t <registry>/rawbbit-mcp-server:<tag> --push mcp-server
```

## Testing and CI

Three levels, all run by [`ci.yml`](.github/workflows/ci.yml) on every push
and pull request:

| Level | What | Run locally |
|---|---|---|
| Unit | `go test -race ./...`, gofmt, vet and amd64/arm64 cross-compile for the MCP server, the plugins and the load generator; the plugin SDK bundle check; the browser SDK's `npm test` | `cd <module> && go test ./...` |
| Integration | MCP server parity cases against a disposable ClickHouse | [`scripts/test-integration.sh`](scripts/test-integration.sh) |
| End to end | deploys the Swarm stack on a single-node swarm; [`rawbbit-loadgen`](tools/loadgen/README.md) sends events, rejected requests, dropped events and duplicates through Caddy and two Vector replicas, then checks through MCP that ClickHouse holds every accepted event exactly once; also checks a dataset and its own MCP endpoint, CORS, backups and MCP auth | [`quickstart/swarm/e2e.sh --build`](quickstart/swarm/e2e.sh) |

The image job runs only for a `v*` tag, and only when all three pass.

## Repository layout

```text
quickstart/swarm/  The deployment (stack, Caddy, Vector, ClickHouse, scripts, e2e)
mcp-server/        Rawbbit MCP server (Go)
sdk/rawbbit-unity/ Copy-in Unity SDK source, integration guide and tests
sdk/rawbbit-browser-game/ Browser-game JavaScript SDK source and integration guide
plugins/rawbbit-game-events/
  claude/          Claude plugin (skills only) with bundled SDKs
  codex/           Codex plugin with bundled SDKs and optional SDK catalog
tools/loadgen/     Synthetic event load generator and end-to-end verifier
scripts/           Integration test runner
docs/              Documentation
```

## Project status

- Event intake, validation, deduplication, dataset routing, backups and the
  MCP/Metabase access layer are implemented and covered by the end-to-end test.
- The copy-in Unity SDK is a source implementation; Unity Editor compilation,
  PlayMode tests and Android/iOS player/device validation are not yet
  complete, so production compatibility is not established.
- The browser-game SDK builds independently of other SDK folders and has a
  passing Playwright run for Chromium, Firefox and WebKit; no exact
  browser/version support matrix or real Safari/mobile-device validation is
  established. The npm package remains private and unpublished.
- The Rawbbit Game Event Tracking plugins are downloadable from the
  [Rawbbit GitHub releases](https://github.com/mirlan-irokez/rawbbit/releases);
  Gawbbit has not published plugin releases. Install them from the release
  ZIPs as described in [`docs/agent-plugins.md`](docs/agent-plugins.md).

## Documentation

- [`quickstart/swarm/README.md`](quickstart/swarm/README.md) — deployment, datasets, backups and operations
- [`docs/architecture.md`](docs/architecture.md)
- [`docs/configuration.md`](docs/configuration.md)
- [`mcp-server/README.md`](mcp-server/README.md) — includes Codex, OpenCode and OpenClaw MCP client examples
- [`tools/loadgen/README.md`](tools/loadgen/README.md)
- [`sdk/rawbbit-unity/README.md`](sdk/rawbbit-unity/README.md) — Unity SDK installation, behavior and validation status
- [`sdk/rawbbit-unity/INTEGRATION.md`](sdk/rawbbit-unity/INTEGRATION.md) — game-adapter and consent integration
- [`sdk/rawbbit-browser-game/README.md`](sdk/rawbbit-browser-game/README.md) — browser-game SDK installation and API overview
- [`sdk/rawbbit-browser-game/INTEGRATION.md`](sdk/rawbbit-browser-game/INTEGRATION.md) — host consent, storage, delivery and recovery integration
- [`docs/browser-game-sdk.md`](docs/browser-game-sdk.md) — browser-game SDK local installation, integration, privacy, limits and validation status
- [`docs/agent-plugins.md`](docs/agent-plugins.md) — download and install the Claude and Codex game-events plugins

## License

This project is released under the Apache License 2.0, like Rawbbit. See [`LICENSE`](LICENSE).
