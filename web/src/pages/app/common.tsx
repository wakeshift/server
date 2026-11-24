// What the app's pages share: the app from the URL, the page header, and
// small pieces (a conversation's harness, how much was read, "More").
import type { ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { NavLink, useParams } from 'react-router-dom'
import { ApiError } from '../../api/client'
import { getOwnedApp, type Conversation, type OwnedApp, type Scanned } from '../../api/developer'
import { appPath, sections } from '../../components/Layout'
import { Badge, Button, Empty, ErrorText, Loading, Notice, PageHeader, TabLabel, TextLink } from '../../components/ui'

export function useApp() {
  const { id = '' } = useParams()
  const app = useQuery({ queryKey: ['developer-app', id], queryFn: () => getOwnedApp(id) })
  return { id, app }
}

// AppTabs: the app's sections, at the very top of each of its pages.
function AppTabs({ id }: { id: string }) {
  return (
    <nav aria-label="App" className="flex gap-1 overflow-x-auto border-b border-rule">
      {sections.map((s) => (
        <NavLink
          key={s.path}
          to={appPath(id, s.path)}
          className={({ isActive }) =>
            `-mb-px whitespace-nowrap border-b-2 px-3 py-2 text-sm transition-colors ${isActive ? 'border-accent text-ink' : 'border-transparent text-ink-2 hover:text-ink'}`
          }
        >
          {({ isActive }) => <TabLabel label={s.label} active={isActive} />}
        </NavLink>
      ))}
    </nav>
  )
}

// AppPage loads the app named in the URL and shows its section, or says
// it isn't yours (another person's app reads as one that doesn't exist).
export function AppPage({ title, intro, actions, children }: { title: string; intro?: ReactNode; actions?: ReactNode; children: (app: OwnedApp) => ReactNode }) {
  const { id, app } = useApp()
  if (app.isPending) return <Loading />
  if (app.isError) {
    if (app.error instanceof ApiError && app.error.status === 404) {
      return (
        <Empty title="No such app">
          You don't have an app with the ID <code>{id}</code>. <TextLink to="/">See your apps</TextLink>.
        </Empty>
      )
    }
    return <ErrorText error={app.error} />
  }
  return (
    <div className="space-y-7">
      <AppTabs id={id} />
      <PageHeader context={app.data.name} title={title} actions={actions}>
        {intro}
      </PageHeader>
      {app.data.deleted_at && (
        <Notice tone="caution" title="This app is deleted">
          Its keys stopped working and its agents' subscriptions ended. You can still read what it did.
        </Notice>
      )}
      {children(app.data)}
    </div>
  )
}

// The programs agents run in that Wakeshift knows, by ID, with the names
// internal/harnesses gives them (retired ones show their ID).
export const harnessNames: Record<string, string> = {
  'claude-code': 'Claude Code',
  codex: 'Codex',
  cursor: 'Cursor',
  'cursor-cloud': 'Cursor cloud agent',
  'grok-bot': 'Grok Bot',
  copilot: 'GitHub Copilot CLI',
  opencode: 'opencode',
  antigravity: 'Antigravity CLI',
  openclaw: 'OpenClaw',
  hermes: 'Hermes Agent',
  chatgpt: 'ChatGPT',
  muse: 'Muse',
  goose: 'Goose',
  cline: 'Cline',
  amp: 'Amp',
  devin: 'Devin (Windsurf)',
  vscode: 'VS Code (Copilot agent mode)',
}

export const harnessName = (h: string) => harnessNames[h] ?? h

const viaNames = { stream: 'stream', poll: 'polling', hooks: 'hooks' }

// ConversationLine: which harness, and (with status) whether it's
// connected now and how.
export function ConversationLine({ c, status = true }: { c?: Conversation; status?: boolean }) {
  if (!c) return <span className="text-ink-3">Conversation ended</span>
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <span>{harnessName(c.harness)}</span>
      {!status ? null : c.connected ? (
        <Badge tone="sage">connected{c.via ? ` · ${viaNames[c.via]}` : ''}</Badge>
      ) : (
        <Badge>not connected</Badge>
      )}
    </span>
  )
}

// ScannedNotice says when a page read only some of the app's workspaces,
// and where to see the rest: the page's own user filter if it has one, else
// Activity's.
export function ScannedNotice({ s, appID, filter }: { s?: Scanned; appID: string; filter?: boolean }) {
  if (!s || s.workspaces_scanned >= s.workspaces_total) return null
  return (
    <Notice>
      Showing the {s.workspaces_scanned} newest of your app's {s.workspaces_total} users and conversations.{' '}
      {filter ? (
        'Filter by user to see anyone else.'
      ) : (
        <>
          To see anyone else, filter by user in <TextLink to={appPath(appID, 'activity')}>Activity</TextLink>.
        </>
      )}
    </Notice>
  )
}

export function More({ query }: { query: { hasNextPage: boolean; isFetchingNextPage: boolean; fetchNextPage: () => unknown } }) {
  if (!query.hasNextPage) return null
  return (
    <Button className="mt-5" size="sm" variant="secondary" disabled={query.isFetchingNextPage} onClick={() => query.fetchNextPage()}>
      {query.isFetchingNextPage ? 'Loading…' : 'More'}
    </Button>
  )
}

// ago says how long ago, briefly: "just now", "5 min ago", "3 h ago", or
// the date.
export function ago(iso?: string): string {
  if (!iso) return 'never'
  const s = (Date.now() - new Date(iso).getTime()) / 1000
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)} min ago`
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`
  return new Date(iso).toLocaleDateString(undefined, { dateStyle: 'medium' })
}

// until says how long until, briefly.
export function until(iso: string): string {
  const s = (new Date(iso).getTime() - Date.now()) / 1000
  if (s <= 0) return 'now'
  if (s < 60) return 'in under a minute'
  if (s < 3600) return `in ${Math.floor(s / 60)} min`
  if (s < 86400) return `in ${Math.floor(s / 3600)} h`
  return `in ${Math.floor(s / 86400)} d`
}

// UserLabel: the app's ID for the person, or what kind of conversation has
// a workspace of its own.
export function UserLabel({ user, harness }: { user?: string; harness?: string }) {
  if (user) return <code className="break-all">{user}</code>
  return <span className="text-ink-3">No user{harness ? ` (a ${harnessName(harness)} conversation)` : ' (one conversation)'}</span>
}

// count is n and the noun, singular or plural.
export const count = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`
