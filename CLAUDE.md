# CLAUDE.md: Luma operating guide

Luma is a household agent platform, shipped as one Helm chart: a Go **Core**
(sessions, auth, memory, cost ledger), a TypeScript **conductor** that hosts the
Claude Agent SDK, and an assistant-ui **PWA**. Product name in the UI: "Luma,
powered by Claude", never "Claude Code". The short public design is
[`docs/design.md`](docs/design.md), which links the full spec; the
phase-1 plan is `docs/plans/phase-1-lean-chat.md` (read a task there before
changing the code it covers). This repo is public and Apache-2.0.

## Architecture

```
browser (PWA, cookie session)
   -> Core (Go, :8080)     the ONLY owner of state: conversations, events,
                           memory, taint, cost ledger, retention. Postgres.
        internal callers authenticate with projected ServiceAccount tokens
        (TokenReview): conductor, evals, reconcile (P20)
   <-> conductor (TS)      one long-lived link to Core (newest link wins, P30);
                           a pool of Agent SDK query()s, one CLI per active
                           conversation (cap 4, evicted after 10 min idle).
                           Executes; never decides, never stores.
   -> agent gateway        agentgateway in front of Anthropic: sees every
                           request, alerts on main-class 400s
   -> smokescreen          egress proxy for WebFetch (HTTPS_PROXY)
```

Layout: `core/` (Go), `conductor/` and `packages/` and `web/` (pnpm workspace),
`agents/` (AGENT.md files, `models.yaml`), `charts/luma/`, `test/`, `hack/`.
Product code from here on follows the plan's "Repository layout".

## Commands

Everything runs inside the flake: `nix develop -c make <target>`.

| Target | What it does |
|---|---|
| `tools-versions` | print every tool's version |
| `fmt`, `vet`, `lint` | gofmt/goimports, go vet, golangci-lint + `helm lint --strict` over `charts/luma/ci/*-values.yaml` |
| `test`, `test-go`, `test-ts` | `go test -race -count=1 ./...`; `pnpm -r test` (never runs spike files) |
| `check-hygiene`, `check-versions`, `check-chart-sync` | repo hygiene; toolchain and SDK/CLI pin agreement; chart copies of `agents/` |
| `vulncheck` | govulncheck |
| `build`, `images`, `chart-test` | build, images, chart render tests |
| `e2e`, `e2e-up`, `e2e-down` | the kind suite and its cluster |
| `gateway-spike`, `sdk-spike`, `transcript-fixture` | Spike G, Spike S, the synthetic CLI transcript |
| `fakeanthropic` | build `bin/fakeanthropic` (Spike S and the conductor's integration tests run it) |

Targets that later tasks fill print "not yet implemented (Task N)" and succeed.
CI (`.github/workflows/ci.yml`) runs `make` targets in the same shell; `ci-ok`
is the one required check. Publish and release are never in `ci-ok`.

## Deviations from the design

Decided in the phase-1 plan; the design doc does not describe them.

- **P17.** AGENT.md gains `builtin_tools` (a subset of `WebSearch`,
  `WebFetch`), `kind: agent|subagent` and optional `effort`. The spec's
  frontmatter has no place for built-in tools.
- **P19.** Evals use the real WebSearch tool name, stubbed by the eval
  recorder. WebFetch is denied in golden tasks; the `guard` suite drives
  scripted WebFetch uses at forbidden targets at no Anthropic spend. The
  large-session and previous-CLI resume tests run offline in CI against
  fakeanthropic. No Anthropic key exists in GitHub (D30).
- **P20.** Internal callers authenticate with projected ServiceAccount tokens
  and TokenReview; principals are `conductor`, `evals`, `reconcile`. No static
  Core bearer exists. Pulled forward from phase 2.
- **P25.** Core, the conductor, the gateway and smokescreen run at priority 0
  (no class); the evals and reconcile Jobs use `luma-batch` (-5,
  `preemptionPolicy: Never`). A deviation from section 7.3's -5 for the control
  plane: shedding is explicit (`shed`), so serving is not first to go.
- **P28.** Phase 1 ships no mid-session effort control. Escalation is
  `deep-think` or a new chat on Smart. Live spike L4 and Spike S15 record
  whether a per-message effort change preserves the cache, for phase 2.
- **P9, taint is per session.** `sessions.tainted_at` is set by a
  WebSearch/WebFetch `tool.started`, an attachment, or the conductor's flag. It
  is never cleared, forks inherit it, and while it is set every remember **and**
  forget is held for the person's confirmation. The spec (section 12.2) holds
  only writes made in a turn that read untrusted content; injected text stays
  in context for every later turn, so a per-turn flag would let a write two
  turns later apply silently.
- **Usage reconcile groups by workspace.** `luma-core usage-reconcile` reads
  the Admin API cost report with `group_by[]=workspace_id`, not the spec's
  `group_by=api_key_id` (section 5.2.5). The ledger records the workspace
  (`api_workspace`: `interactive`, `ci`), which C10 maps to `luma-interactive`
  and `luma-ci`, so the two compare directly.
- **AGENT.md `limits` gains web-search caps.** C5's `limits` adds
  `web_search_per_turn` and `web_search_per_session` (3 and 15) to the spec's
  `max_turns` and `max_tool_calls` (section 11). One WebSearch call can run up
  to 8 searches, so a call count alone does not bound search spend.
- **Smart and deep default to `medium` effort.** `models.yaml` keeps `low`
  for the fast tier only; the spec says `low` for chat (section 6). This is
  the plan's C5 choice; Spike S15 and live spike L4 inform any change.
- **The context fallback is the conductor's, and it is taken.** Spike S5
  showed that an SDK SessionStart callback never runs for `startup`, `resume`
  or `fork` (it does for `compact`), so `CONTEXT_DELIVERY = 'first-user-turn'`:
  the fenced profile opens a fresh session's first user turn and is
  re-injected after each compaction by the conductor (Task 19b), not by Core
  (section 6): only the conductor sees the SDK's `compact_boundary`.
- **Spike S's other taken branches** (`conductor/src/spike-results.ts`,
  `docs/spikes/phase-1.md` section S):
  - **Model switches are read from replies.** The refusal fixture switches no
    model and `PostModelSwitch` fires only for `setModel()`, so
    `MODEL_SWITCH_SIGNAL = 'assistantModelField'`: Task 19a emits
    `model_switched` when a main-loop reply's `message.model` differs.
  - **Search counts come from the result.** `WEB_SEARCH_COUNT_SOURCE =
    'resultDelta'` (S17): PostToolUse counts each WebSearch call as 1 and
    `turn_result` sets the true count, so the worst case is 24 searches a turn
    and 36 a session (S21, Global Constraints).
  - **`precomputeCompactionEnabled` is dropped.** It is in the SDK's
    `Settings` type but changed no request (S7), so Task 16's inline settings
    are `{switchModelsOnFlag: true, idleCompaction: false}`.
  - **Dropped thinking is counted.** The SDK exposes `input_transformations`
    (S16), so Task 16 sets `ANTHROPIC_BETAS=thinking-binding-controls-2026-08-01`
    and Task 19a counts `luma_conductor_thinking_dropped_total`.
  - **Cold resumes are inferred.** Nothing in band marks one
    (`SLO_SIGNALS.coldResume = null`); the conductor compares the idle gap
    with the cache TTL (Task 19b).
