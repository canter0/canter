# Change v0 acceptance — August 28, 2026

## Boundary implemented

Change v0 makes a production mutation a durable object above releases and service drivers. A draft contains its healthy base version, content-addressed candidate release, complete environment, optional expand-only migration, application verification contract, ordered operation program, reversibility classifications, and compensation descriptions. All immutable execution fields share one SHA-256 authorization digest.

Drafting uploads and records the release but does not alter desired production state. A mismatched authorization digest is rejected. Applying requires a conditional, renewable system-wide lease in `m1`; its random fencing token is carried into runtime actions and checked by the node before a service driver can mutate state. A second Change cannot execute against the same system concurrently.

The PostgreSQL driver maintains `canter_schema_migrations`. An expand migration and its ledger insert commit in one database transaction. Redelivery of the same migration ID and digest returns a successful duplicate result without rerunning the SQL; the same ID with a different digest fails.

## Unhealthy release after migration

Change `change-04625044b2dc739e` proposed an intentionally unhealthy release plus migration `project-archiving-v1`. A wrong approval digest was rejected before the exact digest was authorized.

The base-state assertion passed, PostgreSQL committed the migration, and desired state moved to the candidate. The candidate returned HTTP 503 throughout its 30-second health window. Canter restored healthy v3 and terminated with:

- Phase: `reverted`
- Failure: candidate did not become healthy within 30 seconds
- Residual: expand-only migration `project-archiving-v1` remains applied and backward-compatible
- Compensation evidence: healthy base release `ec6b313a6393` restored

This was intentionally not described as a complete rollback because the compatible schema expansion remained.

## Executor death and durable resume

Change `change-fdb43bb810b4a660` proposed valid v4, the already-recorded migration ID, `ENABLE_ARCHIVING=true`, and a verification response containing `"ready":true`. The release used a deliberate 15-second startup delay.

The applying CLI process was killed with `SIGKILL` while operation `04-health` was `running`. At death:

- Base assertion, migration, and desired-release operations were durably `completed`.
- Migration evidence reported that the PostgreSQL idempotency ledger prevented duplicate execution.
- The node continued independently and brought v4 online.
- An immediate second executor was fenced until the abandoned lease expired.
- The public proof already returned `archiving=true`, `schemaExpanded=true`, and `version=v4`.

After lease expiry, another CLI resumed the same Change. Operations 01 through 03 remained at one attempt. Only the interrupted health observation advanced to attempt two. Verification passed and the Change became `committed`. Reapplying the committed Change did not change any attempt count or completion timestamp.

## Verification failure after traffic began

Change `change-622a24eb09867b23` deployed healthy v5 but intentionally required an impossible response body. V5 passed process health and received production traffic. Application verification then observed the real response and failed the Change. Canter rolled traffic back to v4 and recorded `reverted` with exact failure and compensation evidence.

During the v4 to v5 to v4 sequence, the stateful workload completed:

- 4,846 requests; 4,846 succeeded; 0 failed
- 64.41 requests/second overall
- Median latency 198 ms, p95 227 ms, p99 486 ms
- 2,327 feed reads, 976 post creations, 433 updates, 416 searches, 244 logins, 316 identity reads, 110 likes, and 24 signups

The final database check reported 25 users, 269 sessions, 976 posts, and 91 unique likes.

## Drift and competing Changes

A Change drafted against v4 was authorized only after production had deliberately moved to v3 outside the Change path. Its first precondition observed the mismatch, no mutating operation started, and the terminal phase was `rejected` rather than `reverted`.

Two different Changes were then authorized against v4. Change `change-123972fe8691f799` acquired the system execution lease and began a delayed v6 rollout. Applying `change-1493a826cd20aadf` concurrently was refused while every one of its operations remained pending. V6 committed and released the lease immediately. The competing Change could then acquire the boundary, observed that production had moved, and was rejected at operation 01 without mutation.

## Honest v0 limits

- Expand-only SQL is a deliberately narrow validator, not a general SQL migration planner.
- Runtime actions currently use one request slot per system and one built-in PostgreSQL implementation.
- A runtime action must finish within its currently fenced lease window; lease-aware renewal for long migrations is not implemented.
- The execution lease is serialized per system; there is no safe parallel-Change analysis yet.
- Verification v0 supports one HTTP GET response contract.
- Legacy low-level release commands remain available as an administrative escape hatch and can intentionally create drift that Change preconditions will reject.
- Application environment values are stored in release manifests; a dedicated secret-reference capability is still required before sensitive configuration belongs in Changes.
- Unexpected web-process failure still has a recovery gap with one instance.

The result is nevertheless a real transaction kernel: exact review, bounded authority, durable progress, idempotent side effects, application-level proof, explicit compensation, and terminal states that distinguish refusal, successful reversal with residue, and commitment.

The experimental host and its exact managed endpoint policy were deleted. The final provider probe reported zero compute resources; Change records, leases, action results, artifacts, receipts, and evidence remain in `m1`.
