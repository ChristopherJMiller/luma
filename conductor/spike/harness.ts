// Spike S harness: the real Claude Code CLI (the native binary of the direct
// @anthropic-ai/claude-code dependency, S19) driven by a real Agent SDK
// query(), against the fakeanthropic binary. The fake records every request
// the CLI makes, with its thinking-binding verdict.
import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { closeSync, existsSync, mkdirSync, mkdtempSync, openSync, readdirSync, readFileSync, rmSync, writeFileSync, writeSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { createRequire } from 'node:module'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  InMemorySessionStore,
  query,
  type HookCallbackMatcher,
  type HookEvent,
  type HookInput,
  type McpServerConfig,
  type Options,
  type Query,
  type SDKMessage,
  type SDKResultMessage,
  type SDKUserMessage,
  type SessionKey,
  type SessionStore,
  type SessionStoreEntry,
  type Settings,
} from '@anthropic-ai/claude-agent-sdk'

export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
export const FAKE_BIN = join(REPO_ROOT, 'bin/fakeanthropic')

/** The fake's thinking-signature key: a fixed test value, not a secret. */
export const BINDING_KEY_HEX = Buffer.from('luma spike s binding key, not a secret').toString('hex')
/** The gateway route's bearer, as the conductor holds it. Fake. */
export const GATEWAY_TOKEN = 'spike-s-gateway-token-not-a-secret'

/**
 * The session model and the helper model differ on purpose, so a helper
 * request (the WebFetch extraction) is told apart by its model.
 */
export const MODEL = 'claude-sonnet-5-5'
export const HELPER_MODEL = 'claude-haiku-5-5'
export const PERSON_REF = 'u_0123456789'

/** The agent body: the static system prompt. */
export const AGENT_BODY = 'You are Luma, a household assistant (Spike S fixture agent).\nAnswer briefly.'

/** Task 16's inline settings. */
export const INLINE_SETTINGS: Settings = { switchModelsOnFlag: true, idleCompaction: false, precomputeCompactionEnabled: false }

/** The native CLI of the direct @anthropic-ai/claude-code dependency (S19). */
export function cliPath(): string {
  const pkg = createRequire(import.meta.url).resolve('@anthropic-ai/claude-code/package.json')
  const native = createRequire(pkg).resolve(`@anthropic-ai/claude-code-${process.platform}-${process.arch}/package.json`)
  return join(dirname(native), 'claude')
}

// ---------------------------------------------------------------------------
// Per-case resources. Every temp dir, fake, host and server a case starts is
// tracked; endCase() (the test file's afterEach) stops and removes them all,
// and returns every request the case's fakes saw.

const tracked: { dirs: string[]; fakes: Fake[]; hosts: Host[]; stops: (() => Promise<unknown>)[] } = { dirs: [], fakes: [], hosts: [], stops: [] }
const runDirs: string[] = []

/** A fresh temp dir, removed at the end of the case, or of the run. */
export function tempDir(name: string, scope: 'case' | 'run' = 'case'): string {
  const d = mkdtempSync(join(tmpdir(), `spike-s-${name}-`))
  ;(scope === 'case' ? tracked.dirs : runDirs).push(d)
  return d
}

/** Runs stop at the end of the case (it may already have run). */
export function atCaseEnd(stop: () => Promise<unknown>) {
  tracked.stops.push(stop)
}

/** Ends the case: closes its hosts, stops its fakes and servers, removes its temp dirs. */
export async function endCase(): Promise<FakeRequest[]> {
  const { dirs, fakes, hosts, stops } = tracked
  tracked.dirs = []
  tracked.fakes = []
  tracked.hosts = []
  tracked.stops = []
  for (const h of hosts) h.abandon()
  for (const stop of stops) await stop()
  const seen = fakes.flatMap((f) => f.requests())
  for (const f of fakes) await f.stop()
  for (const d of dirs) rmSync(d, { recursive: true, force: true })
  return seen
}

/** Ends the run: removes the run's temp dirs (the clock shim). */
export function endRun() {
  for (const d of runDirs.splice(0)) rmSync(d, { recursive: true, force: true })
}

// ---------------------------------------------------------------------------
// The fake

