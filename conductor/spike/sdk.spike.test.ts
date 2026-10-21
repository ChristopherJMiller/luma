// Spike S: how the pinned Agent SDK 0.3.303 and Claude Code CLI 2.1.303
// behave, against fakeanthropic (docs/spikes/phase-1.md section S). These
// are change detectors (S20): each case asserts that what it observes equals
// the constant in src/spike-results.ts that was typed from it, so a bump
// that changes the behaviour fails `make sdk-spike`.
//
// Every request is answered by the fake, which checks thinking bindings, so
// each resume and fork case is also a preserved-thinking check. The cases
// run in file order; S9's last case reads the requests of all of them.
import { existsSync } from 'node:fs'
import { afterAll, describe, expect, it } from 'vitest'
import * as R from '../src/spike-results.js'
import { ConnectProxy } from './connectproxy.js'
import {
  AGENT_BODY,
  allFakes,
  cacheControls,
  Clock,
  count,
  Fake,
  GATEWAY_TOKEN,
  hasThinking,
  header,
  HELPER_MODEL,
  Host,
  INLINE_SETTINGS,
  isMessages,
  lastMainUuid,
  mainRequests,
  McpServer,
  messagesText,
  MODEL,
  newDataDir,
  normalized,
  PERSON_REF,
  RecordingStore,
  requestClass,
  systemText,
  texts,
  tokenTotals,
  userIdOf,
  withoutMetadata,
  type FakeRequest,
  type Step,
} from './harness.js'
import { makeTlsFixtures } from './tlsfixtures.js'

afterAll(async () => {
  for (const f of allFakes) await f.stop()
})

const TTL_1H = '"cache_control":{"type":"ephemeral","ttl":"1h"}'
const TTL_DEFAULT = '"cache_control":{"type":"ephemeral"}'

/** deep-think as Task 16 defines it (AgentSpec.subagents). */
const DEEP_THINK = { 'deep-think': { description: 'Thinks hard about a question.', prompt: 'You think hard.', tools: [], model: 'claude-opus-5-5', effort: 'high' as const } }

/**
 * A clock offset that puts the CLI at 06:00 UTC today, so that no case whose
 * bytes include the date crosses midnight while it runs.
 */
function morning(): { base: number; day0: string; day1: string } {
  const now = Date.now()
  const six = new Date(now)
  six.setUTCHours(6, 0, 0, 0)
  return {
    base: Math.round((six.getTime() - now) / 1000),
    day0: six.toISOString().slice(0, 10),
    day1: new Date(six.getTime() + 86_400_000).toISOString().slice(0, 10),
  }
}

const sdkMessages = (h: Host, type: string, subtype?: string): any[] =>
  h.messages.filter((m: any) => m.type === type && (subtype === undefined || m.subtype === subtype))
const hooksOf = (h: Host, event: string): any[] => h.hookInputs.filter((i: any) => i.hook_event_name === event)

it('runs the pinned CLI', async () => {
  const fake = await Fake.start({ v: [{ text: 'Hi.' }] })
  const h = new Host({ fake, store: new RecordingStore(), root: 'tree-version' })
  await h.send('Hi [[fake:v]]')
  await h.close()
  expect(sdkMessages(h, 'system', 'init')[0].claude_code_version).toBe('2.1.303')
})

describe('S1: resume is byte-identical', () => {
  const steps = { s1: [{ text: 'One.' }, { text: 'Two.' }, { text: 'Three.' }] }
  const turns = ['First [[fake:s1]]', 'Second [[fake:s1]]', 'Third [[fake:s1]]']
  const dataDir = newDataDir()
  const t = morning()
  let a: FakeRequest[] = []

  // Run B: two turns, close, delete the local JSONL, resume from the store, turn three.
  async function runB(resumeClock: Clock): Promise<FakeRequest[]> {
    const fake = await Fake.start(steps)
    const store = new RecordingStore()
    const h1 = new Host({ fake, store, root: 'tree-s1', dataDir, wipe: true, clock: new Clock(t.base) })
    await h1.send(turns[0]!)
    await h1.send(turns[1]!)
    const sid = h1.sessionId
    await h1.close()
    h1.removeLocalTranscript(sid)
    const h2 = new Host({ fake, store, root: 'tree-s1', dataDir, resume: sid, clock: resumeClock })
    await h2.send(turns[2]!)
    await h2.close()
    return mainRequests(fake.requests())
  }

  it('S1: request 3 after a store resume equals the unbroken run outside metadata, thinking accepted', async () => {
    const fake = await Fake.start(steps)
    const h = new Host({ fake, store: new RecordingStore(), root: 'tree-s1', dataDir, clock: new Clock(t.base) })
    for (const turn of turns) await h.send(turn)
    await h.close()
    a = mainRequests(fake.requests())
    const b = await runB(new Clock(t.base))

    expect(a).toHaveLength(3)
    expect(b).toHaveLength(3)
    expect([...a, ...b].map((r) => r.bindingOk)).toEqual([true, true, true, true, true, true])
    expect(withoutMetadata(b[2]!).equals(withoutMetadata(a[2]!))).toBe(true)
    // metadata.user_id is the session id and the config dir's device id
    // (.claude.json, which the SDK copies into its resume dir): the same
    // after a resume. Only A and B differ there, being two sessions, and the
    // wipe gave B a new device id.
    expect(userIdOf(b[2]!)).toEqual(userIdOf(b[1]!))
    expect(userIdOf(b[2]!).session_id).not.toBe(userIdOf(a[2]!).session_id)
    expect(R.IDLE_EVICT_MS).toBe(600_000)
  })

  it('S1b: a resume a day later keeps the date line in the prefix and notes the new date at the tail', async () => {
    const b = await runB(new Clock(t.base + 86_400))
    expect(b.map((r) => r.bindingOk)).toEqual([true, true, true])
    const am = a[2]!.json.messages
    const bm = b[2]!.json.messages
    expect(bm).toHaveLength(am.length)
    // Everything up to the trailing system message is unchanged, the first
    // turn's "Today's date is" line included...
    expect(bm.slice(0, -1).map((m: any) => JSON.stringify(m))).toEqual(am.slice(0, -1).map((m: any) => JSON.stringify(m)))
    expect(texts(bm[1]).join('')).toContain(`\n\nToday's date is ${t.day0}.`)
    // ...and the new date arrives in the trailing system message, which
    // follows the previous request's cached prefix.
    expect(texts(bm.at(-1))).toEqual([
      `${texts(am.at(-1))[0]}\n\nThe date has changed. Today's date is now ${t.day1}. No need to announce the new date — the user's own clock shows it.`,
    ])
    const rest = (r: FakeRequest) => JSON.stringify({ ...r.json, messages: undefined, metadata: undefined })
    expect(rest(b[2]!)).toBe(rest(a[2]!))
  })
})

