// Spike S results: how the pinned Agent SDK 0.3.303 and Claude Code CLI
// 2.1.303 behave, measured against fakeanthropic (Task 4,
// docs/spikes/phase-1.md section S). Every constant was typed by hand from a
// reviewed observation; conductor/spike/sdk.spike.test.ts asserts that the
// pinned SDK and CLI still behave this way, so a bump that changes one turns
// `make sdk-spike` red (a change detector, S20). Never generate these.

/**
 * S5. How the profile context reaches the model. An SDK SessionStart hook
 * callback never runs for `startup`, `resume` or `fork` (the CLI raises
 * session.start before the SDK's hooks are registered), so a fresh session's
 * context cannot come from it: the conductor prepends it to the first user
 * turn, and again after each compact_boundary (Task 19b).
 */
export const CONTEXT_DELIVERY: 'session-start' | 'first-user-turn' = 'first-user-turn'

/**
 * S4, S5. The SessionStart sources for which the SDK callback ran and its
 * additionalContext reached the next request (in `messages`, exactly once).
 * Only `compact`; the plan expected ['startup', 'compact'].
 */
export const SESSION_START_CONTEXT_SOURCES: ReadonlyArray<'startup' | 'compact'> = ['compact']

/**
 * S6. Whether the first result after each kind of reopen already carries
 * the earlier totals. A clean close writes a cost-state entry, and a resume or
 * a fork restores the latest one in the loaded transcript. A SIGKILL writes
 * none, so the resume restores the last cost-state from before the kill (or
 * nothing, when the session never closed cleanly): the totals at the kill are
 * not restored.
 */
export const TOTALS_RESTORED: { resume: boolean; resumeAfterKill: boolean; fork: boolean } = {
  resume: true,
  resumeAfterKill: false,
  fork: true,
}

/**
 * S6. The transcript entry that carries the saved totals: appended to the
 * store on a clean close, as {type, sessionId, totalCostUSD, modelUsage, …}.
 * The host's baseline is the latest such entry in the transcript it loads,
 * or zero when there is none (never Core's last persisted totals).
 */
export const TOTALS_ENTRY_TYPE: string | null = 'cost-state'

/**
 * S6. The result field that carries the cost basis: every entry of
 * `modelUsage` has `costBasis`, `'list'` against the fake.
 */
export const COST_BASIS_FIELD: string | null = 'modelUsage[model].costBasis'

/**
 * S8. A fork from a second host with an empty local dir, reading the store
 * only: `resume` + `forkSession: true` + `resumeSessionAt` = the uuid of the
 * last main-loop assistant message of the kept turn. The fork's request
 * replays the kept history byte for byte, and its thinking passes the binding
 * check.
 */
export const FORK_STRATEGY: 'resumeSessionAt' | 'storeForkSession' = 'resumeSessionAt'

/**
 * S7. The pinned CLI declares and reads `precomputeCompactionEnabled`
 * (default true), gated by the server flag tengu_sepia_moth, which is off
 * offline: so no precompute request appeared with either value here, and the
 * setting cannot be seen working offline. It also gates the CLI's idle
 * compaction mode compact_on_return. Task 16 passes it as false.
 */
export const PRECOMPUTE_SETTING_EXISTS: boolean = true

/** S11. The API tool name of the subagent tool. */
export const SUBAGENT_TOOL: 'Task' | 'Agent' = 'Agent'

/**
 * S1. A store resume replays the prefix byte for byte (outside `metadata`),
 * so the idle eviction stays at 10 minutes. This sets only the chart default
 * of conductor.idleMinutes; the timer reads LUMA_IDLE_MINUTES (S4).
 */
export const IDLE_EVICT_MS: number = 600_000

/**
 * S4 (S14). With CLAUDE_CODE_AUTO_COMPACT_WINDOW=100000 and
 * CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=85: the CLI compacts before sending the
 * next turn once the last response's input + cache_read + cache_creation +
 * output tokens reach this, for a short next turn (the next turn's own size
 * counts toward it).
 */
export const COMPACTION_TRIGGER_TOKENS: number = 66_965

/**
 * S11. How a model change shows. The refusal fixture switched nothing (the CLI
 * noted the refusal and retried once on the same model; the fake sends no
 * refusal category, so a flag-routed fallback switch was not exercised).
 * PostModelSwitch fired for an SDK setModel(), and not for a reply from
 * another model, which shows only in the assistant message's `message.model`.
 * Whether it fires for a flag-routed switch is untested, so the reply's model
 * is the signal. Live, an alias may come back as a dated snapshot id; the
 * comparison must allow for that (live spike).
 */
export const MODEL_SWITCH_SIGNAL: 'postModelSwitchHook' | 'assistantModelField' = 'assistantModelField'

/**
 * S17. Where a WebSearch call's search count appears. A nested reply with
 * three server_tool_use / web_search_tool_result pairs (and
 * web_search_requests: 3) gives PostToolUse's tool_response `searchCount: 3`,
 * per call, and the result's modelUsage[model].webSearchRequests agrees.
 */
export const WEB_SEARCH_COUNT_SOURCE: 'postToolUse' | 'resultDelta' = 'postToolUse'

/**
 * S16. With ANTHROPIC_BETAS=thinking-binding-controls-2026-08-01, a reply's
 * input_transformations reach the SDK: on the assistant message's
 * `message.input_transformations` and on stream_event message_start.
 */
export const INPUT_TRANSFORMATIONS_VISIBLE: boolean = true

/**
 * S9, S4. The in-band SLO signals. A resume after an idle longer than the
 * cache TTL looks exactly like a warm one (no field, header or SDK message;
 * the SessionStart hook that would carry prompt_cache_likely_expired does not
 * run), so the conductor derives a cold resume from the idle gap. A
 * compaction emits the system/compact_boundary message (and the main
 * requests of the turn after it carry `x-claude-code-context-compacted: auto`).
 */
export const SLO_SIGNALS: { coldResume: string | null; postCompaction: string } = {
  coldResume: null,
  postCompaction: 'compact_boundary',
}

/** S9. Every path the CLI requested in Spike S (each with ?beta=true). */
export const CLI_PATHS: ReadonlyArray<string> = ['/v1/messages']

/** S9. Every x-claude-code-request-class value the CLI sent in Spike S. */
export const REQUEST_CLASSES: ReadonlyArray<string> = ['auxiliary', 'compaction', 'main', 'subagent']
