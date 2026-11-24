import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { forgetUser, listUsers, type OwnedApp } from '../../api/developer'
import { can } from '../../api/orgs'
import { appPath } from '../../components/Layout'
import { Badge, Button, ButtonLink, Card, Empty, ErrorText, Loading, Meta, Row, Rows, when } from '../../components/ui'
import { ago, AppPage, count, More, UserLabel } from './common'

export default function AppUsers() {
  return (
    <AppPage
      title="Users"
      intro="Wakeshift keeps a workspace for each of your users (the ID you pass as call.user), and one for each conversation that calls without a user. Newest first."
    >
      {(app) => <Body app={app} />}
    </AppPage>
  )
}

function Body({ app }: { app: OwnedApp }) {
  const qc = useQueryClient()
  const list = useInfiniteQuery({
    queryKey: ['developer-users', app.id],
    queryFn: ({ pageParam }) => listUsers(app.id, pageParam),
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
  })
  const forget = useMutation({
    mutationFn: (user: string) => forgetUser(app.id, user),
    onSuccess: () => {
      for (const k of ['developer-users', 'developer-subscriptions', 'developer-overview', 'developer-activity']) qc.invalidateQueries({ queryKey: [k, app.id] })
    },
  })
  const rows = list.data?.pages.flatMap((p) => p.users) ?? []

  return (
    <Card>
      {list.isPending && <Loading />}
      {list.isSuccess && rows.length === 0 && (
        <Empty title="No users yet">
          A user appears the first time a conversation subscribes for them. Return your own ID for the person as <code>user</code> from{' '}
          <code>attach</code>'s <code>identify</code>, so their conversations share one workspace and personal events can reach them.
        </Empty>
      )}
      {rows.length > 0 && (
        <Rows>
          {rows.map((u, i) => (
            <Row
              key={u.user ?? `conversation-${i}-${u.created_at}`}
              actions={
                u.user && (
                  <>
                    <ButtonLink size="sm" variant="secondary" to={`${appPath(app.id, 'activity')}?user=${encodeURIComponent(u.user)}`}>
                      Activity
                    </ButtonLink>
                    {can.forgetUser(app.role) && (
                      <Button
                        size="sm"
                        variant="danger"
                        disabled={forget.isPending && forget.variables === u.user}
                        onClick={() =>
                          window.confirm(
                            `Forget ${u.user}? Their workspace is deleted with everything in it: conversations, subscriptions, deliveries and stream links. Their open streams close. This can't be undone.`,
                          ) && forget.mutate(u.user!)
                        }
                      >
                        Forget
                      </Button>
                    )}
                  </>
                )
              }
            >
              <UserLabel user={u.user} harness={u.harness} /> {u.connected > 0 && <Badge tone="sage">{u.connected} connected</Badge>}
              <Meta>
                {count(u.conversations, 'conversation', 'conversations')} · {count(u.subscriptions, 'subscription', 'subscriptions')} · last
                active {ago(u.last_activity_at)} · since {when(u.created_at)}
              </Meta>
            </Row>
          ))}
        </Rows>
      )}
      {forget.data && !forget.data.forgotten && <p className="mt-3 text-sm text-ink-3">There was nothing to forget.</p>}
      <ErrorText error={list.error ?? forget.error} />
      <More query={list} />
    </Card>
  )
}
