# Canter control plane

The control plane is the durable boundary between disposable agent conversations
and the existing Canter execution engine. HTTP, MCP, the CLI, and future WebMCP
adapters must call `Service`; they must not reproduce provider operations.

## Local configuration

```text
CANTER_DATABASE_URL=postgres://localhost/canter?sslmode=disable
CANTER_CONTROLPLANE_ADDR=127.0.0.1:8081
CANTER_NODE_BINARY_PATH=bin/canter-node-linux-amd64
CANTER_NODE_GATEWAY_URL=https://control.example.com
CANTER_PUBLIC_URL=http://localhost:3001
CANTER_COOKIE_SECURE=false
CANTER_REQUIRE_INVITE=false
```

`CANTER_BETA_INVITE` optionally seeds one hashed, single-use invitation. Existing
compute and m1 variables are still required because the control plane initializes
the real SDK engine. No provider credentials are written to PostgreSQL or returned
through an agent-facing endpoint.

## Node capability gateway

Tenant hosts never receive the account-wide m1 access key. Before creating a
host, the server creates a 20-minute, single-use node enrollment scoped in
PostgreSQL to one workspace, one System, and that System's immutable m1 prefix.
The bootstrap exchanges it once for a hashed, revocable `cn_` node credential
and stores only that credential at `/etc/canter/node.token`.

The node then uses only these typed HTTPS operations:

```text
POST /v1/node/enrollments/{id}/exchange
GET  /v1/node/snapshot
GET  /v1/node/artifacts/{sha256}
PUT  /v1/node/observed
PUT  /v1/node/control-acks/{id}
PUT  /v1/node/runtime-actions/{id}/result
```

Artifact reads are restricted to the current desired SHA and its System prefix
or exact workspace-owned staged artifact. The control plane rehashes the bytes
before returning them. Runtime results are accepted only for the currently
leased action. Node request bodies are capped at 256 KiB, provider keys are not
generic gateway operations, and a forwarded HTTPS scheme is trusted only from a
loopback reverse proxy.

`sdk.Client.BootstrapSystemHostViaGateway` is the secure host bootstrap entry
point. The legacy `BootstrapSystemHost` deliberately fails closed; the initial
deployment dispatcher must create the enrollment and call the gateway method in
the same durable execution before provider mutation.

Run it with:

```sh
go run ./cmd/canter-controlplane
```

The API is rooted at `/v1`. Streamable HTTP MCP is available at `POST /mcp` and
uses the same HttpOnly human cookie or short-lived agent bearer token as the API.

The public CLI and transport client use that same boundary:

```sh
canter agent connect --api-url http://127.0.0.1:8081
canter agent bootstrap --access-token "$CANTER_AGENT_ACCESS_TOKEN"
canter agent mcp --access-token "$CANTER_AGENT_ACCESS_TOKEN"
```

Library callers can use `github.com/canter0/canter/sdk/remote` for device
authorization, polling/exchange, refresh, identity, and bootstrap without
depending on CLI state or provider SDKs.

The checked-in integration tests use a dedicated database and never run unless
`CANTER_TEST_DATABASE_URL` is explicitly set.

## Governed first deployment

A fresh agent can create a System without receiving compute or m1 credentials:

1. `POST /v1/workspaces/{workspace}/artifacts` accepts a raw tar.gz body (up to
   64 MiB), plus optional `X-Canter-Filename`. It returns only the content SHA,
   file manifest, size, type, actor, and timestamp; the internal m1 key is never
   exposed. The release `command` is argv, not shell text: `command[0]` must be
   a `./`-prefixed executable file in that manifest. Source-only archives are
   rejected until the agent builds and includes a runnable artifact.
2. `POST /v1/workspaces/{workspace}/initial-deployments` accepts the System
   contract, returned `artifactSha256`, release command/environment/health/port,
   and an exact HTTP verification. It creates an immutable digest and performs
   no provider mutation.
3. A human posts that exact digest to
   `/v1/workspaces/{workspace}/initial-deployments/{id}/authorize`, then queues
   it at the sibling `/apply` route.
4. The server-owned dispatcher registers the System, creates a one-time node
   enrollment, calls `BootstrapSystemHostViaGateway`, publishes the staged release, waits for the public
   endpoint, and records exact verification evidence. Execution is observable
   at `GET /v1/initial-deployment-executions/{id}`.

Agents receive only `canter_upload_artifact`,
`canter_draft_initial_deployment`, `canter_list_initial_deployments`,
`canter_inspect_initial_deployment`, and
`canter_inspect_initial_deployment_execution`. MCP artifact bytes are standard
base64. Agent installations require draft authority to upload or draft; there
are deliberately no agent MCP tools for authorization or apply. Those actions
remain authenticated human HTTP operations.

Every bootstrap response includes this capability contract in machine-readable
form, including concrete operation paths, MCP tool names, artifact limits,
command semantics, and the runtime-provided `PORT`, `CANTER_RELEASE_VERSION`,
and `CANTER_SERVICE_<NAME>_URL` environment bindings. Pending first-deployment
proposals are returned beside pending Changes so a fresh conversation can resume
without repository knowledge or URL guessing.

Current runtime constraints are explicit: the node experiment supports one host,
the control-plane process must be given a Linux node binary through
`CANTER_NODE_BINARY_PATH`, and a partially-created non-ready host stops for
operator recovery instead of being guessed healthy or destructively replaced.

## Durable Change execution and standing policies

The complete Change index returned by bootstrap and list operations includes
the stable execution ID and phase. `canter_inspect_change` embeds the execution
record, while `canter_inspect_change_execution` addresses it directly by
workspace, System, and Change. A new conversation therefore reconstructs the
execution chain without direct database access.

A signed-in human may create an immutable, expiring standing policy from a real
server-generated Change envelope. Policies bind exact installation IDs,
workspace and System revisions, affected services, operation kinds,
availability, data impact, reversibility classes, maximum additional monthly
cost, maximum operation count, per-service replica ranges, temporary-scale
duration, and expiry. Agents can list policies and submit
one exact digest to `canter_apply_change_under_policy`; they cannot create,
widen, or revoke policy authority. A match records the policy digest and human
creator, authorizes as a policy actor, and queues a stable execution. A miss
performs no authorization or apply and requires focused human approval.

## Existing-host application replica Changes

`Change.spec.scale` is a typed application-capacity outcome: an exact public
process service, target replica count, and optional 60-to-86400-second lease.
The engine derives the healthy current count, rejects targets that do not fit
the single already-allocated host's declared memory, and binds the from/to
transition, exact restore time, readiness verification, impact, compensation,
and base revisions into the Change digest. It never accepts provider IDs or
silently creates VMs.

The node runs distinct application processes on private ports and round-robins
only across the ready target set. Scale-up candidates join traffic only after
all are healthy. Scale-down removes targets from rotation, drains in-flight
requests, and then terminates them. Readiness or public verification failure
restores the prior replica count. A temporary scale embeds a desired-state
capacity lease; the node enforces its expiry and returns to the prior count even
when the proposing agent is offline or the control plane is restarting.

The provider-neutral System view exposes `applicationCapacity` with the
declared baseline, desired, ready, and maximum replicas. This primitive is
deliberately not multi-host autoscaling: host expansion, placement across many
VMs, metrics triggers, and a separate durable child execution for lease expiry
remain later Change primitives.

External Change and initial-deployment read models retain environment key names
but replace every value with `[redacted]`, omit internal artifact keys, redact
trailing command arguments, migration SQL, and provider-bearing failures. The
unredacted execution documents remain inside the engine boundary.
