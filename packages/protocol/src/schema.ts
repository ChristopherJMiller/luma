// Runtime shapes for every wire object, one per interface, and the decoders
// built on them.
//
// Each shape is a literal typed against its interface with `satisfies
// Spec<T>`, so `tsc` (make lint) fails when a TS field is renamed, added,
// dropped, or changes optionality, nullability or kind (string, number,
// boolean, array, map, object, tagged union) without the shape following. The
// fixtures test then walks every golden frame through the same shapes, strictly
// (exact keys); the runtime decoders are lenient (S37): they reject a missing
// required key, a wrong kind or an unknown type tag, and ignore unknown keys,
// so an older client can read a newer Core's additive fields.

import type { SessionErrorEvent } from './events'
import type { ErrorInfo } from './errors'
import type {
  AgentList,
  AgentSummary,
  Attachment,
  BudgetStatus,
  CreateSession,
  ForkRequest,
  Me,
  Profile,
  ProfileUpdate,
  SendResult,
  Session,
  SessionList,
  SessionParams,
  SessionPatch,
  Tree,
  TreeMessage,
} from './api'
import type {
  Base64Source,
  ClientEvent,
  ContentBlock,
  Event,
  MemoryWrite,
  Message,
  MessagePart,
  TextBlock,
} from './events'
import type {
  EvalResult,
  EvalRun,
  EvalSpec,
  ReconcileDrift,
  ReconcileWorkspace,
  UsageReconciliation,
  UsageReconciliationResult,
} from './internal'
import type {
  AgentLimits,
  AgentSpec,
  EventFrame,
  HostSpec,
  LinkEvent,
  LinkFrame,
  MemoryResult,
  ModelUsage,
  Resume,
  SubagentSpec,
  TierBinding,
  TurnUsage,
} from './link'

export class ProtocolError extends Error {}

// ---- Spec<T>: the compile-time contract a shape literal must meet ----

type Opt<T, K extends keyof T> = {} extends Pick<T, K> ? { opt: true } : { opt?: false }
type Nul<V> = null extends V ? { nullable: true } : { nullable?: false }

type KindOf<V> = [V] extends [string]
  ? { k: 'str' }
  : [V] extends [number]
    ? { k: 'num' }
    : [V] extends [boolean]
      ? { k: 'bool' }
      : [V] extends [readonly (infer U)[]]
        ? { k: 'arr'; of: FieldOf<U> }
        : string extends keyof V
          ? { k: 'map'; of: FieldOf<V[keyof V & string]> }
          : [V] extends [{ type: string }]
            ? { k: 'union'; variants: { [Ty in V['type']]: Spec<Extract<V, { type: Ty }>> } }
            : { k: 'obj'; shape: Spec<V> }

type FieldOf<V> = KindOf<NonNullable<V>> & Nul<V>

/** Exactly the keys of T, each with the kind, optionality and nullability of its type. */
export type Spec<T> = {
  [K in keyof T]-?: (K extends 'type' ? { k: 'lit'; value: T[K] } : FieldOf<T[K]>) & Opt<T, K>
}

// ---- Builders ----

const str = { k: 'str' } as const
const num = { k: 'num' } as const
const bool = { k: 'bool' } as const
const lit = <const V extends string>(value: V) => ({ k: 'lit', value }) as const
const arr = <const F>(of: F) => ({ k: 'arr', of }) as const
const map = <const F>(of: F) => ({ k: 'map', of }) as const
const obj = <const S>(shape: S) => ({ k: 'obj', shape }) as const
const union = <const V>(variants: V) => ({ k: 'union', variants }) as const
const nullable = <const F extends object>(f: F) => ({ ...f, nullable: true }) as F & { readonly nullable: true }
const optional = <const F extends object>(f: F) => ({ ...f, opt: true }) as F & { readonly opt: true }

type Of<U extends { type: string }, Ty extends U['type']> = Extract<U, { type: Ty }>

// ---- C1: messages and events ----

