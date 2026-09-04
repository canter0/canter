# Canter durable agent control plane

Status: implemented and live-accepted on September 1, 2026. The chronology now
includes RallyReserve and a second, independently built recursive hosting
provider. The current real c1 runs Blackout Recursive Host with private managed
PostgreSQL and a managed public TCP endpoint. The human authorized every exact
installation, deployment digest, destructive replacement, and post-deployment
Change before its corresponding provider mutation.

## Product boundary

Canter persists the system, not the conversation. A coding-agent conversation
is an ephemeral client of a durable installation. The durable record is:

- account and workspace ownership;
- agent installation, bounded authority, and revocation state;
- short-lived agent sessions and rotated refresh-credential families;
- declarative Systems;
- immutable initial-deployment and Change proposals;
- exact human authorizations;
- fenced execution attempts, operations, and evidence;
- scoped node installations and observations.

No provider credentials, m1 storage keys, or provider resource identifiers are
part of the remote agent contract.

## Human story

1. A person creates an account or signs in on the Canter website.
2. Canter asks them to bring the agent they already use.
3. The agent begins device authorization and returns a short one-time code.
4. The website shows the installation name, harness, and requested authority.
5. The person authorizes the durable installation. Apply authority always
   remains `human-approval-required`.
6. The agent exchanges the one-time device credential for a 15-minute access
   session and a rotating installation refresh credential.
7. Any later conversation refreshes into a new session and calls bootstrap.
   Bootstrap reconstructs the same workspace, Systems, pending proposals,
   capabilities, and installation identity.
8. The dashboard is a human review and audit surface. The agent can inspect and
   draft; only the signed-in human can authorize the exact digest and enqueue a
   production execution.
9. Revoking an installation ends its sessions and credential family without
   deleting Systems, Changes, executions, or evidence.

## Approval transport

Approval is now represented as a human-authenticated, digest-bound event in
Canter's ledger rather than requiring the human to find a Change in the full
dashboard. An authenticated agent may request a ten-minute review capability
for one current drafted Change and exact digest. Canter stores only the
capability token hash and binds the durable record to workspace, System,
Change, digest, requesting installation and session, expiry, and the fixed
`authorize-and-apply` action.

The returned route grants no authority by itself. A human must already be
signed in, or sign in and return to the same focused route. The focused page
shows only the requesting installation, summary, exact digest, System, base
release, availability, data impact, cost delta, expiry, and execution plan.
The proposing agent never receives or borrows the human cookie. Consumption is
atomic in PostgreSQL; expiry, supersession, wrong-workspace identity, viewer
role, stale digest, consumed token, and concurrent replay cannot authorize.
Only after the human consumes the route does Canter authorize the exact digest
and enqueue the execution under the human actor. The resulting execution ID is
written back to the capability record.

Agents discover this through `/.well-known/canter`, `/llms.txt`, bootstrap,
HTTP, and MCP tool `canter_request_change_approval`. The dashboard remains the
durable surface for history, policy, revocation, investigation, and detailed
review. Direct inline approval from an authenticated conversation and standing
policies remain deferred. Those policies must pre-authorize a bounded envelope,
such as replica ceiling, duration, trigger, and maximum spend; they must not be
implemented as ambient agent authority.

## Agent surfaces

The CLI exposes:

```text
canter agent connect
canter agent begin
canter agent exchange
canter agent refresh
canter agent whoami
canter agent bootstrap
canter agent mcp
```

Credentials are printed to the caller and are never written unless an explicit
`--env-file` path is supplied. Explicit writes are atomic, mode `0600`, preserve
unrelated values, and reject symlink targets.

The stateless Streamable HTTP MCP endpoint is `/mcp`; it exposes agent inspect,
artifact, and draft capabilities but no authorization or apply tool. The
website also registers
signed-in read/review tools through `document.modelContext` when WebMCP is
available. Browser WebMCP deliberately cannot authorize or apply by borrowing
the human session; those decisions require the corresponding website action.
The browser surface intentionally does not expose large base64 artifact upload;
agents use the remote SDK, HTTP API, CLI, or server MCP for build artifacts.

A clean-room agent can start from the public origin without the repository or a
preinstalled CLI. The HTML advertises `/.well-known/canter` and `/llms.txt`.
Those documents identify the same-origin API/MCP gateway, the unauthenticated
device-begin contract, human review URL, exchange, refresh, and bootstrap flow.

## Governed first deployment

