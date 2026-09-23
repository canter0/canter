# Production deployment

The first Canter deployment intentionally runs outside Canter's own workload
plane. One manually managed host runs Caddy, the standalone Next.js server, the
Go control plane, and PostgreSQL. Customer compute is still created only through
governed Canter executions.

Public routing is same-origin:

- `/`, `/.well-known/canter`, `/llms.txt`, and the dashboard go to Next.js.
- `/v1/*`, `/mcp`, `/healthz`, and `/readyz` go directly to the Go process.
- `/api/canter/*` remains an internal Next.js rewrite for browser calls.
- `/fonts/*` is served by Caddy from `/var/lib/canter-assets/fonts`, outside
  the replaceable Next.js release directory.

Production secrets live only in `/etc/canter/controlplane.env` with root ownership
and group-readable access for the `canter` service account. Never copy the root
repository `.env` to the server. The web process receives only
`CANTER_API_ORIGIN=http://127.0.0.1:8081`,
`CANTER_PUBLIC_URL=https://canter.dev`, `HOSTNAME=127.0.0.1`, and `PORT=3000`.
The explicit public URL prevents agent discovery documents from leaking the
loopback origin seen by Next.js behind Caddy.

Google sign-in requires the OAuth client's authorized redirect URIs to include
`https://canter.dev/api/canter/auth/oauth/google/callback`. The localhost callback
alone does not cover production. Keep `CANTER_PUBLIC_URL=https://canter.dev` and
the matching Google client ID and secret in the control-plane environment.
Check `/api/canter/auth/providers`, then complete a browser sign-in through
Google and verify the return to `/app`; enabled credentials alone do not verify
the callback registration or token exchange.

`postgres-backup.sh` writes a custom-format database archive directly to the
private m1 bucket. Every launch must verify both `pg_restore --list` and one
actual restore into an isolated temporary database before the site is announced.

## Persistent site fonts

Font files are provisioned separately from the public repository and release
archives. Keep the files referenced by `web/src/app/globals.css` in
`/var/lib/canter-assets/fonts` (root-owned directories mode `0755`, files `0644`).
Do not store the only copy under `/opt/canter/web`: release activation replaces
that entire directory. Include the persistent assets in server backups.

After provisioning the fonts, install `deploy/Caddyfile`, run
`caddy validate --config /etc/caddy/Caddyfile`, and reload Caddy. Then run
`python3 scripts/check_fonts.py`. This requests every font referenced by the
stylesheet and validates its WOFF2 header and declared file length; a successful
HTML response is not a font. Release publication and activation verification
both run this check, and `production-status.py` includes the result in health.

When changing fonts, provision the new assets before publishing the release and
retain the previous files for cached pages and rollback. Keep font binaries out
of public Git history and GitHub release archives unless their license permits
redistribution.

## GitHub Actions production releases

After `ci` passes on `main`, `deploy-production` builds the Linux API and static
helper executables, the locked command runtime, and Next standalone server, publishes an immutable `production-<commit>` GitHub
release, and waits for production to report that exact commit. The workflow can
also be run manually for a main commit with successful push CI.

The server fetches releases over HTTPS using `canter-deploy.timer` every three
minutes. Deployment does not require an inbound SSH connection or a laptop on a
specific network. Only the repository's release workflow needs write access;
no production SSH key or provider credentials are stored in Actions.

One-time server setup (root):

```sh
install -m 0755 deploy/pull-release.py /opt/canter/deploy/pull-release.py
install -m 0644 deploy/canter-deploy.service deploy/canter-deploy.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now canter-deploy.timer
```

The updater verifies artifact checksums, safely extracts the archive, takes a
PostgreSQL backup and tests a restore into an isolated database before activation.
It retains the previous web directory, API and static helper executables, command
runtime symlink, and service units, and restores them when readiness checks fail.
Command runtimes live under `/opt/canter-harness/releases/<commit>`, with a
managed `current` symlink. The CI artifact contains locked npm dependencies;
production activation never installs packages or executes npm scripts as root.
The API checks the command socket at startup. Set `CANTER_STATIC_SERVER_BINARY`
to `/opt/canter/bin/canter-static-linux` so static deployments use the release
helper. Install this version of the updater before the first bundled-runtime
release; subsequent releases update the runtime automatically. Schema changes must remain backward compatible; rollback
does not restore the production database and discard live writes. A failed commit
is recorded in `/var/lib/canter-deploy/failed` to prevent repeated failed attempts.
Investigate `journalctl -u canter-deploy.service` before removing that marker.
Release backups are retained under `/opt/canter/releases/actions-<commit>`;
monitor disk capacity and prune old releases only after verifying backup retention.

## Private production access with Tailscale

Tailscale is the default administrative network. Both this Mac and the
`canter-production` server already belong to the existing tailnet. The local
SSH alias `canter-production` points to its private Tailscale address. Normal
OpenSSH key authentication runs over Tailscale; this does not require enabling
Tailscale SSH or changing tailnet permissions.

Start production work with the read-only check:

```sh
python3 scripts/production-status.py
```

The check requires a running Tailscale client and an online production peer,
rejects public-IP/proxy fallback, preserves SSH host-key verification, checks
service/readiness status over private SSH, and compares the active release with
the public HTTPS release. It also reports the local commit and uncommitted work,
so local results are not mistaken for production evidence.

If Tailscale reports `NeedsLogin`, sign in to the existing tailnet and retry. If
the peer is offline, diagnose that connection; do not reopen public SSH. A DERP
relay connection is usable for administration when direct UDP is blocked. Test
an actual SSH command on the network in question; a ping alone is not proof.

For an interactive administrative session after the check:

```sh
ssh canter-production
```

Keep automatic release delivery independent of the laptop: CI publishes the
release, the server pulls it over HTTPS, and Actions verifies public activation.
Use Tailscale for diagnostics and authorized configuration. Do not add a second
SSH-based deployment path merely to use Tailscale in the Actions workflow.

The access mechanism follows the official [Tailscale SSH documentation's
explanation of ordinary SSH over Tailscale](https://tailscale.com/docs/features/tailscale-ssh).