- **`deep` is hidden in the PWA.** The new-chat menu offers Fast and Smart
  only. The spec lets admins pick `deep` (section 5.1), and Core still accepts
  it from an admin through the API; escalation in the UI is `deep-think`
  (P28).

## Hard rules

1. **Toolchain from the flake only.** Bump nixpkgs with `nix flake update
   nixpkgs`, then run `make tools-versions` and `make test` before committing
   the lock. Keep the Go and Node minors in step across `flake.nix`, `go.mod`
   and the Dockerfile (`make check-versions` fails otherwise).
2. **Kubeconfig isolation.** Never read or write `~/.kube/config`, and never
   touch any cluster other than the e2e kind cluster. Its kubeconfig lives only
   at `.e2e/kubeconfig` (gitignored; `$LUMA_E2E_KUBECONFIG` in the shell), made
   with `kind create cluster --name luma-e2e --kubeconfig .e2e/kubeconfig`.
   Pass that file explicitly to every kubectl, helm and client-go call. At most
   one kind cluster exists, named `luma-e2e`; delete it when done.
3. **No secrets, no real family data, no Anthropic key in the repo, ever,
   encrypted or not.** It is public. Fixtures are synthetic. `.gitignore`
   blocks secret, key and archive types; do not work around it (`git add -f`).
   `make check-hygiene` (CI runs it) fails on a tracked binary, a file over 512
   KiB, a forbidden type, or a secret anywhere in the history (gitleaks).
4. **Pinned images.** Every image, test images included, is pinned by tag plus
   digest (`golang:1.26.8-trixie@sha256:...`).
5. **Git hygiene.** Small, logical commits. Stage explicit paths, never `git add
   -A` or `git add .`. Never bypass hooks (`--no-verify`) or signing; if a hook
   fails, fix the cause.
6. **Evidence before claims.** Never say something works without running it.
   Report failures and deviations from the plan plainly.
7. **Metadata-only logging.** Core and the conductor log ids, tool names,
   durations and decisions. Never message content, memory content or tool
   arguments. CLI stderr is not forwarded. WebFetch appears in logs and metrics
   as host only. Session ids never become metric labels.
8. **Hand-written test expectations.** No snapshot files, no `-update` flags,
   no golden output generated by the code under test: a test whose expected
   value comes from the code under test proves nothing. One carve-out: spike
   tests (`test/gateway`, `conductor/spike`) are **change detectors**. Their
   constants are typed by hand from a reviewed observation, never written by
   the test, and the test fails when the pinned SDK, CLI or gateway stops
   behaving that way.