/** One step of a fakeanthropic script (test/fakeanthropic README "Scripts"). */
export interface Step {
  text?: string
  tool_uses?: { name: string; input?: Record<string, unknown> }[]
  no_thinking?: boolean
  redacted_thinking?: boolean
  model?: string
  stop_reason?: string
  usage?: { input?: number; output?: number; cache_read?: number; cache_creation?: number; ephemeral_1h?: number; web_search_requests?: number }
  input_transformations?: Record<string, unknown>[]
  status?: number
  error_type?: string
  error_message?: string
  retry_after?: string
}

export interface FakeRequest {
  seq: number
  method: string
  /** The request URI: path and query, as sent. */
  path: string
  header: Record<string, string[]>
  host: string
  bindingOk: boolean
  body: Buffer
  /** The parsed body; undefined when it is not JSON. */
  json: any
}

export class Fake {
  private readonly proc: ChildProcess
  readonly baseUrl: string
  private readonly recordDir: string

  private constructor(proc: ChildProcess, baseUrl: string, recordDir: string) {
    this.proc = proc
    this.baseUrl = baseUrl
    this.recordDir = recordDir
  }

  static async start(scripts: Record<string, Step[]>): Promise<Fake> {
    const dir = tempDir('fake')
    const scriptsPath = join(dir, 'scripts.json')
    writeFileSync(scriptsPath, JSON.stringify(scripts))
    const recordDir = join(dir, 'rec')
    const proc = spawn(FAKE_BIN, ['--listen', '127.0.0.1:0', '--scripts', scriptsPath, '--record-dir', recordDir, '--binding-key-hex', BINDING_KEY_HEX], {
      stdio: ['ignore', 'pipe', 'pipe'],
    })
    proc.stderr!.resume()
    const addr = await new Promise<string>((ok, fail) => {
      let out = ''
      proc.once('exit', (code) => fail(new Error(`fakeanthropic exited ${code}`)))
      proc.stdout!.on('data', (d: Buffer) => {
        out += d.toString()
        const m = /fakeanthropic listening on (\S+)\n/.exec(out)
        if (m) ok(m[1]!)
      })
    })
    const f = new Fake(proc, `http://${addr}`, recordDir)
    tracked.fakes.push(f)
    return f
  }

  /** host:port of the fake, as a CONNECT target would name it. */
  get hostPort(): string {
    return this.baseUrl.replace(/^http:\/\//, '')
  }

  /** Every request so far, in Seq order. */
  requests(): FakeRequest[] {
    if (!existsSync(this.recordDir)) return []
    return readdirSync(this.recordDir)
      .filter((f) => f.endsWith('.meta.json'))
      .sort()
      .map((f) => {
        const meta = JSON.parse(readFileSync(join(this.recordDir, f), 'utf8'))
        const body = readFileSync(join(this.recordDir, f.replace('.meta.json', '.body')))
        let json: any
        try {
          json = JSON.parse(body.toString('utf8'))
        } catch {
          json = undefined
        }
        return { seq: meta.seq, method: meta.method, path: meta.path, header: meta.header ?? {}, host: meta.host, bindingOk: meta.binding_ok, body, json }
      })
  }

  async stop(): Promise<void> {
    if (this.proc.exitCode !== null || this.proc.signalCode !== null) return
    const exited = new Promise((ok) => this.proc.once('exit', ok))
    this.proc.kill('SIGTERM')
    await exited
  }
}

// ---------------------------------------------------------------------------
// Request helpers

export function header(r: FakeRequest, name: string): string | undefined {
  const want = name.toLowerCase()
  for (const [k, v] of Object.entries(r.header)) if (k.toLowerCase() === want) return v[0]
  return undefined
}

export const isMessages = (r: FakeRequest) => r.method === 'POST' && r.path.split('?')[0] === '/v1/messages'
export const requestClass = (r: FakeRequest) => header(r, 'x-claude-code-request-class')
export const mainRequests = (rs: FakeRequest[]) => rs.filter((r) => isMessages(r) && requestClass(r) === 'main')

/** The text of every text block (or the string content) of a message. */
export function texts(msg: any): string[] {
  if (typeof msg?.content === 'string') return [msg.content]
  return (msg?.content ?? []).filter((b: any) => b?.type === 'text').map((b: any) => b.text as string)
}
export const messagesText = (r: FakeRequest) => (r.json?.messages ?? []).flatMap(texts).join('\n')
export const systemText = (r: FakeRequest) =>
  typeof r.json?.system === 'string' ? r.json.system : (r.json?.system ?? []).map((b: any) => b.text).join('\n')

/** Non-overlapping occurrences of needle in hay. */
export function count(hay: string, needle: string): number {
  let n = 0
  for (let i = hay.indexOf(needle); i >= 0; i = hay.indexOf(needle, i + needle.length)) n++
  return n
}

/** Every cache_control object in a body, as raw JSON text. */
export const cacheControls = (r: FakeRequest) => r.body.toString('utf8').match(/"cache_control":\{[^}]*\}/g) ?? []

