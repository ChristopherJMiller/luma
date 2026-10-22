import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  AGENT_LIST_SHAPE,
  AGENT_SUMMARY_SHAPE,
  ATTACHMENT_SHAPE,
  CREATE_SESSION_SHAPE,
  ERROR_COPY,
  ERROR_INFO_SHAPE,
  EVAL_RUN_SHAPE,
  EVAL_SPEC_SHAPE,
  FORK_REQUEST_SHAPE,
  MAX_ATTACHMENT_BYTES,
  MAX_IMAGE_LONG_EDGE,
  MAX_IMAGE_MEGAPIXELS,
  MAX_PROFILE_BYTES,
  MAX_TOOL_URL_LENGTH,
  MAX_TREE_ATTACHMENT_BYTES,
  MEMORY_WRITE_SHAPE,
  ME_SHAPE,
  PROFILE_SHAPE,
  PROFILE_UPDATE_SHAPE,
  SEND_RESULT_SHAPE,
  SESSION_LIST_SHAPE,
  SESSION_PATCH_SHAPE,
  SESSION_SHAPE,
  TREE_SHAPE,
  USAGE_RECONCILIATION_RESULT_SHAPE,
  USAGE_RECONCILIATION_SHAPE,
  CLIENT_EVENT_FIELD,
  EVENT_FIELD,
  LINK_FRAME_FIELD,
  decodeClientEvent,
  decodeEvent,
  decodeLinkEvent,
  decodeLinkFrame,
} from '../src/index'
import type { RuntimeField } from '../src/index'

const root = join(import.meta.dirname, '..', '..', '..', 'protocol', 'fixtures')

function load(sub: string, file: string): unknown {
  return JSON.parse(readFileSync(join(root, sub, file), 'utf8'))
}

const obj = (shape: object): RuntimeField => ({ k: 'obj', shape: shape as Record<string, RuntimeField> })

