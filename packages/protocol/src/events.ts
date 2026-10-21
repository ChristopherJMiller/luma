import type { ErrorCode } from './errors'

/** An ISO 8601 / RFC 3339 timestamp, as Go's time.Time encodes it. */
export type Timestamp = string

export type SessionStatusValue = 'running' | 'idle' | 'requires_action' | 'error'

// ---- Messages ----

/**
 * One part of a stored message. The `attachment` part appears only in user
 * messages: Core writes one per bound attachment (S15).
 */
export type MessagePart =
  | { type: 'text'; text: string }
  | { type: 'tool'; tool_use_id: string; name: string; summary: string; is_error: boolean }
  | { type: 'attachment'; id: string; media_type: string; filename: string }

/** Mirrors C4 `messages`. */
export interface Message {
  id: string
  session_id: string
  parent_message_id: string | null
  /** The Core turn id of turn.send; set on the user message and its turn's assistant message. */
  turn_id: string
  role: 'user' | 'assistant'
  content: MessagePart[]
  sdk_uuid: string | null
  model: string | null
  status: 'running' | 'complete' | 'error'
  error_code: ErrorCode | null
  created_at: Timestamp
}

/** Mirrors C4 `memory_writes` without person_id. */
export interface MemoryWrite {
  id: string
  session_id: string | null
  turn_id: string
  op: 'remember' | 'forget'
  text: string
  matched: string[] | null
  untrusted: boolean
  status: 'applied' | 'pending' | 'rejected'
  created_at: Timestamp
  decided_at: Timestamp | null
}

/** An Anthropic-shaped input block. Client events carry only text blocks. */
export type ContentBlock =
  | { type: 'text'; text: string }
  | { type: 'image'; source: Base64Source }
  | { type: 'document'; source: Base64Source }

export interface Base64Source {
  type: 'base64'
  media_type: string
  data: string
}

// ---- Client events (client -> Core) ----

export interface UserMessage {
  type: 'user.message'
  client_id: string
  content: ContentBlock[]
  attachment_ids: string[]
}

export interface UserInterrupt {
  type: 'user.interrupt'
}

export type ClientEvent = UserMessage | UserInterrupt

// ---- Events (Core -> client; also the stored events.payload) ----

export interface EventBase {
  id: number
  session_id: string
  created_at: Timestamp
}

export interface SessionStatusEvent extends EventBase {
  type: 'session.status'
  status: SessionStatusValue
}
export interface MessageUserEvent extends EventBase {
  type: 'message.user'
  message: Message
}
export interface MessageStartedEvent extends EventBase {
  type: 'message.started'
  message_id: string
  model: string
}
export interface MessageTextDeltaEvent extends EventBase {
  type: 'message.text_delta'
  message_id: string
  text: string
}
export interface ToolStartedEvent extends EventBase {
  type: 'tool.started'
  message_id: string
  tool_use_id: string
  name: string
  summary: string
}
export interface ToolFinishedEvent extends EventBase {
  type: 'tool.finished'
  message_id: string
  tool_use_id: string
  is_error: boolean
  summary: string
}
export interface MessageCompletedEvent extends EventBase {
  type: 'message.completed'
  message: Message
}
export interface ModelSwitchedEvent extends EventBase {
  type: 'model.switched'
  message_id: string
  from: string
  to: string
}
export interface MemoryWriteEvent extends EventBase {
  type: 'memory.write'
  write: MemoryWrite
}
export interface UsageEvent extends EventBase {
  type: 'usage'
  turn_id: string
  cost_usd: number
}
export interface ErrorEvent extends EventBase {
  type: 'error'
  code: ErrorCode
  retryable: boolean
}

export type Event =
  | SessionStatusEvent
  | MessageUserEvent
  | MessageStartedEvent
  | MessageTextDeltaEvent
  | ToolStartedEvent
  | ToolFinishedEvent
  | MessageCompletedEvent
  | ModelSwitchedEvent
  | MemoryWriteEvent
  | UsageEvent
  | ErrorEvent

// ---- Decoding ----

export class ProtocolError extends Error {}

export interface Shape<T> {
  required: readonly (keyof T & string)[]
  optional?: readonly (keyof T & string)[]
}

/**
 * Checks a decoded object's top-level keys against its shape: every required
 * key present, none outside required + optional.
 */
export function checkKeys(raw: Record<string, unknown>, what: string, required: readonly string[], optional: readonly string[] = []): void {
  for (const k of required) {
    if (!(k in raw)) throw new ProtocolError(`${what}: missing key ${k}`)
  }
  const allowed = new Set([...required, ...optional])
  for (const k of Object.keys(raw)) {
    if (!allowed.has(k)) throw new ProtocolError(`${what}: unexpected key ${k}`)
  }
}

export function asObject(raw: unknown, what: string): Record<string, unknown> {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new ProtocolError(`${what}: not an object`)
  }
  return raw as Record<string, unknown>
}

type ClientEventOf<K extends ClientEvent['type']> = Extract<ClientEvent, { type: K }>
type EventOf<K extends Event['type']> = Extract<Event, { type: K }>

const CLIENT_EVENT_SHAPES = {
  'user.message': { required: ['client_id', 'content', 'attachment_ids'] },
  'user.interrupt': { required: [] },
} satisfies { [K in ClientEvent['type']]: Shape<ClientEventOf<K>> }

const EVENT_SHAPES = {
  'session.status': { required: ['status'] },
  'message.user': { required: ['message'] },
  'message.started': { required: ['message_id', 'model'] },
  'message.text_delta': { required: ['message_id', 'text'] },
  'tool.started': { required: ['message_id', 'tool_use_id', 'name', 'summary'] },
  'tool.finished': { required: ['message_id', 'tool_use_id', 'is_error', 'summary'] },
  'message.completed': { required: ['message'] },
  'model.switched': { required: ['message_id', 'from', 'to'] },
  'memory.write': { required: ['write'] },
  usage: { required: ['turn_id', 'cost_usd'] },
  error: { required: ['code', 'retryable'] },
} satisfies { [K in Event['type']]: Shape<EventOf<K>> }

const EVENT_HEADER = ['id', 'session_id', 'type', 'created_at'] as const

function has<T extends object>(table: T, key: string): key is Extract<keyof T, string> {
  return Object.hasOwn(table, key)
}

/** Validates the envelope and top-level keys of a ClientEvent; unknown types throw. */
export function decodeClientEvent(raw: unknown): ClientEvent {
  const o = asObject(raw, 'client event')
  const type = o['type']
  if (typeof type !== 'string' || !has(CLIENT_EVENT_SHAPES, type)) {
    throw new ProtocolError(`unknown client event type ${String(type)}`)
  }
  checkKeys(o, type, ['type', ...CLIENT_EVENT_SHAPES[type].required])
  return o as unknown as ClientEvent
}

/** Validates the envelope and top-level keys of an Event; unknown types throw. */
export function decodeEvent(raw: unknown): Event {
  const o = asObject(raw, 'event')
  const type = o['type']
  if (typeof type !== 'string' || !has(EVENT_SHAPES, type)) {
    throw new ProtocolError(`unknown event type ${String(type)}`)
  }
  checkKeys(o, type, [...EVENT_HEADER, ...EVENT_SHAPES[type].required])
  return o as unknown as Event
}
