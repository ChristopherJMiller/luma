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

/** An Anthropic-shaped input block, as Core sends it to the conductor. */
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

/** The only block a client event carries. */
export interface TextBlock {
  type: 'text'
  text: string
}

export interface UserMessage {
  type: 'user.message'
  client_id: string
  content: TextBlock[]
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
export interface SessionErrorEvent extends EventBase {
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
  | SessionErrorEvent

