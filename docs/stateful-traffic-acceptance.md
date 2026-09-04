# Stateful traffic acceptance — August 28, 2026

## What was tested

The `stateful-board` contract declared two logical services on one `c1` host: a private 256 MiB `database.postgres` capability and a public 192 MiB `service.http` capability that depends on it. With a 384 MiB system reserve, the compiler reported 192 MiB unallocated. The host bootstrap contained only the generic Canter node. It did not contain Postgres or application setup.

The node read `runtime-plan.json` from `m1`, selected the registered Postgres service driver, installed and initialized a real PostgreSQL server, generated stable host-local credentials, waited for `127.0.0.1:5432`, and exposed only a generic `CANTER_SERVICE_DATABASE_URL` binding to the application release. The observed state reported the database capability independently from the application process.

The application ran real migrations and used PostgreSQL for users, sessions, posts, likes, transactions, joins, indexes, and full-text search. The workload used persistent authenticated actors with randomized think time and this request mix: feed reads, post creation, post updates, search, session renewal, identity reads, and likes.

## Discovery run

The first four-minute run completed 21,958 requests at an overall 91.39 requests/second. Median latency was 208 ms, p95 was 325 ms, and p99 was 410 ms. It included 10,596 feed reads, 4,372 attempted post creations, 1,949 updates, 1,984 searches, 1,064 logins, 1,532 identity reads, 425 likes, and 36 signups.

A v1 to v2 deployment kept the database and host in place, but 3 in-flight requests returned 502. Canter switched the proxy target before stopping v1, but did not wait for requests already routed to v1 to finish. This demonstrated that an atomic target pointer was not equivalent to connection draining.

The original requested-restart behavior intentionally cleared the proxy before recreating the process. Recovery took about 2.35 seconds and produced 88 HTTP 503 responses under traffic. The database remained ready. After the run, PostgreSQL contained 37 users, 1,095 sessions, 4,350 posts, and 388 likes, and v2 was healthy against that state.

## Engine correction and confirmation

The generic node routing layer was changed to account for in-flight requests per backend. A release now starts and health-gates its candidate, switches new requests, waits for the old backend to drain, and only then terminates it. Requested restart now uses the same replacement path at the current version rather than removing the active route first. Genuine unexpected process exit still has a recovery gap until the system can place multiple web instances.

A fresh host running the corrected node automatically reconciled Postgres and the retained desired v2 release. A two-minute confirmation workload sustained 9,593 requests at 79.78 requests/second while Canter performed both a v2 to v3 release and a requested same-version v3 replacement.

- 9,593 succeeded; 0 failed.
- HTTP statuses: 7,165 responses were 200 and 2,428 were 201.
- Median latency was 207 ms, p95 was 358 ms, and p99 was 535 ms.
- The workload included 4,555 feed reads, 1,905 post creations, 860 updates, 883 searches, 491 logins, 680 identity reads, 187 likes, and 32 signups.
- The final database contained 33 users, 524 sessions, 1,905 posts, and 172 unique likes.
- v3 remained healthy, Postgres remained independently `ready`, and the application PID changed during requested replacement.

Both live hosts and their exact managed endpoint policies were deleted. The final provider probe reported zero compute resources. Content-addressed releases, runtime plans, receipts, proofs, and observed history remain in `m1`.

## Boundary proved

This experiment did not make Postgres part of host bootstrap or application code. It established the intended abstraction boundary:

`System intent -> execution graph + runtime plan -> capability driver -> private service binding -> health-gated application release`

Postgres is currently one built-in realization of the database capability. Adding another database or backing primitive means registering another runtime driver; it does not require changing the application lifecycle, release format, host provider adapter, or contract shape.