const TEXT_PART = { type: lit('text'), text: str } satisfies Spec<Of<MessagePart, 'text'>>
const TOOL_PART = {
  type: lit('tool'),
  tool_use_id: str,
  name: str,
  summary: str,
  is_error: bool,
} satisfies Spec<Of<MessagePart, 'tool'>>
const ATTACHMENT_PART = {
  type: lit('attachment'),
  id: str,
  media_type: str,
  filename: str,
} satisfies Spec<Of<MessagePart, 'attachment'>>
const MESSAGE_PART = union({ text: TEXT_PART, tool: TOOL_PART, attachment: ATTACHMENT_PART })

export const MESSAGE_SHAPE = {
  id: str,
  session_id: str,
  parent_message_id: nullable(str),
  turn_id: str,
  role: str,
  content: arr(MESSAGE_PART),
  sdk_uuid: nullable(str),
  model: nullable(str),
  status: str,
  error_code: nullable(str),
  created_at: str,
} satisfies Spec<Message>

export const MEMORY_WRITE_SHAPE = {
  id: str,
  session_id: nullable(str),
  turn_id: str,
  op: str,
  text: str,
  matched: nullable(arr(str)),
  untrusted: bool,
  status: str,
  created_at: str,
  decided_at: nullable(str),
} satisfies Spec<MemoryWrite>

const BASE64_SOURCE = { type: lit('base64'), media_type: str, data: str } satisfies Spec<Base64Source>
const SOURCE = union({ base64: BASE64_SOURCE })
const CONTENT_BLOCK = union({
  text: { type: lit('text'), text: str } satisfies Spec<Of<ContentBlock, 'text'>>,
  image: { type: lit('image'), source: SOURCE } satisfies Spec<Of<ContentBlock, 'image'>>,
  document: { type: lit('document'), source: SOURCE } satisfies Spec<Of<ContentBlock, 'document'>>,
})
const TEXT_BLOCK = { type: lit('text'), text: str } satisfies Spec<TextBlock>

export const CLIENT_EVENT_FIELD = union({
  'user.message': {
    type: lit('user.message'),
    client_id: str,
    content: arr(union({ text: TEXT_BLOCK })),
    attachment_ids: arr(str),
  } satisfies Spec<Of<ClientEvent, 'user.message'>>,
  'user.interrupt': { type: lit('user.interrupt') } satisfies Spec<Of<ClientEvent, 'user.interrupt'>>,
})

const HEADER = { id: num, session_id: str, created_at: str } as const

export const EVENT_FIELD = union({
  'session.status': { ...HEADER, type: lit('session.status'), status: str } satisfies Spec<Of<Event, 'session.status'>>,
  'message.user': {
    ...HEADER,
    type: lit('message.user'),
    message: obj(MESSAGE_SHAPE),
  } satisfies Spec<Of<Event, 'message.user'>>,
  'message.started': {
    ...HEADER,
    type: lit('message.started'),
    message_id: str,
    model: str,
  } satisfies Spec<Of<Event, 'message.started'>>,
  'message.text_delta': {
    ...HEADER,
    type: lit('message.text_delta'),
    message_id: str,
    text: str,
  } satisfies Spec<Of<Event, 'message.text_delta'>>,
  'tool.started': {
    ...HEADER,
    type: lit('tool.started'),
    message_id: str,
    tool_use_id: str,
    name: str,
    summary: str,
  } satisfies Spec<Of<Event, 'tool.started'>>,
  'tool.finished': {
    ...HEADER,
    type: lit('tool.finished'),
    message_id: str,
    tool_use_id: str,
    is_error: bool,
    summary: str,
  } satisfies Spec<Of<Event, 'tool.finished'>>,
  'message.completed': {
    ...HEADER,
    type: lit('message.completed'),
    message: obj(MESSAGE_SHAPE),
  } satisfies Spec<Of<Event, 'message.completed'>>,
  'model.switched': {
    ...HEADER,
    type: lit('model.switched'),
    message_id: str,
    from: str,
    to: str,
  } satisfies Spec<Of<Event, 'model.switched'>>,
  'memory.write': {
    ...HEADER,
    type: lit('memory.write'),
    write: obj(MEMORY_WRITE_SHAPE),
  } satisfies Spec<Of<Event, 'memory.write'>>,
  usage: { ...HEADER, type: lit('usage'), turn_id: str, cost_usd: num } satisfies Spec<Of<Event, 'usage'>>,
  error: { ...HEADER, type: lit('error'), code: str, retryable: bool } satisfies Spec<SessionErrorEvent>,
})

