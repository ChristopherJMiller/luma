# Luma

A self-hosted agent platform for one household. Picture "Claude.ai plus
Claude Code on the web", running on a home Kubernetes cluster:

- chat with per-person memory and tools;
- agents defined in git;
- Home Assistant in both directions;
- Nix sandboxes;
- shareable artifacts.

The data stays in the house. The model behind each agent can be swapped,
so it can be Claude today and a local model later.

**Status: design approved (2026-10-18). Phase 1 is being planned. There is no
code yet.**

## Shape

```
PWA (assistant-ui) ─┐                   ┌─ conductor (TypeScript): Claude Agent SDK host,
ha-luma (HA) ───────┼─► Luma Core (Go) ─┤   one per open conversation
other clients ──────┘   sessions,       └─► agentgateway ─► Anthropic (local models later)
                        event log, memory, approvals, connections vault,
                        cost ledger, luma-auth (token exchange)
MCP servers act as the signed-in person; each one validates its own audience.
```

| Path | What |
|---|---|
| `core/` | Go. The session API (shaped like Anthropic's Managed Agents), the PWA's backend-for-frontend OIDC login, the event log with replay, model pinning, memory, the cost ledger, and later luma-auth and approvals. |
| `conductor/` | TypeScript. Hosts the Claude Agent SDK. Pinned SDK options, a local HTTP MCP server for memory tools, idle eviction. |
| `web/` | The PWA, built on assistant-ui over Core's WebSocket. |
| `charts/` | The Helm chart, published like patchbay's and pinned by the site repo. |
| `images/` | The runner and sandbox images (nix2container, phase 4) and the eval job. |
| `docs/` | `design.md` (product design), `plans/` (implementation plans per phase). |

Planned layout; directories are created as phases land.

## Where the full design lives

The site repository holds the umbrella design, the research behind it, the
cluster wiring and the phase status:

- Design: `luma-homeops/docs/superpowers/specs/2026-10-18-luma-design.md`
- Entry point and status: `luma-homeops/docs/luma.md`
- Research digest: `luma-homeops/docs/luma/research-2026-10-18.md`

## Ground rules

- **This repo is public.** No secrets and no real family data. Evals use
  synthetic fixtures only.
- Builds and CI run inside the Nix flake: `nix develop -c make …`.
- Releases follow patchbay's pattern:
  - images `ghcr.io/christopherjmiller/luma-*:sha-<commit>`;
  - the chart is pinned to image digests;
  - a `ci-ok` aggregate check gates merges.
- Branding: "Luma, powered by Claude". Agent SDK use falls under
  Anthropic's Commercial Terms, with API keys only.

## License

Apache-2.0. See [LICENSE](LICENSE).
