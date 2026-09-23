# Hosted operator harness

The existing dashboard uses the same conversation API and presentation. The
harness adds private working context, history retrieval, saved evidence,
connected-agent task creation, an inbox for follow-ups, and optional Bash
commands through pinned `just-bash` 3.4.2.

## Command environment

The shell is a virtual filesystem and Bash interpreter. It is useful for `jq`,
`rg`, `awk`, `sed`, pipelines, scripts, and scratch notes. It cannot run host
binaries, install packages, reach the network, access credentials, or mutate
infrastructure. Those operations continue through Canter's explicit tools and
their existing authorization. Python, JavaScript, SQLite, compression, and
archive commands are excluded from the command registry.

The model sees `/workspace/context.json`, recent private history in
`/workspace/history.jsonl`, an evidence index in `/workspace/results.json`, and
available tool results under `/results/`. Context and result files are rebuilt
from stored evidence on every call. Only regular UTF-8 files under `/scratch`
persist, in PostgreSQL, scoped to the conversation. Shell variables and cwd reset.
Invalid/oversized scratch snapshots retain the previous complete snapshot.

| Limit | Bound |
| --- | --- |
| Concurrent command processes | 1 per control-plane process; production socket also caps total connections at 1 |
| Production process memory | 192 MiB hard cgroup limit, 144 MiB high watermark, no swap |
| Node old-space heap | 96 MiB ceiling within the process memory budget |
| Idle command memory | No running worker; socket activates a disposable process |
| Execution | 4-second interpreter deadline; 8-second bridge/service limit |
| Virtual filesystem | 4 MiB |
| Persisted scratch | 64 files, 256 KiB including paths; 12 directory levels |
| stdout/stderr | Interpreter aggregate limit of 64 KiB |
| Shell source | 16,000 bytes |
| Tool evidence | Up to 2 MiB per saved result, with bounded previews and paging |
| Working model text | Conservative 96 KiB byte budget, excluding tool schemas and separately bounded attachments |

The focused local command test measured about 66 MiB peak RSS on macOS. A
September 23, 2026 check on the production Linux host used an isolated socket
with the production service settings: pipelines, scratch round trips, denied
host/network commands, and bounded loops passed, with about 71 MiB peak RSS.
Systemd reported the 192 MiB hard cap, zero swap, private networking, and dynamic
user. These measurements exclude the API, PostgreSQL, and Next.js processes.

## Setup

On macOS development with Node >=22.13:

```sh
npm ci --prefix harness --ignore-scripts --no-audit --no-fund
npm test --prefix harness
./scripts/dev.sh
```

The entrypoint discovers the installed local runner. An explicit
`CANTER_OPERATOR_SHELL_RUNNER` and optional `CANTER_OPERATOR_SHELL_NODE` override
the development paths. No tool ever passes its command text to the native shell.
The subprocess receives a clean environment and Node filesystem permissions
limited to the trusted harness package directory.

Normal production releases install the command runtime automatically through
GitHub Actions and the production release timer. For a one-time Linux bootstrap,
copy the CI release (including its locked dependencies), then run:

```sh
sudo sh deploy/install-harness.sh
```

The installer creates an immutable version under `/opt/canter-harness/releases`,
points `current` at it, and enables the socket unit. Dependencies must already be
installed from the lockfile without lifecycle scripts by CI or an unprivileged
user; the root installer never runs npm. Each command uses a
dynamic service user with private networking, read-only host filesystem,
restricted Node permissions, and cgroup limits. The user has no membership in
the control-plane's secret-reading group. The socket is accessible only to the
`canter` service user. Inspect the unit paths if Node is installed elsewhere.

The next control-plane startup discovers `/run/canter-harness.sock` and executes
a small readiness check before advertising Bash. Configure
`CANTER_OPERATOR_SHELL_SOCKET` for a custom socket. Linux's production entrypoint
rejects an uncontained direct runner. The installer does not restart the control
plane, deploy the web application, or run database migrations.

## Continuation, evidence, and authority

Migration 023 permits one running response and up to four queued messages per
conversation. Requests remain idempotent. A new message causes the current run
to yield before its next tool dispatch or final answer; the next run sees prior
answers in logical turn order and the user's latest message. Already submitted
operations retain their own status. Stop cancels both the active run and inbox.
Different conversations can still use the two operator workers concurrently.

Tool effects and retry rules are explicit and default-deny. Scratch commands
with ambiguous completion are not replayed. Task creation uses stable operation
identity, so a retry after a lost response returns the same workspace task.
Task creation requires draft authority and never grants deployment authority or
wakes an offline agent. The existing task claim/finish protocol is unchanged.

`canter_save_context` stores a bounded objective, project, confirmed facts, open
questions, and next step. These are agent-written notes, not verified provider
state or authority. History search/read enforce both account and workspace
scope. Tool evidence and scratch files additionally enforce conversation scope.
Every private-state write is fenced by the current run lease. Conversation
deletion cascades through context, evidence, and scratch records.

