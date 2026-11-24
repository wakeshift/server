import { useInfiniteQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { listLog, logPageSize, type LogEntry, type OwnedApp } from '../../api/developer'
import { Card, Empty, ErrorText, Loading, Meta, plural, Row, Rows, Select, when } from '../../components/ui'
import { AppPage, More } from './common'

export default function AppLog() {
  return (
    <AppPage title="Log" intro="Changes to this app, newest first, and who made them: keys, settings, manifests, publish URLs, ended subscriptions and forgotten users. Publishes and deliveries are in Activity. Only you can see it.">
      {(app) => <Body app={app} />}
    </AppPage>
  )
}

const str = (v: unknown) => (typeof v === 'string' ? v : '')
const num = (v: unknown) => (typeof v === 'number' ? v : 0)

// describe says what an entry means, in a sentence.
function describe(e: LogEntry): string {
  const d = e.detail
  switch (e.action) {
    case 'app.created':
      return d.from_before_the_log ? 'Registered the app (before this log existed)' : `Registered the app with key ${e.key_id}`
    case 'app.key_created':
      return `Made key "${str(d.name)}" (${e.key_id})`
    case 'app.key_revoked':
      return `Revoked key "${str(d.name)}" (${e.key_id}). It stopped working at once.`
    case 'app.key_rotated':
      return `Rotated key "${str(d.name) || 'Default'}": ${e.key_id} replaced ${str(d.old_key_id)}, which stopped working at once.`
    case 'app.key_renamed':
      return `Renamed key ${e.key_id} from "${str(d.old_name)}" to "${str(d.name)}"`
    case 'app.key_first_used':
      return `Key ${e.key_id} signed its first request`
    case 'app.updated':
      return `Changed the app's ${(Array.isArray(d.changes) ? (d.changes as string[]) : []).map((c) => (c === 'mcp_url' ? 'MCP URL' : c)).join(' and ')}`
    case 'app.manifest_published':
      if (d.first) return 'Published its first manifest'
      return d.merged ? 'Updated its manifest from a running server (adds and changes only)' : 'Published a changed manifest'
    case 'app.deleted': {
      const keys = Array.isArray(d.keys_revoked) ? (d.keys_revoked as string[]) : []
      return keys.length ? `Deleted the app. ${plural(keys.length, 'key')} stopped working.` : 'Deleted the app.'
    }
    case 'app.grants_revoked': {
      const what = `Revoked ${plural(num(d.grants), 'grant')} in ${plural(num(d.workspaces), 'workspace')}`
      return d.complete ? what : `${what}, but not all of them. Delete the app again to finish.`
    }
    case 'app.approved_for_live':
      return 'Approved for live keys'
    case 'app.user_forgotten':
      if (d.reason === 'idle')
        return `Forgot a workspace unused for ${plural(num(d.idle_days), 'day')}: deleted ${plural(num(d.conversations), 'conversation')} and ${plural(num(d.subscriptions_ended), 'subscription')}`
      return `Forgot a user: deleted ${plural(num(d.conversations), 'conversation')} and ${plural(num(d.subscriptions_ended), 'subscription')}`
    case 'app.subscription_ended':
      return `Ended a subscription to ${str(d.event ?? d.topic)}`
    case 'app.publish_url_created':
      return `Made a publish URL for ${str(d.event ?? d.topic)}`
    case 'app.publish_url_revoked':
      return `Revoked a publish URL for ${str(d.event ?? d.topic)}`
    case 'app.publish_url_secret_set':
      return `Set the signing secret ${str(d.fingerprint)} on a publish URL for ${str(d.event)}`
    case 'app.publish_url_secret_rotated':
      return `Rotated a publish URL's signing secret to ${str(d.fingerprint)} for ${str(d.event)}; the previous one works for 24 hours`
    case 'app.publish_url_secret_removed':
      return `Removed the signing secret from a publish URL for ${str(d.event)}`
    default:
      return e.action
  }
}

function actor(e: LogEntry): string {
  if (e.actor === 'hub' || e.actor === 'admin') return 'Wakeshift'
  if (e.actor.startsWith('app:')) return 'your app'
  return e.actor_email ?? e.actor
}

const filters: [string, string][] = [
  ['', 'Everything'],
  ['app.key_', 'Keys'],
  ['app.publish_url_', 'Publish URLs'],
  ['app.subscription_', 'Subscriptions'],
  ['app.user_', 'Users'],
  ['app.manifest_', 'Manifests'],
]

function Body({ app }: { app: OwnedApp }) {
  const [params, setParams] = useSearchParams()
  const action = params.get('action') ?? ''
  const log = useInfiniteQuery({
    queryKey: ['developer-app-log', app.id, action],
    queryFn: ({ pageParam }) => listLog(app.id, pageParam, action),
    initialPageParam: 0,
    getNextPageParam: (last: LogEntry[]) => (last.length === logPageSize ? last[last.length - 1].id : undefined),
  })
  const entries = log.data?.pages.flat() ?? []

  return (
    <Card>
      <div className="mb-5 max-w-[16rem]">
        <Select label="Show" value={action} onChange={(e) => setParams(e.target.value ? { action: e.target.value } : {})}>
          {filters.map(([v, label]) => (
            <option key={v} value={v}>
              {label}
            </option>
          ))}
        </Select>
      </div>
      {log.isPending && <Loading />}
      {log.isSuccess && entries.length === 0 && <Empty>{action ? 'Nothing of this kind yet.' : 'Nothing yet.'}</Empty>}
      {entries.length > 0 && (
        <Rows>
          {entries.map((e) => {
            const changes = e.action === 'app.manifest_published' && Array.isArray(e.detail.changes) ? (e.detail.changes as string[]) : []
            return (
              <Row key={e.id}>
                <span className="wrap-break-word">{describe(e)}</span>
                <Meta>
                  <span title={e.action}>
                    {when(e.at)} · by {actor(e)}
                  </span>
                </Meta>
                {changes.length > 0 && (
                  <ul className="mt-1 space-y-0.5 pl-4 text-xs">
                    {changes.map((c) => (
                      <li key={c}>
                        <code>{c}</code>
                      </li>
                    ))}
                  </ul>
                )}
              </Row>
            )
          })}
        </Rows>
      )}
      <ErrorText error={log.error} />
      <More query={log} />
    </Card>
  )
}
