import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  ERROR_COPY,
  MAX_ATTACHMENT_BYTES,
  decodeClientEvent,
  decodeEvent,
  decodeLinkFrame,
} from '../src/index'
import type {
  AgentList,
  AgentSummary,
  Attachment,
  CreateSession,
  ErrorInfo,
  EvalRun,
  EvalSpec,
  ForkRequest,
  MemoryWrite,
  Me,
  Profile,
  ProfileUpdate,
  SendResult,
  Session,
  SessionList,
  SessionPatch,
  Tree,
  UsageReconciliation,
  UsageReconciliationResult,
} from '../src/index'

const root = join(import.meta.dirname, '..', '..', '..', 'protocol', 'fixtures')

function load(sub: string, file: string): unknown {
  return JSON.parse(readFileSync(join(root, sub, file), 'utf8'))
}

// [file, wire type]: the discriminator each hand-written fixture must carry.
const events: [string, string][] = [
  ['event-session-status.json', 'session.status'],
  ['event-message-user.json', 'message.user'],
  ['event-message-started.json', 'message.started'],
  ['event-message-text-delta.json', 'message.text_delta'],
  ['event-tool-started.json', 'tool.started'],
  ['event-tool-finished.json', 'tool.finished'],
  ['event-message-completed.json', 'message.completed'],
  ['event-model-switched.json', 'model.switched'],
  ['event-memory-write.json', 'memory.write'],
  ['event-usage.json', 'usage'],
  ['event-error.json', 'error'],
]

const clientEvents: [string, string][] = [
  ['client-user-message.json', 'user.message'],
  ['client-user-interrupt.json', 'user.interrupt'],
]

const frames: [string, string][] = [
  ['host-open.json', 'host.open'],
  ['host-open-resume.json', 'host.open'],
  ['host-open-resume-plain.json', 'host.open'],
  ['turn-send.json', 'turn.send'],
  ['turn-interrupt.json', 'turn.interrupt'],
  ['host-close.json', 'host.close'],
  ['tree-purge.json', 'tree.purge'],
  ['rpc-result-applied.json', 'rpc.result'],
  ['rpc-result-rejected.json', 'rpc.result'],
  ['rpc-result-error.json', 'rpc.result'],
  ['shutdown.json', 'shutdown'],
  ['hello.json', 'hello'],
  ['host-state-ready.json', 'host.state'],
  ['host-state-ready-sdk.json', 'host.state'],
  ['host-state-closed.json', 'host.state'],
  ['event-message-started.json', 'event'],
  ['event-text-delta.json', 'event'],
  ['event-tool-started.json', 'event'],
  ['event-tool-finished.json', 'event'],
  ['event-tool-finished-bare.json', 'event'],
  ['event-main-request-usage.json', 'event'],
  ['event-model-switched.json', 'event'],
  ['event-turn-result.json', 'event'],
  ['event-turn-result-null-basis.json', 'event'],
  ['event-error.json', 'event'],
  ['event-error-plain.json', 'event'],
  ['rpc.json', 'rpc'],
]

// A body with no `type`: its top-level keys, required then optional. Each key
// is checked against the TS type, so a fixture key the type lacks cannot be
// listed, and an exact-key fixture ties the type to the file.
interface Plain {
  file: string
  required: readonly string[]
  optional: readonly string[]
}
function plain<T>(file: string, required: readonly (keyof T & string)[], optional: readonly (keyof T & string)[] = []): Plain {
  return { file, required, optional }
}

