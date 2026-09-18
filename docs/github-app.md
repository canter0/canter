# Canter GitHub App

Registered on 2026-09-18 under `canter0`:

- Name: **Canter Deploy** (`Canter` is reserved for another GitHub account).
- Public page: https://github.com/apps/canter-deploy
- Settings: https://github.com/organizations/canter0/settings/apps/canter-deploy
- App ID: `4989517` (public identifier, not a credential).
- Homepage: https://canter.dev
- Requested repository permissions: Contents read-only and mandatory Metadata read-only.
- Organization, account and enterprise permissions: none.
- Available for installation by other accounts; each installation grants its own repository selection.
- User token expiration enabled, wildcard callbacks and device flow disabled.
- Webhooks disabled until a verified receiving endpoint is implemented.

## Current status

Registration and both GitHub App / OAuth app horse logos are verified. Renaming the GitHub App to lowercase `canter` was attempted and rejected by GitHub because the name is reserved for `@canter`.

The GitHub App client secret is saved in the ignored `.secrets/github-app.env` and `.env` (mode 0600), separate from existing sign-in OAuth credentials. The app is installed on `canter0` with only `canter0/canter` selected, read-only Contents and Metadata (installation `162743326`). GitHub also permits reads of public repositories.

## Implemented repository flow

- Dedicated `github-app` authorization provider and `/api/canter/auth/oauth/github-app/callback`. It cannot be used for sign-in or identity linking; existing GitHub/Google sign-in remains separate.
- Browser-bound OAuth state and PKCE, account/workspace binding, encrypted access and refresh tokens, and serialized renewal before access-token expiry.
- Migration `021_github_app_connections` preserves legacy repository connections. Existing connections remain usable; reconnect chooses the App when configured.
- Repository picker in chat, mention picker, and account settings use the App when enabled. Select repositories opens GitHub installation settings; refresh reloads available repositories.
- Local startup reads `CANTER_GITHUB_APP_CLIENT_ID`, `CANTER_GITHUB_APP_CLIENT_SECRET`, and `CANTER_GITHUB_APP_SLUG`. Never overwrite the legacy `CANTER_GITHUB_CLIENT_ID` / `CANTER_GITHUB_CLIENT_SECRET`.

The production callback `https://canter.dev/api/canter/auth/oauth/github-app/callback` is registered, and its implementation is present locally. **The new implementation and credentials have not been deployed to production.** A second callback, `http://localhost:3000/api/canter/auth/oauth/github-app/callback`, is registered for local verification.

## Verification on 2026-09-18

- Go control-plane/entrypoint tests, web lint, TypeScript, and diff whitespace checks passed.
- Focused GitHub/OAuth integration tests passed against a separate PostgreSQL database, including callback isolation, source reads, revocation, and concurrent token rotation.
- A real GitHub App authorization returned to the local Canter workspace, displayed repositories, and preserved an unsent composer draft.
- A real source inspection of `canter0/canter` returned HTTP 200. Forcing the local preview connection into its renewal window successfully refreshed the real GitHub token.
- The temporary local preview account's repository connection was disconnected after verification.

## Remaining production work

Deploy the implementation with its three GitHub App environment variables, run migration 021, and verify the HTTPS callback on `canter.dev`. Private repository access was covered by mocked integration tests but has not been exercised against a real private repository.

Two private keys were generated during an earlier browser download attempt (GitHub key IDs `4515985` and `4516037`), but neither file could be located locally. This user-token flow uses the client secret and does not require a private key. Installation-token or bot automation remains a separate capability; unused keys should be removed or replaced when that capability is configured.

GitHub reference: https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app

## Branding

Both app logos use `artifacts/branding/canter-github-logo-padded.png`, rendered from the existing horse favicon. The padding prevents GitHub crop controls from clipping the horse. The OAuth logo was visually checked after saving.