An agent uploads a bounded tar.gz artifact. Canter validates canonical paths,
entry count, header size, expanded size, duplicate paths, file-parent conflicts,
special entries, and executable mode. The provider storage key is derived from
the SHA-256 digest and remains server-only.

The initial-deployment proposal binds:

- the complete canonical System contract;
- workspace revision;
- artifact SHA-256;
- exact argv, environment, health path, and public port;
- exact public HTTP verification method, path, status, and optional body marker.

Authorization stores the human actor and the exact digest. Authorization and
execution both recompute the proposal digest. Immediately before publication,
Canter reloads and rehashes the stored artifact.

Execution is a durable server-owned queue. Every claim receives a random fence
token. Renewal, operation transitions, evidence, and completion require that
token. A reclaimed execution skips already-succeeded operations; a stale worker
cannot finish or append evidence.

Provider mutation has a second write-ahead boundary in m1. Canter conditionally
claims the complete compute intent and each per-replica create attempt before
calling the provider. Reclaimed workers reconcile the exact operation metadata;
an unresolved or duplicate outcome escalates and is never converted into a new
Create merely because a visibility timer elapsed. Public endpoint creation is
fenced the same way, and retries of an in-flight intent are lookup-only.

## Node capability gateway

Tenant compute never receives Canter's m1 credentials. Bootstrap contains a
single-use, short-lived enrollment credential and an HTTPS gateway URL. The
node exchanges it once for a hashed, revocable credential scoped in PostgreSQL
to one workspace, one System, and its server-owned m1 prefix.

The node gateway exposes only typed semantic operations:

- exchange one enrollment;
- read the current sanitized snapshot;
- fetch only the current authorized artifact digest;
- write validated observed release state;
- acknowledge the current control;
- return the current fenced runtime-action result.

There is no generic object-key API. Artifact keys, bucket names, and provider
credentials never cross the gateway. HTTPS is mandatory; forwarded HTTPS is
trusted only from a loopback reverse proxy.

## Security invariants

- Control-plane System prefixes are server-derived as
  `workspaces/<workspace>/systems/<system>` and globally unique.
- Public network policies derive a stable hash and ownership description from
  that canonical prefix. Canter never adopts a provider policy by display name
  alone, so identical System names in different workspaces cannot share rules.
- Unsafe standalone prefixes containing whitespace, traversal, backslashes,
  control characters, or invalid segments are rejected.
- Inspect-only agents cannot call any HTTP or MCP write.
- Cookie-authenticated mutations require the configured exact Origin.
- JSON decoders require `application/json`; cookies are HttpOnly, SameSite=Lax,
  and use the `__Host-` prefix whenever the public URL is HTTPS. HTTPS cannot
  be downgraded by an omitted or stale cookie environment flag.
- Remote agent clients allow cleartext only on loopback and never follow
  credential-bearing redirects.
- Refresh rotation tracks credential families. Replay of a rotated credential,
  including simultaneous replay that causes a serializable transaction retry,
  revokes the entire family and its sessions.
- Installation revocation transactionally ends credentials and sessions.
- Node enrollment and node credentials are stored only as hashes. Exchanging a
  replacement enrollment atomically revokes every prior credential for that
  node identity.
- A host is not deployment-ready until its exact server-bound exposure intent
  and one complete matching provider policy are durably `ready`; compute alone
  cannot advance the deployment queue.
- Compute and exposure provider calls carry operation and attempt identities
  through every post-provider transition. Each attachment, readiness update,
  escalation, and deletion is ETag-CAS fenced against those identities.
  Destroy first terminalizes the active intents and reconciles only exact
  operation-owned resources. An accepted but invisible provider mutation
  remains durably unresolved across repeated destroy attempts rather than being
  declared absent. Late cleanup must CAS-claim the exact old operation and
  attempt in a non-replaceable cleanup phase before touching the provider; it
  cannot resurrect state or delete an object owned by a replacement.

## Blackout acceptance evidence

Without inspecting the Canter repository, the blackout developer built
RallyReserve: a Go/PostgreSQL tennis booking API with row-locked final-slot
reservation. Local real-PostgreSQL proof produced one concurrent `201`, one
`409`, forty successful reads, and `overbooked_slots: 0`.

The deterministic Linux amd64 artifact is:

```text
/tmp/canter-blackout-rallyreserve/dist/rallyreserve-linux-amd64.tar.gz
sha256 2bd674a310b11b5665d20a5b005511c88d9996e530a0e3732610a6cfac3333ed
```

