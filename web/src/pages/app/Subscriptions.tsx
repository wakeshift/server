import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { endSubscription, listSubscriptions, type OwnedApp } from '../../api/developer'
import { Badge, Button, Card, Empty, ErrorText, Loading, Meta, Row, Rows, when } from '../../components/ui'
import { AppPage, ConversationLine, More, ScannedNotice, until, UserLabel } from './common'

export default function AppSubscriptions() {
  return (
    <AppPage
      title="Subscriptions"
      intro="Conversations and clients subscribed to your events now, newest first. Ending one stops its events, as event.end does."
    >
      {(app) => <Body app={app} />}
    </AppPage>
  )
}

const argumentsText = (a: Record<string, unknown>) =>
  Object.entries(a)
    .map(([k, v]) => `${k} = ${JSON.stringify(v)}`)
    .join(', ')

function Body({ app }: { app: OwnedApp }) {
  const qc = useQueryClient()
  const list = useInfiniteQuery({
    queryKey: ['developer-subscriptions', app.id],
    queryFn: ({ pageParam }) => listSubscriptions(app.id, pageParam),
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
  })
  const end = useMutation({
    mutationFn: (sub: string) => endSubscription(app.id, sub),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['developer-subscriptions', app.id] })
      qc.invalidateQueries({ queryKey: ['developer-overview', app.id] })
    },
  })
  const rows = list.data?.pages.flatMap((p) => p.subscriptions) ?? []

  return (
    <>
      <ScannedNotice s={list.data?.pages[0]?.scanned} appID={app.id} />
      <Card>
        {list.isPending && <Loading />}
        {list.isSuccess && rows.length === 0 && (
          <Empty title="No subscriptions">
            Register one of your MCP tools with <code>attached.registerTool</code>, with the event in its <code>subscribes</code>, and
            call <code>call.subscribe(event, args)</code> in it, or let agents use{' '}
            <code>subscribe_events</code>. A subscription shows up here until it
            ends: unsubscribed, ended by you, or expired after its time without a wake-up (30 days unless you set a ttl).
          </Empty>
        )}
        {rows.length > 0 && (
          <Rows>
            {rows.map((s) => (
              <Row
                key={s.id}
                actions={
                  !app.deleted_at && (
                    <Button
                      size="sm"
                      variant="danger"
                      disabled={end.isPending && end.variables === s.id}
                      onClick={() => window.confirm(`End this subscription to ${s.event}? Its events stop reaching it.`) && end.mutate(s.id)}
                    >
                      End
                    </Button>
                  )
                }
              >
                <code className="font-semibold">{s.event}</code> {s.once && <Badge tone="accent">once</Badge>}{' '}
                {(s.destination === 'webhook' || s.destination === 'poll' || s.destination === 'push') && <Badge>native</Badge>}
                {Object.keys(s.arguments ?? {}).length > 0 && (
                  <Meta>
                    Arguments: <code>{argumentsText(s.arguments)}</code>
                  </Meta>
                )}
                {s.reason && <Meta>{s.reason}</Meta>}
                <Meta>
                  <UserLabel user={s.user} />
                  {s.destination === 'conversation' && (
                    <>
                      {' '}
                      · <ConversationLine c={s.conversation} />
                    </>
                  )}
                </Meta>
                <Meta>
                  Since {when(s.created_at)} · ends {until(s.expires_at)} without activity · <code>{s.id}</code>
                </Meta>
              </Row>
            ))}
          </Rows>
        )}
        <ErrorText error={list.error ?? end.error} />
        <More query={list} />
      </Card>
    </>
  )
}
