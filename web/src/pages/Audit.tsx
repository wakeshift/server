import { useInfiniteQuery } from '@tanstack/react-query'
import { listAudit, type AuditEntry } from '../api/client'
import { Button, Card, Empty, ErrorText, Loading, Meta, PageHeader, plural, Row, Rows, TextLink, when } from '../components/ui'

const reasons: Record<string, string> = {
  code_reused: 'an authorization code was used twice',
  refresh_token_reused: 'a refresh token was used twice',
  client_revoked: 'its client signed out',
}

const str = (v: unknown) => (typeof v === 'string' ? v : '')
const num = (v: unknown) => (typeof v === 'number' ? v : 0)

// describe says what an entry means, in a sentence.
function describe(e: AuditEntry): string {
  const d = e.detail
  const app = e.app_id ?? 'an app'
  const agent = e.agent_id ?? 'an agent'
  const forWhom = d.kind === 'workspace' ? 'the workspace' : agent
  switch (e.action) {
    case 'grant.created':
      return d.kind === 'workspace' ? `Allowed ${app} to publish into the workspace` : `Allowed ${app} to act for ${forWhom}`
    case 'grant.revoked':
    case 'grant.expired': {
      const undid = [
        num(d.subscriptions_canceled) > 0 && plural(num(d.subscriptions_canceled), 'subscription') + ' canceled',
        num(d.refresh_tokens_revoked) > 0 && plural(num(d.refresh_tokens_revoked), 'sign-in') + ' ended',
      ].filter(Boolean)
      const what = e.action === 'grant.expired' ? `${app}'s consent for ${forWhom} expired` : `Revoked ${app}'s consent for ${forWhom}`
      return undid.length ? `${what} (${undid.join(', ')})` : what
    }
    case 'signin.authorized':
      return `Signed ${str(d.client_name) || 'a client'} in to ${app} as ${agent}${d.verified ? '' : ' (unverified client)'}, returning to ${str(d.redirect_host)}`
    case 'signin.revoked':
      return `Ended ${agent}'s sign-in: ${reasons[str(d.reason)] ?? str(d.reason)}`
    case 'app.connected':
      return `Started connecting ${app} to the workspace`
    case 'stream_links.revoked':
      return `Revoked ${agent}'s stream links (${num(d.revoked)})`
    case 'stream_link.renewed':
      return `Renewed ${agent}'s expired stream link after it went unused (it works once more, then only the new one)`
    case 'subscription.created':
      return `Subscribed ${agent} to ${str(d.event_type)}`
    case 'subscription.canceled':
      return `Canceled ${agent}'s subscription to ${str(d.event_type)}`
    case 'app.key_registered':
      return d.new_app
        ? `Created ${app} with key ${str(d.key_id)} from wakeshift-cli`
        : `Added key ${str(d.key_id)} to ${app} from wakeshift-cli`
    case 'agent_webhook.registered':
      if (d.kind !== 'cursor-automations') return `Registered ${agent}'s webhook`
      return d.joined
        ? `${app} joined ${agent}'s Grok Bot webhook (confirmed by the bot's key)`
        : `Turned on wake-ups through ${agent}'s Grok Bot webhook for ${app}${d.replaced ? ' (replacing the earlier one)' : ''}`
    case 'agent_webhook.unregistered':
      return d.reason === 'user' ? `Stopped wake-ups through ${agent}'s webhook for ${app}` : `Removed ${agent}'s webhook for ${app}`
    case 'install.elicit': {
      const client = d.harness === 'codex' ? 'Codex' : str(d.harness) || 'their client'
      return d.outcome === 'setup'
        ? `In ${client}'s dialog, chose to set up Wakeshift's plugin themselves (reported by ${app})`
        : `In ${client}'s dialog, chose not to set up notifications now (reported by ${app})`
    }
    case 'bot_key.registered':
      return `${agent}'s Grok Bot proved its Wakeshift key (${str(d.fingerprint)})`
    case 'bot_key.replaced':
      return `Replaced ${agent}'s Grok Bot key (${str(d.old)} with ${str(d.new)})`
    case 'account.created':
      return 'Created your account'
    // Devices (a retired desktop app) are gone; older entries still read.
    case 'device.connected':
      return `Connected ${str(d.name) || 'a device'}`
    case 'device.revoked':
      return 'Revoked a device'
    default:
      return e.action
  }
}

function actor(e: AuditEntry): string {
  if (e.actor === 'hub' || e.actor === 'admin') return 'Wakeshift'
  if (e.actor_email) return e.actor_email
  return e.actor
}

export default function Audit() {
  const log = useInfiniteQuery({
    queryKey: ['audit'],
    queryFn: ({ pageParam }) => listAudit(pageParam),
    initialPageParam: 0,
    getNextPageParam: (last: AuditEntry[]) => (last.length === 50 ? last[last.length - 1].id : undefined),
  })
  const entries = log.data?.pages.flat() ?? []

  return (
    <div className="space-y-8">
      <PageHeader title="Audit">
        Every consent given in this workspace, every agent sign-in, and everything revoked, newest first. What happens to
        an app you build is in its own log, on the app's page (<TextLink to="/">your apps</TextLink>).
      </PageHeader>
      {log.isPending && <Loading />}
      {log.isSuccess && entries.length === 0 && <Empty title="Nothing yet">Consents, sign-ins and revocations in this workspace will be listed here.</Empty>}
      {entries.length > 0 && (
        <Card>
          <Rows>
            {entries.map((e) => (
              <Row key={e.id}>
                <span>{describe(e)}</span>
                <Meta>
                  <span title={e.action}>
                    {when(e.at)} · by {actor(e)}
                  </span>
                </Meta>
                {e.shown && <Shown shown={e.shown} />}
              </Row>
            ))}
          </Rows>
          {log.hasNextPage && (
            <Button className="mt-5" size="sm" variant="secondary" disabled={log.isFetchingNextPage} onClick={() => log.fetchNextPage()}>
              Older
            </Button>
          )}
        </Card>
      )}
      <ErrorText error={log.error} />
    </div>
  )
}

// Shown is what the consent screen said when the user allowed it.
function Shown({ shown }: { shown: NonNullable<AuditEntry['shown']> }) {
  const risks = shown.risks ?? []
  return (
    <details className="mt-1 text-xs">
      <summary className="cursor-pointer text-ink-3 hover:text-ink">What the consent screen showed</summary>
      <ul className="mt-1 space-y-1 pl-4">
        {risks.map((r) => (
          <li key={r.title}>
            <span className={r.level === 'caution' ? 'font-medium text-caution' : 'font-medium'}>{r.title}</span>
            {r.detail && <span className="text-ink-3">: {r.detail}</span>}
          </li>
        ))}
        {risks.length === 0 && <li className="text-ink-3">No risks were listed.</li>}
        <li className="text-ink-3">
          Dry run: {plural(shown.dry_run.events, 'event')} in the last {shown.dry_run.days} days would have woken it.
        </li>
      </ul>
    </details>
  )
}
