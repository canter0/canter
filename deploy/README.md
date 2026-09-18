# Production deployment

The first Canter deployment intentionally runs outside Canter's own workload
plane. One manually managed host runs Caddy, the standalone Next.js server, the
Go control plane, and PostgreSQL. Customer compute is still created only through
governed Canter executions.

Public routing is same-origin:

- `/`, `/.well-known/canter`, `/llms.txt`, and the dashboard go to Next.js.
- `/v1/*`, `/mcp`, `/healthz`, and `/readyz` go directly to the Go process.
- `/api/canter/*` remains an internal Next.js rewrite for browser calls.

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

## GitHub Actions production releases

After `ci` passes on `main`, `deploy-production` builds the Linux executable and
Next standalone server, publishes an immutable `production-<commit>` GitHub
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
It retains the previous web directory and executable, and restores them when
readiness checks fail. Schema changes must remain backward compatible; rollback
does not restore the production database and discard live writes. A failed commit
is recorded in `/var/lib/canter-deploy/failed` to prevent repeated failed attempts.
Investigate `journalctl -u canter-deploy.service` before removing that marker.
Release backups are retained under `/opt/canter/releases/actions-<commit>`;
monitor disk capacity and prune old releases only after verifying backup retention.
