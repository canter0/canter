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
