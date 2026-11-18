# rawbbit-deploy-dbt-runner

Fail-closed VM-two host cutover for the `dbt-runner` service (formerly
`deploy-dbt-runner.py`). It validates the staged Compose/route configuration,
installs `deploy.gate`, drains through the shared `pipeline.lock` without
signalling jobs, verifies the candidate image's deployment contract and route
revision, publishes the staged artifacts, recreates only `dbt-runner`, verifies
the activated container (`--capabilities`, `validate-config`,
`verify-control`, `dbt debug`, `dbt parse`) and writes `deploy-fence.json`.

Command-line arguments, messages, exit codes (0 success, 1 deployment or host
failure, 2 invalid arguments), Docker commands and written files are unchanged
from the Python helper:

```text
rawbbit-deploy-dbt-runner --install-dir DIR --stage-dir DIR [--runtime-dir DIR]
  --compose-file docker-compose.yml [--compose-file OVERLAY ...]
  [--drain-timeout SECONDS] [--bootstrap-stopped] [--recover-gate]
  [--pre-routing-rollback] [--remove-dataset-overlay]
  [--validate-only | --inspect-mcp-env | --check-quiescence]
```

## Build and test

```bash
cd quickstart/vm_rawbbit_two/deployer
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o rawbbit-deploy-dbt-runner ./cmd/rawbbit-deploy-dbt-runner
```

`go test` covers the ported host-transaction tests, a replay of 94 CLI cases
recorded from the Python helper against a fake `docker` executable
(`testdata/cli_cases.json`, `testdata/fake-docker.sh`; needs `/bin/sh`), and
the Ansible role contract tests. The localhost route-rendering tests run only
when `ansible-playbook` is on `PATH`.

## Go rewrite notes

- Unexpected malformed host data (for example a non-object Compose service)
  prints the generic `Invalid staged deployment or host state` message with
  exit 1, where Python could exit 1 with a traceback.
- `internal/pyjson` is a copy of `dbt_project/internal/pyjson` plus byte
  decoding (`json.loads(bytes)` encoding detection); keep both in sync.
