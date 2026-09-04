# Blackout Change v0 acceptance: Flash Allotment

Date: 2026-08-28  
Workspace: `/Users/ace/projects/canter`  
System: `flash-allotment`  
Change: `change-ffc1ed31b85e05a3`

## Result

Pass, with lifecycle friction recorded below. Canter was sufficient to declare one `c1` host, provision a private PostgreSQL capability, expose one public HTTP service, publish v1, and govern the complete v2 mutation as one Change. The 105.465-second concurrent workload crossed the Change with 1,731 measured requests and no transport or server failures. The final public database proof reported 119 active claims, capacity 120, a matching inventory counter, no oversell, and no user with duplicate active claims. Both compute and the network policy were deleted through Canter; the final `canter probe` reported `resources=0`.

The abstraction was sufficient for the application lifecycle. Its rough edges were discoverability of the generic binding name, an initial network-policy timeout/delayed reachability, and teardown retry behavior when the provider completed deletion after Canter's client timed out.

## Application and invariant design

The application reads only `CANTER_SERVICE_DATABASE_URL`; it does not install, launch, address, or credential PostgreSQL. Canter owns database provisioning and injects the discovered private service binding. Application-owned tables are initialized on startup.

PostgreSQL, rather than an in-process lock, enforces both business invariants:

- `allotment_inventory` contains a singleton fixed at capacity 120. `claimed` has `CHECK (claimed >= 0 AND claimed <= capacity)`. Claim allocation uses one conditional atomic `UPDATE ... SET claimed=claimed+1 WHERE claimed < capacity` in the same transaction as claim insertion.
- `CREATE UNIQUE INDEX ... ON claims(user_id) WHERE active` prevents two active claims for one user. Cancellation deactivates the claim and decrements the counter in one transaction.
- `/proof` and `/change-proof` query live database counts, duplicate groups, counter agreement, and v2 schema metadata. They do not report cached application state.

The v2 Change added nullable `claims.confirmed_at`, added a partial confirmation index, set `ENABLE_CONFIRMATIONS=true`, staged a new artifact, and required a database-backed `/change-proof` response containing `"confirmationsEnabled":true`.

## Files

- `examples/blackout-flash-allotment/system.yaml`: System contract.
- `examples/blackout-flash-allotment/app/main.go`: Go HTTP application.
- `examples/blackout-flash-allotment/migrations/confirmations-v2.sql`: expand-only v2 migration.
- `examples/blackout-flash-allotment/traffic.py`: concurrent mixed traffic generator.
- `tmp/blackout-flash-allotment/flash-v1.tar.gz`: v1 artifact, SHA-256 `c5301800368ca166575ac376cbf1968dfdaa2cb8ba9e7a02d6957b6514acaba1`.
- `tmp/blackout-flash-allotment/flash-v2.tar.gz`: v2 artifact, SHA-256 `f33b8a0a4785fddbab3ea310b32af7c42cc4a35c15779d1d766e81a88febdc1d`.
- `tmp/blackout-flash-allotment/traffic-results.json`: raw aggregate workload result, SHA-256 `7fc4dc1b2e86995e154a35db061183c24ce04c33f1756988b9b5e1a18d6a5de7`.

## Exact acceptance commands and observed results

### Contract and artifacts

```sh
./bin/canter compile --file examples/blackout-flash-allotment/system.yaml
```

Result: exit 0. The graph contained exactly `compute/host-1` class `c1`, `service/database-1` kind `database.postgres` with `networking=private`, and `service/web-1` kind `service.http` with `networking=public`. Capacity was host 1024 MiB, reserve 384 MiB, guests 576 MiB, unallocated 64 MiB.

```sh
go test ./examples/blackout-flash-allotment/app
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tmp/blackout-flash-allotment/canter-node-linux-amd64 ./cmd/canter-node
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tmp/blackout-flash-allotment/app-v1 -ldflags '-X main.buildVersion=v1' ./examples/blackout-flash-allotment/app
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tmp/blackout-flash-allotment/app-v2 -ldflags '-X main.buildVersion=v2' ./examples/blackout-flash-allotment/app
```

Result: all exit 0. The app package reported `[no test files]`; both Linux application binaries and the Canter node binary built successfully.

An initial authoring attempt used `canter.io/v1alpha1` and `pgx.PgError`; compile correctly rejected the API version with `system requires apiVersion canter.dev/v1alpha1 and kind System`, and Go rejected the nonexistent type. The contract and import were corrected before any resource creation.

### Preflight, host, and v1

```sh
./bin/canter probe --json
```

Result before creation: compute healthy, `servers: 0`; m1 healthy.

```sh
./bin/canter host bootstrap \
  --file examples/blackout-flash-allotment/system.yaml \
  --node tmp/blackout-flash-allotment/canter-node-linux-amd64 \
  --timeout 5m
```

Result: exit 1 after about 127 seconds while exposing the already-booted host:

```text
canter: Put "https://os-api.hostvds.com/eu-west2/network/v2.0/ports/c8441add-7115-4095-85f5-07f8f11b0bf5": context deadline exceeded (Client.Timeout exceeded while awaiting headers)
```

