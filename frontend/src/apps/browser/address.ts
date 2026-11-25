// The Browser app's address bar: what the user types names a port of the
// server, which the proxy serves at a local port of this machine.

export interface Address {
  port: number
  /** Path, query and hash, starting with '/'. */
  path: string
  /** The server's port is spoken to over HTTPS. */
  secure: boolean
}

/**
 * Reads "localhost:3000/admin", ":3000", "3000", "http://127.0.0.1:8080/x",
 * "localhost/x" (port 80) or "https://localhost/x" (port 443); undefined for
 * anything that is not the server. Without a scheme it is HTTP.
 */
export function parseAddress(text: string): Address | undefined {
  let s = text.trim()
  if (!s) return undefined
  if (/^\d+(\/|$)/.test(s)) s = `localhost:${s}`
  if (s.startsWith(':')) s = `localhost${s}`
  if (!/^[a-z]+:\/\//i.test(s)) s = `http://${s}`
  let u: URL
  try {
    u = new URL(s)
  } catch {
    return undefined
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return undefined
  const secure = u.protocol === 'https:'
  if (!['localhost', '127.0.0.1', '0.0.0.0', '[::1]'].includes(u.hostname)) return undefined
  const port = u.port ? Number(u.port) : defaultPort(secure)
  if (!(port >= 1 && port <= 65535)) return undefined
  return { port, path: `${u.pathname}${u.search}${u.hash}`, secure }
}

const defaultPort = (secure: boolean) => (secure ? 443 : 80)

/** An address as the bar shows it; only HTTPS names its scheme. */
export function serverAddress({ port, path, secure }: Address): string {
  const shown = port === defaultPort(secure) ? '' : `:${port}`
  return `${secure ? 'https://' : ''}localhost${shown}${path}`
}

/** The URL the frame loads: the proxy's port, the path, and the token. */
export function frameURL(localPort: number, path: string, token: string): string {
  const u = new URL(path, `http://127.0.0.1:${localPort}`)
  u.searchParams.set('__davish', token)
  return u.toString()
}

/** How a proxied page's address reads in the bar, as on the server. */
export function displayAddress(
  href: string,
  site: { remotePort: number; secure: boolean },
): string {
  const u = new URL(href)
  u.searchParams.delete('__davish')
  return serverAddress({
    port: site.remotePort,
    path: `${u.pathname}${u.search}${u.hash}`,
    secure: site.secure,
  })
}
