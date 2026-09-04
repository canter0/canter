# Agent contract v1 acceptance — August 30, 2026

Network identifiers in this historical record have been replaced with
documentation-safe placeholders. Execution timing, state, and verification
results are unchanged.

## Result

Pass for the engine boundary. This run corrected the lifecycle defects found by
the prior blackout agent and exercised a strict declarative Change against a
fresh real host, private PostgreSQL, a public application, concurrent traffic,
and repeatable teardown. It is not evidence of outside-user demand and does not
replace the transcript's proposed twenty-Change, three-project falsification
program.

The accepted semantic path is:

```text
inspect System
→ submit typed Change request
→ inspect exact plan and digest
→ authorize exact digest
→ apply deterministic operation ledger
→ inspect evidence and public truth
```

The same SDK types are intended to back the CLI, an HTTP API, remote MCP, and
WebMCP. No interface receives provider operations or provider credentials.

## Boundary changes

- `sdk.ChangeRequest` is a strict YAML/JSON production request. Unknown fields,
  invalid names, cross-System targeting, arbitrary verification methods, and
  malformed release or migration declarations are rejected.
- `canter change init`, `change validate`, and `change draft --request` expose
  that object without requiring agents to reconstruct a long flag sequence.
- `canter change schema` emits the same closed JSON Schema for API, MCP, and
  WebMCP tool-definition adapters.
- `canter inspect` returns declared intent, the deterministic execution graph,
  exact service bindings, observed host state, release state, and independently
  observed public reachability.
- Managed service bindings are now part of the compiled and observed contract.
  The `database` capability explicitly publishes
  `CANTER_SERVICE_DATABASE_URL` to its `web` consumer.
- `release status` distinguishes internal runtime health from public endpoint
  reachability. `release wait` returns only after a direct public health request
  succeeds and records that evidence in `m1`.
- Provider HTTP failures retain typed status codes. Deleting an already-absent
  network policy is successful, while conflict retries remain bounded.
- Destruction persists phase `destroying` and checkpoints each deleted compute
  resource and network policy. An interrupted attempt can resume without
  redoing completed side effects. Terminal destruction is idempotent.
- Group help no longer loads credentials; `canter change --help`, `host --help`,
  and `release --help` work outside a configured workspace.

## Stale-state regression

The previous blackout run had physically deleted its compute host and network
policy, but a provider timeout followed by a `SecurityGroupNotFound` response
left Canter's state marked `ready`.

The revised binary ran the normal supported command:

```sh
./bin/canter host destroy \
  --file examples/blackout-flash-allotment/system.yaml --yes
```

It accepted both resources' confirmed absence, persisted phase `destroyed`, and
returned the ordinary destruction result. `host status` returned the same
terminal state and `canter probe --json` reported zero compute servers. No
provider API, provider CLI, SSH, or direct `m1` operation was used.

## Fresh live lifecycle

Before live mutation, the revised compiler was also run against all four
existing application contracts: the two-instance Firecracker/MySQL topology,
the stateless reconciled HTTP service, the stateful board, and Flash Allotment.
All compiled successfully with their original capacity and invariant counts;
managed database bindings appeared for MySQL and PostgreSQL without adding
provider concepts to any contract. This is local compatibility coverage, not
four-project production acceptance.

A current Linux node and two application artifacts were rebuilt. Their hashes
were:

- v1: `27be455c235cba8f9cc62973be9759a3461c91504a52af02e16d68c0add74574`
- v2: `5b3e4a54444f8dc98cd244ad09c652db7d4fed6f67712df6680f906860bfab17`

`host bootstrap` created one real `c1` resource, waited for its independent boot
proof, reconciled private PostgreSQL, and attached the managed TCP/8080 policy.
It completed in 88.127 seconds with resource
`c388e905-ef8a-4a2b-b8b0-ec6785ccc4fa` at `203.0.113.10`.