it('S2: every session of a tree writes under projectKey = the root', async () => {
  const fake = await Fake.start({ s2: [{ text: 'One.' }, { text: 'Fork.' }] })
  const store = new RecordingStore()
  const dataDir = newDataDir()
  const h1 = new Host({ fake, store, root: 'tree-s2', dataDir })
  const t1 = await h1.send('One [[fake:s2]]')
  const sid = h1.sessionId
  await h1.close()
  const h2 = new Host({ fake, store, root: 'tree-s2', dataDir, resume: sid, forkSession: true, resumeSessionAt: lastMainUuid(t1.messages) })
  await h2.send('Fork [[fake:s2]]')
  const sid2 = h2.sessionId
  await h2.close()

  expect(sid2).not.toBe(sid)
  const keys = new Set(store.appends.map((x) => `${x.key.projectKey}|${x.key.sessionId}|${x.key.subpath ?? ''}`))
  expect([...keys].sort()).toEqual([`tree-s2|${sid}|`, `tree-s2|${sid2}|`].sort())
  // The local copy of the fresh session sits in the project dir the root names.
  expect(existsSync(h1.transcriptPath(sid))).toBe(true)
})

it('S3: main requests cache for 1h; subagent requests for the default 5m; helper requests not at all', async () => {
  const fake = await Fake.start({
    s3: [
      { tool_uses: [{ name: 'Agent', input: { subagent_type: 'deep-think', description: 'Think', prompt: 'Think it over [[fake:s3sub]]' } }] },
      { tool_uses: [{ name: 'WebSearch', input: { query: 'rain [[fake:s3search]]' } }] },
      { text: 'Done.' },
    ],
    s3sub: [{ text: 'Deep.' }],
    s3search: [{ text: 'Searched.' }],
  })
  const h = new Host({
    fake,
    store: new RecordingStore(),
    root: 'tree-s3',
    tools: ['WebSearch', 'WebFetch', 'Agent'],
    agents: DEEP_THINK,
    env: { CLAUDE_CODE_DISABLE_BACKGROUND_TASKS: '1' },
  })
  await h.send('Go [[fake:s3]]')
  await h.close()
  const rs = fake.requests()
  expect(rs.map(requestClass)).toEqual(['main', 'subagent', 'main', 'auxiliary', 'main'])
  for (const r of rs) {
    const cc = cacheControls(r)
    if (requestClass(r) === 'main') {
      expect(cc.length).toBeGreaterThan(0)
      expect(cc.every((c) => c === TTL_1H)).toBe(true)
      expect(header(r, 'anthropic-beta')!.split(',')).toContain('extended-cache-ttl-2025-04-11')
    } else if (requestClass(r) === 'subagent') {
      expect(cc.length).toBeGreaterThan(0)
      expect(cc.every((c) => c === TTL_DEFAULT)).toBe(true)
    } else {
      expect(cc).toEqual([])
    }
  }
})

describe('S4: compaction, with S14 the threshold', () => {
  const CTX = 'LUMA-S4-PROFILE-CONTEXT'
  // Four turns, the fourth reply reporting `contextTokens`, then a fifth (and sixth).
  async function probe(contextTokens: number, o: { sixth?: boolean; settings?: Record<string, unknown> } = {}) {
    const fake = await Fake.start({
      p: [{ text: 'A.' }, { text: 'B.' }, { text: 'C.' }, { text: 'D.', usage: { input: contextTokens } }, { text: 'E.' }, { text: 'F.' }, { text: 'G.' }, { text: 'H.' }],
    })
    const h = new Host({
      fake,
      store: new RecordingStore(),
      root: 'tree-s4',
      ...(o.settings ? { settings: o.settings } : {}),
      hooks: {
        SessionStart: [
          {
            matcher: 'startup|compact',
            hooks: [async (i: any) => ({ hookSpecificOutput: { hookEventName: 'SessionStart' as const, additionalContext: `${CTX} ${i.source}` } })],
          },
        ],
      },
    })
    const turns = []
    for (const w of ['One', 'Two', 'Three', 'Four', 'Five', ...(o.sixth ? ['Six'] : [])]) turns.push(await h.send(`${w} [[fake:p]]`))
    await h.close()
    return { h, turns, reqs: fake.requests() }
  }

  it('S14: the CLI compacts at COMPACTION_TRIGGER_TOKENS of context, not one token before', async () => {
    const below = await probe(R.COMPACTION_TRIGGER_TOKENS - 1)
    expect(below.reqs.map(requestClass)).toEqual(['main', 'main', 'main', 'main', 'main'])
    const at = await probe(R.COMPACTION_TRIGGER_TOKENS)
    expect(at.reqs.map(requestClass)).toEqual(['main', 'main', 'main', 'main', 'compaction', 'main', 'main'])
  })

  it('S4: compact_boundary, SessionStart(compact) context in messages, and one binding retry', async () => {
    const { h, turns, reqs } = await probe(90_000, { sixth: true })
    expect(reqs.map(requestClass)).toEqual(['main', 'main', 'main', 'main', 'compaction', 'main', 'main', 'main'])
    const [compaction, first, retry, next] = reqs.slice(4) as [FakeRequest, FakeRequest, FakeRequest, FakeRequest]

    // S3's compaction half: the session model, the default 5m cache, marked auto.
    expect(compaction.json.model).toBe(MODEL)
    expect(header(compaction, 'x-claude-code-compaction')).toBe('auto')
    expect(cacheControls(compaction).length).toBeGreaterThan(0)
    expect(cacheControls(compaction).every((c) => c === TTL_DEFAULT)).toBe(true)

    // The signals: the SDK's compact_boundary, and a header on the main
    // requests of the turn after it (not on later turns).
    const boundary = turns[4]!.messages.filter((m: any) => m.type === 'system' && m.subtype === 'compact_boundary')
    expect(boundary.map((m: any) => m.compact_metadata.trigger)).toEqual(['auto'])
    expect(R.SLO_SIGNALS.postCompaction).toBe('compact_boundary')
    expect([first, retry, next].map((r) => header(r, 'x-claude-code-context-compacted'))).toEqual(['auto', 'auto', undefined])

    // SessionStart fires once, with source compact, and its context lands in
    // messages, exactly once, from the request after the compaction on.
    expect(hooksOf(h, 'SessionStart').map((i) => i.source)).toEqual(['compact'])
    for (const r of [first, retry, next]) {
      expect(count(messagesText(r), `${CTX} compact`)).toBe(1)
      expect(count(systemText(r), CTX)).toBe(0)
    }
    expect(R.SESSION_START_CONTEXT_SOURCES).toEqual(['compact'])

    // The first main request after the compaction replays the kept turn's
    // thinking, signed over the pre-compaction prefix: the fake refuses it as
    // the API would, and the CLI strips the thinking and retries. The plan
    // expected this request to pass.
    expect(first.bindingOk).toBe(false)
    expect(hasThinking(first)).toBe(true)
    expect(retry.bindingOk).toBe(true)
    expect(hasThinking(retry)).toBe(false)
    expect(next.bindingOk).toBe(true)
  })
})

