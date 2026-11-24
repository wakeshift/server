import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, matchPath, NavLink, useLocation, useMatch, useNavigate, type Location } from 'react-router-dom'
import {
  AppWindow,
  Bell,
  BookOpen,
  ClipboardText,
  DotsThree,
  Gear,
  Lightning,
  Plus,
  Plugs,
  RocketLaunch,
  Robot,
  WarningCircle,
  type Icon,
} from '@phosphor-icons/react'
import { listApps, type Workspace } from '../api/client'
import { getOwnedApp, listOwnedApps } from '../api/developer'
import { roleLabel } from '../api/orgs'
import { useCurrentOrg } from '../hooks/useOrg'
import { useSession } from '../hooks/useSession'
import { useSite } from '../lib/site'
import { Badge, Logo } from './ui'
import './rail.css'

const workspaceLabel = (w: Workspace) => (w.kind === 'personal' ? 'Personal' : w.name)

// An app's sections, each with its own URL: /apps/<id>/<section>.
// They're tabs across the top of the app's pages (pages/app/common.tsx).
export const sections = [
  { path: 'overview', label: 'Overview' },
  { path: 'keys', label: 'Keys' },
  { path: 'subscriptions', label: 'Subscriptions' },
  { path: 'activity', label: 'Activity' },
  { path: 'users', label: 'Users' },
  { path: 'publish-urls', label: 'Publish URLs' },
  { path: 'log', label: 'Log' },
  { path: 'settings', label: 'Settings' },
] as const

export const appPath = (id: string, section = 'overview') => `/apps/${encodeURIComponent(id)}/${section}`

// Escape closes the innermost open thing, and only it: each open menu or
// drawer pushes its close here when it opens (so a menu opened inside the
// drawer is above it), and one listener calls the top one.
const escapes: { close: () => void }[] = []
let escapeListening = false
function onEscape(e: KeyboardEvent) {
  const top = escapes[escapes.length - 1]
  if (e.key !== 'Escape' || !top) return
  e.preventDefault()
  top.close()
}

// useEscape gives Escape to close while active.
function useEscape(active: boolean, close: () => void) {
  const latest = useRef(close)
  latest.current = close
  useEffect(() => {
    if (!active) return
    const entry = { close: () => latest.current() }
    escapes.push(entry)
    if (!escapeListening) {
      document.addEventListener('keydown', onEscape)
      escapeListening = true
    }
    return () => {
      const i = escapes.indexOf(entry)
      if (i >= 0) escapes.splice(i, 1)
    }
  }, [active])
}

// useOutside closes a menu on a click outside it or Escape.
function useOutside(open: boolean, close: () => void) {
  const ref = useRef<HTMLDivElement>(null)
  useEscape(open, close)
  useEffect(() => {
    if (!open) return
    const click = (e: MouseEvent) => ref.current && !ref.current.contains(e.target as Node) && close()
    document.addEventListener('mousedown', click)
    return () => document.removeEventListener('mousedown', click)
  }, [open, close])
  return ref
}

const menuItem = 'block rounded-md px-3 py-2 text-sm text-ink-2 hover:bg-paper-2 hover:text-ink'
const menuHeading = 'px-3 pb-1 pt-2 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-ink-3'

export const orgPath = (id: string, section = 'members') => `/orgs/${encodeURIComponent(id)}/${section}`

// background is the page an organization's modal (pages/org/OrgModal) was
// opened over: a link to one carries it in its state.
export const background = (location: Location) => (location.state as { background?: Location } | null)?.background

// The collapse glyph: a panel with its edge, and an arrow toward it. Flipped
// when collapsed (rail.css).
const CollapseGlyph = () => (
  <svg viewBox="0 0 20 20" width="16" height="16" fill="none" aria-hidden>
    <g stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
      <path d="M15.8333 2.5H4.16667C3.24619 2.5 2.5 3.24619 2.5 4.16667V15.8333C2.5 16.7538 3.24619 17.5 4.16667 17.5H15.8333C16.7538 17.5 17.5 16.7538 17.5 15.8333V4.16667C17.5 3.24619 16.7538 2.5 15.8333 2.5Z" />
      <path d="M7.5 2.5V17.5" />
      <path d="M13.3333 12.5L10.8333 10L13.3333 7.5" />
    </g>
  </svg>
)