// ---- C1: REST bodies ----

export const SESSION_PARAMS_SHAPE = {
  tier: optional(str),
  effort: optional(str),
  incognito: optional(bool),
  workspace: optional(str),
} satisfies Spec<SessionParams>

export const CREATE_SESSION_SHAPE = {
  agent: str,
  params: obj(SESSION_PARAMS_SHAPE),
} satisfies Spec<CreateSession>

export const SESSION_SHAPE = {
  id: str,
  root_id: str,
  parent_id: nullable(str),
  fork_at_message_id: nullable(str),
  agent_id: str,
  agent_version: str,
  title: str,
  tier: str,
  model: str,
  effort: str,
  incognito: bool,
  workspace: str,
  source: str,
  status: str,
  tainted: bool,
  archived_at: nullable(str),
  created_at: str,
  updated_at: str,
  last_activity_at: str,
} satisfies Spec<Session>

export const SESSION_LIST_SHAPE = {
  sessions: arr(obj(SESSION_SHAPE)),
  next: optional(str),
} satisfies Spec<SessionList>

export const SESSION_PATCH_SHAPE = { title: str } satisfies Spec<SessionPatch>
export const FORK_REQUEST_SHAPE = { at_message: str } satisfies Spec<ForkRequest>
export const SEND_RESULT_SHAPE = { message: optional(obj(MESSAGE_SHAPE)) } satisfies Spec<SendResult>

const TREE_MESSAGE_SHAPE = {
  message: obj(MESSAGE_SHAPE),
  parent_id: nullable(str),
  session_id: str,
} satisfies Spec<TreeMessage>

export const TREE_SHAPE = {
  messages: arr(obj(TREE_MESSAGE_SHAPE)),
  head_id: nullable(str),
  last_event_ids: map(num),
  pending_writes: arr(obj(MEMORY_WRITE_SHAPE)),
} satisfies Spec<Tree>

export const ATTACHMENT_SHAPE = {
  id: str,
  media_type: str,
  filename: str,
  size_bytes: num,
  width: optional(num),
  height: optional(num),
  pages: optional(num),
} satisfies Spec<Attachment>

export const PROFILE_SHAPE = {
  content: str,
  version: num,
  updated_at: str,
  pending: arr(obj(MEMORY_WRITE_SHAPE)),
} satisfies Spec<Profile>

export const PROFILE_UPDATE_SHAPE = { content: str, version: num } satisfies Spec<ProfileUpdate>

const BUDGET_STATUS_SHAPE = {
  spent_usd: num,
  cap_usd: num,
  ratio: num,
  warn: bool,
  exhausted: bool,
  resets_at: str,
} satisfies Spec<BudgetStatus>

export const ME_SHAPE = {
  person: obj({ id: str, display_name: str, tier: str } satisfies Spec<Me['person']>),
  budget: obj(BUDGET_STATUS_SHAPE),
  maintenance: bool,
} satisfies Spec<Me>

export const AGENT_SUMMARY_SHAPE = {
  id: str,
  name: str,
  description: str,
  tiers: arr(str),
  default_tier: str,
} satisfies Spec<AgentSummary>

export const AGENT_LIST_SHAPE = { agents: arr(obj(AGENT_SUMMARY_SHAPE)) } satisfies Spec<AgentList>