it('S5: a SessionStart callback never runs for startup, resume or fork, so its context reaches no request', async () => {
  const CTX = 'LUMA-S5-PROFILE-CONTEXT'
  const hooks = {
    SessionStart: [{ matcher: 'startup|compact', hooks: [async () => ({ hookSpecificOutput: { hookEventName: 'SessionStart' as const, additionalContext: CTX } })] }],
  }
  const fake = await Fake.start({ s5: [{ text: 'One.' }, { text: 'Two.' }, { text: 'Three.' }, { text: 'Fork.' }] })
  const store = new RecordingStore()
  const dataDir = newDataDir()
  const fresh = new Host({ fake, store, root: 'tree-s5', dataDir, hooks })
  const t1 = await fresh.send('One [[fake:s5]]')
  await fresh.send('Two [[fake:s5]]')
  const sid = fresh.sessionId
  await fresh.close()
  fresh.removeLocalTranscript(sid)
  const resumed = new Host({ fake, store, root: 'tree-s5', dataDir, hooks, resume: sid })
  await resumed.send('Three [[fake:s5]]')
  await resumed.close()
  const forked = new Host({ fake, store, root: 'tree-s5', dataDir, wipe: true, hooks, resume: sid, forkSession: true, resumeSessionAt: lastMainUuid(t1.messages) })
  await forked.send('Fork [[fake:s5]]')
  await forked.close()

  // Every host also records every SessionStart, whatever its source: none ran.
  for (const h of [fresh, resumed, forked]) expect(hooksOf(h, 'SessionStart')).toEqual([])
  const rs = fake.requests()
  expect(rs).toHaveLength(4)
  for (const r of rs) {
    expect(r.bindingOk).toBe(true)
    expect(r.body.includes(CTX)).toBe(false)
  }
  expect(R.CONTEXT_DELIVERY).toBe('first-user-turn')
})

it('S6: totals run within a query, are restored from cost-state entries, and give non-negative deltas', async () => {
  // Turn k reports input 1000·k and output 100·k.
  const steps: Step[] = [1, 2, 3, 4, 5, 6].map((k) => ({ text: `T${k}.`, usage: { input: 1000 * k, output: 100 * k } }))
  const fake = await Fake.start({ s6: steps, s6b: steps.slice(0, 2) })
  const store = new RecordingStore()
  const dataDir = newDataDir()
  const tokens = (r: any) => tokenTotals(r)[MODEL]!.slice(0, 2)
  // The host's baseline rule: the latest TOTALS_ENTRY_TYPE entry of the
  // transcript it loads, or zero when there is none.
  const baseline = (sessionId: string): [number, number] => {
    const e: any = store.getEntries({ projectKey: 'tree-s6', sessionId }).filter((x) => x.type === R.TOTALS_ENTRY_TYPE).at(-1)
    return e ? [e.modelUsage[MODEL].inputTokens, e.modelUsage[MODEL].outputTokens] : [0, 0]
  }
  const delta = (r: any, b: [number, number]) => tokens(r).map((v, i) => v - b[i]!)
  const results: any[] = []

  // One query: results are running totals.
  const h1 = new Host({ fake, store, root: 'tree-s6', dataDir })
  const r1 = (await h1.send('One [[fake:s6]]')).result
  const r2 = (await h1.send('Two [[fake:s6]]')).result
  results.push(r1, r2)
  const sid = h1.sessionId
  expect(tokens(r1)).toEqual([1000, 100])
  expect(tokens(r2)).toEqual([3000, 300])
  await h1.close()
  h1.removeLocalTranscript(sid)

  // A clean close appends one cost-state entry with the last result's totals.
  const saved: any[] = store.ofType('cost-state', sid)
  expect(R.TOTALS_ENTRY_TYPE).toBe('cost-state')
  expect(saved).toHaveLength(1)
  expect(saved[0].totalCostUSD).toBe(r2.total_cost_usd)
  expect([saved[0].modelUsage[MODEL].inputTokens, saved[0].modelUsage[MODEL].outputTokens]).toEqual([3000, 300])

  // Clean-close resume: restored.
  let base = baseline(sid)
  const h2 = new Host({ fake, store, root: 'tree-s6', dataDir, resume: sid })
  const r3 = (await h2.send('Three [[fake:s6]]')).result
  expect(tokens(r3)).toEqual([6000, 600])
  expect(delta(r3, base)).toEqual([3000, 300])
  const r4 = (await h2.send('Four [[fake:s6]]')).result
  results.push(r3, r4)
  expect(tokens(r4)).toEqual([10000, 1000])

  // SIGKILL: no cost-state is written, and the resume restores the one from
  // before the kill. Core's last totals (r4) would make the delta negative.
  await h2.kill()
  expect(store.ofType('cost-state', sid)).toHaveLength(1)
  base = baseline(sid)
  const h3 = new Host({ fake, store, root: 'tree-s6', dataDir, resume: sid })
  const r5 = (await h3.send('Five [[fake:s6]]')).result
  results.push(r5)
  expect(tokens(r5)).toEqual([8000, 800])
  expect(delta(r5, base)).toEqual([5000, 500])
  expect(delta(r5, tokens(r4) as [number, number])).toEqual([-2000, -200])
  await h3.close()

  // Fork from turn 1, from an empty local dir: restores the source's latest cost-state.
  base = baseline(sid)
  expect(base).toEqual([8000, 800])
  const h4 = new Host({ fake, store, root: 'tree-s6', dataDir, wipe: true, resume: sid, forkSession: true, resumeSessionAt: lastMainUuid(h1.messages.slice(0, h1.messages.indexOf(r1) + 1)) })
  const r6 = (await h4.send('Six [[fake:s6]]')).result
  results.push(r6)
  expect(tokens(r6)).toEqual([14000, 1400])
  expect(delta(r6, base)).toEqual([6000, 600])
  await h4.close()

  // A session killed before it ever closed cleanly: nothing to restore, so
  // the baseline is zero (Core's last totals would undercount).
  const k1 = new Host({ fake, store, root: 'tree-s6', dataDir })
  const b1 = (await k1.send('One [[fake:s6b]]')).result
  const ksid = k1.sessionId
  await k1.kill()
  expect(store.ofType('cost-state', ksid)).toEqual([])
  base = baseline(ksid)
  const k2 = new Host({ fake, store, root: 'tree-s6', dataDir, resume: ksid })
  const b2 = (await k2.send('Two [[fake:s6b]]')).result
  results.push(b1, b2)
  await k2.close()
  expect(tokens(b2)).toEqual([2000, 200])
  expect(delta(b2, base)).toEqual([2000, 200])
  expect(delta(b2, tokens(b1) as [number, number])).toEqual([1000, 100])

  expect(R.TOTALS_RESTORED).toEqual({ resume: true, resumeAfterKill: false, fork: true })
  // Every delta in dollars is non-negative too, against the same baselines.
  expect(r3.total_cost_usd).toBeGreaterThan(r2.total_cost_usd)
  expect(r5.total_cost_usd).toBeGreaterThan(saved[0].totalCostUSD)
  // The cost basis rides on every result's modelUsage entries.
  expect(R.COST_BASIS_FIELD).toBe('modelUsage[model].costBasis')
  for (const r of results) expect(Object.values(r.modelUsage).map((u: any) => u.costBasis)).toEqual(['list'])
})

