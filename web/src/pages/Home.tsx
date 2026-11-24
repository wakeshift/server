import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { getInvite, getStatus } from '../api/client'
import { getOverview, listOwnedApps, type OwnedApp } from '../api/developer'
import { appPath } from '../components/Layout'
import { Badge, ButtonLink, Card, copy, Empty, ErrorText, Loading, Meta, PageHeader, Row, Rows, when } from '../components/ui'
import { Check, Copy } from '@phosphor-icons/react'
import { useCurrentOrg } from '../hooks/useOrg'
import { useSession } from '../hooks/useSession'
import { ago, count, harnessName } from './app/common'

const Step = ({ n, title, children }: { n: number; title: string; children: React.ReactNode }) => (
  <li className="grid grid-cols-[2rem_1fr] py-4">
    <span className="font-serif text-xl italic text-accent-text">{n}</span>
    <div>
      <b className="block text-[15px] font-semibold">{title}</b>
      <div className="mt-1 text-sm leading-relaxed text-ink-2">{children}</div>
    </div>
  </li>
)

function Stat({ label, value, sub }: { label: string; value: React.ReactNode; sub?: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <div className="font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-ink-3">{label}</div>
      <div className="mt-1 font-serif text-2xl leading-none">{value}</div>
      {sub && <div className="mt-1 truncate text-xs text-ink-3">{sub}</div>}
    </div>
  )
}

// AppCard: one of your apps, with what's happening in it now.
function AppCard({ app }: { app: OwnedApp }) {
  const o = useQuery({ queryKey: ['developer-overview', app.id], queryFn: () => getOverview(app.id) })
  const harnesses = Object.entries(o.data?.connected_by_harness ?? {})
    .sort((a, b) => b[1] - a[1])
    .map(([h, n]) => `${n} ${harnessName(h)}`)
    .join(', ')
  return (
    <Link to={appPath(app.id)} className="group block rounded-xl border border-rule bg-card p-5 transition-colors hover:border-ink-3 sm:p-6">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="truncate font-serif text-xl leading-tight group-hover:text-accent-text">{app.name}</h2>
          <code className="text-xs text-ink-3">{app.id}</code>
        </div>
        {app.suspended_at && <Badge tone="caution">suspended</Badge>}
      </div>
      <div className="mt-5 grid grid-cols-3 gap-4 border-t border-rule pt-4">
        <Stat label="Connected" value={o.data?.connected ?? '–'} sub={harnesses || undefined} />
        <Stat label="Wakes, 24 h" value={o.data?.wakes_24h ?? '–'} sub={o.data ? count(o.data.publishes_24h, 'publish', 'publishes') : undefined} />
        <Stat label="Last activity" value={<span className="text-base">{o.data ? ago(o.data.last_activity_at) : '–'}</span>} />
      </div>
      {!!o.data?.failures_24h && <p className="mt-3 text-xs text-danger">{count(o.data.failures_24h, 'delivery', 'deliveries')} failed in the last 24 hours</p>}
    </Link>
  )
}

