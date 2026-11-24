import { useState, type ReactNode } from 'react'
import { Check, Copy } from '@phosphor-icons/react'
import { useQuery } from '@tanstack/react-query'
import { getOverview, type OwnedApp, type Overview } from '../../api/developer'
import { appPath } from '../../components/Layout'
import { Badge, Card, copy, Empty, ErrorText, Facts, linkClass, Loading, Meta, Row, Rows, TextLink, when } from '../../components/ui'
import { useSite } from '../../lib/site'
import { DEFAULT_CORE } from '../../components/KeySDKNote'
import { ago, AppPage, count, harnessName, ScannedNotice } from './common'

function Tile({ label, value, children, tone }: { label: string; value: ReactNode; children?: ReactNode; tone?: 'danger' }) {
  return (
    <div className="rounded-xl border border-rule bg-card p-4 sm:p-5">
      <div className="font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-ink-3">{label}</div>
      <div className={`mt-1.5 font-serif text-3xl leading-none ${tone === 'danger' ? 'text-danger' : ''}`}>{value}</div>
      {children && <div className="mt-2 text-xs leading-relaxed text-ink-3">{children}</div>}
    </div>
  )
}

// How a conversation is connected (appapi's connectedVia), in plain words.
const via: Record<string, string> = {
  hooks: "through Wakeshift's setup", // its hooks, plugin or connector on the user's machine
  stream: 'by a live connection',
  poll: 'by checking in',
  webhook: 'by webhook',
}

// Who's here: conversations, and people only for an app that signs them in
// (identify returns a user). The Connected now tile has the count by
// harness; the row here says how they're connected.
function WhosHere({ app, d }: { app: OwnedApp; d: Overview }) {
  const users = appPath(app.id, 'users')
  const signIn = d.users > 0
  const methods = Object.entries(d.connected_via).filter(([, n]) => n > 0).sort((a, b) => b[1] - a[1])
  const items: [string, ReactNode][] = [
    ['Conversations', signIn ? <>{d.conversations}</> : <TextLink to={users}>{d.conversations}</TextLink>],
  ]
  if (signIn) {
    items.push([
      'People',
      <>
        <TextLink to={users}>{d.users}</TextLink>
        {d.conversation_workspaces > 0 && (
          <div className="mt-0.5 text-xs text-ink-3">plus {count(d.conversation_workspaces, 'conversation', 'conversations')} without sign-in</div>
        )}
      </>,
    ])
  }
  items.push(
    ['Subscriptions', <TextLink to={appPath(app.id, 'subscriptions')}>{d.subscriptions}</TextLink>],
    ['Connected now', methods.length ? methods.map(([k, n]) => `${n} ${via[k] ?? `by ${k}`}`).join(', ') : <span className="text-ink-3">nobody right now</span>],
    ['Publish URLs', <TextLink to={appPath(app.id, 'publish-urls')}>{d.publish_urls}</TextLink>],
  )
  return <Facts items={items} />
}

// The quickstart, with this app's ID in it. A current key goes to
// production's core unless WAKESHIFT_CORE says otherwise, so the line is
// there only on another console (staging, local). The console issues only
// current keys. An older wst_test_ key still in use on production needs
// the line; without it, every SDK stops with an error that says to set it.
function TryIt({ app }: { app: OwnedApp }) {
  const site = useSite()
  const [copied, setCopied] = useState(false)
  const core = window.location.origin
  const coreLine = core === DEFAULT_CORE ? '' : `\nWAKESHIFT_CORE=${core}`
  const snippet = `# .env
WAKESHIFT_KEY=wst_…   # a key from Keys${coreLine}

// server.ts
import { createServer } from "node:http"
import { McpServer, createMcpHandler } from "@modelcontextprotocol/server"
import { toNodeHandler } from "@modelcontextprotocol/node"
import { z } from "zod"
import { Wakeshift } from "@wakeshift/sdk"

const wake = new Wakeshift({ key: process.env.WAKESHIFT_KEY! })

// An event your app offers. Required arguments route it; values are lowercase IDs.
const ciFinished = wake.defineEvent("ci.finished", {
  description: "CI finished on a GitHub repo",
  args: z.object({ owner: z.string(), repo: z.string() }),
  payload: z.object({ conclusion: z.string(), branch: z.string() }),
  text: (run, { owner, repo }) => \`CI \${run.conclusion} on \${owner}/\${repo} \${run.branch}\`,
})

// A server for each request: agents subscribe through your tools.
// identify returns your own ID for the person (userOf is your sign-in's code).
const mcp = createMcpHandler(() => {
  const server = new McpServer({ name: "${app.id}", version: "1.0.0" })
  const attached = wake.attach(server, { identify: (ctx) => ({ user: userOf(ctx.http?.authInfo) }) })

  // A tool of yours that subscribes the conversation calling it. Wakeshift adds
  // what the agent needs to be woken to whatever it returns.
  attached.registerTool(
    "watch_ci",
    { description: "Watch CI on a repo", inputSchema: z.object({ owner: z.string(), repo: z.string() }), subscribes: [ciFinished] },
    async ({ owner, repo }, ctx, call) => {
      await call.subscribe(ciFinished, { owner, repo }, { reason: \`Watching CI on \${owner}/\${repo}\` })
      return \`Watching CI on \${owner}/\${repo}.\`
    },
  )
  return server
})
createServer(toNodeHandler(mcp)).listen(3000)

// Anywhere in your app: wake every agent subscribed to acme/api.
await ciFinished.emit({ owner: "acme", repo: "api" }, { conclusion: "failure", branch: "main" })`
  return (
    <Card
      title="Try it"
      description={
        <>
          Define an event, subscribe conversations to it from your own tool, and emit it to wake every subscribed agent. The{' '}
          <a className={linkClass} href={`${site.docs}/quickstart`}>
            quickstart
          </a>{' '}
          has the whole thing.
        </>
      }
    >
      <div className="relative">
        <pre id="try-it" className="h-[600px] overflow-auto rounded-lg border border-rule bg-paper p-4 pr-14 text-xs leading-relaxed">
          <code>{snippet}</code>
        </pre>
        <button
          type="button"
          className="absolute right-5 top-2 rounded-md bg-paper p-2 text-ink-2 transition-colors hover:bg-paper-2 hover:text-ink focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ink"
          aria-label={copied ? 'Copied' : 'Copy code'}
          title={copied ? 'Copied' : 'Copy code'}
          onClick={async () => setCopied(await copy(snippet, document.getElementById('try-it')))}
        >
          {copied ? <Check size={16} aria-hidden /> : <Copy size={16} aria-hidden />}
        </button>
      </div>
    </Card>
  )
}