Large observations stay in the existing tool ledger and are returned as bounded
previews with stable `/results/` handles. `canter_read_result` provides paging;
Bash can process the full loaded results. An index identifies results omitted
from the shell's input budget. Older observations can be masked in model input
without changing their stored evidence or breaking tool-call/result pairing.
The current user request and pinned working notes are preserved during masking. Model events record a harness version, latency, and provider-reported usage when supplied; unavailable usage remains null.

Fresh context reads are bounded in SQL, and only the most recent attachment set
is loaded. This avoids materializing an entire conversation's images before
discarding them. Historical tool results are evidence of what happened then;
the agent still needs live tools for current resource state.

The hosted grant remains separate from external-agent Write defaults. Compute
planning is available in conversation for managed app deployments and standalone
one- or multi-VM topologies. The operator estimates Canter compute usage at $3
per 720 hours per billable unit, where a unit is the larger of vCPU count or
memory rounded up to whole GiB; local disk and public IPv4 are included. This is
Canter's usage rate, not an upstream provider quote, and workspace credits are
not folded into the estimate. The current operator still has no live provider
inventory or standalone VPS provisioning path, so plans must label assumptions
and provide human-readable CPU, memory, disk, and OS targets without presenting
internal allocation labels as VM sizes or claiming exact provider quotes. Bucket provisioning also remains
unavailable. Adding a shell does not make those operations supported. Payment and
provider-event automatic resumption are not implemented by this inbox: this
version resumes on user messages and uses explicit task/status inspection.

## Verification

```sh
npm test --prefix harness
CANTER_TEST_DATABASE_URL='<isolated database>' go test ./internal/controlplane -run TestOperator -count=1
go test ./...
go vet ./...
cd web && pnpm lint && pnpm exec tsc --noEmit
```

Database fixtures truncate their configured database; never use a shared or
production database. Coverage includes actual command execution, private-file
persistence, ambiguous-write recovery, evidence paging, cross-user history
isolation, revoked/cancelled leases, queued corrections, and task deduplication.
The opt-in live model test requires `CANTER_HARNESS_LIVE_MODEL=1` plus the
`CANTER_HARNESS_TEST_API_KEY`, `_MODEL`, and `_BASE_URL` variables. Its test member
has viewer permissions, preventing infrastructure/task mutations.

## Public web search (Exa)

Set `EXA_API_KEY` in the control plane's server environment or ignored `.env`;
restart the API to advertise search. The key is never included in tool schemas,
model input, browser bundles, or provider errors. Migration 024 adds private
source snapshots and a conversation-scoped cache; normal startup applies it.
The existing model provider remains unchanged.

The hosted assistant uses three tools:

- `canter_search_web`: one public query, optional domain/date filters, up to five
  previews of 500 bytes each. Uses Exa `auto`, without generated answers or
  summaries. Equivalent queries reuse a ten-minute conversation cache.
- `canter_open_web`: one public URL, an immutable provider-extracted snapshot of
  at most 128 KiB, and a 500-byte preview. Text stays in PostgreSQL, outside the
  model checkpoint. Pages reuse a 24-hour cache; `refresh` bypasses it and sends
  Exa `maxAgeHours: 0` instead of 24. Retrieval timestamps do not prove crawl or
  publication time. Provider extraction can omit content; reaching the local
  or requested size cap sets `truncated`.
- `canter_read_web`: list source handles (paged), find literal passages across
  saved documents, or read a snapshot in UTF-8-safe 6,000-byte pages. No network
  request or provider charge. Source handles survive restart and refresh.

The durable tool ledger enforces eight searches and eight opens per response,
including failures and cache hits. Limits survive worker recovery and apply to
multi-call batches. Ambiguous paid operations are not automatically repeated.
The provider-reported `costDollars`, request ID, and cache status are recorded in
tool results/events; cache hits report zero incremental cost. Missing provider
cost is null, not an estimated zero. Model token usage is recorded separately.
These are request bounds, not a contractual dollar spending cap.

Outbound requests use only the fixed Exa API host, with a 40-second deadline,
2 MiB response cap, and no HTTP redirects or automatic retries. Public page
fetching is performed by Exa, never the local shell or an arbitrary host HTTP
client. Local names, IP literals, credential-bearing URLs, and non-HTTP ports
are rejected. This validation is not a substitute for Exa's fetch isolation.
Public search queries must not contain private workspace text or credentials.
All returned text is untrusted evidence and conveys no execution authority.

Source access uses the current authorized conversation, and persistence is
fenced by the run lease. Deleting a conversation cascades through its sources
and cache. The UI exposes links from successful document evidence under
“Sources consulted”; the assistant cites claims with ordinary Markdown links.
This verifies source provenance, not the factual entailment of every generated
sentence. Full-page dumping, hidden summarizer calls, and unmeasured token-saving
claims are intentionally absent.

See [the research and tradeoffs](../artifacts/research/search-retrieval-design-2026-09-22.md),
[Exa Search](https://exa.ai/docs/reference/search), and
[Exa Contents](https://exa.ai/docs/reference/get-contents).

Focused verification (use a disposable database: the integration fixture clears it):

```sh
CANTER_TEST_DATABASE_URL=postgres://localhost/canter_test go test ./internal/controlplane -run 'TestOperator(Web|Exa)' -count=1
cd web
node --experimental-strip-types --test tests/operator-web-sources.test.mjs
```