The blackout agent learned `PORT`, service bindings, artifact rules, routes,
MCP tools, and the human boundary from bootstrap alone. It surfaced and drove
fixes for missing capabilities and the declared-but-undeployed System case.
The preserved inert proposal is `dep_vIGeLYBAgVSrlccP`; it has never been
authorized or applied.

A second clean-room agent built LinguaQueue, a different Go/PostgreSQL service,
without reading the Canter repository or reusing an installation. Its live
20-card contention test produced zero duplicate active claims, and its
deterministic Linux amd64 artifact has SHA-256
`7a3690ad62db786e74759bd876fb6c673331b47b5487e5570afa4d2e3e978b02`.
That agent initially could not discover how to connect from the public origin;
after `/.well-known/canter` and `/llms.txt` were added, it discovered the full
protocol and safely began a new device request. It stopped before approval and
therefore never received a credential or drafted infrastructure.

A third clean-room agent started with only `http://localhost:3001` and no
repository, environment, browser cookie, credentials, or prior report. It
traversed the landing, sign-in, account creation, discovery, instructions,
device authorization, MCP initialization, identity, bootstrap, and refresh
surfaces. Its report is `/tmp/canter-blackout-site-journey.md`. The journey
exposed an unauthenticated approval dead-end, a hard-coded Codex page title,
and an incorrect published refresh URL. Canter now preserves the exact review
path through sign-in/account creation, uses an agent-neutral title, and
publishes the working refresh method, body, credential semantics, and token
rotation rule. A second clean-room session then refreshed and bootstrapped the
same durable installation and workspace without repository context.

## Live provider acceptance

The human authorized initial deployment `dep_NsoIs2tGpjU1YpYW`, digest
`0a6448043e0f01263f52eaf658f5e9c9eb5b2d8b2c17801d8b684682ff4aded6`,
for the RallyReserve artifact already recorded above. Canter provisioned one
c1 host, installed the credential-free node, created the exact managed TCP
exposure, started a private PostgreSQL service, published the application, and
reported a healthy public endpoint.

The first queue execution revealed a real startup race: operation 04 treated
the not-yet-created `observed.json` object as a terminal storage error. The
host and application continued to become healthy. Canter now retries only the
object-store `NoSuchKey`/`NotFound` startup condition within the existing
execution deadline and lease; authentication, transport, decoding, and other
storage failures remain terminal.

A governed retry of the same authorized digest created a new historical
execution, preserved and skipped successful operations 01–03, reran health
operation 04, and completed exact HTTP verification operation 05. The final
proposal phase is `succeeded`; its five evidence records include public
`/healthz` and `/proof` status 200. Five concurrent traffic journeys performed
500 ordinary reads and five last-slot races. Every race produced exactly one
201 and one 409, PostgreSQL retained five reservations, and
`overbooked_slots` remained zero.

## Recursive provider acceptance

A later clean-room agent received only Canter's public discovery and authorized
agent surfaces, not this repository or provider credentials. It built Blackout
Recursive Host: a bounded, multi-tenant Go service that persists artifacts,
bots, encrypted per-bot secrets, ownership, desired state, and run identity in
managed PostgreSQL. Each bot is a separately supervised process using a fixed
manifest interpreter, allowlisted environment, bounded logs, global c1-sized
admission limits, tenant-scoped queries, and fenced run IDs. No Discord network
login is claimed; the fixtures prove hosting and supervision mechanics.

The human authorized initial deployment `dep_he1_1RdcTVom9mUw`, digest
`aefbbb11a624a28bb2f434435fea795df145af8914ed91386fb555a8a3b5d79a`.
The live path exposed three failures that a simulator would have missed:

- a provider policy call timed out after acceptance, leaving an ambiguous
  exposure mutation. Ordinary reconciliation remained lookup-only. A separate,
  explicit human retry was allowed to reconcile only that exact escalated
  intent;
- the application's declared `PORT=8080` overrode Canter's candidate runtime
  port and collided with the node proxy. Runtime-owned release variables and
  managed service bindings now take precedence without duplicate keys;
- after an explicitly authorized failed-host replacement, a previously
  succeeded bootstrap receipt incorrectly suppressed the new host bootstrap.
  Canter now reopens only the destroyed-host operation for a later human retry
  of the same failed execution history.

