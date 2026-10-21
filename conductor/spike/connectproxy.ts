// A recording HTTP CONNECT proxy, standing in for smokescreen (HTTPS_PROXY).
// Every CONNECT target and every plain proxied request is logged. CONNECTs to
// the hosts in `routes` are tunnelled to local TLS servers that present a
// certificate from tlsfixtures.ts' CA; anything else gets 403, as smokescreen
// answers a denied destination.
import { createServer as createHttpServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http'
import { createServer as createHttpsServer } from 'node:https'
import { connect, type AddressInfo, type Socket } from 'node:net'
import { atCaseEnd } from './harness.js'
import type { TlsFixtures } from './tlsfixtures.js'

export interface Origin {
  /** host:port as the client asks for it, e.g. site.test:443. */
  target: string
  handler: (req: IncomingMessage, res: ServerResponse) => void
}

export interface OriginRequest {
  target: string
  method: string
  path: string
}

export class ConnectProxy {
  /** CONNECT targets, in arrival order. */
  readonly connects: string[] = []
  /** Absolute-form (plain http) proxy requests: "<method> <url>". */
  readonly plain: string[] = []
  /** Requests the tunnelled origins served. */
  readonly originRequests: OriginRequest[] = []
  url = ''
  private proxy: Server
  private origins: Server[] = []
  private routes = new Map<string, number>()
  private readonly sockets = new Set<Socket>()

  private constructor() {
    this.proxy = createHttpServer((req, res) => {
      this.plain.push(`${req.method} ${req.url}`)
      res.writeHead(403).end()
    })
    this.proxy.on('connect', (req: IncomingMessage, client: Socket, head: Buffer) => {
      const target = req.url ?? ''
      this.connects.push(target)
      this.sockets.add(client)
      client.on('close', () => this.sockets.delete(client))
      const port = this.routes.get(target)
      if (port === undefined) {
        client.end('HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n')
        return
      }
      const upstream = connect(port, '127.0.0.1', () => {
        client.write('HTTP/1.1 200 Connection Established\r\n\r\n')
        if (head.length) upstream.write(head)
        upstream.pipe(client)
        client.pipe(upstream)
      })
      this.sockets.add(upstream)
      upstream.on('close', () => this.sockets.delete(upstream))
      upstream.on('error', () => client.destroy())
      client.on('error', () => upstream.destroy())
    })
  }

  static async start(tls: TlsFixtures, origins: Origin[]): Promise<ConnectProxy> {
    const p = new ConnectProxy()
    for (const o of origins) {
      const host = o.target.split(':')[0]!
      const leaf = tls.leaf[host]
      if (!leaf) throw new Error(`no certificate for ${host}`)
      const srv = createHttpsServer({ key: leaf.key, cert: leaf.cert }, (req, res) => {
        p.originRequests.push({ target: o.target, method: req.method ?? '', path: req.url ?? '' })
        o.handler(req, res)
      })
      await new Promise<void>((ok) => srv.listen(0, '127.0.0.1', ok))
      p.origins.push(srv)
      p.routes.set(o.target, (srv.address() as AddressInfo).port)
    }
    await new Promise<void>((ok) => p.proxy.listen(0, '127.0.0.1', ok))
    p.url = `http://127.0.0.1:${(p.proxy.address() as AddressInfo).port}`
    atCaseEnd(() => p.stop())
    return p
  }

  async stop(): Promise<void> {
    for (const s of this.sockets) s.destroy()
    for (const s of this.origins) s.closeAllConnections()
    await Promise.all([this.proxy, ...this.origins].map((s) => new Promise((ok) => s.close(ok))))
  }
}
