// Organizations: who manages which app, under /api/v1/orgs, and joining
// one through an invite (/api/v1/join). A non-member gets 404
// everywhere, as for an organization that doesn't exist; a member whose
// role doesn't allow something gets 403.
import { apiFetch } from './client'

export type Role = 'owner' | 'admin' | 'developer'

export interface Org {
  id: string
  name: string
  personal: boolean
  role: Role
  members: number
  apps: number
  created_at: string
}

export interface Member {
  user_id: string
  email: string
  name: string
  role: Role
  invited_by?: string
  joined_at: string
}

export interface OrgInvite {
  id: string
  email: string
  role: Role
  invited_by: string
  created_at: string
  expires_at: string
  expired: boolean
}

// A sent invite: its link (shown once) and whether it was emailed.
export interface SentInvite {
  invite: OrgInvite
  url: string
  emailed: boolean
}

export interface OrgLogEntry {
  id: number
  at: string
  actor: string
  actor_email?: string
  action: string
  detail: Record<string, unknown>
}

export interface Made {
  keys: { app_id: string; key_id: string; name: string; created_at: string }[]
  publish_urls: { app_id: string; id: string; topic: string; created_at: string }[]
}

const org = (id: string) => `/api/v1/orgs/${encodeURIComponent(id)}`

export const listOrgs = () => apiFetch<{ orgs: Org[] }>('/api/v1/orgs').then((r) => r.orgs)
export const getOrg = (id: string) => apiFetch<Org>(org(id))
export const createOrg = (name: string) => apiFetch<Org>('/api/v1/orgs', { method: 'POST', json: { name } })
export const renameOrg = (id: string, name: string) => apiFetch<Org>(org(id), { method: 'PATCH', json: { name } })
export const deleteOrg = (id: string) => apiFetch<{ deleted: boolean }>(org(id), { method: 'DELETE' })

export const listMembers = (id: string) => apiFetch<{ members: Member[] }>(`${org(id)}/members`).then((r) => r.members)
export const setRole = (id: string, user: string, role: Role) =>
  apiFetch<{ role: Role }>(`${org(id)}/members/${encodeURIComponent(user)}`, { method: 'PATCH', json: { role } })
export const removeMember = (id: string, user: string) =>
  apiFetch<{ removed: boolean }>(`${org(id)}/members/${encodeURIComponent(user)}`, { method: 'DELETE' })
export const memberMade = (id: string, user: string) => apiFetch<Made>(`${org(id)}/members/${encodeURIComponent(user)}/made`)

export const listInvites = (id: string) => apiFetch<{ invites: OrgInvite[]; email: boolean }>(`${org(id)}/invites`)
export const createInvite = (id: string, email: string, role: Role) =>
  apiFetch<SentInvite>(`${org(id)}/invites`, { method: 'POST', json: { email, role } })
export const revokeInvite = (id: string, invite: string) =>
  apiFetch<{ revoked: boolean }>(`${org(id)}/invites/${encodeURIComponent(invite)}/revoke`, { method: 'POST' })
export const resendInvite = (id: string, invite: string) =>
  apiFetch<SentInvite>(`${org(id)}/invites/${encodeURIComponent(invite)}/resend`, { method: 'POST' })

export const orgLog = (id: string, before?: number) =>
  apiFetch<{ entries: OrgLogEntry[] }>(`${org(id)}/log?limit=50${before ? `&before=${before}` : ''}`).then((r) => r.entries)

export interface JoinPreview {
  org_name: string
  invited_by: string
  role: Role
}

export const previewInvite = (token: string) => apiFetch<JoinPreview>('/api/v1/join/preview', { method: 'POST', json: { token } })
export const acceptInvite = (token: string) => apiFetch<{ org_id: string }>('/api/v1/join', { method: 'POST', json: { token } })
// keepForSignIn hands the token to the sign-in that follows, in a cookie
// (never the URL).
export const keepForSignIn = (token: string) => apiFetch<{ ok: boolean }>('/api/v1/join/sign-in', { method: 'POST', json: { token } })

// What each role may do: the server's table (internal/orgs/roles.go), so
// pages show only the buttons that work. The server enforces it anyway.
export const can = {
  updateApp: (r?: Role) => r === 'owner' || r === 'admin',
  deleteApp: (r?: Role) => r === 'owner' || r === 'admin',
  forgetUser: (r?: Role) => r === 'owner' || r === 'admin',
  // linkKey: add @wakeshift/cli's key, from its link, to an app that exists.
  linkKey: (r?: Role) => r === 'owner' || r === 'admin',
  renameOrg: (r?: Role) => r === 'owner' || r === 'admin',
  deleteOrg: (r?: Role) => r === 'owner',
  invite: (r?: Role) => r === 'owner' || r === 'admin',
  changeRoles: (r?: Role) => r === 'owner',
  // manage: remove a member, or revoke or resend an invite, with role target.
  manage: (r: Role | undefined, target: Role) => r === 'owner' || (r === 'admin' && target === 'developer'),
  // grant: invite as role.
  grant: (r: Role | undefined, role: Role) => r === 'owner' || (r === 'admin' && role === 'developer'),
}

export const roleLabel: Record<Role, string> = { owner: 'Owner', admin: 'Admin', developer: 'Developer' }