Supported inspection showed that the host itself was ready and database provisioning had succeeded:

```sh
./bin/canter host status --file examples/blackout-flash-allotment/system.yaml
./bin/canter release status --file examples/blackout-flash-allotment/system.yaml
```

Result: one ACTIVE resource `45f5fd45-bb1c-4d3d-92f4-b8d36b941330` at `31.59.105.168`; release phase `waiting`; private `database/postgres` phase `ready`, endpoint `127.0.0.1:5432`.

```sh
./bin/canter host expose --file examples/blackout-flash-allotment/system.yaml
```

Result: exit 0; Canter attached network policy `09616c6d-6e5d-43fa-a41a-be8adef271b6`, TCP/8080, rule `9f115000-8d66-4d1e-bf75-88f403eeb631`.

```sh
./bin/canter release publish \
  --file examples/blackout-flash-allotment/system.yaml \
  --artifact tmp/blackout-flash-allotment/flash-v1.tar.gz \
  --command ./app --health /health --port 8080
```

Result: exit 0; version `c5301800368c`, exact artifact SHA-256 `c5301800368ca166575ac376cbf1968dfdaa2cb8ba9e7a02d6957b6514acaba1`. `canter release status` then reported phase `running`, healthy true, PID 3191, internal port 18080, and PostgreSQL ready.

The public endpoint initially timed out, then became reachable about one minute after exposure. Repeated direct checks returned HTTP 200:

```sh
curl --noproxy '*' -fsS http://31.59.105.168:8080/health
```

```json
{"ok":true,"version":"v1"}
```

Baseline public flow:

```sh
curl --noproxy '*' -fsS http://31.59.105.168:8080/availability
curl --noproxy '*' -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"id":"baseline-neighbor"}' http://31.59.105.168:8080/accounts
curl --noproxy '*' -fsS -X POST http://31.59.105.168:8080/claims/baseline-neighbor
curl --noproxy '*' -fsS http://31.59.105.168:8080/proof
```

Observed in order:

```json
{"available":120,"capacity":120,"claimed":0}
{"id":"baseline-neighbor"}
{"active":true,"claimId":1,"user":"baseline-neighbor"}
{"activeClaims":1,"capacity":120,"confirmationColumn":false,"confirmationIndex":false,"confirmationsEnabled":false,"confirmedActiveClaims":0,"counterMatches":true,"duplicateActiveUsers":0,"inventoryCounter":1,"ok":true,"oversold":false,"version":"v1"}
```

### Timed traffic and governed v2 Change

```sh
python3 examples/blackout-flash-allotment/traffic.py \
  --base http://31.59.105.168:8080 \
  --duration 105 --workers 8 \
  --output tmp/blackout-flash-allotment/traffic-results.json
```

The generator first created 240 neighbor accounts concurrently, then measured 105 seconds of eight-worker traffic. The measured window was 2026-08-28T15:50:34Z through 2026-08-28T15:52:19Z. It mixed reads, claim attempts, cancellations, confirmation attempts, proof reads, and account retries. The Change was drafted at 15:50:43Z, applied from 15:50:55Z, committed at 15:51:04Z, and traffic continued for roughly 75 seconds after commit.

While that command remained active:

```sh
./bin/canter change draft \
  --file examples/blackout-flash-allotment/system.yaml \
  --artifact tmp/blackout-flash-allotment/flash-v2.tar.gz \
  --command ./app --health /health --port 8080 \
  --summary 'Enable database-backed allotment confirmations' \
  --migration examples/blackout-flash-allotment/migrations/confirmations-v2.sql \
  --migration-id confirmations-v2 --database database \
  --env ENABLE_CONFIRMATIONS=true \
  --verify /change-proof --contains '"confirmationsEnabled":true'
```

Result: exit 0. Change `change-ffc1ed31b85e05a3` was drafted on base `c5301800368c` with digest `df4ead09ae4f98a36a866f23b817e0f1ab3ab7df7ab6ef299535246c10f2c9bf`. Its plan contained the new artifact, environment flag, migration `confirmations-v2` classified `expand-only`, and the database-backed HTTP verification.

Honest exact-digest safety test:

```sh
./bin/canter change authorize \
  --file examples/blackout-flash-allotment/system.yaml \
  --id change-ffc1ed31b85e05a3 \
  --digest df4ead09ae4f98a36a866f23b817e0f1ab3ab7df7ab6ef299535246c10f2c9be
```

Result: exit 1, `canter: authorization digest does not match the immutable change plan`. `canter change inspect` immediately afterward still reported phase `drafted` with all operations pending and zero attempts.

```sh
./bin/canter change authorize \
  --file examples/blackout-flash-allotment/system.yaml \
  --id change-ffc1ed31b85e05a3 \
  --digest df4ead09ae4f98a36a866f23b817e0f1ab3ab7df7ab6ef299535246c10f2c9bf

./bin/canter change apply \
  --file examples/blackout-flash-allotment/system.yaml \
  --id change-ffc1ed31b85e05a3 --timeout 3m
```