Replacement execution `ide_7dr15kNPmy6kOsVb` succeeded after two fenced
attempts. All five operations have durable receipts. The resulting System has
one ready c1, ready managed exposure, private ready PostgreSQL, and a healthy
public release. Public `/healthz` and `/proof` return 200 with
`databaseReachable:true`. The initial live story persisted two tenants'
artifacts and bots, rejected cross-tenant read and stop attempts with
non-disclosing 404 responses, kept secret values out of responses and logs,
and proved a stopped bot could restart with a new PID and run ID. One hundred
concurrent health requests and one hundred concurrent proof requests all
returned 200.

That traffic exposed a fifth-restart defect in the provider's watcher. It
created a four-second database context before a four-second backoff, then reused
the expired context for the post-backoff state check. The clean-room agent
fixed the provider in its own workspace, added a focused regression, passed
three consecutive real-PostgreSQL acceptance runs, and uploaded deterministic
artifact
`6b4f40533db4e29d57ce2898d3088e12199df59ecf6047eccb9ed6f794949f0b`.

The agent drafted governed Change `change-4937b9c50e5c00be`, digest
`71f2f7f3906c9458905071c92487499739e5882f0f80eb3710d47d6f7416baef`.
It could not authorize or apply its own proposal. In the existing Zen window,
the signed-in human reviewed that exact digest, unchanged c1/PostgreSQL/data/
environment/port contract, zero incremental monthly cost, and compensation to
the prior release. The human authorized and applied it. Execution
`exe_wOxjdkbIpWoxgc8f` committed all four operations: base-state assertion,
exact artifact publication, healthy-release wait, and `/proof` verification.
The verification returned 200 and recorded the response-body digest.

After the rollout, the four existing artifacts and four existing bots remained
in PostgreSQL, no test process was left running, unauthenticated bot access
still returned 401, and an independent 2,000-request mixed health/proof journey
returned 2,000 HTTP 200 responses with zero contract failures. The clean-room
agent then started an existing heartbeat peer and a fresh deterministic crash
fixture under different tenants. The crash fixture progressed through restart
counts 8 and 9, then entered the configured policy-capped `failed`/stopped
state, while the heartbeat peer remained running. Cross-tenant reads and stops
returned 404; credential and secret-key scans found no leakage. A corrected
eight-worker, 12-second authenticated/public workload produced 110 expected
200 responses, 20 expected cross-tenant 404 responses, zero transport errors,
and zero unexpected outcomes. Cleanup stopped both bots; final proof reported
PostgreSQL reachable, `desiredRunning=0`, and `activeProcesses=0`.

The agent-facing System representation was also tightened during this run. It
now exposes provider-neutral class, count, semantic resource names, status,
managed protocol, and public application endpoint only. Compute UUIDs, private
or provider addresses, policy/rule IDs, proof keys, operation identities, and
provider error URLs remain server-side.

## Verification completed

- full `go test ./...` with real PostgreSQL integration;
- race tests for control plane, node client, node, SDK, and remote SDK;
- deterministic reclaimed-worker tests for concurrent intent/attempt CAS,
  provider visibility loss, duplicate escalation, and lookup-only endpoint
  recovery;
- deterministic and repeated overlap tests for create versus destroy,
  exposure versus destroy, replacement after destroy, and late-owner cleanup;
- `go vet ./...` and `git diff --check`;
- web ESLint and TypeScript checks;
- Linux amd64 static node build;
- real PostgreSQL enrollment exchange, replay rejection, revocation, refresh
  family replay, lease fencing, digest tamper, and artifact mutation tests;
- Zen landing, account creation, and agent-review flow up to the final human
  authorization button;
- one human-authorized real c1 deployment, node enrollment through external
  TLS, public release health/proof verification, governed retry, and sustained
  PostgreSQL-backed contention traffic;
- clean-room website-to-device-to-MCP onboarding and refresh into a second
  conversation while preserving installation and workspace identity;
- a second clean-room, human-governed initial deployment that exposed and fixed
  real provider-ambiguity, runtime-environment, and receipt-reopening defects;
- one exact post-deployment Change authorized in Zen, committed through the
  durable queue, verified publicly, and independently exercised with 2,000
  mixed health/proof requests and zero failures;
- revocation of disposable installation `agt_Wy16tVB7KulC66la`: its prior
  access and refresh credentials both returned 401 afterward, while the two
  Systems, three initial-deployment proposals, committed post-deployment
  Change, execution receipts, and evidence remained visible to the human;
