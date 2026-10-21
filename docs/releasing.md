# CI, dependency updates and releases

## What runs, and when

Everything in `.github/workflows/ci.yml` runs inside the repo's flake
(`nix develop -c make ...`), so a red job reproduces locally with the same
`make` target.

| Job | What it proves | Runs on |
|---|---|---|
| `check` | actionlint, `pnpm install --frozen-lockfile`, `go mod verify`, tool versions, hygiene (no binaries, keys or secrets; gitleaks over all history), toolchain and SDK/CLI pin agreement, lint, unit tests, govulncheck | every push, PR, weekly |
| `ci-ok` | every job in its `needs` succeeded | always (the required check) |

Later tasks add `gateway-spike` (Task 3), `sdk-spike` (Task 4), `image`
(Task 25) and `e2e` (Task 28a), each also listed in `ci-ok`'s `needs`. Publish
and release (Task 29) are never in `ci-ok`.

`renovate-config.yml` validates `renovate.json` whenever it changes. The weekly
run (Mondays 06:17 UTC, on main) catches what changes without a commit of ours:
a new vulnerability report, a pinned digest that disappeared, a runner image
that moved.

## The main ruleset

`main` should have one repository ruleset: the status check **`ci-ok`** is
required, no force pushes, no deletion. Repository admins bypass it, so a
maintainer can push straight to main (that push gets the same CI run).

## Renovate

Renovate reads `renovate.json`.

- **Merges itself:** every non-major update once `ci-ok` passes (GitHub does
  the merge), and every vulnerability fix at once. Go modules, npm packages,
  actions and images wait 3 days after a release; npm versions are pinned
  exactly.
- **Waits for a human:** major updates (label `major`); the weekly groups
  `assistant-ui` and `agentgateway`; and `agent-sdk` (the Agent SDK and the
  Claude Code CLI, Mondays before 06:00, label `eval-gate`), which is merged
  only after the evals have run. `hack/check-versions.sh` requires SDK `0.3.N`
  and CLI `2.1.N` with the same N.
- **Never proposed:** a new Go or Node minor (flake, `go.mod` and Dockerfile
  move together, by hand), our own `ghcr.io/christopherjmiller/luma-*` images,
  kind node images.

## Releases

Written with Task 29 (publish and release jobs, the first release candidate).