// A strict walker, independent of the runtime decoder: every key a shape
// lists is present (unless optional), no key outside the shape is, and every
// value has its kind. Returns the problems found.
function strictWalk(v: unknown, f: RuntimeField, path: string, errs: string[] = []): string[] {
  if (v === null) {
    if (!f.nullable) errs.push(`${path}: null`)
    return errs
  }
  const isObject = typeof v === 'object' && !Array.isArray(v)
  const record = (): Record<string, unknown> => v as Record<string, unknown>
  switch (f.k) {
    case 'str':
      if (typeof v !== 'string') errs.push(`${path}: not a string`)
      break
    case 'num':
      if (typeof v !== 'number') errs.push(`${path}: not a number`)
      break
    case 'bool':
      if (typeof v !== 'boolean') errs.push(`${path}: not a boolean`)
      break
    case 'lit':
      if (v !== f.value) errs.push(`${path}: not ${f.value}`)
      break
    case 'arr':
      if (!Array.isArray(v)) errs.push(`${path}: not an array`)
      else v.forEach((x, i) => strictWalk(x, f.of as RuntimeField, `${path}[${i}]`, errs))
      break
    case 'map':
      if (!isObject) errs.push(`${path}: not an object`)
      else for (const [k, x] of Object.entries(record())) strictWalk(x, f.of as RuntimeField, `${path}.${k}`, errs)
      break
    case 'obj':
    case 'union': {
      if (!isObject) {
        errs.push(`${path}: not an object`)
        break
      }
      const o = record()
      let shape = f.shape
      if (f.k === 'union') {
        const tag = o['type']
        shape = typeof tag === 'string' ? f.variants?.[tag] : undefined
        if (!shape) {
          errs.push(`${path}: unknown type ${String(tag)}`)
          break
        }
      }
      const fields = shape as Record<string, RuntimeField>
      for (const [key, field] of Object.entries(fields)) {
        if (!(key in o)) {
          if (!field.opt) errs.push(`${path}: missing ${key}`)
          continue
        }
        strictWalk(o[key], field, `${path}.${key}`, errs)
      }
      for (const key of Object.keys(o)) if (!(key in fields)) errs.push(`${path}: unexpected ${key}`)
      break
    }
  }
  return errs
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
  ['host-close-error.json', 'host.close'],
  ['tree-purge.json', 'tree.purge'],
  ['rpc-result-applied.json', 'rpc.result'],
  ['rpc-result-full.json', 'rpc.result'],
  ['rpc-result-rejected.json', 'rpc.result'],
  ['rpc-result-pending.json', 'rpc.result'],
  ['rpc-result-disabled.json', 'rpc.result'],
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

// Bodies with no `type`: file and the shape it must match.
const clientPlain: [string, RuntimeField][] = [
  ['create-session.json', obj(CREATE_SESSION_SHAPE)],
  ['create-session-minimal.json', obj(CREATE_SESSION_SHAPE)],
  ['session.json', obj(SESSION_SHAPE)],
  ['session-list.json', obj(SESSION_LIST_SHAPE)],
  ['session-patch.json', obj(SESSION_PATCH_SHAPE)],
  ['fork-request.json', obj(FORK_REQUEST_SHAPE)],
  ['send-result.json', obj(SEND_RESULT_SHAPE)],
  ['tree.json', obj(TREE_SHAPE)],
  ['tree-empty.json', obj(TREE_SHAPE)],
  ['attachment.json', obj(ATTACHMENT_SHAPE)],
  ['attachment-pdf.json', obj(ATTACHMENT_SHAPE)],
  ['profile.json', obj(PROFILE_SHAPE)],
  ['profile-update.json', obj(PROFILE_UPDATE_SHAPE)],
  ['memory-write.json', obj(MEMORY_WRITE_SHAPE)],
  ['me.json', obj(ME_SHAPE)],
  ['agent-summary.json', obj(AGENT_SUMMARY_SHAPE)],
  ['agent-list.json', obj(AGENT_LIST_SHAPE)],
  ['error-info.json', obj(ERROR_INFO_SHAPE)],
  ['error-info-not-found.json', obj(ERROR_INFO_SHAPE)],
]

const internalPlain: [string, RuntimeField][] = [
  ['eval-spec.json', obj(EVAL_SPEC_SHAPE)],
  ['eval-run.json', obj(EVAL_RUN_SHAPE)],
  ['usage-reconciliation.json', obj(USAGE_RECONCILIATION_SHAPE)],
  ['usage-reconciliation-result.json', obj(USAGE_RECONCILIATION_RESULT_SHAPE)],
]

const listed = (sub: string): string[] => {
  if (sub === 'client') {
    return [...events, ...clientEvents].map(([f]) => f).concat(clientPlain.map(([f]) => f))
  }
  if (sub === 'link') return frames.map(([f]) => f)
  return internalPlain.map(([f]) => f)
}

describe('round-trips every fixture', () => {
  for (const [file, type] of events) {
    it(`client/${file}`, () => {
      const raw = load('client', file)
      const e = decodeEvent(raw)
      expect(e.type).toBe(type)
      expect(strictWalk(raw, EVENT_FIELD, 'event')).toEqual([])
      expect(JSON.parse(JSON.stringify(e))).toEqual(raw)
    })
  }
  for (const [file, type] of clientEvents) {
    it(`client/${file}`, () => {
      const raw = load('client', file)
      const e = decodeClientEvent(raw)
      expect(e.type).toBe(type)
      expect(strictWalk(raw, CLIENT_EVENT_FIELD, 'client event')).toEqual([])
      expect(JSON.parse(JSON.stringify(e))).toEqual(raw)
    })
  }
  for (const [file, type] of frames) {
    it(`link/${file}`, () => {
      const raw = load('link', file)
      const f = decodeLinkFrame(raw)
      expect(f.type).toBe(type)
      expect(strictWalk(raw, LINK_FRAME_FIELD, 'frame')).toEqual([])
      expect(JSON.parse(JSON.stringify(f))).toEqual(raw)
    })
  }
  for (const [sub, list] of [['client', clientPlain], ['internal', internalPlain]] as const) {
    for (const [file, shape] of list) {
      it(`${sub}/${file}`, () => {
        const raw = load(sub, file)
        expect(strictWalk(raw, shape, file)).toEqual([])
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

// The walker and the decoder must be able to fail, or the tests above prove
// nothing. Each case mutates a deep clone of a real fixture.
describe('shapes catch drift', () => {
  const clone = (x: unknown): any => JSON.parse(JSON.stringify(x))

  it('the walker finds an extra key, a missing key, a wrong kind and a bad nullable, deep', () => {
    const base = load('link', 'host-open.json')

    const extra = clone(base)
    extra.spec.agent.limits.surprise = 1
    expect(strictWalk(extra, LINK_FRAME_FIELD, 'f')).toEqual(['f.spec.agent.limits: unexpected surprise'])

    const missing = clone(base)
    delete missing.spec.agent.tier_binding.pdf_max_pages
    expect(strictWalk(missing, LINK_FRAME_FIELD, 'f')).toEqual(['f.spec.agent.tier_binding: missing pdf_max_pages'])

    const kind = clone(base)
    kind.spec.agent.subagents['deep-think'].tools = 'none'
    expect(strictWalk(kind, LINK_FRAME_FIELD, 'f')).toEqual(['f.spec.agent.subagents.deep-think.tools: not an array'])

    const nul = clone(base)
    nul.spec.person.ref = null
    expect(strictWalk(nul, LINK_FRAME_FIELD, 'f')).toEqual(['f.spec.person.ref: null'])

    const part = clone(load('client', 'event-message-completed.json'))
    part.message.content[0].type = 'video'
    expect(strictWalk(part, EVENT_FIELD, 'e')).toEqual(['e.message.content[0]: unknown type video'])
  })

  it('the decoders reject a missing nested key, a wrong kind and a wrong nested type tag', () => {
    const missing = clone(load('link', 'host-open.json'))
    delete missing.spec.agent.limits.max_turns
    expect(() => decodeLinkFrame(missing)).toThrow(/limits: missing key max_turns/)

    const kind = clone(load('link', 'turn-send.json'))
    kind.untrusted = 'yes'
    expect(() => decodeLinkFrame(kind)).toThrow(/untrusted: expected boolean/)

    const block = clone(load('link', 'turn-send.json'))
    block.content[1].type = 'audio'
    expect(() => decodeLinkFrame(block)).toThrow(/unknown type audio/)

    const nul = clone(load('client', 'event-message-completed.json'))
    nul.message.turn_id = null
    expect(() => decodeEvent(nul)).toThrow(/turn_id: unexpected null/)

    const result = clone(load('link', 'event-turn-result.json'))
    delete result.event.totals.models['claude-haiku-5-5'].cost_usd
    expect(() => decodeLinkFrame(result)).toThrow(/missing key cost_usd/)
  })

  it('the decoders ignore unknown keys, at any depth (S37)', () => {
    const f = clone(load('link', 'host-open.json'))
    f.added_later = true
    f.spec.agent.limits.also_later = 1
    expect(decodeLinkFrame(f).type).toBe('host.open')

    const e = clone(load('client', 'event-message-completed.json'))
    e.message.content[0].extra = 'x'
    e.future = 1
    expect(decodeEvent(e).type).toBe('message.completed')
  })

  it('decodeLinkEvent checks a bare link event', () => {
    expect(decodeLinkEvent({ type: 'text_delta', text: 'x' }).type).toBe('text_delta')
    expect(() => decodeLinkEvent({ type: 'text_delta' })).toThrow(/missing key text/)
  })
})

describe('unknown types are rejected', () => {
  it('link frame', () => {
    expect(() => decodeLinkFrame({ type: 'host.explode', session_id: 'x' })).toThrow(/unknown link frame type/)
    expect(() => decodeLinkFrame({ session_id: 'x' })).toThrow()
  })
  it('link event', () => {
    expect(() => decodeLinkFrame({ type: 'event', session_id: 'x', turn_id: 't', event: { type: 'mystery' } })).toThrow(
      /unknown type mystery/,
    )
  })
  it('client event', () => {
    expect(() => decodeEvent({ id: 1, session_id: 'x', type: 'message.vanished', created_at: 'now' })).toThrow(/unknown event type/)
    expect(() => decodeClientEvent({ type: 'user.shout' })).toThrow(/unknown client event type/)
  })
  it('a missing top-level key', () => {
    expect(() => decodeLinkFrame({ type: 'shutdown' })).toThrow(/missing key reason/)
  })
})

describe('limits', () => {
  it('MAX_ATTACHMENT_BYTES is 20 MiB', () => {
    expect(MAX_ATTACHMENT_BYTES).toBe(20971520)
  })
  it('every limit equals protocol/fixtures/limits.json, which the Go test also reads', () => {
    const shared = JSON.parse(readFileSync(join(root, 'limits.json'), 'utf8'))
    expect({
      max_attachment_bytes: MAX_ATTACHMENT_BYTES,
      max_tree_attachment_bytes: MAX_TREE_ATTACHMENT_BYTES,
      max_image_megapixels: MAX_IMAGE_MEGAPIXELS,
      max_image_long_edge: MAX_IMAGE_LONG_EDGE,
      max_tool_url_length: MAX_TOOL_URL_LENGTH,
      max_profile_bytes: MAX_PROFILE_BYTES,
    }).toEqual(shared)
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
  not_found: "Luma can't find that. It may have been deleted.",
  invalid_request: "Luma didn't understand that request. Reload the page and try again.",
  request_too_large: 'That message is too long for Luma. Shorten it and try again.',
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
        'invalid_request',
        'maintenance',
        'memory_disabled',
        'memory_full',
        'memory_pending_limit',
        'not_found',
        'not_invited',
        'overloaded',
        'pdf_too_long',
        'rate_limited',
        'request_too_large',
        'unsupported_attachment',
        'upstream_error',
      ],
    )
  })
  it('equals the C3 table', () => {
    expect({ ...ERROR_COPY }).toEqual(C3)
  })
})