// The sidebar's icons, from Phosphor, in the text's color.
const icons = {
  quickstart: RocketLaunch,
  agents: Robot,
  subscriptions: Bell,
  events: Lightning,
  connected: Plugs,
  audit: ClipboardText,
  docs: BookOpen,
} satisfies Record<string, Icon>

const NavIcon = ({ name }: { name: keyof typeof icons }) => {
  const I = icons[name]
  return (
    <span className="railItemIcon" aria-hidden>
      <I size={16} />
    </span>
  )
}

interface Item {
  to: string
  label: string
  icon: keyof typeof icons
  shown?: boolean
  end?: boolean
}

// RailLink is a nav item: its icon and its label (only the icon when the
// rail is collapsed, so the label is also its accessible name and tooltip).
function RailLink({ to, label, icon, end }: Item) {
  return (
    <NavLink to={to} end={end} className="railItem" title={label}>
      <NavIcon name={icon} />
      <span className="railItemLabel">{label}</span>
    </NavLink>
  )
}

// AppsSection: the organization's apps, the one you're in (if any) open,
// and New app below them. A suspended or deleted app is marked as needing
// you, and says which.
function AppsSection({ current }: { current?: string }) {
  const adding = !!useMatch('/new-app')
  const org = useCurrentOrg().current
  const apps = useQuery({ queryKey: ['developer-apps', org?.id], queryFn: () => listOwnedApps(org?.id), enabled: !!org })
  const rows = apps.data?.filter((a) => !a.deleted_at || a.id === current) ?? []
  return (
    <div className="railSection">
      <div className="railSectionHead">
        <p className="railSectionLabel">Apps</p>
      </div>
      <div className="railThreadList">
        {rows.map((a) => {
          const open = a.id === current
          const state = a.deleted_at ? 'deleted' : a.suspended_at ? 'suspended' : undefined
          return (
            <div key={a.id} className={`railThreadRow${open ? ' active' : ''}`}>
              <Link
                to={appPath(a.id)}
                className="railThread"
                title={a.id}
                aria-current={open ? 'page' : undefined}
                aria-label={`${a.name}${state ? `, ${state}` : ''}`}
              >
                <span className="railThreadStatus" aria-hidden>
                  {state ? (
                    <WarningCircle className="railThreadStatusIcon railThreadStatusIcon--warn" size={16} weight="fill" />
                  ) : (
                    <AppWindow className="railThreadStatusIcon" size={16} />
                  )}
                </span>
                <span className="railThreadLabel">{a.name}</span>
                {state && <span className="railThreadTime">{state}</span>}
              </Link>
            </div>
          )
        })}
        <div className={`railThreadRow${adding ? ' active' : ''}`}>
          <Link to="/new-app" className="railThread" title="New app" aria-current={adding ? 'page' : undefined}>
            <span className="railThreadStatus" aria-hidden>
              <Plus className="railThreadStatusIcon" size={16} />
            </span>
            <span className="railThreadLabel">New app</span>
          </Link>
        </div>
      </div>
    </div>
  )
}