/** Whether any assistant message of the request carries a thinking block. */
export const hasThinking = (r: FakeRequest) =>
  (r.json?.messages ?? []).some((m: any) => m.role === 'assistant' && Array.isArray(m.content) && m.content.some((b: any) => b.type === 'thinking' || b.type === 'redacted_thinking'))

/**
 * A message as the API compares it for the binding check and the cache: no
 * cache_control, and a lone text block read as its string. The CLI moves its
 * breakpoints every turn, and sends the trailing role:"system" message as a
 * block while it is last and as a string after that.
 */
export function normalized(msg: any): string {
  const strip = (v: any): any => {
    if (Array.isArray(v)) return v.map(strip)
    if (v && typeof v === 'object') return Object.fromEntries(Object.entries(v).filter(([k]) => k !== 'cache_control').map(([k, x]) => [k, strip(x)]))
    return v
  }
  const m = strip(msg)
  if (Array.isArray(m.content) && m.content.length === 1 && m.content[0].type === 'text' && Object.keys(m.content[0]).length === 2) m.content = m.content[0].text
  return JSON.stringify(m)
}

// metadata.user_id carries the session id and a per-config-dir device id.
const METADATA_RE = /"metadata":\{"user_id":"(?:[^"\\]|\\.)*"\}/g

/** The raw body with its one metadata member emptied. Throws unless there is exactly one. */
export function withoutMetadata(r: FakeRequest): Buffer {
  const s = r.body.toString('utf8')
  const n = s.match(METADATA_RE)?.length ?? 0
  if (n !== 1) throw new Error(`request ${r.seq}: ${n} metadata members`)
  return Buffer.from(s.replace(METADATA_RE, '"metadata":{}'), 'utf8')
}

export const userIdOf = (r: FakeRequest): { device_id: string; account_uuid: string; session_id: string } => JSON.parse(r.json.metadata.user_id)

// ---------------------------------------------------------------------------
// A recording SessionStore

export interface AppendCall {
  key: SessionKey
  entries: SessionStoreEntry[]
}

/** The SDK's own InMemorySessionStore, with every append() recorded. */
export class RecordingStore extends InMemorySessionStore {
  readonly appends: AppendCall[] = []
  override async append(key: SessionKey, entries: SessionStoreEntry[]): Promise<void> {
    this.appends.push({ key, entries: structuredClone(entries) })
    return super.append(key, entries)
  }
  /** Every appended entry of one type, in order. */
  ofType(type: string, sessionId?: string): SessionStoreEntry[] {
    return this.appends.filter((a) => !sessionId || a.key.sessionId === sessionId).flatMap((a) => a.entries.filter((e) => e.type === type))
  }
}

