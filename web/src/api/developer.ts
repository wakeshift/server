// The developer API: the apps you build, under /api/v1/developer/apps.
// Every route is for the members of the app's organization: anyone else's
// app is 404, as one that doesn't exist.
import { apiFetch } from './client'
import type { Role } from './orgs'

export const base = '/api/v1/developer/apps'
const app = (id: string) => `${base}/${encodeURIComponent(id)}`

export interface OwnedApp {
  id: string
  name: string
  mcp_url: string
  created_at: string
  keys: number
  manifest_updated_at?: string
  deleted_at?: string
  // Set while Wakeshift has suspended the app: its keys and publish URLs
  // are refused. The reason is written for its owners.
  suspended_at?: string
  suspended_reason?: string
  /** The organization it belongs to, and your role there. */
  org_id: string
  role: Role
}

export interface AppKey {
  key_id: string
  name: string
  created_at: string
  last_used_at?: string
  revoked_at?: string
  /** cli_link: registered from @wakeshift/cli's link. */
  via?: 'cli_link'
}

// A new key: the key itself, shown once, and what's kept of it.
export interface NewKey {
  key: string
  key_info: AppKey
}

// listOwnedApps lists an organization's apps (without one, your personal
// organization's).
export const listOwnedApps = (org?: string) =>
  apiFetch<{ apps: OwnedApp[]; org_id: string }>(org ? `${base}?org=${encodeURIComponent(org)}` : base).then((r) => r.apps)
export const getOwnedApp = (id: string) => apiFetch<OwnedApp>(app(id))
export const createOwnedApp = (n: { org_id?: string; id: string; name: string; mcp_url: string; key_name?: string }) =>
  apiFetch<NewKey & { app: OwnedApp }>(base, { method: 'POST', json: n })
// registerApp and registerKey register a key @wakeshift/cli made on the
// developer's machine, by its public key (the links' pk): a new app with it
// as its only key, or one more key for an app (owners and admins). Nothing
// secret comes back. key_code is what the person typed from their terminal.
export const registerApp = (n: { org_id?: string; id: string; name: string; public_key: string; key_code: string }) =>
  apiFetch<{ app: OwnedApp; key_info: AppKey }>(`${base}/register`, { method: 'POST', json: n })
export const registerKey = (id: string, k: { public_key: string; name: string; key_code: string }) =>
  apiFetch<{ key_info: AppKey }>(`${app(id)}/keys/register`, { method: 'POST', json: k })
export const updateOwnedApp = (id: string, s: { name?: string; mcp_url?: string }) => apiFetch<OwnedApp>(app(id), { method: 'PATCH', json: s })
export const deleteOwnedApp = (id: string) => apiFetch<{ deleted: boolean; grants_revoked: number }>(app(id), { method: 'DELETE' })

export const listKeys = (id: string) => apiFetch<{ keys: AppKey[]; max: number }>(`${app(id)}/keys`)
export const createKey = (id: string, name: string) => apiFetch<NewKey>(`${app(id)}/keys`, { method: 'POST', json: { name } })
export const rotateKey = (id: string, key: string) => apiFetch<NewKey>(`${app(id)}/keys/${encodeURIComponent(key)}/rotate`, { method: 'POST' })
export const renameKey = (id: string, key: string, name: string) =>
  apiFetch<{ key_info: AppKey }>(`${app(id)}/keys/${encodeURIComponent(key)}`, { method: 'PATCH', json: { name } })
export const revokeKey = (id: string, key: string) => apiFetch<{ key_info: AppKey }>(`${app(id)}/keys/${encodeURIComponent(key)}/revoke`, { method: 'POST' })

// How much of the app a page read: at most 200 of the workspaces it keeps
// for its users.
export interface Scanned {
  workspaces_scanned: number
  workspaces_total: number
}

export interface Overview {
  app: OwnedApp
  users: number
  /** Conversations without a user, each in its own workspace. */
  conversation_workspaces: number
  /** Every conversation using the app, with a user or without (of the workspaces scanned, plus conversations' own beyond them). */
  conversations: number
  subscriptions: number
  connected: number
  connected_by_harness: Record<string, number>
  connected_via: Record<string, number>
  publishes_24h: number
  wakes_24h: number
  failures_24h: number
  recent_failures: { at: string; text: string; error?: string; user?: string }[]
  publish_urls: number
  last_activity_at?: string
  scanned: Scanned
  /** Wakeshift connector versions in use, per harness (conversations active in the last 7 days). */
  connectors: ConnectorUse[]
}

export interface ConnectorUse {
  harness: string
  connector: string
  /** "0": from before connectors said their version. */
  version: string
  conversations: number
  /** Older than Wakeshift serves: refused until the user updates it. */
  outdated: boolean
}

export const getOverview = (id: string) => apiFetch<Overview>(`${app(id)}/overview`)

export interface Conversation {
  harness: string
  /** A public ID for the conversation (never its session key). */
  id: string
  connected: boolean
  via?: 'stream' | 'poll' | 'hooks'
  last_active_at?: string
}

