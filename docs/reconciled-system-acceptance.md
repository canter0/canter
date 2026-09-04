# Reconciled application acceptance — 2026-08-28

This experiment separated application lifecycle from compute lifecycle. One `c1` host received only a content-verified `canter-node` binary through cloud-init. The node started with no desired release and reported `waiting` to `m1`; no application code was embedded in host bootstrap.

The `reconciled-http` System contract compiled into one compute host, one process runtime, one process instance, one HTTP service, one `m1` namespace, a 128 MiB process memory invariant, and an HTTP readiness invariant.

## Measured lifecycle

| Operation | Result |
| --- | --- |
| Cold host and node bootstrap | 77.015 seconds |
| Publish v1 artifact to healthy | 4.278 seconds |
| Update v1 to v2 on the same host | 4.143 seconds |
| Forced process restart to healthy v2 | 2.790 seconds |
| Roll back v2 to retained v1 | 1.793 seconds |

The public endpoint returned the declared application version after v1, v2, recovery, and rollback. The compute resource ID remained unchanged across releases.

An intentionally unhealthy candidate listened but returned HTTP 503 from its health endpoint. During its full 30-second health window and after rejection, the public endpoint continued serving healthy v1. Observed state recorded `release-failed`, the desired failed version, the still-running v1 version and PID, and `candidate did not become healthy within 30s`. Returning desired state to v1 cleared the failed phase without replacing the healthy process.

The experiment also exposed `networking: public` as a real reconciliation requirement. Internal health initially passed while public TCP/8080 timed out. Canter created and attached a narrowly scoped managed ingress policy, persisted its IDs in system state, and subsequently deleted the policy during teardown.

The host, compute port, and managed network policy were destroyed. A final provider probe returned zero compute resources and a separate network query returned zero matching managed policies. Observed application state was closed as `host-destroyed`; content-addressed artifacts, desired/observed history, boot proof, and lifecycle receipts remain in `m1`.

## Remaining boundaries

The node currently receives bucket-scoped `m1` credentials at bootstrap; production needs per-system, least-privilege credentials with rotation. The experiment is single-host and HTTP-only, without TLS, domains, multi-node placement, log streaming, or durable application volumes. Those are unimplemented rather than simulated.
