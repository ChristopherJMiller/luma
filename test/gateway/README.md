# Spike G: agentgateway passthrough

Spike G (plan Task 3; spec section 17, row 1, offline half) checks that
agentgateway v1.6.0, run standalone with Luma's config, passes Claude Code
CLI traffic to the Anthropic Messages API faithfully. It runs the pinned
image in front of [fakeanthropic](../fakeanthropic/README.md). The results
are in [docs/spikes/phase-1.md](../../docs/spikes/phase-1.md), section G.

```
nix develop -c make gateway-spike
```

It needs Docker. The gateway runs on the host network, so host ports 8080,
15000, 15020 and 15021 must be free. A run takes about 4 minutes, most of it
the 90-second long-stream test and the fake's 2-second SSE step delay. CI
runs it as the `gateway-spike` job, which `ci-ok` requires.

| File | What it is |
|---|---|
| `agentgateway.yaml` | the canonical gateway config |
| `result.go` | the spike's recorded results: change detectors, typed by hand |
| `gateway_test.go` | the spike (build tag `gateway`) |

## The canonical config

`agentgateway.yaml` is the one gateway config. Task 26b renders it into the
chart and `test/chart` diffs the two; the deploy plan's live spike runs it
against the API. A consumer changes two things in its own copy, and nothing
else.

**The upstream tokens.** The upstream is never written in. Each consumer
replaces three tokens textually before the file is loaded:

| Token | This harness | Task 26b | Deploy (live) |
|---|---|---|---|
| `__UPSTREAM_HOST__` | `localhost` | `gateway.upstream.host` | `api.anthropic.com` |
| `__UPSTREAM_PORT__` | the fake's port | `gateway.upstream.port` | `443` |
| `__UPSTREAM_TLS__` | `null` | `{}` if `gateway.upstream.tls`, else `null` | `{}` |

`__UPSTREAM_TLS__` is the value of the backend's `backendTLS` policy, not a
boolean. agentgateway has no TLS switch: TLS is on exactly when a
`backendTLS` policy is present, and a `hostOverride` turns the Anthropic
provider's default TLS off. `{}` is TLS with the system roots; `null` is
plain http. A consumer that writes `true` or `false` gets a config that does
not load.

The harness names the fake `localhost`, not `127.0.0.1`: the gateway sets
the upstream `Host` to a hostname target (as it will to `api.anthropic.com`)
but passes the client's `Host` through to an IP target.

**The routes.** Each route has a `name`, `interactive` or `ci` (P16), so a
consumer drops one with
`yq 'del(.routes[] | select(.name == "interactive"))'`. Each route:

- matches exactly `/<route>/v1/messages` and
  `/<route>/v1/messages/count_tokens`. An `ai.routes` path that is not
  listed is parsed as OpenAI `completions`, so nothing else may reach the
  backend: anything else is a 404;
- requires its own bearer (`apiKey` in `strict` mode; every mode defaults to
  `optional`), from `$GATEWAY_TOKEN_INTERACTIVE` or `$GATEWAY_TOKEN_CI`;
- strips its prefix with a CEL `:path` transformation, which keeps the query
  (`?beta=true`);
- forwards to one `ai` backend, Anthropic, with the workspace key from
  `/run/secrets/anthropic/<route>` (the file is watched).

`config.metrics.fields.add` labels every metric with `person`, `agent`,
`tier`, `source` and `request_class`, each from its header when the value is
in the label's fixed set and `other` otherwise. `request_class`'s set is
`main`, `auxiliary`, `compaction` and `subagent`: every class the pinned CLI
sent in Spike S (`REQUEST_CLASSES` in `conductor/src/spike-results.ts`).

The whole file goes through shellexpand, comments included, so it holds no
dollar sign except the two bearer variables. The CEL patterns anchor their
ends with `\z`.

### Where the config differs from the plan's sketch

The plan (Task 3) sketched the config before the spike ran. Four things had
to change for v1.6.0 to load it or behave as specified:

- **`urlRewrite: {path: {prefix: /}}` cannot be used.** A prefix rewrite
  needs a prefix match, and these matches are exact: the gateway answers 500
  `prefix redirect requires prefix match`. A second rule per route cannot
  carry a full rewrite either, because two entries with one `name` share a
  route key and the last one wins. The rewrite is a CEL transformation:
  `":path": 'request.pathAndQuery.stripPrefix("/<route>")'`.
- **`__UPSTREAM_TLS__` is `{}` or `null`**, not `true` or `false` (above).
- **`maxBufferSize` is a number of bytes**, `33554432`; `32Mi` does not
  parse.
- **The `ai` backend needs a `name`** (`anthropic`).

## The harness

`TestMain` pulls the image and starts the upstream: an `httptest` server that
hands each request to the current test's fake and records every response as
the upstream wrote it, so a test can compare it with what the client got. A
request with `x-spike-upstream: long-stream` or `split-usage` gets one of the
harness's own SSE streams instead: 90 seconds of pings, or a stream whose
`message_delta` carries only the output count.

`gateway(t, routeType)` makes sure the gateway container runs the canonical
config with the tokens filled in a temporary copy and `/v1/messages` given
`routeType`, restarting it when a test needs another. Each test takes a fresh
fake (`h.up.use`), so its records start empty. Metrics are not reset between
tests on one gateway, so each test sends its own `x-luma-person` ref.

Every key and bearer in the harness is fake, and the bodies are synthetic.

## Updating the results

`result.go` holds what the reviewed run observed: the route type Spike G
chose, whether bodies are byte-equal, and what each route type did. The tests
assert those values, so they fail when a new gateway behaves differently.
After a gateway bump that turns them red, read the failure, rerun, and type
the new values by hand once a person has reviewed them; record the run in
section G. Never generate them from the test's output.
