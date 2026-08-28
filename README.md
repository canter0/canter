# Canter

Canter is an agent-operated, human-governed hosting control plane. An agent states the desired sandbox in `canter.yaml`; Canter compiles that intent into a typed plan, validates it against policy, reconciles real `compute` and `m1` resources, and emits verifiable receipts.

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

`apply` is real: it creates compute resources. Each resource must become active and independently write a boot proof to a short-lived signed `m1` URL before Canter marks the sandbox ready. State and operation receipts remain in `m1` after teardown.

## System contracts

`sdk.System` is the higher application layer. An application declares logical services, isolation, readiness, capacity constraints, and durable state; `sdk.CompileSystem` expands that contract into a typed execution graph before the lower reconciler touches infrastructure.

```sh
./bin/canter compile --file examples/blackbox-firecracker-mysql/system.yaml
```

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

The first live Change acceptance killed its executor mid-application, rejected a bad digest, contained an unhealthy release, resumed an abandoned ledger, prevented duplicate migration execution, compensated after business verification failed under traffic, rejected stale starting state, and serialized competing Changes. See `doc/(initial steps, august 2026)/change-v0-acceptance.md`.