describe('S7: settings', () => {
  it('S7: inline settings reach the CLI as its --settings flag (the flag settings layer)', async () => {
    const fake = await Fake.start({ s7: [{ text: 'One.' }] })
    const h = new Host({ fake, store: new RecordingStore(), root: 'tree-s7' })
    await h.send('One [[fake:s7]]')
    const { argv } = h.cliProcess()
    await h.close()
    expect(argv[argv.indexOf('--settings') + 1]).toBe('{"switchModelsOnFlag":true,"idleCompaction":false}')
    expect(argv).toContain('--setting-sources=')
    expect(argv).toContain('--strict-mcp-config')
    expect(argv).toContain('--permission-mode=default')
  })

  it('S7: precomputeCompactionEnabled, true or false, changes no request below or at the threshold', async () => {
    async function classes(contextTokens: number, precompute: boolean): Promise<(string | undefined)[]> {
      const fake = await Fake.start({
        p: [{ text: 'A.' }, { text: 'B.' }, { text: 'C.' }, { text: 'D.', usage: { input: contextTokens } }, { text: 'E.' }, { text: 'F.' }, { text: 'G.' }],
      })
      const h = new Host({ fake, store: new RecordingStore(), root: 'tree-s7p', settings: { ...INLINE_SETTINGS, precomputeCompactionEnabled: precompute } })
      for (const w of ['One', 'Two', 'Three', 'Four']) await h.send(`${w} [[fake:p]]`)
      // Time for a background precompute to start, if there were one.
      await new Promise((ok) => setTimeout(ok, 3000))
      await h.send('Five [[fake:p]]')
      await h.close()
      return fake.requests().map(requestClass)
    }
    for (const precompute of [true, false]) {
      expect(await classes(R.COMPACTION_TRIGGER_TOKENS - 3000, precompute)).toEqual(['main', 'main', 'main', 'main', 'main'])
      expect(await classes(R.COMPACTION_TRIGGER_TOKENS, precompute)).toEqual(['main', 'main', 'main', 'main', 'compaction', 'main', 'main'])
    }
    expect(R.PRECOMPUTE_SETTING_EXISTS).toBe(false)
  })
})

it('S8: a fork at a message, from a second host reading only the store, replays the kept turn byte for byte', async () => {
  const fake = await Fake.start({
    s8: [{ tool_uses: [{ name: 'WebSearch', input: { query: 'rain tomorrow [[fake:s8search]]' } }] }, { text: 'Rain.' }, { text: 'Two.' }, { text: 'Two, edited.' }],
    s8search: [{ text: 'Search summary.' }],
  })
  const store = new RecordingStore()
  const dataDir = newDataDir()
  const h1 = new Host({ fake, store, root: 'tree-s8', dataDir })
  const t1 = await h1.send('Will it rain? [[fake:s8]]')
  await h1.send('And after? [[fake:s8]]')
  const sid = h1.sessionId
  await h1.close()

  // A1 is thinking + a WebSearch tool use, its result, then thinking + text.
  const a1 = t1.messages.filter((m: any) => m.type === 'assistant' && m.parent_tool_use_id === null).map((m: any) => m.message.content[0].type)
  expect(a1).toEqual(['thinking', 'tool_use', 'thinking', 'text'])

  const h2 = new Host({ fake, store, root: 'tree-s8', dataDir, wipe: true, resume: sid, forkSession: true, resumeSessionAt: lastMainUuid(t1.messages) })
  await h2.send('And after, edited? [[fake:s8]]')
  await h2.close()
  expect(h2.sessionId).not.toBe(sid)

  const mains = mainRequests(fake.requests())
  const orig = mains[2]!.json.messages // turn 2: [U1, A1…, U2, trailing system]
  const fork = mains[3]!
  expect(fork.bindingOk).toBe(true)
  expect(fork.json.messages).toHaveLength(orig.length)
  expect(fork.json.messages.slice(0, -2).map((m: any) => JSON.stringify(m))).toEqual(orig.slice(0, -2).map((m: any) => JSON.stringify(m)))
  expect(fork.json.messages.at(-2)).toEqual({ role: 'user', content: 'And after, edited? [[fake:s8]]' })
  expect(fork.json.messages.slice(0, -2).filter((m: any) => m.role === 'assistant').flatMap((m: any) => m.content.map((b: any) => b.type))).toEqual([
    'thinking',
    'tool_use',
    'thinking',
    'text',
  ])
  expect(R.FORK_STRATEGY).toBe('resumeSessionAt')
})

