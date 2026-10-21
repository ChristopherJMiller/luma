// A throwaway test CA and one leaf certificate per host name, made with the
// dev shell's openssl into a temp dir at run time and removed, keys included,
// at the end of the case. Nothing is committed: the repo is public, and
// .gitignore refuses *.pem and *.key anyway.
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from './harness.js'

export interface TlsFixtures {
  /** The CA certificate's path: the CLI's NODE_EXTRA_CA_CERTS. */
  caPath: string
  /** PEM key and certificate per host. */
  leaf: Record<string, { key: string; cert: string }>
}

const openssl = (args: string[], cwd: string) => execFileSync('openssl', args, { cwd, stdio: ['ignore', 'ignore', 'pipe'] })

export function makeTlsFixtures(hosts: string[]): TlsFixtures {
  const dir = tempDir('tls')
  const ec = ['-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes']
  openssl(
    ['req', '-x509', ...ec, '-keyout', 'ca.key.pem', '-out', 'ca.cert.pem', '-days', '2', '-subj', '/CN=Luma Spike S test CA',
      '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign'],
    dir,
  )
  const leaf: TlsFixtures['leaf'] = {}
  hosts.forEach((host, i) => {
    openssl(
      ['req', '-x509', '-CA', 'ca.cert.pem', '-CAkey', 'ca.key.pem', ...ec, '-keyout', `${i}.key.pem`, '-out', `${i}.cert.pem`, '-days', '2',
        '-subj', `/CN=${host}`, '-addext', `subjectAltName=DNS:${host}`, '-addext', 'basicConstraints=critical,CA:FALSE',
        '-addext', 'keyUsage=critical,digitalSignature', '-addext', 'extendedKeyUsage=serverAuth'],
      dir,
    )
    leaf[host] = { key: readFileSync(join(dir, `${i}.key.pem`), 'utf8'), cert: readFileSync(join(dir, `${i}.cert.pem`), 'utf8') }
  })
  return { caPath: join(dir, 'ca.cert.pem'), leaf }
}
