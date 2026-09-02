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

`postgres-backup.sh` writes a custom-format database archive directly to the
private m1 bucket. Every launch must verify both `pg_restore --list` and one
actual restore into an isolated temporary database before the site is announced.