// AccountMenu: you, and the organization whose apps you're looking at (the
// others you belong to, with your role), and sign out.
function AccountMenu() {
  const { session, logout } = useSession()
  const s = session.data
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  const close = () => setOpen(false)
  const ref = useOutside(open, close)
  const { orgs, current, choose } = useCurrentOrg()
  return (
    <div ref={ref} className="railAccountWrap">
      <button className="railAccount" aria-expanded={open} aria-label="Account" title={s?.user?.email} onClick={() => setOpen(!open)}>
        <span aria-hidden className="railAvatar">
          {s?.user?.email?.[0] ?? '?'}
        </span>
        <span className="railAccountText">
          <b>{s?.user?.email}</b>
          <small>{current?.name ?? (orgs.isPending ? '…' : 'No organization yet')}</small>
        </span>
      </button>
      {open && (
        <div className="absolute bottom-full left-0 z-30 mb-1 max-h-[75vh] w-[248px] overflow-y-auto rounded-lg border border-rule bg-card p-1 shadow-[0_12px_30px_-18px_rgba(60,40,10,0.35)]">
          <p className={menuHeading}>Organization</p>
          {orgs.data?.map((o) => (
            <button
              key={o.id}
              className={`${menuItem} flex w-full items-center justify-between gap-2 text-left ${o.id === current?.id ? 'text-ink' : ''}`}
              onClick={() => {
                choose(o.id)
                close()
                navigate('/')
              }}
            >
              <span className="truncate">{o.name}</span>
              <span className="flex shrink-0 items-center gap-1.5">
                <Badge>{roleLabel[o.role]}</Badge>
                {o.id === current?.id && <span className="text-accent-text">•</span>}
              </span>
            </button>
          ))}
          <Link to="/new-org" className={`${menuItem} font-semibold text-accent-text`} onClick={close}>
            + New organization
          </Link>
          <div className="my-1 border-t border-rule" />
          <button className={`${menuItem} w-full text-left`} onClick={() => logout.mutate()}>
            Sign out
          </button>
        </div>
      )}
    </div>
  )
}

// OrgSettings: the gear beside the account, opening the organization's
// settings in a modal over this page.
function OrgSettings() {
  const location = useLocation()
  const current = useCurrentOrg().current
  if (!current) return null
  return (
    <Link
      to={orgPath(current.id, 'settings')}
      state={{ background: background(location) ?? location }}
      className="railItem railItem--icon"
      aria-label="Organization settings"
      title="Organization settings"
    >
      <span className="railItemIcon" aria-hidden>
        <Gear size={16} />
      </span>
    </Link>
  )
}