// ---------------------------------------------------------------------------
// Clock control
//
// The plan named libfaketime (LD_PRELOAD=$LUMA_FAKETIME_LIB, FAKETIME=…). It
// hangs the CLI at start: libfaketime's lazy init calls dlsym, which
// allocates through the CLI's own allocator, which reads the clock, which
// re-enters libfaketime's init under pthread_once (docs/spikes/phase-1.md
// section S, "Clock control"). This shim moves the clock and nothing else:
// CLOCK_REALTIME, gettimeofday and time run ahead by an offset that the
// harness can change while the CLI runs, and with LUMA_CLOCK_MONOTONIC the
// monotonic clocks jump too. It makes raw syscalls only, so it never
// allocates, and it depends on no glibc symbol newer than syscall(). It maps
// the offset in a constructor, which the loader runs before the CLI's own
// initialisers and before any thread exists, so no lazy init can race.
const CLOCK_SHIM_C = String.raw`
#define _GNU_SOURCE
#include <fcntl.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

static volatile int64_t *offset_p;
static int all_clocks;

/* Maps the offset, int64 seconds (little endian) at the start of
   $LUMA_CLOCK_FILE, shared, so a rewrite in place moves a running clock. */
__attribute__((constructor)) static void clockshim_init(void) {
  all_clocks = getenv("LUMA_CLOCK_MONOTONIC") != 0;
  const char *path = getenv("LUMA_CLOCK_FILE");
  if (!path) return;
  long fd = syscall(SYS_openat, AT_FDCWD, path, O_RDONLY | O_CLOEXEC);
  if (fd < 0) return;
  void *p = (void *)syscall(SYS_mmap, 0, 8, PROT_READ, MAP_SHARED, fd, 0);
  syscall(SYS_close, fd);
  if (p != MAP_FAILED) offset_p = (volatile int64_t *)p;
}

static int64_t offset(void) {
  return offset_p ? *offset_p : 0;
}

int clock_gettime(clockid_t id, struct timespec *tp) {
  long r = syscall(SYS_clock_gettime, id, tp);
  if (r != 0) return (int)r;
  int64_t off = offset();
  if (id == CLOCK_REALTIME || id == CLOCK_REALTIME_COARSE) tp->tv_sec += off;
  else if (all_clocks && (id == CLOCK_MONOTONIC || id == CLOCK_MONOTONIC_RAW || id == CLOCK_MONOTONIC_COARSE || id == CLOCK_BOOTTIME)) tp->tv_sec += off;
  return 0;
}

int gettimeofday(struct timeval *tv, void *tz) {
  long r = syscall(SYS_gettimeofday, tv, tz);
  if (r == 0 && tv) tv->tv_sec += offset();
  return (int)r;
}

time_t time(time_t *t) {
  struct timespec ts;
  syscall(SYS_clock_gettime, CLOCK_REALTIME, &ts);
  time_t now = ts.tv_sec + offset();
  if (t) *t = now;
  return now;
}
`

let shimPath: string | undefined

/** Builds the clock shim once per process, with the dev shell's cc. */
function clockShim(): string {
  if (shimPath) return shimPath
  const dir = tempDir('shim', 'run')
  writeFileSync(join(dir, 'clockshim.c'), CLOCK_SHIM_C)
  execFileSync('cc', ['-shared', '-fPIC', '-O2', '-fno-delete-null-pointer-checks', '-Wno-nonnull-compare', '-o', join(dir, 'clockshim.so'), join(dir, 'clockshim.c')])
  shimPath = join(dir, 'clockshim.so')
  return shimPath
}

/**
 * A CLI clock that runs ahead of the real one by a settable offset. With
 * `monotonic`, the monotonic clocks jump too, as under libfaketime's FAKETIME,
 * so the CLI's own timers see the idle time pass.
 */
export class Clock {
  readonly file: string
  readonly monotonic: boolean
  constructor(offsetSeconds = 0, opts: { monotonic?: boolean } = {}) {
    this.monotonic = opts.monotonic ?? false
    this.file = join(tempDir('clock'), 'offset')
    writeFileSync(this.file, Buffer.alloc(8))
    this.set(offsetSeconds)
  }
  /** Moves the clock; a running CLI sees it at once. */
  set(offsetSeconds: number) {
    const b = Buffer.alloc(8)
    b.writeBigInt64LE(BigInt(Math.trunc(offsetSeconds)))
    const fd = openSync(this.file, 'r+')
    writeSync(fd, b, 0, 8, 0)
    closeSync(fd)
  }
  env(): Record<string, string> {
    return { LD_PRELOAD: clockShim(), LUMA_CLOCK_FILE: this.file, ...(this.monotonic ? { LUMA_CLOCK_MONOTONIC: '1' } : {}) }
  }
}

// ---------------------------------------------------------------------------
// A local HTTP MCP server (SDK #370: MCP proxies are local HTTP servers)