- focused approval integration against isolated real PostgreSQL: exact digest
  binding, agent-only creation, human identity, wrong-workspace denial, expiry,
  supersession, one-time consumption, simultaneous replay, execution enqueue,
  and retained human requester all passed. A new clean-room conversation then
  discovered the capability from public surfaces, drafted
  `change-ae9c968334baaa8e`, and requested its exact review route without gaining
  authorization. The human later approved only digest
  `f555333e498628ba611d68c2e447c80d549c3737f5260b8172529dadddbaabe5`
  through that focused route. Execution `exe_JSoz4E0zEpBT-JVX` ran each of its
  four operations once and succeeded; the Change committed, its `/proof`
  contract returned 200, and reopening the consumed capability returned not
  found rather than replaying the authorization.

## Current operational state

- The current c1 host, Blackout Recursive Host, PostgreSQL service, node
  credential, managed public endpoint, control plane, and temporary ngrok
  gateway remain active.
- RallyReserve's obsolete c1 host, managed TCP/8080 exposure, application,
  host-local PostgreSQL runtime, node installation, and active node credential
  were explicitly destroyed or revoked. Its Systems, proposals, authorization,
  executions, operation evidence, audit events, and revoked identity records
  remain in Canter as durable history. The deleted VM and host-local database
  cannot be recovered through Canter because no Rally snapshot or backup was
  created; restoring it would require a newly governed deployment.
- Governed watcher-fix Change `change-4937b9c50e5c00be` is committed and its
  durable execution evidence records the exact replacement artifact, healthy
  node process, and successful `/proof` contract.
- The temporary gateway should be replaced by a durable Canter-owned HTTPS
  endpoint before treating this as production infrastructure.
- The unused first blackout installation was authorized but never exchanged;
  it remains visible for explicit human revocation rather than being silently
  deleted.
- Disposable installation `Blackout Site Journey Retry` is revoked. Its access
  session and rotated refresh family no longer authenticate, while all durable
  infrastructure history remains intact. The live Blackout Recursive Host
  installation remains authorized and was not touched.
- The one-time focused approval capability has completed live acceptance. The
  clean-room agent requested the route but could not authorize it; the human
  approved the exact digest in Zen, the durable execution succeeded, and the
  consumed URL rejected replay.
- Fresh-conversation execution continuity is now explicit: bootstrap, Change
  listing, Change inspection, and `canter_inspect_change_execution` all expose
  execution `exe_JSoz4E0zEpBT-JVX` as succeeded for the committed focused
  Change. No direct database access is required by the agent.
- Human-authored standing policy envelopes are implemented and passed isolated
  real-PostgreSQL acceptance. Policies are immutable, expiring, revision-bound,
  limited to exact active agent installations and server-generated operation,
  impact, reversibility, cost, and operation-count bounds. Agents may list and
  evaluate an exact digest, but cannot create, widen, or revoke authority. No
  live Blackout policy has been created; doing so remains an exact human choice.
- A context-free blackout inspection exposed a release-blocking read-model bug:
  stored application environment values were returned by Change inspection.
  External Change, approval, policy, and initial-deployment responses now retain
  environment key names only, redact every value and trailing command argument,
  omit internal artifact keys and migration SQL, and sanitize failure strings.
  A repeat blackout inspection found no known application or agent credential.
  Thirty-five historical local response snapshots were mechanically redacted;
  the canonical local application credential file remains mode `0600`.
- Direct authenticated inline-conversation approval remains a later transport:
  Canter still requires a focused one-time route whenever no standing policy
  matches because the current agent harness cannot prove the human speaker's
  identity to Canter.
- The first real capacity primitive now exists without provider leakage.
  `Change.spec.scale` binds one public process service, the observed starting
  count, target replicas, existing-host capacity mode, verification, and
  compensation into the digest. The node starts distinct processes, routes
  traffic only to ready replicas, drains scale-down targets, and reports
  desired versus ready capacity through the System read model.
- Replica standing policies now require an exact per-service inclusive range
  and separately bind permanent authority or a maximum temporary duration; an
  operation-kind-only policy cannot authorize scaling.
- Optional 60-to-86400-second capacity leases bind the exact restore time and
  prior replica count into the Change. The node restores that count after
  expiry without depending on the proposing conversation. This first primitive
  uses one existing host and reports zero additional provider cost because it
  consumes already allocated memory. It does not claim multi-host placement or
  silently provision VMs. Lease expiry is durable desired/observed state but
  does not yet emit a separate child execution record.
- No live Blackout scale Change or standing policy was created during this
  implementation. Those remain exact human-authorized mutations.