// Home: the apps you build.
export default function Home() {
  const { session } = useSession()
  const { orgs, current: org } = useCurrentOrg()
  const apps = useQuery({ queryKey: ['developer-apps', org?.id], queryFn: () => listOwnedApps(org?.id), enabled: !!org })
  const status = useQuery({ queryKey: ['status'], queryFn: getStatus, refetchInterval: 30_000 })
  const live = apps.data?.filter((a) => !a.deleted_at) ?? []
  const deleted = apps.data?.filter((a) => a.deleted_at) ?? []
  const name = session.data?.user?.email?.split('@')[0]

  if (orgs.isSuccess && !org) {
    return (
      <div className="space-y-8">
        <PageHeader title={name ? `Welcome, ${name}.` : 'Welcome.'}>
          Apps belong to an organization, and you're not in one. Make one, or ask someone to invite you to theirs.
        </PageHeader>
        <Empty title="No organization" action={<ButtonLink to="/new-org">New organization</ButtonLink>}>
          An organization holds apps and the people who manage them with you.
        </Empty>
      </div>
    )
  }

  return (
    <div className="space-y-8">
      <PageHeader
        kicker={org && !org.personal ? org.name : undefined}
        title={live.length ? 'Apps' : name ? `Welcome, ${name}.` : 'Welcome.'}
      >
        Apps that wake people's agents when something happens. People add your app to their agent and use it without a
        Wakeshift account.
        {live.length > 0 && (
          <div className="mt-5">
            <ButtonLink to="/new-app">New app</ButtonLink>
          </div>
        )}
      </PageHeader>

      {(orgs.isPending || apps.isPending) && <Loading />}
      <ErrorText error={orgs.error} />
      <ErrorText error={apps.error} />

      {apps.isSuccess && live.length === 0 && (
        <Card title="Build your first app">
          <ol className="-my-4">
            <Step n={1} title="Register it">
              Pick an ID and a name. You get a key at once.
            </Step>
            <Step n={2} title="Give your app the key">
              Put it in your app's environment as <code>WAKESHIFT_KEY</code> and install the SDK (<code>@wakeshift/sdk</code>), or
              call the HTTP API from any language.
            </Step>
            <Step n={3} title="Define an event and emit it">
              Define the events your app offers with <code>defineEvent</code> and serve them on your MCP server. Agents subscribe,
              and when you <code>emit</code> one, each subscribed agent wakes with it, whatever program it runs in.
            </Step>
          </ol>
          <div className="mt-5">
            <ButtonLink to="/new-app">Create a new app</ButtonLink>
          </div>
        </Card>
      )}

      {live.length > 0 && (
        <div className="grid gap-5 md:grid-cols-2">
          {live.map((a) => (
            <AppCard key={a.id} app={a} />
          ))}
        </div>
      )}

      {deleted.length > 0 && (
        <Card title="Deleted apps" description="Their keys don't work any more. Their logs stay readable for 30 days after they were deleted.">
          <Rows>
            {deleted.map((a) => (
              <Row key={a.id} muted actions={<ButtonLink to={appPath(a.id, 'log')} size="sm" variant="secondary">Log</ButtonLink>}>
                {a.name} <code className="ml-1 text-xs">{a.id}</code>
                <Meta>Deleted {when(a.deleted_at!)}</Meta>
              </Row>
            ))}
          </Rows>
        </Card>
      )}

      <Invite />

      {status.data && (
        <p className="font-mono text-xs text-ink-3">
          Server {status.data.version} · database {status.data.database ? 'connected' : <span className="text-danger">unreachable</span>}
        </p>
      )}
    </div>
  )
}

function Invite() {
  const invite = useQuery({ queryKey: ['invite'], queryFn: getInvite })
  const [copied, setCopied] = useState(false)
  if (!invite.data) return null
  const { uses_left, url } = invite.data
  return (
    <Card title="Invite people">
      {uses_left > 0 ? (
        <>
          <p className="mb-3 text-sm leading-relaxed text-ink-2">
            Share this link to give {uses_left} more {uses_left === 1 ? 'person' : 'people'} an account right away.
          </p>
          <div className="flex items-center gap-2 rounded-lg border border-rule bg-paper py-1 pl-3 pr-1">
            <code id="invite-link" className="block min-w-0 flex-1 truncate text-xs">
              {url}
            </code>
            <button
              className="shrink-0 rounded-md p-2 text-ink-2 transition-colors hover:bg-paper-2 hover:text-ink focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ink"
              aria-label={copied ? 'Copied' : 'Copy link'}
              title={copied ? 'Copied' : 'Copy link'}
              onClick={async () => setCopied(await copy(url, document.getElementById('invite-link')))}
            >
              {copied ? <Check size={16} aria-hidden /> : <Copy size={16} aria-hidden />}
            </button>
          </div>
        </>
      ) : (
        <p className="text-sm text-ink-3">You've used all your invites.</p>
      )}
    </Card>
  )
}