export interface AppSubscription {
  id: string
  event: string
  arguments: Record<string, unknown>
  destination: 'conversation' | 'webhook' | 'poll' | 'push'
  reason?: string
  once: boolean
  created_at: string
  expires_at: string
  user?: string
  conversation?: Conversation
}

export interface Page {
  next_cursor: string
}

const q = (params: Record<string, string | undefined>) => {
  const s = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v) s.set(k, v)
  return s.toString()
}

export const listSubscriptions = (id: string, cursor: string) =>
  apiFetch<Page & { subscriptions: AppSubscription[]; scanned: Scanned }>(`${app(id)}/subscriptions?${q({ limit: '50', cursor })}`)
export const endSubscription = (id: string, sub: string) =>
  apiFetch<{ ended: boolean }>(`${app(id)}/subscriptions/${encodeURIComponent(sub)}/end`, { method: 'POST' })

export type ActivityStatus = 'published' | 'unmatched' | 'woke' | 'waiting' | 'failed' | 'canceled'

export interface Activity {
  id: string
  kind: 'publish' | 'delivery' | 'notice'
  at: string
  status: ActivityStatus
  topics?: string[]
  text: string
  data?: unknown
  matched?: number
  publish_url?: string
  user?: string
  conversation?: Conversation
  attempts?: number
  error?: string
  finished_at?: string
  /** The Wakeshift connector it was last handed to: "wakeshift/<version> <harness>". */
  connector?: string
  /** Its text and data were deleted once it was delivered, as its event asks. */
  content_deleted?: boolean
}

export interface ActivityFilter {
  topic?: string
  user?: string
  status?: string
}

export const listActivity = (id: string, f: ActivityFilter, cursor: string) =>
  apiFetch<Page & { activity: Activity[]; scanned: Scanned }>(`${app(id)}/activity?${q({ limit: '50', cursor, ...f })}`)

export interface AppUser {
  user?: string
  harness?: string
  created_at: string
  conversations: number
  connected: number
  subscriptions: number
  last_activity_at?: string
}

export const listUsers = (id: string, cursor: string) => apiFetch<Page & { users: AppUser[] }>(`${app(id)}/users?${q({ limit: '50', cursor })}`)
export const forgetUser = (id: string, user: string) =>
  apiFetch<{ forgotten: boolean }>(`${app(id)}/users/forget`, { method: 'POST', json: { user } })

export interface PublishURL {
  id: string
  topic: string
  // An event's: the event a POST emits, with these arguments, and (a
  // personal event's) to this one user.
  event?: string
  arguments?: Record<string, unknown>
  user?: string
  created_at: string
  last_used_at?: string
  revoked_at?: string
  publishes_24h: number
  // Set when POSTs must carry a Standard Webhooks signature: the secret's
  // fingerprint, never the secret.
  signing?: PublishSigning
}

// PublishSigning is a publish URL's signing secret as it's shown: the first
// 12 hex digits of the SHA-256 of the whsec_ string, when it was set, and
// until when the previous one is still accepted after a rotation.
export interface PublishSigning {
  fingerprint: string
  set_at: string
  previous_valid_until?: string
}

// PublishEvent is one of the app's events a publish URL can emit.
export interface PublishEvent {
  name: string
  description: string
  personal: boolean
  arguments: { name: string; type: string; required: boolean }[]
}

export const listPublishURLs = (id: string) =>
  apiFetch<{ publish_urls: PublishURL[]; events?: PublishEvent[] }>(`${app(id)}/publish-urls`).then((r) => ({ urls: r.publish_urls, events: r.events ?? [] }))
export const createEventPublishURL = (id: string, event: string, args: Record<string, unknown>, user?: string, signingSecret?: string) =>
  apiFetch<{ id: string; event: string; url: string; signing?: PublishSigning }>(`${app(id)}/publish-urls`, {
    method: 'POST',
    json: { event, arguments: args, ...(user ? { user } : {}), ...(signingSecret ? { signing_secret: signingSecret } : {}) },
  })
export const setPublishURLSecret = (id: string, url: string, secret: string) =>
  apiFetch<{ signing: PublishSigning }>(`${app(id)}/publish-urls/${encodeURIComponent(url)}/signing-secret`, { method: 'PUT', json: { secret } })
export const removePublishURLSecret = (id: string, url: string) =>
  apiFetch<{ removed: boolean }>(`${app(id)}/publish-urls/${encodeURIComponent(url)}/signing-secret`, { method: 'DELETE' })
export const revokePublishURL = (id: string, url: string) =>
  apiFetch<{ revoked: boolean }>(`${app(id)}/publish-urls/${encodeURIComponent(url)}/revoke`, { method: 'POST' })

export interface LogEntry {
  id: number
  at: string
  kind: string
  actor: string
  actor_email?: string
  action: string
  key_id?: string
  detail: Record<string, unknown>
}

export const logPageSize = 50

export const listLog = (id: string, before: number, action: string) =>
  apiFetch<{ entries: LogEntry[] }>(`${app(id)}/log?${q({ limit: String(logPageSize), before: before ? String(before) : undefined, action })}`).then(
    (r) => r.entries,
  )
