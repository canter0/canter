# Workspace agent

The dashboard starts a persistent conversation with the hosted Canter agent. It uses the control plane's `OPENROUTER_API_KEY`; the browser never receives that key. The default model is `openai/gpt-5.6-luna`. Override it with `CANTER_OPERATOR_MODEL` when needed.

## Run locally

With the repository's `.env` configured and PostgreSQL running:

```sh
CANTER_DEV_WEB_PORT=3000 CANTER_DEV_API_PORT=8081 ./scripts/dev.sh
```

This builds both the current control plane and its trusted Linux static server, then starts the web app and API together. Sign in at `http://127.0.0.1:3000/app` using a local account. The normal `.env` database is used. The configured GitHub/Google OAuth callbacks must match this origin; password sign-in is also available.

## What runs

- Real streamed model responses and multi-turn conversation history.
- Workspace-scoped reads for apps, deployments, changes, agent access, audit activity, and billing.
- Native billing, app, deployment, and change views opened alongside the conversation.
- Public and connected private GitHub repository inspection at an immutable commit, plus bounded file reads.
- Packaging and uploading static websites, creating an immutable initial deployment proposal, and showing the real approval UI.
- Existing governed Change tools for deployed applications.

Static preparation accepts a checked-in `index.html` at the repository root or a selected output directory. It does not run repository build scripts on the control-plane host. Hosted source builds are not implemented. Payments require configured Stripe billing and the human checkout UI; the agent cannot initiate checkout or grant itself infrastructure approval.

## Persistence and authority

Conversations are private to their account and workspace. A conversation can have one active response at a time. Requests carry an idempotency key. A worker leases a durable run, checkpoints model messages, and records tool results before continuing. Closing a browser does not cancel a run; reopening replays persisted events.

Cancelled or superseded workers cannot append events, checkpoint, or finish a run. On interruption, completed tools return their saved result. A write with an uncertain outcome is not automatically repeated. Membership and the hosted agent's grant are checked between operations. The agent uses its own installation and session, never the user's human principal.

Deployments remain proposals until the authenticated user approves the exact digest through the native UI, or an existing bounded standing policy authorizes an eligible Change.

## Verification

`operator_integration_test.go` covers actual PostgreSQL persistence, isolation, membership changes, revocation, idempotency, cancellation, stale-worker fencing, interrupted writes, and multi-turn tool protocol. These tests use `CANTER_TEST_DATABASE_URL` and truncate that database; use an isolated database.

The live browser workflow also uses the configured external model, real GitHub reads, actual artifact storage, and stored deployment proposals. It must leave infrastructure proposals awaiting human approval unless deployment was explicitly authorized.
## GitHub inside the workspace

An unspecific deployment request opens the GitHub connection panel in the current
conversation. Users authorize the GitHub App when configured, return to the same
conversation, choose a repository, and continue with the hosted agent. Public
repository URLs work without connecting. Unsent conversation drafts survive the
round trip, and selecting a repository does not overwrite them.

Repository authorization is separate from GitHub sign-in. The GitHub App requests
read-only code and metadata access, with private repository selection managed on
GitHub. Public repositories are also readable. Legacy OAuth repository connections
remain supported until users reconnect through the App.

The App callback is `/api/canter/auth/oauth/github-app/callback`; legacy OAuth
uses `/api/canter/auth/oauth/github/callback`. PKCE,
single-use state, browser binding, signed-in account binding, and workspace
membership are checked before storing the credential. Tokens are encrypted with
AES-GCM and bound to the account and workspace. The encryption key derives from
the respective provider client secret with domain separation; rotating that secret requires
reconnecting repository access. Tokens are never returned to the browser, model,
conversation, or audit event. Disconnect removes the local connection for this
user and workspace. GitHub-side revocation and expired credentials prompt a
reconnect. App refresh tokens are separately encrypted; concurrent refreshes are
serialized in the database to prevent reusing a rotated token.

Private archive downloads authenticate only to `api.github.com`; the subsequent
signed archive redirect is restricted to HTTPS `codeload.github.com` and receives
no Authorization header. Repository contents remain untrusted input. Hosted
preparation currently supports static websites and checked-in static build output;
it does not execute arbitrary package install or source build commands.

## Conversation and code views

The hosted model defaults to `openai/gpt-5.6-luna` through OpenRouter, with explicit `none` reasoning for Chat Completions tool calls. The composer displays the server configuration, rather than an unrelated model preference.

Every turn replays its public preamble, nearby tool activity, and final answer from durable events. Work expands while running and collapses after completion. GitHub connection and repository selection appear inline; resource results automatically open a toggleable right panel. The plus menu opens real workspace context, and drafts survive navigation and GitHub connection.

Source reads open syntax-highlighted, line-numbered files at an immutable commit. `canter_show_repository_changes` compares two exact GitHub commits and opens a red/green diff. Both views use the signed-in member's server-held repository connection; they do not expose credentials or borrow another member's access. Binary or oversized patches are identified as unavailable. These are repository reading and comparison capabilities, not source editing.

Focused presentation checks: `node --test web/tests/operator-presentation.test.mjs`. Database integration checks must use an isolated `CANTER_TEST_DATABASE_URL`; the integration fixture truncates its database.
