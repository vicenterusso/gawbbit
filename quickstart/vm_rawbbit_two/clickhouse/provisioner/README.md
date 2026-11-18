# rawbbit-provision-datasets

Privileged ClickHouse dataset provisioner for VM two. It replaces
`clickhouse/provision_datasets.py` with a static Go binary and keeps the same
command line, files, output, exit codes and SQL.

It validates the protected provisioning registry and the staged MCP runtime
registry, preflights ownership and main-user isolation, then idempotently
creates or reconciles each managed dataset database, canonical `events` table,
read-only role and MCP user. Every mutation is recorded in the ownership
journal before it is sent to ClickHouse so an interrupted run fails closed.
Admin credentials are only sent to the loopback ClickHouse HTTP endpoint.

## Usage

```text
rawbbit-provision-datasets --config CONFIG --runtime-config RUNTIME_CONFIG
                           --schema SCHEMA --journal JOURNAL [--preflight-only]
```

| Argument | Meaning |
| --- | --- |
| `--config` | root-owned, mode `0600` provisioning registry (`datasets-provision.json`) |
| `--runtime-config` | mode `0600` staged MCP runtime registry (`datasets-runtime.json.stage`) |
| `--schema` | canonical `analytics.events` DDL (`schema_analytics_events.sql`) |
| `--journal` | ownership journal (`datasets-ownership.json`), created if missing |
| `--preflight-only` | validate and verify without changing ClickHouse or the journal |

Output and exit codes:

| Result | stdout / stderr | exit |
| --- | --- | --- |
| preflight passed | `dataset provisioner: preflight passed` | 0 |
| changes applied | `dataset provisioner: configuration reconciled` | 0 |
| nothing to do | `dataset provisioner: configuration unchanged` | 0 |
| validation, ownership or ClickHouse error | `dataset provisioner: <reason>` on stderr | 1 |
| usage error | argparse-style usage and error on stderr | 2 |

The binary must run as root; the config and journal files and all of their
parent directories must be root-owned and not group/world writable. Long
options may be abbreviated (`--runtime`, `--pre`) exactly like argparse.

## Build

```bash
cd quickstart/vm_rawbbit_two/clickhouse/provisioner
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" \
  -o rawbbit-provision-datasets ./cmd/rawbbit-provision-datasets
# OCI Ampere / other arm64 hosts: GOARCH=arm64
```

## Test

```bash
go test ./...      # ported unit tests, argparse goldens, no services needed
go vet ./...

# Differential parity suite (30 scenarios, 161 runs) against a real ClickHouse
# whose admin user has access management; it resets team_* databases and
# rawbbit*/ref_* users and roles:
PROVISION_PARITY_CLICKHOUSE=http://admin:password@127.0.0.1:8123 \
  go test -tags integration ./cmd/rawbbit-provision-datasets
```

The parity suite compares, for every step, the exit code, stdout, stderr, the
ownership journal bytes, every SQL statement issued (from
`system.query_log`, including the restricted-credential probes) and the final
users, roles, grants, role grants and settings profiles with goldens recorded
from the original Python implementation
(`cmd/rawbbit-provision-datasets/testdata/parity/golden`). UUIDs are
normalized by first appearance.

## Go rewrite notes

- The journal is still written with `json.dumps(sort_keys=True,
  separators=(",", ":"))` semantics (ASCII escapes, trailing newline) via a
  temporary `.<name>.XXXXXXXX` file, `fsync`, atomic rename and directory
  `fsync`.
- Reference datasets granted several roles were checked in Python set order
  (randomized per process); the Go binary checks them in sorted order.
- Unexpected internal failures that Python reported as a traceback are
  reported as `dataset provisioner: unexpected failure: ...` with exit code 1.
- Help output wraps at `COLUMNS` (default 80) like argparse but does not query
  the terminal size.
