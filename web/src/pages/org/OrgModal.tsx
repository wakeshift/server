// OrgModal: an organization's settings, members (with invites) and log, as tabs
// in a modal over the page you opened it from. Each tab keeps its own URL
// (/orgs/<id>/<tab>); the page behind is the location the link carried in
// its state (background), or Quick start when the URL was opened directly.
import { useEffect, useRef } from 'react'
import { Navigate, NavLink, useLocation, useNavigate, useParams } from 'react-router-dom'
import { X } from '@phosphor-icons/react'
import { background, orgPath } from '../../components/Layout'
import { TabLabel } from '../../components/ui'
import { useOrgFromURL } from './common'
import OrgLog from './Log'
import OrgMembers from './Members'
import OrgSettings from './Settings'

export type OrgTab = 'settings' | 'members' | 'log'

const tabs = {
  settings: { label: 'Settings', page: OrgSettings },
  members: { label: 'Members', page: OrgMembers },
  log: { label: 'Log', page: OrgLog },
} as const

export default function OrgModal() {
  const { tab = '' } = useParams()
  const location = useLocation()
  const navigate = useNavigate()
  const { id, org } = useOrgFromURL()
  const dialog = useRef<HTMLDialogElement>(null)
  const behind = background(location)
  const close = () => navigate(behind ? `${behind.pathname}${behind.search}${behind.hash}` : '/')

  useEffect(() => {
    const d = dialog.current
    if (d && !d.open) d.showModal()
  }, [tab]) // tab: the dialog first renders after the redirect from invites

  // Invites moved into Members; old links still land there.
  if (tab === 'invites') return <Navigate to={orgPath(id, 'members')} state={location.state} replace />
  if (!(tab in tabs)) return null
  const Page = tabs[tab as OrgTab].page
  const shown = Object.keys(tabs) as OrgTab[]

  return (
    <dialog
      ref={dialog}
      aria-label={org.data ? `${org.data.name}: ${tabs[tab as OrgTab].label}` : tabs[tab as OrgTab].label}
      className="m-auto h-[min(85vh,40rem)] w-[min(48rem,calc(100vw-2rem))] flex-col overflow-hidden rounded-xl border border-rule bg-paper p-0 text-ink shadow-[0_24px_60px_-24px_rgba(60,40,10,0.45)] backdrop:bg-ink/30 open:flex"
      onCancel={(e) => {
        e.preventDefault()
        close()
      }}
      onClick={(e) => e.target === dialog.current && close()}
    >
      <header className="flex items-start justify-between gap-4 border-b border-rule px-6 pt-5">
        <div className="min-w-0">
          <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-ink-3">Organization</p>
          <h2 className="truncate font-serif text-2xl">{org.data?.name ?? '…'}</h2>
          <nav aria-label="Organization" className="-mb-px mt-3 flex gap-1 overflow-x-auto">
            {shown.map((t) => (
              <NavLink
                key={t}
                to={orgPath(id, t)}
                state={location.state}
                replace
                className={({ isActive }) =>
                  `whitespace-nowrap border-b-2 px-3 py-2 text-sm transition-colors ${isActive ? 'border-accent text-ink' : 'border-transparent text-ink-2 hover:text-ink'}`
                }
              >
                {({ isActive }) => <TabLabel label={tabs[t].label} active={isActive} />}
              </NavLink>
            ))}
          </nav>
        </div>
        <button aria-label="Close" className="-mr-2 rounded-md p-2 text-ink-3 hover:bg-paper-2 hover:text-ink" onClick={close}>
          <X size={18} aria-hidden />
        </button>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto px-6 py-6">
        <Page />
      </div>
    </dialog>
  )
}