const clientPlain: Plain[] = [
  plain<CreateSession>('create-session.json', ['agent', 'params']),
  plain<CreateSession>('create-session-minimal.json', ['agent', 'params']),
  plain<Session>('session.json', ['id', 'root_id', 'parent_id', 'fork_at_message_id', 'agent_id', 'agent_version', 'title', 'tier', 'model', 'effort', 'incognito', 'workspace', 'source', 'status', 'tainted', 'archived_at', 'created_at', 'updated_at', 'last_activity_at']),
  plain<SessionList>('session-list.json', ['sessions'], ['next']),
  plain<SessionPatch>('session-patch.json', ['title']),
  plain<ForkRequest>('fork-request.json', ['at_message']),
  plain<SendResult>('send-result.json', [], ['message']),
  plain<Tree>('tree.json', ['messages', 'head_id', 'last_event_ids', 'pending_writes']),
  plain<Tree>('tree-empty.json', ['messages', 'head_id', 'last_event_ids', 'pending_writes']),
  plain<Attachment>('attachment.json', ['id', 'media_type', 'filename', 'size_bytes'], ['width', 'height', 'pages']),
  plain<Attachment>('attachment-pdf.json', ['id', 'media_type', 'filename', 'size_bytes'], ['width', 'height', 'pages']),
  plain<Profile>('profile.json', ['content', 'version', 'updated_at', 'pending']),
  plain<ProfileUpdate>('profile-update.json', ['content', 'version']),
  plain<MemoryWrite>('memory-write.json', ['id', 'session_id', 'turn_id', 'op', 'text', 'matched', 'untrusted', 'status', 'created_at', 'decided_at']),
  plain<Me>('me.json', ['person', 'budget', 'maintenance']),
  plain<AgentSummary>('agent-summary.json', ['id', 'name', 'description', 'tiers', 'default_tier']),
  plain<AgentList>('agent-list.json', ['agents']),
  plain<ErrorInfo>('error-info.json', ['code', 'message']),
]

const internalPlain: Plain[] = [
  plain<EvalSpec>('eval-spec.json', ['agent', 'helper_model', 'timezone', 'webfetch_deny_domains']),
  plain<EvalRun>('eval-run.json', ['suite', 'sdk_version', 'cli_version', 'image', 'started_at', 'finished_at', 'passed', 'failed', 'cost_usd', 'results']),
  plain<UsageReconciliation>('usage-reconciliation.json', ['day', 'workspaces']),
  plain<UsageReconciliationResult>('usage-reconciliation-result.json', ['workspaces']),
]

const listed = (sub: string): string[] => {
  if (sub === 'client') {
    return [...events, ...clientEvents].map(([f]) => f).concat(clientPlain.map((p) => p.file))
  }
  if (sub === 'link') return frames.map(([f]) => f)
  return internalPlain.map((p) => p.file)
}

describe('round-trips every fixture', () => {
  for (const [file, type] of events) {
    it(`client/${file}`, () => {
      const raw = load('client', file)
      const e = decodeEvent(raw)
      expect(e.type).toBe(type)
      expect(JSON.parse(JSON.stringify(e))).toEqual(raw)
    })
  }
  for (const [file, type] of clientEvents) {
    it(`client/${file}`, () => {
      const raw = load('client', file)
      const e = decodeClientEvent(raw)
      expect(e.type).toBe(type)
      expect(JSON.parse(JSON.stringify(e))).toEqual(raw)
    })
  }
  for (const [file, type] of frames) {
    it(`link/${file}`, () => {
      const raw = load('link', file)
      const f = decodeLinkFrame(raw)
      expect(f.type).toBe(type)
      expect(JSON.parse(JSON.stringify(f))).toEqual(raw)
    })
  }
  for (const [sub, list] of [['client', clientPlain], ['internal', internalPlain]] as const) {
    for (const p of list) {
      it(`${sub}/${p.file}`, () => {
        const raw = load(sub, p.file) as Record<string, unknown>
        const keys = Object.keys(raw)
        for (const k of p.required) expect(keys, `${p.file} lacks ${k}`).toContain(k)
        for (const k of keys) expect([...p.required, ...p.optional], `${p.file} has unexpected ${k}`).toContain(k)
        expect(JSON.parse(JSON.stringify(raw))).toEqual(raw)
      })
    }
  }
})

