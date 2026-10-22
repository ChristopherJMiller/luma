import type { ErrorCode, ErrorInfo } from './errors'
import type { ContentBlock, MessagePart } from './events'

// ---- Usage ----

export interface ModelUsage {
  input: number
  output: number
  cache_read: number
  cache_creation: number
  web_search_requests: number
  cost_usd: number
}

/** A per-turn delta. */
export interface TurnUsage {
  models: Record<string, ModelUsage>
  cost_usd: number
}

/** TurnUsage's shape, as running totals. */
export type Totals = TurnUsage

// ---- AgentSpec and HostSpec ----

export interface SubagentSpec {
  description: string
  prompt: string
  model: string
  effort: string
  tools: string[]
}

export interface AgentLimits {
  max_turns: number
  max_tool_calls: number
  web_search_per_turn: number
  web_search_per_session: number
}

export interface TierBinding {
  model: string
  effort: string
  cache_ttl: '5m' | '1h'
  auto_compact_window?: number
  auto_compact_pct?: number
  pdf_max_pages: number
}

/** The compiled output of agents.Compile. */
export interface AgentSpec {
  id: string
  version: string
  system_prompt: string
  /** Sorted. */
  tools: string[]
  builtin_tools: string[]
  mcp: string[]
  untrusted_inputs: string[]
  subagents: Record<string, SubagentSpec>
  limits: AgentLimits
  memory: 'person' | 'none'
  tier_binding: TierBinding
}

/**
 * Set on every host.open of a session with an sdk_session_id (S2).
 * `baseline_totals` is never the host's delta baseline (Spike S6).
 */
export interface Resume {
  sdk_session_id: string
  fork?: { at_sdk_uuid: string }
  baseline_totals?: Totals
}

export interface HostSpec {
  session_id: string
  root_id: string
  /** The opaque person ref only (P21). */
  person: { ref: string }
  agent: AgentSpec
  model: string
  helper_model: string
  effort: string
  cache_ttl: '5m' | '1h'
  /** The session's api_workspace (gateway route, Anthropic workspace), not the sandbox. */
  workspace: 'interactive' | 'ci'
  source: 'web' | 'ci'
  timezone: string
  /** The stored profile_snapshot, already fenced. */
  profile_context: string
  webfetch_deny_domains: string[]
  resume: Resume | null
}

// ---- Frames: Core -> conductor ----

export interface HostOpen {
  type: 'host.open'
  session_id: string
  spec: HostSpec
}

/** Attachments are inlined as base64 blocks or fenced text; `<luma-now>` is already appended. */
export interface TurnSend {
  type: 'turn.send'
  session_id: string
  turn_id: string
  content: ContentBlock[]
  /** The session is tainted. */
  untrusted: boolean
}

export interface TurnInterrupt {
  type: 'turn.interrupt'
  session_id: string
}

export interface HostClose {
  type: 'host.close'
  session_id: string
  reason: 'deleted' | 'shutdown' | 'maintenance' | 'error' // error: Core gave up on a stuck turn (S48)
}

/** Remove the tree's project dirs after its hosts close, even when none is open (S14). */
export interface TreePurge {
  type: 'tree.purge'
  root_id: string
  person_ref: string
}

export interface MemoryResult {
  status: 'applied' | 'pending' | 'disabled' | 'full' | 'rejected'
  write_id?: string
  removed?: number
  code?: ErrorCode
}

export interface RpcResult {
  type: 'rpc.result'
  rpc_id: string
  ok: boolean
  result?: MemoryResult
  error?: ErrorInfo
}

export interface Shutdown {
  type: 'shutdown'
  reason: string
}

// ---- Frames: conductor -> Core ----

export interface Hello {
  type: 'hello'
  conductor_version: string
  sdk_version: string
  cli_version: string
  max_hosts: number
}

/**
 * A host sends `ready` when it opens, and `ready` again with `sdk_session_id`
 * as soon as the SDK reports it (S2).
 */
export interface HostState {
  type: 'host.state'
  session_id: string
  state: 'ready' | 'closed'
  sdk_session_id?: string
  reason?: 'idle' | 'evicted' | 'capacity' | 'error' | 'shutdown' | 'deleted' | 'maintenance'
}

export interface EventFrame {
  type: 'event'
  session_id: string
  turn_id: string
  event: LinkEvent
}

export interface Rpc {
  type: 'rpc'
  rpc_id: string
  session_id: string
  turn_id: string
  method: 'memory.remember' | 'memory.forget'
  params: { text: string; untrusted: boolean }
}

/**
 * The ack of `tree.purge`: sent once the tree's hosts are closed and its
 * project dirs removed, also after a purge that failed (logged). Core deletes
 * the tree's rows after it (S39).
 */
export interface TreePurged {
  type: 'tree.purged'
  root_id: string
}

export type LinkFrame =
  | HostOpen
  | TurnSend
  | TurnInterrupt
  | HostClose
  | TreePurge
  | RpcResult
  | Shutdown
  | Hello
  | HostState
  | EventFrame
  | Rpc
  | TreePurged

// ---- LinkEvent (the turn is implied by the envelope) ----

export interface LinkMessageStarted {
  type: 'message_started'
  model: string
}

/** Coalesced for about 100 ms (P24). */
export interface LinkTextDelta {
  type: 'text_delta'
  text: string
}

export interface LinkToolStarted {
  type: 'tool_started'
  tool_use_id: string
  name: string
  summary: string
}

export interface LinkToolFinished {
  type: 'tool_finished'
  tool_use_id: string
  is_error: boolean
  summary?: string
}

/** The API message's own usage fields: `input` is the uncached input_tokens (S28). */
export interface MainRequestUsage {
  type: 'main_request_usage'
  api_message_id: string
  model: string
  slo_exclusion: 'none' | 'first' | 'cold_resume' | 'post_compaction'
  input: number
  cache_read: number
  cache_creation: number
}

export interface LinkModelSwitched {
  type: 'model_switched'
  from: string
  to: string
}

/** Core persists `totals` as sessions.last_totals (S2). */
export interface TurnResult {
  type: 'turn_result'
  sdk_uuid: string
  content: MessagePart[]
  usage: TurnUsage
  totals: Totals
  /** From the result's modelUsage[model].costBasis: list, managed or unknown. */
  cost_basis: string | null
}

/** Core counts `detail: "thinking_binding"` in luma_thinking_binding_errors_total (S16). */
export interface LinkError {
  type: 'error'
  code: ErrorCode
  retryable: boolean
  detail?: 'thinking_binding'
}

export type LinkEvent =
  | LinkMessageStarted
  | LinkTextDelta
  | LinkToolStarted
  | LinkToolFinished
  | MainRequestUsage
  | LinkModelSwitched
  | TurnResult
  | LinkError

