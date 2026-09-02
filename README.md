# Canter

Canter is an agent-operated, human-governed hosting control plane. An agent states the desired sandbox in `canter.yaml`; Canter compiles that intent into a typed plan, validates it against policy, reconciles real `compute` and `m1` resources, and emits verifiable receipts.

The product interface lives in `web/`. It carries the same control-plane vocabulary through account creation, agent connection, System inspection, exact Change review, human authorization, and execution evidence.

```sh
cd web
pnpm install
pnpm dev
```

The model is an intent compiler, not an infrastructure driver. It never receives provider credentials or arbitrary API access. Only deterministic adapters may mutate resources.

```sh
go build -o bin/canter ./cmd/canter
./bin/canter init
./bin/canter probe
./bin/canter plan
./bin/canter apply
./bin/canter status
./bin/canter destroy --yes
```

## Connect an agent

An agent installation outlives any one conversation. `connect` opens a device
authorization, waits for the human to approve it in Canter, then exchanges the
single-use private device credential for installation-bound credentials:

A clean-room agent without this repository or CLI can instead start at the
website origin. The landing HTML advertises `/.well-known/canter` and
`/llms.txt`, which describe the same-origin device, API, and Streamable HTTP MCP
contract without exposing provider details.

```sh
./bin/canter agent connect \
  --api-url http://127.0.0.1:8081 \
  --name 'Codex on my Mac' \
  --harness codex
```

Without `--env-file`, credentials and MCP instructions are printed as JSON and
Canter writes nothing. Persistence is always explicit:

```sh
./bin/canter agent connect --env-file .canter/agent.env
./bin/canter agent bootstrap --env-file .canter/agent.env
./bin/canter agent mcp --env-file .canter/agent.env
./bin/canter agent refresh --env-file .canter/agent.env
```

The credential file is written atomically with mode `0600`, retains unrelated
values, and rejects symbolic-link targets. A fresh conversation calls
`bootstrap` to reconstruct the workspace, Systems, pending Changes, installation
authority, and current session without needing prior chat history. Run
`canter agent --help` for the split `begin`, `exchange`, `whoami`, and `refresh`
flows used by non-interactive harnesses.

The complete durable identity, governed first-deployment, scoped node-gateway,
and blackout acceptance contract is documented in
`doc/(initial steps, august 2026)/durable-agent-control-plane.md`.

`apply` is real: it creates compute resources. Each resource must become active and independently write a boot proof to a short-lived signed `m1` URL before Canter marks the sandbox ready. State and operation receipts remain in `m1` after teardown.

## System contracts

`sdk.System` is the higher application layer. An application declares logical services, isolation, readiness, capacity constraints, and durable state; `sdk.CompileSystem` expands that contract into a typed execution graph before the lower reconciler touches infrastructure.

```sh
./bin/canter compile --file examples/blackbox-firecracker-mysql/system.yaml
```

`compile` makes private capability consumption explicit. A PostgreSQL service
named `database`, for example, is delivered to dependent applications as
`CANTER_SERVICE_DATABASE_URL`; application code never needs provider addresses
or database provisioning logic.

The Firecracker/MySQL example uses the SDK to express one logical two-instance MySQL service. Its acceptance contract compiles to one 1 GiB `c1` host, a Firecracker runtime, two 250 MiB microVMs, two MySQL readiness invariants, and one `m1` namespace. See `examples/blackbox-firecracker-mysql/README.md` for the generated application and the recorded live capability result.

The reconciled HTTP example exercises the complete versioned lifecycle: content-addressed artifacts, a host-independent desired release, node-reported observed state, health-gated updates, public proxying, process recovery, failed-release containment, rollback, and managed endpoint policy cleanup. See `examples/reconciled-http-app/README.md`.

The stateful board example adds a private Postgres capability behind the same abstraction. A compiled runtime plan is reconciled by replaceable service drivers, releases receive generic discovered bindings, and the node drains old backends during rolling replacement. Its live acceptance run sustained mixed authenticated traffic across database-backed deployment and restart transitions. See `examples/stateful-board/README.md`.

## Production Changes

`canter change` is the governed mutation layer above releases and runtime services. Drafting stages an immutable artifact without changing desired state, binds configuration, an optional expand-only migration, ordered operations, compensation semantics, and application verification into one digest. Authorization applies to that exact digest. Execution uses a system-wide conditional lease, a durable operation ledger, fenced node actions, idempotent database migrations, and explicit `committed`, `rejected`, `reverted`, or `escalated` outcomes.

```sh
./bin/canter change draft --file system.yaml --artifact release.tar.gz \
  --summary 'Enable project archiving' \
  --migration migrations/project-archiving-v1.sql \
  --migration-id project-archiving-v1 \
  --env ENABLE_ARCHIVING=true \
  --verify /api/change-proof --contains '"ready":true'

./bin/canter change authorize --file system.yaml --id CHANGE_ID --digest EXACT_DIGEST
./bin/canter change apply --file system.yaml --id CHANGE_ID
./bin/canter change inspect --file system.yaml --id CHANGE_ID
```

Agents do not need to assemble that flag sequence. The same boundary has a
strict declarative request understood by the Go SDK and CLI:

```sh
./bin/canter change init --file change.yaml
./bin/canter change schema > change-request.schema.json
./bin/canter change validate --file system.yaml --request change.yaml
./bin/canter change draft --file system.yaml --request change.yaml
```

The request can contain an immutable release, configuration, one expand-only
migration, and an application verification contract. Unknown fields are
rejected, the request is bound to one named System, and it cannot express
provider calls or credentials. `sdk.ChangeRequest` is the transport-neutral
type future HTTP, MCP, and WebMCP adapters should use.

`canter inspect --file system.yaml` returns the shared semantic read model:
declared intent, compiled graph, exact service bindings, observed host and
release state, and public-endpoint reachability. `canter release status` reports
internal release health separately from public reachability, while
`canter release wait` waits for the latter.

This gives every interface the same small vocabulary:

```text
inspect system -> draft Change -> inspect exact digest -> authorize -> apply -> inspect evidence
```

The website should expose these same objects and operations. WebMCP is an
adapter into this vocabulary, not a privileged infrastructure interface.

The first live Change acceptance killed its executor mid-application, rejected a bad digest, contained an unhealthy release, resumed an abandoned ledger, prevented duplicate migration execution, compensated after business verification failed under traffic, rejected stale starting state, and serialized competing Changes. See `doc/(initial steps, august 2026)/change-v0-acceptance.md`.
