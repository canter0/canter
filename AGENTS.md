# Production operations

Use Tailscale for Canter production administration. Start with
`python3 scripts/production-status.py`; it verifies the tailnet peer, private SSH,
service health, and the live release. The existing SSH alias is
`canter-production`. Do not fall back to public SSH or open a public firewall
rule when Tailscale is offline. Reconnect the existing tailnet first.

GitHub Actions and Tailscale serve different purposes: successful CI on `main`
publishes a release, and production's `canter-deploy.timer` pulls it over HTTPS.
Normal deployment does not need a laptop connection or manual SSH activation.
Use private SSH for inspection, diagnostics, and authorized server configuration.
See `deploy/README.md` before changing the release path.

Before preparing a release, fetch `origin` and inspect the difference from
`origin/main`. An older working branch may not contain the current deployment
workflow. Preserve existing automation and the user's uncommitted work. Verify
the tested commit against public `/release.json` and `/readyz` before reporting
that a deployment succeeded. Never print secrets or copy the local `.env` into
production.