it('S9: a resume after an idle longer than the cache TTL looks exactly like a warm one', async () => {
  const steps = { s9: [{ text: 'One.' }, { text: 'Two.' }] }
  const dataDir = newDataDir()
  const t = morning()
  async function resumeAfter(idleSeconds: number) {
    const fake = await Fake.start(steps)
    const store = new RecordingStore()
    const h1 = new Host({ fake, store, root: 'tree-s9', dataDir, wipe: true, clock: new Clock(t.base) })
    await h1.send('One [[fake:s9]]')
    const sid = h1.sessionId
    await h1.close()
    h1.removeLocalTranscript(sid)
    const h2 = new Host({ fake, store, root: 'tree-s9', dataDir, resume: sid, clock: new Clock(t.base + idleSeconds) })
    await h2.send('Two [[fake:s9]]')
    await h2.close()
    return { req: mainRequests(fake.requests())[1]!, h2 }
  }
  const warm = await resumeAfter(0)
  const cold = await resumeAfter(70 * 60)
  expect(cold.req.bindingOk).toBe(true)
  expect(withoutMetadata(cold.req).equals(withoutMetadata(warm.req))).toBe(true)
  expect(Object.keys(cold.req.header).sort()).toEqual(Object.keys(warm.req.header).sort())
  expect(hooksOf(cold.h2, 'SessionStart')).toEqual([])
  expect(JSON.stringify(cold.h2.messages)).not.toContain('expired')
  expect(R.SLO_SIGNALS.coldResume).toBeNull()
})

it('S10: polling mcpServerStatus() until connected means the first request lists the memory tools', async () => {
  const mcp = await McpServer.start({ initializeDelayMs: 1000 })
  const fake = await Fake.start({ s10: [{ text: 'One.' }] })
  const h = new Host({
    fake,
    store: new RecordingStore(),
    root: 'tree-s10',
    mcpServers: mcp.config(),
    tools: ['WebSearch', 'WebFetch', 'mcp__luma__remember', 'mcp__luma__forget'],
  })
  const polls = await h.waitMcpConnected()
  await h.send('One [[fake:s10]]')
  await h.close()
  await mcp.stop()
  expect(polls).toBeGreaterThan(1) // it was not connected yet: initialize took 1 s
  const first = fake.requests()[0]!
  expect(first.json.tools.map((x: any) => x.name).sort()).toEqual(['WebFetch', 'WebSearch', 'mcp__luma__forget', 'mcp__luma__remember'])
  expect(first.json.tools.some((x: any) => 'defer_loading' in x)).toBe(false)
  expect([...new Set(mcp.authorization)]).toEqual([`Bearer ${mcp.token}`])
  // The server's instructions travel in the first turn's environment block
  // (messages), not in system.
  expect(count(messagesText(first), 'Luma memory tools (Spike S fixture).')).toBe(1)
  expect(count(systemText(first), 'Luma memory tools')).toBe(0)
})