// WorkspaceSection: your own Wakeshift workspace (your agents, apps left
// with grants from account mode, and trying a wake-up by hand). With more
// than one workspace, its menu switches between them.
function WorkspaceSection() {
  const { session, changeWorkspace } = useSession()
  const s = session.data
  const path = useLocation().pathname
  const apps = useQuery({ queryKey: ['apps'], queryFn: listApps })
  const [open, setOpen] = useState(false)
  const close = () => setOpen(false)
  const ref = useOutside(open, close)
  const items = (
    [
      { to: '/agents', label: 'Agents', icon: 'agents' },
      { to: '/subscriptions', label: 'Subscriptions', icon: 'subscriptions' },
      { to: '/events', label: 'Events', icon: 'events' },
      { to: '/apps', label: 'Connected apps', icon: 'connected', shown: !!apps.data?.length },
      { to: '/audit', label: 'Audit', icon: 'audit' },
    ] satisfies Item[]
  ).filter((i) => i.shown !== false || path === i.to)
  const several = !!s?.workspaces && s.workspaces.length > 1
  return (
    <nav aria-label="Your Wakeshift workspace" className="railSection">
      <div className="railSectionHead">
        <p className="railSectionLabel">{several && s?.workspace ? workspaceLabel(s.workspace) : 'Workspace'}</p>
        {several && (
          <div ref={ref} className="railSectionMore">
            <button
              className={open ? 'railSectionAdd is-open' : 'railSectionAdd'}
              aria-label="Switch workspace"
              aria-haspopup="menu"
              aria-expanded={open}
              onClick={() => setOpen(!open)}
            >
              <DotsThree size={13} weight="bold" aria-hidden />
            </button>
            {open && (
              <div role="menu" aria-label="Workspaces" className="absolute right-0 top-full z-30 mt-1 w-max min-w-[168px] max-w-[240px] rounded-lg border border-rule bg-card p-1 leading-normal shadow-[0_12px_30px_-18px_rgba(60,40,10,0.35)]">
                {s!.workspaces!.map((w) => (
                  <button
                    key={w.id}
                    role="menuitemradio"
                    aria-checked={w.id === s?.workspace?.id}
                    className={`${menuItem} flex w-full items-center justify-between gap-2 whitespace-nowrap text-left ${w.id === s?.workspace?.id ? 'text-ink' : ''}`}
                    onClick={() => {
                      changeWorkspace.mutate(w.id)
                      close()
                    }}
                  >
                    <span className="truncate">{workspaceLabel(w)}</span>
                    {w.id === s?.workspace?.id && <span className="text-accent-text">•</span>}
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
      <div className="railThreadList">
        {items.map((i) => (
          <RailLink key={i.to} {...i} end />
        ))}
      </div>
    </nav>
  )
}

// Sidebar: the console's rail (rail.css), the same on every page. The
// wordmark and collapse; Quick start; the organization's apps ending in New
// app (an app's pages are tabs on them); your workspace's pages; Docs; and
// your account beside the organization's settings.
function Sidebar({ appID, collapsed, onCollapse }: { appID?: string; collapsed: boolean; onCollapse: () => void }) {
  const site = useSite()
  const org = useCurrentOrg().current
  return (
    <div className="chrome-rail">
      <div className="brand">
        <Link to="/" className="brandLockup" aria-label="Wakeshift">
          <Logo size="sm" />
        </Link>
        <button className="railCollapse" aria-label={collapsed ? 'Expand navigation' : 'Collapse navigation'} aria-expanded={!collapsed} onClick={onCollapse}>
          <CollapseGlyph />
        </button>
      </div>
      <div className="railNav">
        {org && (
          <nav aria-label="Console">
            <RailLink to="/" label="Quick start" icon="quickstart" end />
          </nav>
        )}
        {org && <AppsSection current={appID} />}
        <WorkspaceSection />
        <div className="railSpacer" aria-hidden />
        <a href={site.docs} className="railItem" title="Docs">
          <NavIcon name="docs" />
          <span className="railItemLabel">Docs ↗</span>
        </a>
      </div>
      <div className="asideFoot">
        <div className="asideFootBar">
          <AccountMenu />
          <OrgSettings />
        </div>
      </div>
    </div>
  )
}

// useDesktop is whether the sidebar is always shown (Tailwind's lg).
function useDesktop() {
  const query = '(min-width: 1024px)'
  const [desktop, setDesktop] = useState(() => window.matchMedia(query).matches)
  useEffect(() => {
    const m = window.matchMedia(query)
    const change = () => setDesktop(m.matches)
    m.addEventListener('change', change)
    return () => m.removeEventListener('change', change)
  }, [])
  return desktop
}

// Whether you collapsed the sidebar, remembered in this browser.
const foldedKey = 'wakeshift.sidebarCollapsed'
function readFolded() {
  try {
    return localStorage.getItem(foldedKey) === '1'
  } catch {
    return false
  }
}

const focusable = 'a[href], button:not([disabled]), select:not([disabled]), input:not([disabled]), [tabindex]:not([tabindex="-1"])'

// The console's frame: a sidebar (the app switcher, the app's sections,
// your workspace's pages, docs, and your account and organization) and the page. On a phone the sidebar is a drawer
// opened from the header: closed, it's inert (out of the tab order and the
// accessibility tree); open, it holds focus until Escape or a link closes it.
export default function Layout({ children }: { children: ReactNode }) {
  const location = useLocation()
  // The sidebar is the page's: behind an organization's modal, the page it
  // was opened over.
  const appID = matchPath('/apps/:id/*', (background(location) ?? location).pathname)?.params.id
  const orgID = useMatch('/orgs/:org/*')?.params.org
  // Opening an app, or an organization's page, switches to its
  // organization.
  const { current, choose } = useCurrentOrg()
  const app = useQuery({ queryKey: ['developer-app', appID], queryFn: () => getOwnedApp(appID!), enabled: !!appID }).data
  const wanted = orgID ?? (app?.id === appID ? app?.org_id : undefined)
  useEffect(() => {
    if (wanted && current && wanted !== current.id) choose(wanted)
  }, [wanted, current, choose])
  const desktop = useDesktop()
  // Collapsed to icons, on a desktop only: the phone's drawer is always full.
  const [folded, setFolded] = useState(readFolded)
  const collapsed = desktop && folded
  const toggleCollapsed = () => {
    setFolded(!folded)
    try {
      localStorage.setItem(foldedKey, folded ? '0' : '1')
    } catch {
      // Not remembered: it still collapses for this visit.
    }
  }
  const [open, setOpen] = useState(false)
  const drawer = !desktop
  const aside = useRef<HTMLElement>(null)
  const menuButton = useRef<HTMLButtonElement>(null)
  useEffect(() => setOpen(false), [location.pathname])

  const close = useCallback((refocus: boolean) => {
    setOpen(false)
    if (refocus) menuButton.current?.focus()
  }, [])

  useEffect(() => {
    if (aside.current) aside.current.inert = drawer && !open
  }, [drawer, open])
  // Escape closes the drawer, unless a menu in it is open (that closes first).
  useEscape(drawer && open, () => close(true))

  useEffect(() => {
    if (!drawer || !open || !aside.current) return
    const el = aside.current
    // The visible ones: the logo link is hidden in the drawer.
    const items = () => [...el.querySelectorAll<HTMLElement>(focusable)].filter((i) => i.getClientRects().length > 0)
    items()[0]?.focus()
    const key = (e: KeyboardEvent) => {
      if (e.key !== 'Tab') return
      const all = items()
      if (all.length === 0) return
      const first = all[0]
      const last = all[all.length - 1]
      if (e.shiftKey && (document.activeElement === first || !el.contains(document.activeElement))) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && (document.activeElement === last || !el.contains(document.activeElement))) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', key)
    return () => document.removeEventListener('keydown', key)
  }, [drawer, open, close])

  return (
    <div className="min-h-screen lg:grid lg:grid-cols-[auto_minmax(0,1fr)]">
      <header className="sticky top-0 z-20 flex items-center justify-between border-b border-rule bg-paper/95 px-5 py-3 backdrop-blur-sm lg:hidden">
        <Link to="/">
          <Logo />
        </Link>
        <button
          ref={menuButton}
          className="rounded-full border border-rule px-3.5 py-1.5 text-[13px] font-semibold"
          aria-expanded={open}
          aria-controls="console-sidebar"
          onClick={() => (open ? close(false) : setOpen(true))}
        >
          {open ? 'Close' : 'Menu'}
        </button>
      </header>
      {open && <div className="fixed inset-0 z-30 bg-ink/20 lg:hidden" aria-hidden onClick={() => close(true)} />}
      <aside
        ref={aside}
        id="console-sidebar"
        aria-label="Console menu"
        className={`railShell fixed inset-y-0 left-0 z-40 w-[280px] max-w-[85vw] overflow-y-auto border-r border-rule lg:visible ${collapsed ? 'is-collapsed' : ''} lg:static lg:z-auto lg:w-auto lg:max-w-none lg:translate-x-0 lg:overflow-visible ${open ? 'visible translate-x-0 transition-transform' : 'invisible -translate-x-full transition-[translate,visibility]'}`}
      >
        <div className={`h-full lg:sticky lg:top-0 lg:h-screen ${collapsed ? 'lg:overflow-visible' : 'lg:overflow-y-auto'}`}>
          <Sidebar appID={appID} collapsed={collapsed} onCollapse={toggleCollapsed} />
        </div>
      </aside>
      <main className="mx-auto w-full max-w-5xl px-5 py-8 sm:px-8 sm:py-10">{children}</main>
    </div>
  )
}
