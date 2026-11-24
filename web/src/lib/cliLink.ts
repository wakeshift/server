// Links from @wakeshift/cli:
// /new-app#pk=<public key>&id=<app id>&name=<display name> and
// /apps/<id>/keys#pk=<public key>. The key was made on the developer's
// machine; the fragment carries only its public half, and never reaches a
// server except as the body of the request that registers it.
//
// Signing in goes through the provider and back to return_to, which carries
// no fragment: the link is kept in sessionStorage (this tab only) until the
// console is back on the same page.

export interface CLILink {
  /** The public key, as in the link: 43 characters of unpadded base64url. */
  pk: string
  /** The app ID and name the CLI proposed (/new-app only). */
  id: string
  name: string
}

const storageKey = 'wakeshift.cli-link'
// How long a link waits for its sign-in.
const maxAge = 60 * 60 * 1000

const pkRE = /^[A-Za-z0-9_-]{43}$/

// parseLink reads a link's fragment ("#pk=…&id=…&name=…"); null without a pk.
export function parseLink(hash: string): CLILink | null {
  const p = new URLSearchParams(hash.replace(/^#/, ''))
  const pk = p.get('pk')
  if (pk === null) return null
  return { pk: pk.trim(), id: (p.get('id') ?? '').trim(), name: (p.get('name') ?? '').trim() }
}

// publicKeyBytes decodes a link's public key: 32 bytes, or null. (The server
// checks that it's a valid Ed25519 point.)
export function publicKeyBytes(pk: string): Uint8Array<ArrayBuffer> | null {
  if (!pkRE.test(pk)) return null
  try {
    const bin = atob(pk.replace(/-/g, '+').replace(/_/g, '/') + '=')
    if (bin.length !== 32) return null
    return Uint8Array.from(bin, (c) => c.charCodeAt(0))
  } catch {
    return null
  }
}

const base64url = (b: Uint8Array) => btoa(String.fromCharCode(...b)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')

// keyCode is the key code both the CLI and the console's key list show: the
// first 8 characters of the key's kid (base64url of the first 16 bytes of
// the public key's SHA-256), the key's key_id.
export async function keyCode(pk: Uint8Array<ArrayBuffer>): Promise<string> {
  const sum = new Uint8Array(await crypto.subtle.digest('SHA-256', pk))
  return base64url(sum.slice(0, 6))
}

interface Pending {
  path: string
  hash: string
  at: number
}

// keepLink remembers the current page's CLI link across signing in.
export function keepLink() {
  if (!parseLink(window.location.hash)) return
  const p: Pending = { path: window.location.pathname, hash: window.location.hash, at: Date.now() }
  try {
    sessionStorage.setItem(storageKey, JSON.stringify(p))
  } catch {
    // Private mode: the person opens the link again after signing in.
  }
}

// keptLink reports whether a CLI link is waiting for this tab's sign-in.
export function keptLink(): boolean {
  return read() !== null
}

// keptNewAppLink is the /new-app link waiting for this tab's sign-in, left
// in place: the waitlist page tells Wakeshift its key is the person's, so
// the CLI stops waiting (POST /api/v1/waitlist/key).
export function keptNewAppLink(): CLILink | null {
  const p = read()
  if (!p || p.path !== '/new-app') return null
  const link = parseLink(p.hash)
  return link && publicKeyBytes(link.pk) ? link : null
}

function read(): Pending | null {
  try {
    const p = JSON.parse(sessionStorage.getItem(storageKey) ?? 'null') as Pending | null
    if (!p || typeof p.hash !== 'string' || typeof p.path !== 'string' || Date.now() - p.at > maxAge) return null
    return p
  } catch {
    return null
  }
}

// takeLink returns the CLI link on this page: the URL's own, or the one kept
// across signing in for this very page, which it puts back in the URL (so a
// reload keeps it). A kept link is used once.
export function takeLink(): CLILink | null {
  const own = parseLink(window.location.hash)
  const p = read()
  if (p) {
    try {
      sessionStorage.removeItem(storageKey)
    } catch {
      // Nothing kept.
    }
  }
  if (own) return own
  if (!p || p.path !== window.location.pathname) return null
  window.history.replaceState(window.history.state, '', p.path + window.location.search + p.hash)
  return parseLink(p.hash)
}

// forgetLink takes the link out of the URL once it's used, so going back
// or reloading doesn't offer it again.
export function forgetLink() {
  if (window.location.hash) window.history.replaceState(window.history.state, '', window.location.pathname + window.location.search)
}