/**
 * A minimal stateless Streamable HTTP MCP server with the two memory tools.
 * JSON responses only. It can hold initialize back, and answer tools/call
 * with any text.
 */
export class McpServer {
  url = ''
  readonly token = 'spike-s-mcp-token-not-a-secret'
  readonly methods: string[] = []
  readonly authorization: string[] = []
  private readonly srv: Server

  private constructor(o: { initializeDelayMs?: number; callResult?: (name: string, args: Record<string, unknown>) => string }) {
    this.srv = createServer(async (req, res) => {
      if (req.method !== 'POST') {
        res.writeHead(405, { allow: 'POST' }).end()
        return
      }
      this.authorization.push(req.headers.authorization ?? '')
      let body = ''
      for await (const chunk of req) body += chunk
      const msg = JSON.parse(body)
      const replies: unknown[] = []
      for (const m of Array.isArray(msg) ? msg : [msg]) {
        this.methods.push(m.method)
        if (m.id === undefined) continue // a notification
        replies.push({ jsonrpc: '2.0', id: m.id, ...(await answer(m)) })
      }
      if (replies.length === 0) {
        res.writeHead(202).end()
        return
      }
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(Array.isArray(msg) ? replies : replies[0]))
    })
    const answer = async (m: any): Promise<{ result: unknown } | { error: unknown }> => {
      switch (m.method) {
        case 'initialize':
          await new Promise((ok) => setTimeout(ok, o.initializeDelayMs ?? 0))
          return {
            result: {
              protocolVersion: m.params?.protocolVersion ?? '2025-06-18',
              capabilities: { tools: {} },
              serverInfo: { name: 'luma', version: '0.0.0-spike' },
              instructions: 'Luma memory tools (Spike S fixture).',
            },
          }
        case 'ping':
          return { result: {} }
        case 'tools/list':
          return {
            result: {
              tools: [
                { name: 'remember', description: 'Save one short fact.', inputSchema: { type: 'object', properties: { fact: { type: 'string', maxLength: 300 } }, required: ['fact'] } },
                { name: 'forget', description: 'Remove saved facts that contain this text.', inputSchema: { type: 'object', properties: { text: { type: 'string', minLength: 3 } }, required: ['text'] } },
              ],
            },
          }
        case 'tools/call':
          return { result: { content: [{ type: 'text', text: o.callResult?.(m.params?.name, m.params?.arguments ?? {}) ?? 'Saved.' }] } }
        default:
          return { error: { code: -32601, message: `method not found: ${m.method}` } }
      }
    }
  }

  static async start(o: { initializeDelayMs?: number; callResult?: (name: string, args: Record<string, unknown>) => string } = {}): Promise<McpServer> {
    const s = new McpServer(o)
    await new Promise<void>((ok) => s.srv.listen(0, '127.0.0.1', ok))
    s.url = `http://127.0.0.1:${(s.srv.address() as AddressInfo).port}/mcp`
    atCaseEnd(() => s.stop())
    return s
  }

  /** The query() mcpServers entry, as Task 16 builds it. */
  config(): Record<string, McpServerConfig> {
    return { luma: { type: 'http', url: this.url, headers: { Authorization: `Bearer ${this.token}` } } }
  }

  async stop(): Promise<void> {
    this.srv.closeAllConnections()
    await new Promise((ok) => this.srv.close(ok))
  }
}

// ---------------------------------------------------------------------------
// A conversation host

export interface HostOptions {
  fake: Fake
  store: SessionStore
  /** The tree root: CLAUDE_CODE_PROJECT_DIR_NAME, and the cwd conv/<root>. */
  root: string
  /**
   * The conductor's data dir (LUMA_DATA_DIR). The CLI writes the cwd into
   * the conversation (its environment block), so the hosts of one tree share
   * this path, as the conductor's do. A fresh directory unless given.
   */
  dataDir?: string
  /** Empty dataDir first: the same path on a new pod. */
  wipe?: boolean
  model?: string
  tools?: string[]
  settings?: Settings
  hooks?: Partial<Record<HookEvent, HookCallbackMatcher[]>>
  mcpServers?: Record<string, McpServerConfig>
  agents?: Options['agents']
  resume?: string
  forkSession?: boolean
  resumeSessionAt?: string
  /** Added or replaced CLI environment variables. */
  env?: Record<string, string | undefined>
  /** The CLI's clock. */
  clock?: Clock
  effort?: Options['effort']
}

