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