export const ERROR_INFO_SHAPE = { code: str, message: str } satisfies Spec<ErrorInfo>

// ---- C2: the link ----

const MODEL_USAGE_SHAPE = {
  input: num,
  output: num,
  cache_read: num,
  cache_creation: num,
  web_search_requests: num,
  cost_usd: num,
} satisfies Spec<ModelUsage>

const TURN_USAGE_SHAPE = { models: map(obj(MODEL_USAGE_SHAPE)), cost_usd: num } satisfies Spec<TurnUsage>

const SUBAGENT_SHAPE = {
  description: str,
  prompt: str,
  model: str,
  effort: str,
  tools: arr(str),
} satisfies Spec<SubagentSpec>

const AGENT_LIMITS_SHAPE = {
  max_turns: num,
  max_tool_calls: num,
  web_search_per_turn: num,
  web_search_per_session: num,
} satisfies Spec<AgentLimits>

const TIER_BINDING_SHAPE = {
  model: str,
  effort: str,
  cache_ttl: str,
  auto_compact_window: optional(num),
  auto_compact_pct: optional(num),
  pdf_max_pages: num,
} satisfies Spec<TierBinding>

export const AGENT_SPEC_SHAPE = {
  id: str,
  version: str,
  system_prompt: str,
  tools: arr(str),
  builtin_tools: arr(str),
  mcp: arr(str),
  untrusted_inputs: arr(str),
  subagents: map(obj(SUBAGENT_SHAPE)),
  limits: obj(AGENT_LIMITS_SHAPE),
  memory: str,
  tier_binding: obj(TIER_BINDING_SHAPE),
} satisfies Spec<AgentSpec>

const RESUME_SHAPE = {
  sdk_session_id: str,
  fork: optional(obj({ at_sdk_uuid: str } satisfies Spec<NonNullable<Resume['fork']>>)),
  baseline_totals: optional(obj(TURN_USAGE_SHAPE)),
} satisfies Spec<Resume>

const HOST_SPEC_SHAPE = {
  session_id: str,
  root_id: str,
  person: obj({ ref: str } satisfies Spec<HostSpec['person']>),
  agent: obj(AGENT_SPEC_SHAPE),
  model: str,
  helper_model: str,
  effort: str,
  cache_ttl: str,
  workspace: str,
  source: str,
  timezone: str,
  profile_context: str,
  webfetch_deny_domains: arr(str),
  resume: nullable(obj(RESUME_SHAPE)),
} satisfies Spec<HostSpec>

const MEMORY_RESULT_SHAPE = {
  status: str,
  write_id: optional(str),
  removed: optional(num),
  code: optional(str),
} satisfies Spec<MemoryResult>

export const LINK_EVENT_FIELD = union({
  message_started: { type: lit('message_started'), model: str } satisfies Spec<Of<LinkEvent, 'message_started'>>,
  text_delta: { type: lit('text_delta'), text: str } satisfies Spec<Of<LinkEvent, 'text_delta'>>,
  tool_started: {
    type: lit('tool_started'),
    tool_use_id: str,
    name: str,
    summary: str,
  } satisfies Spec<Of<LinkEvent, 'tool_started'>>,
  tool_finished: {
    type: lit('tool_finished'),
    tool_use_id: str,
    is_error: bool,
    summary: optional(str),
  } satisfies Spec<Of<LinkEvent, 'tool_finished'>>,
  main_request_usage: {
    type: lit('main_request_usage'),
    api_message_id: str,
    model: str,
    slo_exclusion: str,
    input: num,
    cache_read: num,
    cache_creation: num,
  } satisfies Spec<Of<LinkEvent, 'main_request_usage'>>,
  model_switched: {
    type: lit('model_switched'),
    from: str,
    to: str,
  } satisfies Spec<Of<LinkEvent, 'model_switched'>>,
  turn_result: {
    type: lit('turn_result'),
    sdk_uuid: str,
    content: arr(MESSAGE_PART),
    usage: obj(TURN_USAGE_SHAPE),
    totals: obj(TURN_USAGE_SHAPE),
    cost_basis: nullable(str),
  } satisfies Spec<Of<LinkEvent, 'turn_result'>>,
  error: {
    type: lit('error'),
    code: str,
    retryable: bool,
    detail: optional(str),
  } satisfies Spec<Of<LinkEvent, 'error'>>,
})

