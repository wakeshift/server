import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { listEvents, sendTestEvent } from '../api/client'
import { Badge, Button, Card, Empty, ErrorText, Input, Loading, PageHeader, thClass, when } from '../components/ui'
import { parsePairs } from '../lib/pairs'

export default function Events() {
  const qc = useQueryClient()
  const events = useQuery({ queryKey: ['events'], queryFn: listEvents, refetchInterval: 5_000 })
  const [type, setType] = useState('test.ping')
  const [subject, setSubject] = useState('')
  const [attrs, setAttrs] = useState('')
  const send = useMutation({
    mutationFn: async () => sendTestEvent({ type, subject, attrs: parsePairs(attrs) }),
    onSuccess: () => {
      setSubject('')
      qc.invalidateQueries({ queryKey: ['events'] })
    },
  })

  return (
    <div className="space-y-8">
      <PageHeader title="Events">
        What happened in this workspace, newest first. Send a test event to wake an agent subscribed to <code>test.*</code>{' '}
        without connecting anything.
      </PageHeader>
      <Card title="Send a test event">
        <form
          className="grid items-end gap-3 md:grid-cols-[10rem_1fr_12rem_auto]"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            send.mutate()
          }}
        >
          <Input label="Type" value={type} onChange={(e) => setType(e.target.value)} />
          <Input label="Subject" placeholder="The build is broken" value={subject} onChange={(e) => setSubject(e.target.value)} />
          <Input label="Attributes" placeholder="channel=dev" value={attrs} onChange={(e) => setAttrs(e.target.value)} />
          <Button type="submit" disabled={!type || send.isPending}>
            Send
          </Button>
        </form>
        <ErrorText error={send.error} />
      </Card>

      {events.isPending && <Loading />}
      {events.data?.length === 0 && <Empty title="No events yet">Events from apps, and the test events you send, show up here.</Empty>}
      {!!events.data?.length && (
        <Card title="Most recent">
          <div className="-mx-5 overflow-x-auto px-5 sm:-mx-6 sm:px-6">
            <table className="w-full min-w-136 text-left text-sm">
              <thead>
                <tr>
                  <th className={thClass}>#</th>
                  <th className={thClass}>Type</th>
                  <th className={thClass}>Subject</th>
                  <th className={thClass}>When</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-rule border-t border-rule">
                {events.data.map((e) => (
                  <tr key={e.id} className="align-top">
                    <td className="py-2.5 pr-4 font-mono text-xs text-ink-3">{e.seq}</td>
                    <td className="py-2.5 pr-4">
                      <code>{e.type}</code>
                      <div className="text-xs text-ink-3">
                        from {e.from.kind} {e.from.id}
                        {e.trust === 'external' && (
                          <span className="ml-2">
                            <Badge tone="caution">outside input</Badge>
                          </span>
                        )}
                      </div>
                    </td>
                    <td className="py-2.5 pr-4">{e.subject}</td>
                    <td className="whitespace-nowrap py-2.5 text-xs text-ink-3">{when(e.occurred_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
      <ErrorText error={events.error} />
    </div>
  )
}