export const newDataDir = () => tempDir('data')

export interface Turn {
  messages: SDKMessage[]
  result: SDKResultMessage
}

/** The query's input: held open until close (SDK #348). */
class InputQueue implements AsyncIterable<SDKUserMessage> {
  private items: SDKUserMessage[] = []
  private waiter: ((r: IteratorResult<SDKUserMessage>) => void) | null = null
  private done = false
  push(m: SDKUserMessage) {
    const w = this.waiter
    this.waiter = null
    if (w) w({ value: m, done: false })
    else this.items.push(m)
  }
  end() {
    this.done = true
    const w = this.waiter
    this.waiter = null
    if (w) w({ value: undefined, done: true })
  }
  [Symbol.asyncIterator](): AsyncIterator<SDKUserMessage> {
    return {
      next: () => {
        const m = this.items.shift()
        if (m) return Promise.resolve({ value: m, done: false })
        if (this.done) return Promise.resolve({ value: undefined, done: true })
        return new Promise((ok) => (this.waiter = ok))
      },
    }
  }
}

/** The hook events every host records, alongside a case's own hooks. */
const RECORDED_HOOKS: HookEvent[] = ['SessionStart', 'PostToolUse', 'PreCompact', 'PostCompact', 'PreModelSwitch', 'PostModelSwitch']

/**
 * One long-lived query() over a held-open input queue, configured as Task 16
 * configures the conductor's (options and environment), plus the case's.
 */
export class Host {
  readonly o: HostOptions
  readonly dataDir: string
  readonly cwd: string
  readonly configDir: string
  readonly home: string
  readonly messages: SDKMessage[] = []
  readonly hookInputs: HookInput[] = []
  readonly stderr: string[] = []
  readonly q: Query
  /** Marks this host's CLI in its environment (hosts of one tree share HOME). */
  private readonly marker = randomUUID()
  private readonly input = new InputQueue()
  private waiters: (() => void)[] = []
  private ended = false
  private iterError: unknown = null

  constructor(o: HostOptions) {
    this.o = o
    tracked.hosts.push(this)
    this.dataDir = o.dataDir ?? newDataDir()
    if (o.wipe) for (const d of readdirSync(this.dataDir)) rmSync(join(this.dataDir, d), { recursive: true, force: true })
    this.cwd = join(this.dataDir, 'conv', o.root)
    this.configDir = join(this.dataDir, 'config', PERSON_REF)
    this.home = join(this.dataDir, 'home', PERSON_REF)
    for (const d of [this.cwd, this.configDir, this.home, join(this.dataDir, 'tmp')]) mkdirSync(d, { recursive: true })

    const record = async (input: HookInput) => {
      this.hookInputs.push(JSON.parse(JSON.stringify(input)))
      return {}
    }
    const hooks: Partial<Record<HookEvent, HookCallbackMatcher[]>> = { ...(o.hooks ?? {}) }
    for (const ev of RECORDED_HOOKS) hooks[ev] = [...(hooks[ev] ?? []), { hooks: [record] }]

    const tools = o.tools ?? ['WebSearch', 'WebFetch']
    this.q = query({
      prompt: this.input,
      options: {
        pathToClaudeCodeExecutable: cliPath(),
        cwd: this.cwd,
        env: this.env(),
        settings: o.settings ?? INLINE_SETTINGS,
        settingSources: [],
        strictMcpConfig: true,
        permissionMode: 'default',
        tools,
        allowedTools: tools,
        systemPrompt: AGENT_BODY,
        model: o.model ?? MODEL,
        effort: o.effort ?? 'low',
        includePartialMessages: true,
        sessionStore: o.store,
        hooks,
        stderr: (d) => this.stderr.push(d),
        ...(o.mcpServers ? { mcpServers: o.mcpServers } : {}),
        ...(o.agents ? { agents: o.agents } : {}),
        ...(o.resume ? { resume: o.resume } : {}),
        ...(o.forkSession ? { forkSession: true } : {}),
        ...(o.resumeSessionAt ? { resumeSessionAt: o.resumeSessionAt } : {}),
      },
    })
    void this.pump()
  }