describe('S11: errors, the subagent tool and model switches', () => {
  it('S11: what the SDK reports for a 429, a 529 and a workspace-limit 400', async () => {
    const limit = 'You have reached your specified workspace API usage limits. You will regain access on 2026-11-01 at 00:00 UTC.'
    const report = async (step: Step) => {
      const fake = await Fake.start({ e: [step, step, step] })
      // One retry instead of the default ten (about three minutes of backoff).
      const h = new Host({ fake, store: new RecordingStore(), root: 'tree-s11e', env: { CLAUDE_CODE_MAX_RETRIES: '1' } })
      const t = await h.send('Hello [[fake:e]]')
      await h.close()
      const assistant: any = t.messages.find((m: any) => m.type === 'assistant')
      const r: any = t.result
      return {
        retries: t.messages.filter((m: any) => m.type === 'system' && m.subtype === 'api_retry').map((m: any) => [m.error, m.error_status]),
        error: assistant.error,
        text: assistant.message.content[0].text,
        result: [r.subtype, r.is_error, r.api_error_status, r.terminal_reason],
        requests: fake.requests(),
      }
    }
    const r429 = await report({ status: 429, retry_after: '0' })
    expect(r429.retries).toEqual([['rate_limit', 429]])
    expect(r429.error).toBe('rate_limit')
    expect(r429.text).toBe('API Error: Request rejected (429) · Rate limit exceeded')
    expect(r429.result).toEqual(['success', true, 429, 'api_error'])

    const r529 = await report({ status: 529 })
    expect(r529.retries).toEqual([['overloaded', 529]])
    expect(r529.error).toBe('server_error')
    expect(r529.text.startsWith('API Error: 529 Overloaded. This is a server-side issue, usually temporary')).toBe(true)
    expect(r529.result).toEqual(['success', true, 529, 'api_error'])

    // A 400 is retried once without the thinking-display beta, and is no api_retry.
    const r400 = await report({ status: 400, error_type: 'invalid_request_error', error_message: limit })
    expect(r400.retries).toEqual([])
    expect(r400.error).toBe('unknown')
    expect(r400.text).toBe(`API Error: 400 ${limit}`)
    expect(r400.result).toEqual(['success', true, 400, 'api_error'])
    expect(r400.requests).toHaveLength(2)
    expect(header(r400.requests[0]!, 'anthropic-beta')!.split(',')).toContain('thinking-display-updates-2026-08-18')
    expect(header(r400.requests[1]!, 'anthropic-beta')!.split(',')).not.toContain('thinking-display-updates-2026-08-18')
    expect([r400.requests[0]!.json.thinking, r400.requests[1]!.json.thinking]).toEqual([{ type: 'adaptive', display: 'updates' }, { type: 'adaptive' }])
  })

  it('S11: a binding 400 is invisible to the SDK: the CLI strips the thinking and retries', async () => {
    const fake = await Fake.start({ b: [{ text: 'One.' }, { text: 'Two.' }] })
    const store = new RecordingStore()
    const dataDir = newDataDir()
    const h1 = new Host({ fake, store, root: 'tree-s11b', dataDir })
    await h1.send('First message [[fake:b]]')
    const sid = h1.sessionId
    await h1.close()
    h1.removeLocalTranscript(sid)
    // Edit the stored first user message: A1's thinking no longer binds.
    const u: any = store.getEntries({ projectKey: 'tree-s11b', sessionId: sid }).find((e) => e.type === 'user')
    u.message.content = 'First message, edited [[fake:b]]'
    const h2 = new Host({ fake, store, root: 'tree-s11b', dataDir, resume: sid })
    const t = await h2.send('Second [[fake:b]]')
    await h2.close()
    const rs = fake.requests()
    expect(rs.map((r) => [r.bindingOk, hasThinking(r)])).toEqual([
      [true, false],
      [false, true],
      [true, false],
    ])
    expect(t.messages.filter((m: any) => m.type === 'system' && m.subtype === 'api_retry')).toEqual([])
    expect(t.messages.filter((m: any) => m.type === 'assistant' && m.error)).toEqual([])
    expect([(t.result as any).subtype, (t.result as any).is_error]).toEqual(['success', false])
  })

  it('S11: the subagent tool is Agent, and its subagent runs in the background unless CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1', async () => {
    async function run(background: boolean) {
      const fake = await Fake.start({
        s11: [{ tool_uses: [{ name: 'Agent', input: { subagent_type: 'deep-think', description: 'Think hard', prompt: 'Consider it [[fake:s11sub]]' } }] }, { text: 'Done.' }],
        s11sub: [{ text: 'Deep answer.' }],
      })
      const h = new Host({
        fake,
        store: new RecordingStore(),
        root: 'tree-s11s',
        tools: ['WebSearch', 'WebFetch', 'Agent'],
        agents: DEEP_THINK,
        ...(background ? {} : { env: { CLAUDE_CODE_DISABLE_BACKGROUND_TASKS: '1' } }),
      })
      const t = await h.send('Think [[fake:s11]]')
      // In the background, the task notification starts a turn of its own.
      if (background) await h.until((ms) => ms.filter((m) => m.type === 'result').length === 2)
      await h.close()
      const toolResult: any = t.messages.find((m: any) => m.type === 'user' && m.parent_tool_use_id === null)
      return { h, rs: fake.requests(), started: sdkMessages(h, 'system', 'task_started')[0], resultText: toolResult.message.content[0].content[0].text as string }
    }
    const bg = await run(true)
    const agentTool = bg.rs[0]!.json.tools.find((x: any) => x.name === R.SUBAGENT_TOOL)
    expect(bg.rs[0]!.json.tools.map((x: any) => x.name).sort()).toEqual(['Agent', 'WebFetch', 'WebSearch'])
    expect(Object.keys(agentTool.input_schema.properties)).toEqual(expect.arrayContaining(['subagent_type', 'model', 'run_in_background']))
    expect(bg.rs.find((r) => requestClass(r) === 'subagent')!.json.model).toBe('claude-opus-5-5')
    expect(bg.started.is_backgrounded).toBe(true)
    expect(bg.resultText.startsWith('Async agent launched successfully.')).toBe(true)
    // The subagent's request races the main loop's next one.
    expect(bg.rs.map(requestClass).sort()).toEqual(['main', 'main', 'main', 'subagent'])

    const fg = await run(false)
    expect(fg.started.is_backgrounded).toBe(false)
    expect(fg.resultText.startsWith('[Subagent hand-back]')).toBe(true)
    expect(fg.rs.map(requestClass)).toEqual(['main', 'subagent', 'main'])
    expect(sdkMessages(fg.h, 'result')).toHaveLength(1)
  })

  it('S11: the refusal fixture switches no model, and PostModelSwitch fires only for setModel()', async () => {
    const fake = await Fake.start({ r: [{ text: 'I cannot.', stop_reason: 'refusal' }, { text: 'Two.', model: 'claude-other-5-5' }, { text: 'Three.' }] })
    const h = new Host({ fake, store: new RecordingStore(), root: 'tree-s11r' })
    const t1 = await h.send('Flagged [[fake:r]]')
    // The CLI notes the refusal and retries once, on the same model.
    expect(t1.messages.filter((m: any) => m.type === 'system' && m.subtype === 'informational').map((m: any) => m.content)).toEqual([
      "Sonnet 5.5's safeguards stopped the response above · continuing once with that noted",
    ])
    expect(t1.messages.filter((m: any) => m.type === 'user' && m.isSynthetic).map((m: any) => m.refused_message_id)).toEqual(['msg_fake_1'])
    expect(t1.messages.filter((m: any) => m.type === 'system' && String(m.subtype).startsWith('model_refusal'))).toEqual([])
    expect(fake.requests().map((r) => r.json.model)).toEqual([MODEL, MODEL])

    // A reply from another model shows only in the assistant message.
    const t2 = await h.send('Two [[fake:r]]')
    expect([...new Set(t2.messages.filter((m: any) => m.type === 'assistant').map((m: any) => m.message.model))]).toEqual(['claude-other-5-5'])
    expect(Object.keys(t2.result.modelUsage)).toEqual([MODEL])
    expect([...hooksOf(h, 'PreModelSwitch'), ...hooksOf(h, 'PostModelSwitch')]).toEqual([])

    await h.q.setModel('claude-opus-5-5')
    await h.send('Three [[fake:r]]')
    await h.close()
    expect(hooksOf(h, 'PostModelSwitch').map((i) => [i.from_model, i.to_model, i.source])).toEqual([[MODEL, 'claude-opus-5-5', 'sdk']])
    expect(R.MODEL_SWITCH_SIGNAL).toBe('assistantModelField')
  })
})

