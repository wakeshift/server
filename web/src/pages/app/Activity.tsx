import { useEffect, useState, type FormEvent } from 'react'
import { useInfiniteQuery } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { listActivity, type Activity, type ActivityFilter, type OwnedApp } from '../../api/developer'
import { Badge, Button, Card, Empty, ErrorText, Input, Loading, Select, when } from '../../components/ui'
import { AppPage, ConversationLine, count, More, ScannedNotice, UserLabel } from './common'

export default function AppActivity() {
  return (
    <AppPage
      title="Activity"
      intro="What your app published, and each delivery to a conversation: woken, waiting, failed or canceled. Newest first. Publishes are kept for a day."
    >
      {(app) => <Body app={app} />}
    </AppPage>
  )
}

const statusTone: Record<string, 'neutral' | 'accent' | 'sage' | 'caution' | 'danger'> = {
  published: 'sage',
  unmatched: 'neutral',
  woke: 'sage',
  waiting: 'caution',
  failed: 'danger',
  canceled: 'neutral',
}

const statusLabel: Record<string, string> = {
  published: 'published',
  unmatched: 'no subscribers',
  woke: 'woke',
  waiting: 'waiting',
  failed: 'failed',
  canceled: 'canceled',
}

function Item({ e }: { e: Activity }) {
  const [open, setOpen] = useState(false)
  const what = e.kind === 'publish' ? 'Publish' : e.kind === 'notice' ? 'Notice from Wakeshift' : 'Delivery'
  return (
    <li className="py-3 text-sm">
      <button className="flex w-full flex-col gap-1 text-left sm:flex-row sm:items-start sm:gap-4" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="shrink-0 whitespace-nowrap font-mono text-xs text-ink-3 sm:w-44">{when(e.at)}</span>
        <span className="min-w-0 flex-1">
          <span className="flex flex-wrap items-center gap-2">
            <span className="font-medium">{what}</span>
            <Badge tone={statusTone[e.status]}>{statusLabel[e.status] ?? e.status}</Badge>
            {e.topics?.map((t) => (
              <code key={t} className="text-xs text-ink-2">
                {t}
              </code>
            ))}
          </span>
          <span className="mt-0.5 block truncate text-ink-2">{e.content_deleted ? <em>Content deleted after delivery</em> : e.text}</span>
          <span className="mt-0.5 block text-xs text-ink-3">
            {e.kind === 'publish' ? (
              <>
                {count(e.matched ?? 0, 'conversation', 'conversations')} matched{e.publish_url && ' · through a publish URL'}
              </>
            ) : (
              <>
                <UserLabel user={e.user} /> · <ConversationLine c={e.conversation} status={false} />
                {!!e.attempts && e.attempts > 1 && ` · ${e.attempts} attempts`}
                {e.connector && <> · via <code>{e.connector.split(' ')[0]}</code></>}
              </>
            )}
          </span>
        </span>
        <span aria-hidden className="flex shrink-0 items-center gap-1 self-end text-xs text-ink-3 sm:self-start">
          <span className="hidden sm:inline">{open ? 'Hide' : 'Show'}</span>
          <svg viewBox="0 0 12 12" width="12" height="12" className={`transition-transform ${open ? 'rotate-180' : ''}`}>
            <path d="M3 4.5 6 7.5 9 4.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
          </svg>
        </span>
      </button>
      {open && (
        <div className="mt-3 space-y-3 sm:ml-48">
          {e.error && <p className="text-sm text-danger">Error: {e.error}</p>}
          {e.content_deleted && <p className="text-sm text-ink-2">Its text and data were deleted once it was delivered, as the event asks.</p>}
          <div>
            <div className="mb-1 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-ink-3">Text, as sent</div>
            <pre className="overflow-x-auto whitespace-pre-wrap wrap-break-word rounded-lg border border-rule bg-paper p-3 text-xs">{e.text}</pre>
          </div>
          <div>
            <div className="mb-1 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-ink-3">Data</div>
            {e.data === undefined || e.data === null ? (
              <p className="text-xs text-ink-3">None</p>
            ) : (
              <pre className="overflow-x-auto rounded-lg border border-rule bg-paper p-3 text-xs">{JSON.stringify(e.data, null, 2)}</pre>
            )}
          </div>
          <p className="font-mono text-[11px] text-ink-3">{e.id}</p>
        </div>
      )}
    </li>
  )
}

function Body({ app }: { app: OwnedApp }) {
  // The filter is in the URL (?topic=&user=&status=), so a link can open
  // one user's activity and back goes back.
  const [params, setParams] = useSearchParams()
  const filter: ActivityFilter = { topic: params.get('topic') ?? undefined, user: params.get('user') ?? undefined, status: params.get('status') ?? undefined }
  const [draft, setDraft] = useState<ActivityFilter>(filter)
  // Back and Forward change the URL's filter: show it in the form too.
  useEffect(() => setDraft(filter), [params.toString()])
  const setFilter = (f: ActivityFilter) => {
    const next = new URLSearchParams()
    for (const [k, v] of Object.entries(f)) if (v) next.set(k, v)
    setParams(next)
  }
  const list = useInfiniteQuery({
    queryKey: ['developer-activity', app.id, filter],
    queryFn: ({ pageParam }) => listActivity(app.id, filter, pageParam),
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
  })
  const rows = list.data?.pages.flatMap((p) => p.activity) ?? []
  const filtered = !!(filter.topic || filter.user || filter.status)

  return (
    <>
      <Card>
        <form
          className="grid gap-3 sm:grid-cols-[1fr_1fr_12rem_auto] sm:items-end"
          onSubmit={(ev: FormEvent) => {
            ev.preventDefault()
            setFilter({ topic: draft.topic?.trim(), user: draft.user?.trim(), status: draft.status })
          }}
        >
          <Input label="Topic" placeholder="ev/ci.finished/acme/api" value={draft.topic ?? ''} onChange={(ev) => setDraft({ ...draft, topic: ev.target.value })} />
          <Input label="User" placeholder="Your ID for them" value={draft.user ?? ''} onChange={(ev) => setDraft({ ...draft, user: ev.target.value })} />
          <Select label="Status" value={draft.status ?? ''} onChange={(ev) => setDraft({ ...draft, status: ev.target.value })}>
            <option value="">Any</option>
            <option value="published">Published</option>
            <option value="unmatched">No subscribers</option>
            <option value="woke">Woke</option>
            <option value="waiting">Waiting</option>
            <option value="failed">Failed</option>
            <option value="canceled">Canceled</option>
          </Select>
          <div className="flex gap-2">
            <Button type="submit" variant="secondary">
              Filter
            </Button>
            {filtered && (
              <Button type="button" variant="secondary" onClick={() => (setDraft({}), setFilter({}))}>
                Clear
              </Button>
            )}
          </div>
        </form>
      </Card>
      <ScannedNotice s={list.data?.pages[0]?.scanned} appID={app.id} filter />
      <Card>
        {list.isPending && <Loading />}
        {list.isSuccess && rows.length === 0 &&
          (filtered ? (
            <Empty>Nothing matches. Clear the filter to see everything.</Empty>
          ) : (
            <Empty title="No activity yet">
              Emit one of your events (<code>event.emit</code>), or post to one of its publish URLs. Each emit, and each
              conversation it wakes, shows up here.
            </Empty>
          ))}
        {rows.length > 0 && (
          <ul className="-my-3 divide-y divide-rule">
            {rows.map((e) => (
              <Item key={e.id} e={e} />
            ))}
          </ul>
        )}
        <ErrorText error={list.error} />
        <More query={list} />
      </Card>
    </>
  )
}