  /** Task 16's buildEnv, with the spike's fake and the case's additions. */
  private env(): Record<string, string | undefined> {
    const o = this.o
    return {
      CLAUDE_CONFIG_DIR: this.configDir,
      CLAUDE_CODE_PROJECT_DIR_NAME: o.root,
      HOME: this.home,
      TMPDIR: join(this.dataDir, 'tmp'),
      PATH: '/usr/bin:/bin',
      TZ: 'UTC',
      // Task 16's CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1 is left out: CLI
      // 2.1.303 refuses to start with it unless bubblewrap is installed
      // (docs/spikes/phase-1.md section S, "Environment").
      CLAUDE_CODE_DISABLE_AUTO_MEMORY: '1',
      DISABLE_AUTOUPDATER: '1',
      DISABLE_TELEMETRY: '1',
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1',
      CLAUDE_AGENT_SDK_CLIENT_APP: 'luma',
      CLAUDE_CODE_GATEWAY_HINT_HEADERS: '1',
      CLAUDE_AGENT_SDK_DISABLE_BUILTIN_AGENTS: '1',
      ANTHROPIC_BASE_URL: o.fake.baseUrl,
      ANTHROPIC_AUTH_TOKEN: GATEWAY_TOKEN,
      ANTHROPIC_DEFAULT_HAIKU_MODEL: HELPER_MODEL,
      ANTHROPIC_DEFAULT_SONNET_MODEL: o.model ?? MODEL,
      ANTHROPIC_DEFAULT_OPUS_MODEL: o.model ?? MODEL,
      ANTHROPIC_CUSTOM_HEADERS: `x-luma-person: ${PERSON_REF}\nx-luma-agent: luma\nx-luma-tier: fast\nx-luma-source: web`,
      CLAUDE_CODE_PROMPT_CACHE_TTL: '1h',
      CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL: '5m',
      CLAUDE_CODE_AUTO_COMPACT_WINDOW: '100000',
      CLAUDE_AUTOCOMPACT_PCT_OVERRIDE: '85',
      NO_PROXY: '127.0.0.1,localhost',
      LUMA_SPIKE_HOST: this.marker,
      ...(o.clock ? o.clock.env() : {}),
      ...(o.env ?? {}),
    }
  }

  private async pump() {
    try {
      for await (const m of this.q) {
        this.messages.push(m)
        this.wake()
      }
    } catch (e) {
      this.iterError = e
    } finally {
      this.ended = true
      this.wake()
    }
  }

  private wake() {
    const ws = this.waiters
    this.waiters = []
    for (const w of ws) w()
  }

  /** Waits until pred holds over the messages. */
  async until(pred: (ms: SDKMessage[]) => boolean, timeoutMs = 60_000): Promise<void> {
    const deadline = Date.now() + timeoutMs
    while (!pred(this.messages)) {
      if (this.ended) throw new Error(`query ended: ${String(this.iterError ?? 'no error')}\n${this.stderr.join('').slice(-2000)}`)
      if (Date.now() > deadline) throw new Error(`timed out; stderr tail:\n${this.stderr.join('').slice(-2000)}`)
      await Promise.race([new Promise<void>((ok) => this.waiters.push(ok)), new Promise((ok) => setTimeout(ok, 250))])
    }
  }

  /**
   * Polls mcpServerStatus() every 100 ms until every server is connected,
   * as the conductor does before the first prompt. Returns the polls made.
   */
  async waitMcpConnected(timeoutMs = 20_000): Promise<number> {
    const deadline = Date.now() + timeoutMs
    for (let polls = 1; ; polls++) {
      const st = await this.q.mcpServerStatus()
      if (st.length > 0 && st.every((s) => s.status === 'connected')) return polls
      if (st.some((s) => s.status === 'failed')) throw new Error(`MCP failed: ${JSON.stringify(st)}`)
      if (Date.now() > deadline) throw new Error(`MCP not connected: ${JSON.stringify(st)}`)
      await new Promise((ok) => setTimeout(ok, 100))
    }
  }