describe('S12: WebFetch through a CONNECT proxy', () => {
  const tls = makeTlsFixtures(['site.test', 'api.anthropic.com'])
  const page = (_req: unknown, res: any) => {
    res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' })
    res.end('<html><head><title>Spike page</title></head><body><h1>Hello</h1><p>The page body.</p></body></html>')
  }
  const domainInfo = (_req: unknown, res: any) => {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify({ domain: 'site.test', can_fetch: true }))
  }
  async function fetchOnce(origins: { target: string; handler: (q: any, s: any) => void }[], settings = INLINE_SETTINGS) {
    const proxy = await ConnectProxy.start(tls, origins)
    const fake = await Fake.start({
      w: [{ tool_uses: [{ name: 'WebFetch', input: { url: 'https://site.test/page', prompt: 'What does it say?' } }] }, { text: 'It says hello.' }],
    })
    const h = new Host({
      fake,
      store: new RecordingStore(),
      root: 'tree-s12',
      settings,
      env: { HTTPS_PROXY: proxy.url, HTTP_PROXY: proxy.url, NO_PROXY: '127.0.0.1,localhost', NODE_EXTRA_CA_CERTS: tls.caPath },
    })
    const t = await h.send('Read the page [[fake:w]]')
    await h.close()
    await proxy.stop()
    const toolResult: any = t.messages.find((m: any) => m.type === 'user' && m.parent_tool_use_id === null)
    return { proxy, fake, rs: fake.requests(), toolResult: toolResult.message.content[0] }
  }

  it('S12: the page and a domain check go through the proxy; the extraction request goes to the fake on the helper model', async () => {
    const { proxy, fake, rs, toolResult } = await fetchOnce([
      { target: 'site.test:443', handler: page },
      { target: 'api.anthropic.com:443', handler: domainInfo },
    ])
    expect(proxy.connects).toEqual(['api.anthropic.com:443', 'site.test:443'])
    expect(proxy.connects).not.toContain(fake.hostPort)
    expect(proxy.plain).toEqual([])
    expect(proxy.originRequests).toEqual([
      { target: 'api.anthropic.com:443', method: 'GET', path: '/api/web/domain_info?domain=site.test' },
      { target: 'site.test:443', method: 'GET', path: '/page' },
    ])
    expect(rs.map((r) => [requestClass(r), r.json.model])).toEqual([
      ['main', MODEL],
      ['auxiliary', HELPER_MODEL],
      ['main', MODEL],
    ])
    expect(messagesText(rs[1]!)).toContain('Web page content:\n---\nSpike page\n\nHello\n=====\n\nThe page body.\n---\n\nWhat does it say?')
    expect(toolResult.is_error).toBeUndefined()
  })

  it('S12: a denied domain check fails the WebFetch, unless skipWebFetchPreflight', async () => {
    const denied = await fetchOnce([{ target: 'site.test:443', handler: page }])
    expect(denied.proxy.connects).toEqual(['api.anthropic.com:443'])
    expect(denied.toolResult.is_error).toBe(true)
    expect(denied.toolResult.content).toBe(
      'Unable to verify if domain site.test is safe to fetch. This may be due to network restrictions or enterprise security policies blocking claude.ai.',
    )
    const skipped = await fetchOnce([{ target: 'site.test:443', handler: page }], { ...INLINE_SETTINGS, skipWebFetchPreflight: true })
    expect(skipped.proxy.connects).toEqual(['site.test:443'])
    expect(skipped.toolResult.is_error).toBeUndefined()
  })
})

it('S13: an idle hour after six 4 KB tool results changes no earlier message, with or without idleCompaction: false', async () => {
  const tls = makeTlsFixtures(['site.test', 'api.anthropic.com'])
  const big = (i: number) => `Result ${i}: ` + 'lorem ipsum dolor sit amet '.repeat(200)
  async function run(settings: Record<string, unknown>) {
    const proxy = await ConnectProxy.start(tls, [
      {
        target: 'site.test:443',
        handler: (req: any, res: any) => {
          res.writeHead(200, { 'content-type': 'text/html' })
          res.end(`<html><body><p>Page ${req.url}</p></body></html>`)
        },
      },
      {
        target: 'api.anthropic.com:443',
        handler: (_q: any, res: any) => {
          res.writeHead(200, { 'content-type': 'application/json' })
          res.end('{"domain":"site.test","can_fetch":true}')
        },
      },
    ])
    // Each WebFetch's extraction prompt carries a marker, so its reply (the
    // tool result) is a scripted 5 KB.
    const scripts: Record<string, Step[]> = {
      s13: [
        ...Array.from({ length: 6 }, (_, i) => ({ tool_uses: [{ name: 'WebFetch', input: { url: `https://site.test/p${i}`, prompt: `Summarize [[fake:x${i}]]` } }] })),
        { text: 'Done.' },
        { text: 'Later.' },
      ],
    }
    for (let i = 0; i < 6; i++) scripts[`x${i}`] = [{ text: big(i) }]
    const fake = await Fake.start(scripts)
    const clock = new Clock(0, { monotonic: true })
    const h = new Host({
      fake,
      store: new RecordingStore(),
      root: 'tree-s13',
      clock,
      settings,
      env: { HTTPS_PROXY: proxy.url, HTTP_PROXY: proxy.url, NO_PROXY: '127.0.0.1,localhost', NODE_EXTRA_CA_CERTS: tls.caPath },
    })
    const t1 = await h.send('Fetch six [[fake:s13]]', 180_000)
    const before = fake.requests().length
    clock.set(65 * 60)
    await new Promise((ok) => setTimeout(ok, 5000))
    await h.send('Next turn [[fake:s13]]')
    await h.close()
    await proxy.stop()
    const results = t1.messages.filter((m: any) => m.type === 'user' && m.parent_tool_use_id === null).map((m: any) => m.message.content[0].content as string)
    return { rs: fake.requests(), before, results }
  }
  for (const settings of [INLINE_SETTINGS, { switchModelsOnFlag: true }]) {
    const { rs, before, results } = await run(settings)
    expect(results).toHaveLength(6)
    expect(results.every((r) => r.length >= 4096)).toBe(true)
    // No request while idle, and none but the next turn's after it.
    expect(rs.length).toBe(before + 1)
    const prev = rs[before - 1]!
    const next = rs[before]!
    expect(requestClass(next)).toBe('main')
    expect(next.bindingOk).toBe(true)
    const pm = prev.json.messages
    expect(next.json.messages.slice(0, pm.length).map(normalized)).toEqual(pm.map(normalized))
    expect(next.json.messages.slice(pm.length).map((m: any) => m.role)).toEqual(['assistant', 'user', 'system'])
    const rest = (r: FakeRequest) => JSON.stringify({ ...r.json, messages: undefined })
    expect(rest(next)).toBe(rest(prev))
  }
})