describe('fixture table', () => {
  for (const sub of ['client', 'link', 'internal']) {
    it(`lists every file in fixtures/${sub} exactly once`, () => {
      const onDisk = readdirSync(join(root, sub)).sort()
      expect([...listed(sub)].sort()).toEqual(onDisk)
    })
  }
})

describe('unknown types are rejected', () => {
  it('link frame', () => {
    expect(() => decodeLinkFrame({ type: 'host.explode', session_id: 'x' })).toThrow(/unknown link frame type/)
    expect(() => decodeLinkFrame({ session_id: 'x' })).toThrow()
  })
  it('link event', () => {
    expect(() => decodeLinkFrame({ type: 'event', session_id: 'x', turn_id: 't', event: { type: 'mystery' } })).toThrow(/unknown link event type/)
  })
  it('client event', () => {
    expect(() => decodeEvent({ id: 1, session_id: 'x', type: 'message.vanished', created_at: 'now' })).toThrow(/unknown event type/)
    expect(() => decodeClientEvent({ type: 'user.shout' })).toThrow(/unknown client event type/)
  })
  it('unexpected and missing keys', () => {
    expect(() => decodeLinkFrame({ type: 'shutdown', reason: 'x', extra: 1 })).toThrow(/unexpected key extra/)
    expect(() => decodeLinkFrame({ type: 'shutdown' })).toThrow(/missing key reason/)
  })
})

describe('limits', () => {
  it('MAX_ATTACHMENT_BYTES is 20 MiB', () => {
    expect(MAX_ATTACHMENT_BYTES).toBe(20971520)
  })
})

// The C3 codes and copy, typed by hand: an intended duplicate of the table.
const C3: Record<string, string> = {
  rate_limited: 'Luma is getting a lot of requests right now. Wait a moment and try again.',
  overloaded: 'Claude is overloaded right now. Try again in a minute.',
  conductor_restarted: 'Luma restarted while answering. Your conversation is safe — send your message again.',
  budget_exhausted: "Luma has used this month's budget. It will be back on {resets_at, local date}.",
  budget_warning: "Luma has used {pct}% of this month's budget.",
  maintenance: 'Luma is down for maintenance. Try again later.',
  capacity: 'Luma is busy with other conversations. Try again in a minute.',
  busy: 'Luma is still answering in this conversation.',
  attachment_too_large: 'That file is over 20 MB.',
  image_too_large: 'That image is too large. Photos up to 40 megapixels work.',
  pdf_too_long: 'That PDF has more than {max} pages, the most Luma reads in this mode.',
  conversation_attachments_full: 'This conversation already holds 20 MB of files. Start a new chat to add more.',
  unsupported_attachment: 'Luma can read photos, PDFs and text files.',
  memory_full: "The profile is full. Ask the person to tidy it in Luma's profile page.",
  memory_disabled: 'Memory is off in this conversation.',
  memory_pending_limit: "Several memory changes are already waiting for the person's review. Don't save more in this turn.",
  forget_too_short: 'Say at least 3 characters of what to forget.',
  upstream_error: 'Something went wrong talking to Claude. Try again.',
  not_invited: "This account isn't invited to Luma.",
}

describe('ERROR_COPY', () => {
  it('keys are exactly the C3 codes', () => {
    expect(Object.keys(ERROR_COPY).sort()).toEqual(
      [
        'attachment_too_large',
        'budget_exhausted',
        'budget_warning',
        'busy',
        'capacity',
        'conductor_restarted',
        'conversation_attachments_full',
        'forget_too_short',
        'image_too_large',
        'maintenance',
        'memory_disabled',
        'memory_full',
        'memory_pending_limit',
        'not_invited',
        'overloaded',
        'pdf_too_long',
        'rate_limited',
        'unsupported_attachment',
        'upstream_error',
      ],
    )
  })
  it('equals the C3 table', () => {
    expect({ ...ERROR_COPY }).toEqual(C3)
  })
})