  /** Sends one user turn and waits for its result. */
  async send(content: string, timeoutMs = 90_000): Promise<Turn> {
    const start = this.messages.length
    this.input.push({ type: 'user', message: { role: 'user', content }, parent_tool_use_id: null })
    await this.until((ms) => ms.slice(start).some((m) => m.type === 'result'), timeoutMs)
    const messages = this.messages.slice(start)
    return { messages, result: messages.find((m) => m.type === 'result') as SDKResultMessage }
  }

  /** The session id the SDK reported. */
  get sessionId(): string {
    const init = this.messages.find((m) => m.type === 'system' && (m as any).subtype === 'init') as { session_id: string } | undefined
    if (!init) throw new Error('no system/init yet')
    return init.session_id
  }

  /**
   * This host's CLI processes, found by the LUMA_SPIKE_HOST marker in their
   * environment. Neither HOME nor CLAUDE_CONFIG_DIR would do: the hosts of one
   * tree share HOME, and for a store resume the SDK replaces
   * CLAUDE_CONFIG_DIR with a temp dir of its own (claude-resume-<uuid>).
   */
  cliPids(): number[] {
    const marker = `LUMA_SPIKE_HOST=${this.marker}`
    const pids: number[] = []
    for (const p of readdirSync('/proc')) {
      if (!/^\d+$/.test(p)) continue
      try {
        if (readFileSync(`/proc/${p}/environ`, 'utf8').split('\0').includes(marker)) pids.push(Number(p))
      } catch {
        // gone, or not ours
      }
    }
    return pids
  }

  /** The argv and environment of this host's own CLI process. */
  cliProcess(): { argv: string[]; env: Record<string, string> } {
    const pids = this.cliPids()
    if (pids.length !== 1) throw new Error(`want this host's one CLI process, found ${pids.length}`)
    const argv = readFileSync(`/proc/${pids[0]}/cmdline`, 'utf8').split('\0').slice(0, -1)
    const env = Object.fromEntries(
      readFileSync(`/proc/${pids[0]}/environ`, 'utf8').split('\0').filter(Boolean).map((kv) => [kv.slice(0, kv.indexOf('=')), kv.slice(kv.indexOf('=') + 1)]),
    )
    return { argv, env }
  }

  /** Clean close: end the input, let the CLI exit, close(). */
  async close(): Promise<void> {
    this.input.end()
    await this.until(() => this.ended, 30_000).catch(() => undefined)
    this.q.close()
  }

  /** Ends a host its case left open (endCase): the SDK kills the CLI. */
  abandon() {
    if (this.ended) return
    this.input.end()
    this.q.close()
  }

  /** SIGKILL the CLI, as a crashed pod would. */
  async kill(): Promise<void> {
    const pids = this.cliPids()
    if (pids.length === 0) throw new Error('no CLI process to kill')
    for (const pid of pids) process.kill(pid, 'SIGKILL')
    await this.until(() => this.ended, 30_000).catch(() => undefined)
    this.q.close()
  }

  /** The local JSONL of a fresh (not store-resumed) session of this host. */
  transcriptPath(sessionId: string): string {
    return join(this.configDir, 'projects', this.o.root, `${sessionId}.jsonl`)
  }

  /** What the conductor's host close removes (P18). */
  removeLocalTranscript(sessionId: string) {
    rmSync(this.transcriptPath(sessionId), { force: true })
    rmSync(join(this.configDir, 'projects', this.o.root, sessionId), { recursive: true, force: true })
  }
}

/** The last main-loop assistant message's uuid among msgs (P23's sdk_uuid). */
export function lastMainUuid(msgs: SDKMessage[]): string {
  const m = msgs.filter((x: any) => x.type === 'assistant' && x.parent_tool_use_id === null).at(-1) as { uuid: string } | undefined
  if (!m) throw new Error('no main-loop assistant message')
  return m.uuid
}

/** Token totals of a result, per model: [input, output, webSearchRequests]. */
export function tokenTotals(r: SDKResultMessage): Record<string, [number, number, number]> {
  return Object.fromEntries(Object.entries(r.modelUsage).map(([k, v]) => [k, [v.inputTokens, v.outputTokens, v.webSearchRequests]]))
}
