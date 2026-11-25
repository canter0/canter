# Repository workflow

- Work on a focused `codex/<description>` branch based on current `origin/main`.
- Preserve existing uncommitted work. Use a separate worktree when the checkout
  contains unrelated changes; never sweep those changes into a commit.
- Commit meaningful increments as work progresses and push checkpoints to GitHub.
  Do not wait until an entire project is finished to save work remotely.
- Stage explicit paths and inspect the staged diff. Keep credentials, generated
  artifacts, caches, and unrelated work out of commits.
- Open a pull request targeting `main`. Use a draft PR while work is incomplete.
- Run checks appropriate to the change, report their actual results, and require
  the `go` and `web` CI checks to pass before merging. Do not bypass failed checks.
- Merge with a merge commit to preserve the individual commits. Do not squash,
  rewrite published history, or push directly to `main` as the normal workflow.
- Commit/push/PR preparation is part of implementing requested work. Merge when
  the user authorizes landing the change. Production releases require authorization
  and must identify the tested commit from `main` and verify live health afterward.
- Read `CONTRIBUTING.md` for the workflow and `deploy/README.md` for deployment.