export const LINK_FRAME_FIELD = union({
  'host.open': { type: lit('host.open'), session_id: str, spec: obj(HOST_SPEC_SHAPE) } satisfies Spec<
    Of<LinkFrame, 'host.open'>
  >,
  'turn.send': {
    type: lit('turn.send'),
    session_id: str,
    turn_id: str,
    content: arr(CONTENT_BLOCK),
    untrusted: bool,
  } satisfies Spec<Of<LinkFrame, 'turn.send'>>,
  'turn.interrupt': { type: lit('turn.interrupt'), session_id: str } satisfies Spec<Of<LinkFrame, 'turn.interrupt'>>,
  'host.close': { type: lit('host.close'), session_id: str, reason: str } satisfies Spec<Of<LinkFrame, 'host.close'>>,
  'tree.purge': { type: lit('tree.purge'), root_id: str, person_ref: str } satisfies Spec<Of<LinkFrame, 'tree.purge'>>,
  'rpc.result': {
    type: lit('rpc.result'),
    rpc_id: str,
    ok: bool,
    result: optional(obj(MEMORY_RESULT_SHAPE)),
    error: optional(obj(ERROR_INFO_SHAPE)),
  } satisfies Spec<Of<LinkFrame, 'rpc.result'>>,
  shutdown: { type: lit('shutdown'), reason: str } satisfies Spec<Of<LinkFrame, 'shutdown'>>,
  hello: {
    type: lit('hello'),
    conductor_version: str,
    sdk_version: str,
    cli_version: str,
    max_hosts: num,
  } satisfies Spec<Of<LinkFrame, 'hello'>>,
  'host.state': {
    type: lit('host.state'),
    session_id: str,
    state: str,
    sdk_session_id: optional(str),
    reason: optional(str),
  } satisfies Spec<Of<LinkFrame, 'host.state'>>,
  event: {
    type: lit('event'),
    session_id: str,
    turn_id: str,
    event: LINK_EVENT_FIELD,
  } satisfies Spec<EventFrame>,
  rpc: {
    type: lit('rpc'),
    rpc_id: str,
    session_id: str,
    turn_id: str,
    method: str,
    params: obj({ text: str, untrusted: bool } satisfies Spec<Of<LinkFrame, 'rpc'>['params']>),
  } satisfies Spec<Of<LinkFrame, 'rpc'>>,
})

// ---- C10: internal endpoints ----

export const EVAL_SPEC_SHAPE = {
  agent: obj(AGENT_SPEC_SHAPE),
  helper_model: str,
  timezone: str,
  webfetch_deny_domains: arr(str),
} satisfies Spec<EvalSpec>

const EVAL_RESULT_SHAPE = {
  id: str,
  passed: bool,
  failures: arr(str),
  cost_usd: num,
} satisfies Spec<EvalResult>

export const EVAL_RUN_SHAPE = {
  suite: str,
  sdk_version: str,
  cli_version: str,
  image: str,
  started_at: str,
  finished_at: str,
  passed: num,
  failed: num,
  cost_usd: num,
  results: arr(obj(EVAL_RESULT_SHAPE)),
} satisfies Spec<EvalRun>

export const USAGE_RECONCILIATION_SHAPE = {
  day: str,
  workspaces: arr(obj({ name: str, api_usd: num } satisfies Spec<ReconcileWorkspace>)),
} satisfies Spec<UsageReconciliation>