export default function AppOverview() {
  return (
    <AppPage title="Overview">
      {(app) => <Body app={app} />}
    </AppPage>
  )
}

function Body({ app }: { app: OwnedApp }) {
  const site = useSite()
  const o = useQuery({ queryKey: ['developer-overview', app.id], queryFn: () => getOverview(app.id), refetchInterval: 15_000 })
  if (o.isPending) return <Loading />
  if (o.isError) return <ErrorText error={o.error} />
  const d = o.data
  const harnesses = Object.entries(d.connected_by_harness).sort((a, b) => b[1] - a[1])
  const quiet = d.users + d.conversation_workspaces === 0 && d.publishes_24h === 0
  return (
    <>
      {app.suspended_at && !app.deleted_at && (
        <Card>
          <p className="font-medium">Wakeshift suspended this app {when(app.suspended_at)}.</p>
          <p className="mt-1 text-ink-2">
            Its keys and publish URLs are refused, and new ones can't be made. Deliveries already made still reach their agents, and its data, grants and subscriptions are kept.
          </p>
          {app.suspended_reason && <p className="mt-2 whitespace-pre-wrap">Reason: {app.suspended_reason}</p>}
          <p className="mt-2 text-ink-3">To ask about it, write to {site.support_email}.</p>
        </Card>
      )}
      <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <Tile label="Connected now" value={d.connected}>
          {harnesses.length ? harnesses.map(([h, n]) => `${n} ${harnessName(h)}`).join(', ') : 'conversations'}
        </Tile>
        <Tile label="Wakes, 24 h" value={d.wakes_24h}>
          deliveries that reached a conversation
        </Tile>
        <Tile label="Publishes, 24 h" value={d.publishes_24h}>
          <TextLink to={appPath(app.id, 'activity')}>See activity</TextLink>
        </Tile>
        <Tile label="Failed, 24 h" value={d.failures_24h} tone={d.failures_24h ? 'danger' : undefined}>
          deliveries that gave up
        </Tile>
      </div>

      <Card>
        <Facts
          items={[
            ['Status', app.deleted_at ? <Badge tone="caution">deleted</Badge> : app.suspended_at ? <Badge tone="caution">suspended</Badge> : <><Badge tone="sage">active</Badge><span className="ml-2 text-ink-3">acts for its own users</span></>],
            ['App ID', <code>{app.id}</code>],
            ['MCP URL', app.mcp_url || <span className="text-ink-3">none</span>],
            ['Keys', <TextLink to={appPath(app.id, 'keys')}>{count(app.keys, 'key in use', 'keys in use')}</TextLink>],
            ['Registered', when(app.created_at)],
            ['Last activity', ago(d.last_activity_at)],
          ]}
        />
      </Card>

      {quiet && !app.deleted_at && (
        <>
          <Empty title="Nothing has happened yet">
            Once a conversation subscribes through one of your tools, its users, subscriptions and wake-ups show up here.
          </Empty>
          <TryIt app={app} />
        </>
      )}

      <div className="grid gap-5 lg:grid-cols-2">
        <Card title="Who's here">
          <WhosHere app={app} d={d} />
        </Card>
        <Card title="Connector versions">
          {d.connectors.length === 0 ? (
            <p className="text-sm text-ink-3">No conversation used Wakeshift's hooks or plugins in the last 7 days.</p>
          ) : (
            <Rows>
              {d.connectors.map((c) => (
                <Row key={`${c.harness} ${c.connector} ${c.version}`}>
                  <span>
                    {harnessName(c.harness)} <code>{c.connector}/{c.version === '0' ? 'unknown' : c.version}</code>
                    {c.outdated && <> <Badge tone="caution">out of date</Badge></>}
                  </span>
                  <Meta>{count(c.conversations, 'conversation', 'conversations')} in the last 7 days</Meta>
                </Row>
              ))}
            </Rows>
          )}
        </Card>
        <Card title="Recent failures">
          {d.recent_failures.length === 0 ? (
            <p className="text-sm text-ink-3">None in the last 24 hours.</p>
          ) : (
            <Rows>
              {d.recent_failures.map((f, i) => (
                <Row key={i}>
                  <span className="wrap-break-word">{f.text}</span>
                  <Meta>
                    {ago(f.at)}
                    {f.user && <> · <code>{f.user}</code></>}
                    {f.error && <> · {f.error}</>}
                  </Meta>
                </Row>
              ))}
            </Rows>
          )}
        </Card>
      </div>
      <ScannedNotice s={d.scanned} appID={app.id} />
      {!quiet && !app.deleted_at && <TryIt app={app} />}
    </>
  )
}
