import type { MemoryWrite, Message, SessionStatusValue, Timestamp } from './events'

export type Tier = 'fast' | 'smart' | 'deep'

/** Optional create-time parameters. `workspace` accepts only "none" in phase 1. */
export interface SessionParams {
  tier?: Tier
  effort?: string
  incognito?: boolean
  workspace?: 'none'
}

/** POST /v1/sessions. There is no `source`: Core derives it from the auth path. */
export interface CreateSession {
  agent: string
  params: SessionParams
}

/** The client's view of a session row. */
export interface Session {
  id: string
  root_id: string
  parent_id: string | null
  fork_at_message_id: string | null
  agent_id: string
  agent_version: string
  title: string
  tier: Tier
  model: string
  effort: string
  incognito: boolean
  /** The sandbox workspace: "none" in phase 1. */
  workspace: 'none'
  source: 'web' | 'ci'
  status: SessionStatusValue
  tainted: boolean
  archived_at: Timestamp | null
  created_at: Timestamp
  updated_at: Timestamp
  last_activity_at: Timestamp
}

/** GET /v1/sessions. */
export interface SessionList {
  sessions: Session[]
  next?: string
}

/** PATCH /v1/sessions/{id}. */
export interface SessionPatch {
  title: string
}

/** POST /v1/sessions/{id}/fork. */
export interface ForkRequest {
  at_message: string
}

/** The result of POST /v1/sessions/{id}/events. */
export interface SendResult {
  message?: Message
}

export interface TreeMessage {
  message: Message
  parent_id: string | null
  session_id: string
}

/** GET /v1/sessions/{root}/tree. */
export interface Tree {
  messages: TreeMessage[]
  head_id: string | null
  last_event_ids: Record<string, number>
  pending_writes: MemoryWrite[]
}

/** POST /v1/attachments. */
export interface Attachment {
  id: string
  media_type: string
  filename: string
  size_bytes: number
  width?: number
  height?: number
  pages?: number
}

/** GET /v1/memory/profile. */
export interface Profile {
  content: string
  version: number
  updated_at: Timestamp
  pending: MemoryWrite[]
}

/** PUT /v1/memory/profile. */
export interface ProfileUpdate {
  content: string
  version: number
}

/** The month's spend against the global cap. */
export interface BudgetStatus {
  spent_usd: number
  cap_usd: number
  ratio: number
  warn: boolean
  exhausted: boolean
  resets_at: Timestamp
}

/** GET /v1/me. */
export interface Me {
  person: { id: string; display_name: string; tier: 'admin' | 'family' }
  budget: BudgetStatus
  maintenance: boolean
}

/** GET /v1/agents/{id}, and an element of GET /v1/agents. */
export interface AgentSummary {
  id: string
  name: string
  description: string
  tiers: Tier[]
  default_tier: Tier
}

/** GET /v1/agents. */
export interface AgentList {
  agents: AgentSummary[]
}