Result: both exit 0; final phase `committed`. All five operations completed with exactly one attempt:

1. Base release remained healthy and unchanged.
2. Expand-only migration committed.
3. Desired release was set to exact artifact `f33b8a0a4785fddbab3ea310b32af7c42cc4a35c15779d1d766e81a88febdc1d`.
4. Release `f33b8a0a4785` became healthy on PID 5059.
5. `/change-proof` returned 200 and matched the approved response contract.

A second supported apply tested terminal executor idempotency:

```sh
./bin/canter change apply \
  --file examples/blackout-flash-allotment/system.yaml \
  --id change-ffc1ed31b85e05a3 --timeout 30s
```

Result: exit 0 and returned the same committed Change. Every operation remained completed with `attempts: 1`; the migration and release were not executed twice.

The traffic result was:

```json
{
  "durationSeconds": 105.465,
  "requestCount": 1731,
  "statuses": {"200": 658, "201": 268, "404": 58, "409": 747},
  "failureCount": 0,
  "businessConflicts": 805,
  "transportErrors": 0,
  "serverErrors": 0,
  "latencyMs": {"p50": 383.05, "p95": 416.81, "p99": 518.02},
  "operationMix": {
    "account_retry": 72,
    "availability": 204,
    "cancel": 311,
    "claim": 767,
    "confirm": 239,
    "proof": 138
  }
}
```

The 805 business conflicts were kept separate from failures. They comprise expected HTTP 409 contention outcomes and 58 HTTP 404 confirmation attempts made while v1 still had confirmations disabled. Transport errors and HTTP 5xx responses were both zero.

Post-traffic public proof:

```sh
curl --noproxy '*' -fsS http://31.59.105.168:8080/change-proof
curl --noproxy '*' -fsS http://31.59.105.168:8080/availability
```

```json
{"activeClaims":119,"capacity":120,"confirmationColumn":true,"confirmationIndex":true,"confirmationsEnabled":true,"confirmedActiveClaims":39,"counterMatches":true,"duplicateActiveUsers":0,"inventoryCounter":119,"ok":true,"oversold":false,"version":"v2"}
{"available":1,"capacity":120,"claimed":119}
```

This proves from the public application that the workload did not oversell, no user held two active claims, cancellations and retries preserved counter agreement, and the v2 schema and feature flag were live.

### Mandatory teardown

```sh
./bin/canter host destroy --file examples/blackout-flash-allotment/system.yaml --yes
```

The first destroy removed the compute host and initiated network-policy deletion, but exited 1 after the provider did not answer before the client deadline:

```text
canter: delete network policy 09616c6d-6e5d-43fa-a41a-be8adef271b6: Delete "https://os-api.hostvds.com/eu-west2/network/v2.0/security-groups/09616c6d-6e5d-43fa-a41a-be8adef271b6": context deadline exceeded (Client.Timeout exceeded while awaiting headers)
```

No provider CLI or API was used. A supported Canter retry established that the policy was already absent, although Canter treated the provider's idempotent 404 as fatal:

```sh
./bin/canter host destroy --file examples/blackout-flash-allotment/system.yaml --yes
```

```text
canter: delete network policy 09616c6d-6e5d-43fa-a41a-be8adef271b6: compute request failed with HTTP 404: {"NeutronError": {"type": "SecurityGroupNotFound", "message": "Security group 09616c6d-6e5d-43fa-a41a-be8adef271b6 does not exist", "detail": ""}}
```

The same supported status path reported the compute instance absent with HTTP 404. Final required verification:

```sh
./bin/canter probe
```

```text
model   ok  994ms
compute ok  1421ms  resources=0 shapes=12 images=73 networks=48
m1      ok  157ms
total        1421ms
```

There are no live hosts and the one created network policy is absent. Canter's durable state remains marked ready rather than destroyed because deletion completed after the timeout and the retry's `SecurityGroupNotFound` is not accepted as success. This is a receipt/state bookkeeping defect, not a live-resource leak.

## Product friction and bugs

1. Generic binding discoverability: the root README says releases receive generic discovered bindings but does not name the environment variable convention. CLI help also does not expose it. I had to inspect public runtime source to discover `CANTER_SERVICE_DATABASE_URL`.
2. Group help is inconsistent: `canter change --help` is parsed as an unknown subcommand, while `canter change draft -h` works.
3. Bootstrap/exposure timeout: host bootstrap completed compute boot and private PostgreSQL readiness, but its network operation timed out. `canter host expose` was a safe supported recovery.
4. Public-policy propagation: after exposure and a healthy release, TCP/8080 initially timed out and became reachable about one minute later. Canter reported no propagation/readiness state for the public path.
5. Teardown idempotency/state: the provider completed host and policy deletion after Canter timed out. Canter then treated `SecurityGroupNotFound` as an error instead of successful idempotent absence, so it could not mark its durable state destroyed or emit a clean destroy receipt even though `canter probe` showed `resources=0` and the retry proved the policy absent.

No direct provider API/CLI, SSH, m1 access, hidden credentials, or release mutation outside `canter change` was used.
