// Typed API client. Every request goes through apiFetch, which sends the
// console's session cookie and turns the API's error shape
// ({"error": {"code", "message"}}) into an ApiError.

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly redirect?: string,
  ) {
    super(message)
  }
}

export async function apiFetch<T>(path: string, init?: RequestInit & { json?: unknown }): Promise<T> {
  const { json, ...rest } = init ?? {}
  const res = await fetch(path, {
    ...rest,
    credentials: 'same-origin',
    headers: {
      Accept: 'application/json',
      ...(json !== undefined ? { 'Content-Type': 'application/json' } : {}),
      ...rest.headers,
    },
    body: json !== undefined ? JSON.stringify(json) : rest.body,
  })
  if (!res.ok) {
    const body = await res.json().catch(() => null)
    throw new ApiError(res.status, body?.error?.code ?? 'http_error', body?.error?.message ?? res.statusText, body?.redirect)
  }
  return res.json() as Promise<T>
}

export interface Status {
  version: string
  database: boolean
}

export const getStatus = () => apiFetch<Status>('/api/v1/status')

// ── Session ───────────────────────────────────────────────────────────────

export interface Workspace {
  id: string
  name: string
  kind: 'personal' | 'team'
  role: 'owner' | 'member'
}

export interface Session {
  authenticated: boolean
  user?: { id: string; email: string; name: string }
  workspace?: Workspace
  workspaces?: Workspace[]
  sign_in: { google: boolean; github: boolean; dev: boolean }
  site: Site
  // Signed out, and on the waitlist since their last sign-in.
  waitlist?: WaitlistEntry
}

// What the console links to and names, as the server is configured.
export interface Site {
  docs: string
  // The Terms new accounts accept and the Privacy Policy beside them; none
  // on a server without Terms.
  terms?: string
  privacy?: string
  support_email: string
}

export interface WaitlistEntry {
  joined_at: string
  note: string
  invited: boolean
}

export const saveWaitlistNote = (note: string) =>
  apiFetch<{ saved: boolean }>('/api/v1/waitlist/note', { method: 'POST', json: { note } })
// Holds the CLI link's public key against the person's waitlist entry, so
// the CLI waiting on it hears they're waitlisted (403 waitlisted).
export const holdWaitlistKey = (publicKey: string) =>
  apiFetch<{ held: boolean }>('/api/v1/waitlist/key', { method: 'POST', json: { public_key: publicKey } })

export interface Invite {
  code: string
  uses_left: number
  url: string
}

export const getInvite = () => apiFetch<Invite>('/api/v1/invite')
export const checkInvite = (code: string) =>
  apiFetch<{ valid: boolean }>(`/api/v1/invites/${encodeURIComponent(code)}`)

export const getSession = () => apiFetch<Session>('/api/v1/session')
export const devSignIn = ({ email, token }: { email: string; token: string }) =>
  apiFetch<{ authenticated: boolean }>('/api/v1/session/dev', { method: 'POST', json: { email, token } })
export const signOut = () => apiFetch<{ authenticated: boolean }>('/api/v1/session', { method: 'DELETE' })
export const switchWorkspace = (id: string) =>
  apiFetch<{ workspace: string }>('/api/v1/session/workspace', { method: 'POST', json: { id } })

// ── Agents ────────────────────────────────────────────────────────────────

export interface Agent {
  id: string
  display_name: string
  created_at: string
}

export const listAgents = () => apiFetch<{ agents: Agent[] }>('/api/v1/agents').then((r) => r.agents)
export const createAgent = (id: string, displayName: string) =>
  apiFetch<Agent>('/api/v1/agents', { method: 'POST', json: { id, display_name: displayName } })
export const revokeStreamLinks = (agentID: string) =>
  apiFetch<{ revoked: number }>(`/api/v1/agents/${encodeURIComponent(agentID)}/stream-links/revoke`, { method: 'POST' })

export const streamLink = (agentID: string) =>
  apiFetch<{ url: string; expires_at: string }>(`/api/v1/agents/${encodeURIComponent(agentID)}/stream-link`, {
    method: 'POST',
    json: { ack: false },
  })

// ── Subscriptions ─────────────────────────────────────────────────────────

export interface Subscription {
  id: string
  agent_id: string
  event_type: string
  filters: Record<string, string>
  status: 'active' | 'canceled'
  reason?: string
  created_by: string
  created_at: string
}

export const listSubscriptions = () =>
  apiFetch<{ subscriptions: Subscription[] }>('/api/v1/subscriptions').then((r) => r.subscriptions)
export const createSubscription = (s: {
  agent_id: string
  event_type: string
  filters: Record<string, string>
  reason: string
}) => apiFetch<Subscription>('/api/v1/subscriptions', { method: 'POST', json: s })
export const cancelSubscription = (id: string) =>
  apiFetch<Subscription>(`/api/v1/subscriptions/${encodeURIComponent(id)}/cancel`, { method: 'POST' })

// An app's event an agent here is subscribed to, by name and arguments:
// what the app may wake it for, and where it goes.
export interface EventSubscription {
  id: string
  source_kind: string
  source_id: string
  source_name: string
  event: string
  description: string
  arguments: Record<string, unknown>
  principal?: string
  destination: 'conversation' | 'webhook' | 'poll' | 'push'
  agent_id: string
  webhook_host?: string
  created_at: string
  refreshed_at: string
  ended_at?: string
  ended_reason?: string
  // Why the app subscribed, as it said it, if it did.
  reason?: string
}

export const listEventSubscriptions = () =>
  apiFetch<{ subscriptions: EventSubscription[] }>('/api/v1/event-subscriptions').then((r) => r.subscriptions)
export const revokeEventSubscription = (id: string) =>
  apiFetch<EventSubscription>(`/api/v1/event-subscriptions/${encodeURIComponent(id)}/revoke`, { method: 'POST' })

// ── Events ────────────────────────────────────────────────────────────────

export interface WakeEvent {
  seq: number
  id: string
  type: string
  source: string
  occurred_at: string
  from: { kind: string; id: string }
  subject?: string
  attrs?: Record<string, string>
  trust: 'external' | 'internal'
  hop: number
}

export const listEvents = () => apiFetch<{ events: WakeEvent[] }>('/api/v1/events').then((r) => r.events)

export const sendTestEvent = (e: { type: string; subject: string; attrs: Record<string, string> }) =>
  apiFetch<WakeEvent>('/api/v1/events/test', { method: 'POST', json: e })

// The audit log: consents and revocations.

export interface AuditEntry {
  id: number
  at: string
  actor: string // user:<id>, app:<id>, client:<id> or hub
  actor_email?: string // for a user
  action: string
  agent_id?: string
  app_id?: string
  grant_id?: string
  detail: Record<string, unknown>
  // For grant.created: what the consent screen showed.
  shown?: { risks: { level: string; title: string; detail: string }[] | null; dry_run: { days: number; events: number } }
}

export const listAudit = (before?: number) =>
  apiFetch<{ entries: AuditEntry[] }>(`/api/v1/audit?limit=50${before ? `&before=${before}` : ''}`).then((r) => r.entries)

// ── Apps in this workspace ────────────────────────────────────────────────

export interface Grant {
  id: string
  app_id: string
  kind: 'agent' | 'workspace'
  agent_id?: string
  created_at: string
  expires_at: string
}

export interface AppInfo {
  id: string
  name: string
  description: string
  grants: Grant[]
}

// listApps lists the apps with access left in this workspace (grants from
// before apps stopped acting in people's own workspaces), with it.
export const listApps = () => apiFetch<{ apps: AppInfo[] }>('/api/v1/apps').then((r) => r.apps)
