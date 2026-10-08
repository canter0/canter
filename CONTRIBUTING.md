# Development workflow

`codex/<change>` → frequent commits and pushes → pull request → passing CI → merge into `main` → production release.

Start each focused change from the latest `origin/main`. If another task has
unfinished changes in the current checkout, create a separate worktree rather
than moving or committing its work.

Make small, meaningful commits at working checkpoints and push them regularly.
Open a draft PR when saving work that is not ready to merge. Describe the resulting
behavior, validation performed, and any remaining limitations in the PR.

Before merging, the `go` and `web` checks must pass. Merge using **Create a merge
commit** so the branch's individual commits remain in the default branch history.
Land changes through PRs rather than direct pushes to `main`.

GitHub contribution credit depends on commits reaching the default branch and
using an author email associated with your GitHub account. Keep authentic commit
dates and authorship; save actual progress rather than empty activity commits.
See [GitHub's contribution reference](https://docs.github.com/en/account-and-profile/reference/profile-contributions-reference).

## Production

Merging into `main` currently does **not** deploy Canter automatically. The
production host is manually managed as described in [deploy/README.md](deploy/README.md).
Release an explicitly authorized, tested commit from `main`; record its SHA,
retain a rollback target, and verify `/readyz` and the affected public behavior
before reporting the release as live.

An automatic deployment workflow must be wired to Canter's actual host and
protected deployment credentials before enabling it. Autodisc's host-specific
deployment workflow is not interchangeable with Canter's.
