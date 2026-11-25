# Personal API keys

Personal API keys let a human account authenticate CLI, SDK, and script
requests without a browser session or an agent installation. Scope: personal
account keys in Settings → API → API keys. Not organization keys, not provider
secret management, not agent pairing, not deployment.

## API

| Route | Auth | Behavior |
|---|---|---|
| `GET /v1/me/api-keys` | cookie only | `{ "keys": [...] }` — active keys and recent history, up to 100 (active keys always included since the cap is 50) |
| `POST /v1/me/api-keys` | cookie only + recent auth | `{"key": <metadata>, "secret": "canter_pk_…"}` (201); secret shown once |
| `DELETE /v1/me/api-keys/{id}` | cookie only | 204; idempotent for own revoked key; foreign/missing → 404 |
| `GET /v1/me` | cookie, or `Authorization: Bearer canter_pk_…` | account + workspaces + onboardingComplete |

Management is **browser-only**: `h.human` rejects any nonempty `Authorization`
header, so a valid key (or agent token) can never mint, list, or revoke keys,
touch `/v1/auth/*`, or approve agent devices — even alongside a valid cookie.
Unsafe management requests require a trusted Origin and are rate-limited at
`api-keys-account` 30/10m. `POST` requires step-up auth (428 → confirm
identity at `/sign-in?reauth=1&next=…`) and revalidates the session inside
the account row lock before insert. `DELETE` does not need recent auth,
matching session sign-out usability.

### Token format and storage

`canter_pk_` + 32 random bytes as unpadded base64url (53 chars total).
Only the SHA-256 hash and an 18-char display prefix are stored; the plaintext
never reaches the database, logs, security events, or error messages.

### Permission semantics

A personal key is a **full-fidelity credential, not a scoped token**: it acts
as the owning account with the account's *current* role in every workspace it
can reach — reads and writes included, wherever existing policy allows the
human. Enforcement is live per request:

- `expiry` (required, 1–365 days, default 90) and `revoked_at` are checked on
  every resolve; revoked/expired keys → 401.
- `auth_version` binds the key to the account's credential epoch, and the
  central credential-invalidation transaction also marks keys revoked:
  password/security credential changes invalidate keys. Signing out a
  browser session or other browser sessions does not revoke API keys;
  revoke them separately in Settings > API > API keys.
- Disabled or unverified accounts → 401. Membership is checked per request,
  so losing a workspace role (or membership) takes effect immediately;
  `/v1/me` still resolves the account itself.
- The principal's `SessionID` is empty — the key is not a forged human
  session — and `Principal.APIKey` records provenance. `last_used_at` is
  touched at most once a minute.
- Malformed bearer input never falls back to an ambient cookie; non-Bearer
  `Authorization` values are rejected outright.

Account cap: 50 active keys, enforced under the account row lock.
`API key created` / `API key revoked` security events are written in the same
transaction as the change (revocation emits exactly once).

## CLI

```sh
export CANTER_API_KEY=canter_pk_…
canter auth whoami                      # account + workspaces via GET /v1/me
canter sandbox list --workspace wrk_…   # workspace is explicit for personal keys
canter ci status --workspace wrk_…
```

- `resolveAPICredential` precedence: ambient `CANTER_API_KEY` → ambient
  `CANTER_AGENT_ACCESS_TOKEN` → env-file `CANTER_API_KEY` → env-file
  `CANTER_AGENT_ACCESS_TOKEN` (`--env-file`). Blank values ignored; malformed
  tokens fail with a generic error. No raw-key flags; nothing persisted.
- Personal keys never guess a workspace: pass `--workspace` or set
  `CANTER_WORKSPACE_ID` (env or env-file). Agent tokens still infer their
  installation workspace via `/v1/agent/whoami`.
- `canter auth whoami` accepts only a personal key — the agent endpoint
  cannot serve account identity, so there is no agent fallback.
- `canter agent *`/`refresh` env names unchanged; flag defaults no longer
  leak ambient credentials into `--help`.

## SDKs

- **Go** `sdk/remote`: `client.Me(ctx, apiKey)` → `AccountIdentity{Account, Workspaces, OnboardingComplete}`.
- **Python** `canter_sandboxes.SandboxClient.me()` and **TS**
  `SandboxClient.me()` — same endpoint; constructors already take the token
  string/callback (`os.environ["CANTER_API_KEY"]`, `process.env.CANTER_API_KEY`).
- No key create/revoke in SDKs — management is intentionally browser-only.
- `SandboxClient` accepts either credential; agent tokens cannot call `/v1/me`.

## Web

`/app/account/api-keys` (Settings → API → API keys): name + expiry
(30/90/365, default 90) create form, one-time secret reveal (copy/select only,
in-memory until Done/unmount — never storage or URL), list with
created/expires/last-used and revoked/expired badges, inline revoke
confirmation, 428 → link to `/sign-in?reauth=1&next=/app/account/api-keys`.
