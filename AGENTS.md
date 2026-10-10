# AGENTS.md

House rules for agents working in this repo.

## Shipping: work autonomously, never leave a PR open

Jairo does not review PRs. Every session owns its change end to end, from code to production.

1. **Start from the latest `main` and target `main`.** Never branch off, or open a PR against, another unmerged branch or PR (no stacked PRs). If earlier agent work is still open, merge it first or rebase onto `main`.
2. **Integrate it yourself.** If you can push to `main`, push to `main`. If your environment only lets you push your own branch, open the PR as ready for review (not draft), wait for CI, fix it until it is green, and merge it yourself (`gh pr merge <n> --merge --delete-branch`, or your GitHub tool). Do not ask for review and do not finish your turn with your PR open.
3. **Deploy what you changed.** After merging, deploy following this repo's deploy notes (`make install` and restart the service, see README "Build and install the binary"; releases: tag the next `v*` on `main` to run `.github/workflows/release.yml`; `docs/` publishes to GitHub Pages on its own). If your environment cannot reach the server, say so in your final message with the exact command still to run.
4. **Safety net.** `.github/workflows/agent-automerge.yml` merges green `claude/*` and `cursor/*` PRs into `main`. It does not fix red CI and it does not deploy; both are your job.
5. **Only stop to ask** before destructive or irreversible operations: force-push, deleting production data, running a migration that drops data, or rotating or removing secrets.