Immediately after publishing v1, `release status` reported the release as not
internally healthy and public reachability as `waiting`; it did not present the
managed endpoint as ready merely because the policy existed. `release wait`
then returned a combined observation only after:

```json
{
  "version": "27be455c235c",
  "phase": "ready",
  "url": "http://203.0.113.10:8080/health",
  "statusCode": 200,
  "message": "public endpoint is reachable"
}
```

The running service observation included the capability binding:

```json
{
  "name": "database",
  "binding": "CANTER_SERVICE_DATABASE_URL",
  "kind": "database",
  "engine": "postgres",
  "phase": "ready"
}
```

## Declarative Change

The exact request is
`examples/blackout-flash-allotment/change.yaml`. Local validation compiled it
to a release, expand-only migration, and application verification without
accessing credentials.

The first live draft attempt encountered a genuine transient R2 `PutObject`
transport failure after the SDK's bounded retries. It stopped before creating a
Change or altering desired production state. Repeating the same immutable
request succeeded in 1.79 seconds. This is recorded as real provider behavior,
not counted as a successful first attempt.

Change `change-b37d651d6f9d755c` bound the reviewed plan to digest
`bfcdfeaa21e0323a04667628bdf8f2ca3acf0b9480fbaab7b0f8a76eba354374`.
Authorization and application committed all five operations with exactly one
attempt each:

1. Assert the healthy base release remained `27be455c235c`.
2. Commit expand-only PostgreSQL migration `confirmations-v2`.
3. Select exact artifact SHA-256 `5b3e4a54444f...`.
4. Observe release `5b3e4a54444f` healthy.
5. Verify `/change-proof` returned HTTP 200 and matched the approved response.

Reapplying the terminal Change returned the same committed object. Every
operation remained at `attempts: 1`.

## Concurrent public traffic and invariant proof

Eight workers sustained ordinary reads, claims, cancellations, account retries,
confirmation attempts, and database proofs across the Change:

```json
{
  "durationSeconds": 60.477,
  "requestCount": 957,
  "failureCount": 0,
  "transportErrors": 0,
  "serverErrors": 0,
  "businessConflicts": 398,
  "latencyMs": {"p50": 392.94, "p95": 472.83, "p99": 504.32}
}
```

The raw ignored traffic aggregate is
`tmp/blackout-flash-allotment/agent-contract-traffic.json`, SHA-256
`35ba7a7f6c3ee03e2bfa509a56d3c55a857185467c0ee25c9cd004a8efd71336`.
Expected application-level contention was separated from transport and server
failure.

The final public PostgreSQL-backed proof was:

```json
{
  "activeClaims": 120,
  "capacity": 120,
  "confirmationColumn": true,
  "confirmationIndex": true,
  "confirmationsEnabled": true,
  "confirmedActiveClaims": 35,
  "counterMatches": true,
  "duplicateActiveUsers": 0,
  "inventoryCounter": 120,
  "ok": true,
  "oversold": false,
  "version": "v2"
}
```

## Repeatable teardown

The first teardown removed the fresh compute resource and managed network
policy and persisted `destroyed`. An immediate second identical command returned
the same state and original `destroyedAt`; it did not contact missing resources
as though they were still live. `host status` agreed, and the final provider
probe reported `servers: 0`.

## Honest remaining boundary

- This run validates one real System and builds on the prior no-context blackout
  agent run. It is not yet the outside-user, three-project program prescribed by
  the transcript.
- There is no authenticated remote control-plane API in this repository yet.
  The website should adapt these SDK objects rather than shell out to the CLI.
- Sensitive configuration still needs a dedicated secret-reference capability;
  environment values are currently embedded in reviewed release manifests.
- Change v1 still supports one HTTP verification, one built-in PostgreSQL
  driver, expand-only SQL, and serialized per-System execution.
- The transient artifact-upload failure is safely retryable but is not yet
  classified for clients with a structured retry hint.

Those are the next constraints to test; they do not invalidate the engine
boundary proven here.
