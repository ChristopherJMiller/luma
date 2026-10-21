# Luma: product design

Luma is a self-hosted household agentic assistant. It aims to be "Claude.ai
plus Claude Code on the web, for this household": the data stays in the home
cluster, every tool has a declared owner and scope, and the model behind any
agent can be swapped. The product name in the UI is "Luma, powered by Claude".
Users are told they are talking to an AI.

This page is the short public overview. The full design (every phase, every
decision and the research behind them) is the
[design spec](https://github.com/ChristopherJMiller/luma-homeops/blob/main/docs/superpowers/specs/2026-10-18-luma-design.md)
in the public `luma-homeops` repo. This page does not copy it; where it says
"spec section N", that is where to look.

## Components

Luma ships as one Helm chart with three first-party parts and two supporting
services.

- **Core (Go).** The only owner of state and the trust anchor. It is the
  backend-for-frontend for the browser (OIDC login, an `HttpOnly` cookie
  session), and it owns conversations, the append-only event log, the
  conversation tree, memory, the cost ledger, maintenance mode and retention.
  It stores everything in Postgres.
- **Conductor (TypeScript).** Hosts the Claude Agent SDK. It executes turns
  and never decides or stores anything: one long-lived link to Core, and a
  small pool of SDK `query()` sessions, each backed by a local Claude Code CLI
  subprocess with no shell or file tools.
- **PWA (assistant-ui).** The chat client for phone and laptop, served by Core.
- **Agent gateway.** Sits in front of Anthropic. It holds the real API keys,
  sees every model request, and alerts on malformed ones.
- **Egress proxy.** Web fetches leave the conductor only through this proxy.

```
browser (PWA, cookie session)
   -> Core (Go)            sessions, events, tree, memory, taint, ledger
        <-> conductor (TS) Agent SDK host; CLI subprocess per open chat
                -> agent gateway -> Anthropic
                -> egress proxy  -> the web (WebFetch only)
   Core -> Postgres
```

## Trust boundaries

From the spec's security model (section 16), trimmed to what exists in phase 1:

1. **Clients.** Untrusted. No tokens live in the browser, its storage or URLs.
2. **The edge.** TLS and routing only.
3. **Core.** The trust anchor. Identity comes from signed tokens and the
   session cookie, never from forwarded headers or tool arguments.
4. **The conductor.** Semi-trusted. It parses model output and tool results,
   and it holds no user credentials. It sees an opaque per-person reference,
   never a name.
5. **Conductor-local CLIs.** They make model-chosen tool calls, with no shell.
6. **Downstream services.** The gateway and the egress proxy each enforce
   their own limits. Later phases add MCP servers and sandboxes (spec
   sections 7 and 9), which are out of scope here.

Prompt injection is handled by treating memory as data and tracking taint:
once a session has read untrusted content (web results, an attachment), every
memory write in it waits for the person's confirmation.

## Phase 1 scope

Phase 1 is "lean chat" for one person, the owner, on an Android phone and a
laptop:

- streaming answers with web search and fetch;
- a per-person memory profile that the person can edit, with `remember` and
  `forget` tools;
- image, camera and PDF attachments;
- a history list, with edit and regenerate as branches of the conversation;
- fast and smart model tiers, mapped through site config;
- a cost ledger with a monthly cap and a warning, reconciled nightly against
  Anthropic's usage report;
- incognito chats and delete, with retention disclosed in the UI.

## Non-goals for phase 1

- Other users: relatives and the rest of the household come in later phases.
- Sandboxes, a Nix dev environment and computer use (spec section 7).
- MCP servers, Home Assistant, Gramps and approvals (spec sections 9 and 10).
- Shareable artifacts (spec section 12.3).
- Local models (spec section 15).
- Mid-session effort control.

## Request path

1. The PWA sends a message to Core with its session cookie. Core checks the
   cookie and the `Origin`, applies the budget and maintenance gates, and
   appends the user message to the event log.
2. Core hands the turn to the conductor over the internal link, with the
   person's memory snapshot, fenced as data, and the current time.
3. The conductor runs the turn in the conversation's CLI. Model requests go
   through the gateway to Anthropic; web fetches go through the egress proxy.
4. The conductor streams events back to Core. Core assigns each event a
   monotonically increasing id, stores it, and fans it out to connected
   clients.
5. A client that drops reconnects with `since=<id>` and replays what it
   missed. The turn runs to completion on the server whether or not a client
   is connected.
6. When the turn ends, Core records the final message and the cost delta.

## Retention

Deleting a conversation removes its whole tree from live storage immediately.
An incognito chat reads and writes no memory, and its transcript is deleted 24
hours after the chat goes idle. Anthropic's API keeps its own copy of requests
under its retention policy; the UI says so.

## Decisions

Where phase 1 departs from or refines the spec, the reasoning is recorded in
the [phase-1 plan](plans/phase-1-lean-chat.md), under "Decisions this plan
makes" (P1 onward). Deviations that bind the code are also listed in
[CLAUDE.md](../CLAUDE.md).
