# canter

Hosting, operated by your agent.

Canter is an agent-operated, human-governed hosting control plane. Agents
prepare exact production changes; people or standing policies authorize them;
Canter executes them through deterministic adapters and records the result.

![Canter](web/public/og.png)

## What works

- Typed `canter.yaml` and `System` contracts compiled into deterministic plans
- Reconciliation for compute, object storage, releases, endpoints, and Postgres
- Health-gated deploys, rolling replacement, restart, rollback, and teardown
- Immutable Changes with exact-digest approval, leases, fencing, compensation,
  and verification evidence
- Durable agent installations with device authorization, scoped credentials,
  bootstrap context, HTTP API, and Streamable HTTP MCP
- A web interface for connecting agents, inspecting Systems, and reviewing
  Changes before they run

## Requirements

- Go 1.26
- Node.js 22 and pnpm 11 for the web interface
- PostgreSQL 17 for the control plane
- Supported compute and S3-compatible storage credentials for real deployments

## Quick start

Build the CLI and compile an example System without touching infrastructure:

```sh
go build -o bin/canter ./cmd/canter
./bin/canter compile --file examples/reconciled-http-app/system.yaml
```

Run the test suite:

```sh
go test ./...
```

Run the web interface locally:

```sh
cd web
pnpm install --frozen-lockfile
pnpm dev
```

The control plane requires PostgreSQL and the provider configuration described
in [`.env.example`](.env.example):

```sh
cp .env.example .env
go run ./cmd/canter-controlplane
```

## How it works

Canter keeps intelligence and infrastructure authority separate:

```text
agent intent
    -> typed System or Change
    -> deterministic validation and planning
    -> human or policy authorization
    -> fenced execution
    -> observed state and evidence
```

The model may prepare intent, but it never receives provider credentials or
arbitrary infrastructure access. Only typed adapters can mutate resources, and
authorization applies to the exact plan that was reviewed.

An agent connects through a device flow and receives installation-bound,
short-lived credentials:

```sh
./bin/canter agent connect \
  --api-url http://127.0.0.1:8081 \
  --name "Codex on my Mac" \
  --harness codex
```

`canter agent bootstrap` reconstructs the installation's current workspace,
Systems, pending Changes, and authority without relying on chat history.

## Project layout

| Path | Purpose |
| --- | --- |
| `cmd/canter` | CLI |
| `cmd/canter-controlplane` | HTTP, MCP, and execution service |
| `cmd/canter-node` | Workload-node runtime |
| `sdk` | Contracts, compiler, reconciler, and remote client |
| `internal/controlplane` | Durable identity, policy, and Change state |
| `examples` | Runnable contracts and acceptance workloads |
| `web` | Next.js interface |
| `deploy` | Reference production deployment |

## Status

Canter is early-stage software. Its core reconciliation and governed Change
paths are implemented and covered by unit and integration tests, but it is not
yet a general-purpose replacement for an established production platform.
Provider operations are real and may create billable infrastructure.

## License

[GNU Affero General Public License v3.0 only](LICENSE)