it('S15: applyFlagSettings({effortLevel: "medium"}) changes output_config and the new turn tail only', async () => {
  const fake = await Fake.start({ s15: [{ text: 'One.' }, { text: 'Two.' }, { text: 'Three.' }] })
  const h = new Host({ fake, store: new RecordingStore(), root: 'tree-s15' })
  await h.send('One [[fake:s15]]')
  await h.send('Two [[fake:s15]]')
  await h.q.applyFlagSettings({ effortLevel: 'medium' })
  await h.send('Three [[fake:s15]]')
  await h.close()
  const [r1, r2, r3] = fake.requests() as [FakeRequest, FakeRequest, FakeRequest]
  expect([r1, r2, r3].map((r) => r.bindingOk)).toEqual([true, true, true])
  expect([r1, r2, r3].map((r) => r.json.output_config)).toEqual([{ effort: 'low' }, { effort: 'low' }, { effort: 'medium' }])
  // The earlier messages are unchanged...
  const pm = r2.json.messages
  expect(r3.json.messages.slice(0, pm.length).map(normalized)).toEqual(pm.map(normalized))
  // ...and the new turn's trailing system message carries the new effort.
  const tail = r3.json.messages.slice(pm.length)
  expect(tail.map((m: any) => m.role)).toEqual(['assistant', 'user', 'system'])
  expect(tail[2].output_config).toEqual({ effort: 'medium' })
  expect(r2.json.messages.at(-1).output_config).toBeUndefined()
  expect(JSON.stringify({ ...r3.json, messages: undefined, output_config: undefined })).toBe(JSON.stringify({ ...r2.json, messages: undefined, output_config: undefined }))
  expect(header(r3, 'anthropic-beta')).toBe(header(r2, 'anthropic-beta'))
})

it('S16: with the thinking-binding beta, input_transformations reach the SDK messages', async () => {
  const dropped = { type: 'thinking_dropped', path: 'messages.2.content.0', reason: 'prefix_binding_mismatch' }
  const fake = await Fake.start({ s16: [{ text: 'One.' }, { text: 'Two.', input_transformations: [dropped] }] })
  const h = new Host({ fake, store: new RecordingStore(), root: 'tree-s16', env: { ANTHROPIC_BETAS: 'thinking-binding-controls-2026-08-01' } })
  await h.send('One [[fake:s16]]')
  const t2 = await h.send('Two [[fake:s16]]')
  await h.close()
  for (const r of fake.requests()) expect(header(r, 'anthropic-beta')!.split(',')).toContain('thinking-binding-controls-2026-08-01')
  const assistants = t2.messages.filter((m: any) => m.type === 'assistant')
  expect(assistants.length).toBeGreaterThan(0)
  for (const m of assistants as any[]) expect(m.message.input_transformations).toEqual([dropped])
  const start: any = t2.messages.find((m: any) => m.type === 'stream_event' && m.event.type === 'message_start')
  expect(start.event.message.input_transformations).toEqual([dropped])
  expect(R.INPUT_TRANSFORMATIONS_VISIBLE).toBe(true)
})

it('S17: a WebSearch call whose nested request reports 3 searches shows 3 only in the result delta', async () => {
  const fake = await Fake.start({
    s17: [{ tool_uses: [{ name: 'WebSearch', input: { query: 'rain tomorrow [[fake:s17search]]' } }] }, { text: 'Rain.' }],
    s17search: [{ text: 'Search summary.', usage: { web_search_requests: 3 } }],
  })
  const h = new Host({ fake, store: new RecordingStore(), root: 'tree-s17' })
  const t = await h.send('Will it rain? [[fake:s17]]')
  await h.close()
  const nested = fake.requests()[1]!
  expect(requestClass(nested)).toBe('auxiliary')
  expect(nested.json.model).toBe(MODEL)
  expect(nested.json.tools).toEqual([{ type: 'web_search_20250305', name: 'web_search', max_uses: 8 }])
  const post = hooksOf(h, 'PostToolUse')
  // PostToolUse carries searchCount, the reply's server_tool_use blocks, and no usage.
  expect(post.map((i) => [i.tool_name, i.tool_response.searchCount])).toEqual([['WebSearch', 1]])
  expect(Object.keys(post[0].tool_response).sort()).toEqual(['durationSeconds', 'query', 'results', 'searchCount'])
  expect(tokenTotals(t.result)[MODEL]![2]).toBe(3)
  expect(R.WEB_SEARCH_COUNT_SOURCE).toBe('resultDelta')
})

it('S9: paths, request classes, headers and system blocks of every request above', () => {
  const rs = allFakes.flatMap((f) => f.requests())
  expect(rs.length).toBeGreaterThan(100)
  expect([...new Set(rs.map((r) => r.path.split('?')[0]))].sort()).toEqual([...R.CLI_PATHS])
  expect([...new Set(rs.map((r) => r.path))]).toEqual(['/v1/messages?beta=true'])
  expect([...new Set(rs.map((r) => requestClass(r)))].sort()).toEqual([...R.REQUEST_CLASSES])
  for (const r of rs) {
    expect(isMessages(r)).toBe(true)
    expect(header(r, 'authorization')).toBe(`Bearer ${GATEWAY_TOKEN}`)
    expect(header(r, 'x-api-key')).toBeUndefined()
    expect([header(r, 'x-luma-person'), header(r, 'x-luma-agent'), header(r, 'x-luma-tier'), header(r, 'x-luma-source')]).toEqual([PERSON_REF, 'luma', 'fast', 'web'])
    expect(header(r, 'user-agent')).toBe('claude-cli/2.1.303 (external, sdk-ts, agent-sdk/0.3.303, client-app/luma)')
    expect(header(r, 'x-client-app')).toBe('luma')
    expect(r.json.system[0].text.startsWith('x-anthropic-billing-header: cc_version=2.1.303.')).toBe(true)
  }
  // Our prompt is its own system block on every main request, after the billing header and the SDK's line.
  for (const r of mainRequests(rs)) {
    expect(r.json.system.map((b: any) => b.text).slice(1)).toEqual(["You are a Claude agent, built on Anthropic's Claude Agent SDK.", AGENT_BODY])
  }
})