export const USAGE_RECONCILIATION_RESULT_SHAPE = {
  workspaces: arr(
    obj({ name: str, api_usd: num, ledger_usd: num, drift_ratio: num } satisfies Spec<ReconcileDrift>),
  ),
} satisfies Spec<UsageReconciliationResult>

// ---- Runtime checking ----

/** What the runtime sees of a shape: the Spec<T> types erase to this. */
export interface RuntimeField {
  readonly k: string
  readonly opt?: boolean
  readonly nullable?: boolean
  readonly value?: string
  readonly of?: RuntimeField
  readonly shape?: Readonly<Record<string, RuntimeField>>
  readonly variants?: Readonly<Record<string, Readonly<Record<string, RuntimeField>>>>
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

function checkObject(o: Record<string, unknown>, shape: Readonly<Record<string, RuntimeField>>, path: string): void {
  for (const [key, field] of Object.entries(shape)) {
    const v = o[key]
    if (v === undefined) {
      if (field.opt) continue
      throw new ProtocolError(`${path}: missing key ${key}`)
    }
    check(v, field, `${path}.${key}`)
  }
}

function check(v: unknown, f: RuntimeField, path: string): void {
  if (v === null) {
    if (f.nullable) return
    throw new ProtocolError(`${path}: unexpected null`)
  }
  switch (f.k) {
    case 'str':
    case 'num':
    case 'bool': {
      const want = { str: 'string', num: 'number', bool: 'boolean' }[f.k]
      if (typeof v !== want) throw new ProtocolError(`${path}: expected ${want}`)
      return
    }
    case 'lit':
      if (v !== f.value) throw new ProtocolError(`${path}: expected ${String(f.value)}`)
      return
    case 'obj':
      if (!isObject(v)) throw new ProtocolError(`${path}: expected an object`)
      checkObject(v, f.shape ?? {}, path)
      return
    case 'arr': {
      if (!Array.isArray(v)) throw new ProtocolError(`${path}: expected an array`)
      v.forEach((item, i) => check(item, f.of as RuntimeField, `${path}[${i}]`))
      return
    }
    case 'map': {
      if (!isObject(v)) throw new ProtocolError(`${path}: expected an object`)
      for (const [k, item] of Object.entries(v)) check(item, f.of as RuntimeField, `${path}.${k}`)
      return
    }
    case 'union': {
      if (!isObject(v)) throw new ProtocolError(`${path}: expected an object`)
      const tag = v['type']
      const variants = f.variants ?? {}
      if (typeof tag !== 'string' || !Object.hasOwn(variants, tag)) {
        throw new ProtocolError(`${path}: unknown type ${String(tag)}`)
      }
      checkObject(v, variants[tag] as Readonly<Record<string, RuntimeField>>, path)
      return
    }
  }
  throw new ProtocolError(`${path}: unknown shape kind ${f.k}`)
}

function decodeUnion<T>(raw: unknown, field: RuntimeField, label: string): T {
  if (!isObject(raw)) throw new ProtocolError(`${label}: expected an object`)
  const tag = raw['type']
  if (typeof tag !== 'string' || !Object.hasOwn(field.variants ?? {}, tag)) {
    throw new ProtocolError(`unknown ${label} type ${String(tag)}`)
  }
  check(raw, field, label)
  return raw as T
}

/** Decodes an Event; unknown types, missing keys and wrong kinds throw, unknown keys are ignored (S37). */
export function decodeEvent(raw: unknown): Event {
  return decodeUnion<Event>(raw, EVENT_FIELD, 'event')
}

export function decodeClientEvent(raw: unknown): ClientEvent {
  return decodeUnion<ClientEvent>(raw, CLIENT_EVENT_FIELD, 'client event')
}

export function decodeLinkEvent(raw: unknown): LinkEvent {
  return decodeUnion<LinkEvent>(raw, LINK_EVENT_FIELD, 'link event')
}

export function decodeLinkFrame(raw: unknown): LinkFrame {
  return decodeUnion<LinkFrame>(raw, LINK_FRAME_FIELD, 'link frame')
}
