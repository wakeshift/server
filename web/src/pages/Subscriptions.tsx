import { useMemo, useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  cancelSubscription,
  createSubscription,
  listAgents,
  listEventSubscriptions,
  listSubscriptions,
  revokeEventSubscription,
  type EventSubscription,
} from '../api/client'
import { Badge, Button, Card, Empty, ErrorText, Hint, Input, Loading, Meta, Modal, PageHeader, Row, Rows, Select, TextLink, when } from '../components/ui'
import { parsePairs } from '../lib/pairs'

export default function Subscriptions() {
  const qc = useQueryClient()
  const subs = useQuery({ queryKey: ['subscriptions'], queryFn: listSubscriptions })
  const agents = useQuery({ queryKey: ['agents'], queryFn: listAgents })
  const [agent, setAgent] = useState('')
  const [type, setType] = useState('')
  const [filters, setFilters] = useState('')
  const [reason, setReason] = useState('')
  const [adding, setAdding] = useState(false)

  // The selected agent, or the first one: what the form will submit.
  const chosen = agents.data?.some((a) => a.id === agent) ? agent : (agents.data?.[0]?.id ?? '')
  const parsed = useMemo(() => {
    try {
      return { filters: parsePairs(filters), error: null }
    } catch (e) {
      return { filters: {}, error: e as Error }
    }
  }, [filters])

  const refresh = () => qc.invalidateQueries({ queryKey: ['subscriptions'] })
  const create = useMutation({
    mutationFn: () => createSubscription({ agent_id: chosen, event_type: type, filters: parsed.filters, reason }),
    onSuccess: () => {
      setType('')
      setFilters('')
      setReason('')
      setAdding(false)
      refresh()
    },
  })
  const cancel = useMutation({ mutationFn: cancelSubscription, onSuccess: refresh })

  return (
    <div className="space-y-8">
      <PageHeader title="Subscriptions">
        What wakes each agent here. Agents and apps make most of these themselves; add one by hand to try it.
        <div className="mt-5">
          <Button onClick={() => setAdding(true)}>Subscribe agent</Button>
        </div>
      </PageHeader>

      {subs.isPending && <Loading />}
      {!!subs.data?.length && (
        <Card title="All subscriptions">
          <Rows>
            {subs.data.map((s) => (
              <Row
                key={s.id}
                muted={s.status !== 'active'}
                actions={
                  s.status === 'active' && (
                    <Button
                      size="sm"
                      variant="danger"
                      onClick={() => {
                        if (window.confirm(`Cancel ${s.agent_id}'s subscription to ${s.event_type}?`)) cancel.mutate(s.id)
                      }}
                      disabled={cancel.isPending && cancel.variables === s.id}
                    >
                      Cancel
                    </Button>
                  )
                }
              >
                <span className="font-medium">{s.agent_id}</span> <span className="text-ink-3">←</span> <code>{s.event_type}</code>{' '}
                {s.status !== 'active' && <Badge>canceled</Badge>}
                {Object.entries(s.filters).map(([k, v]) => (
                  <span key={k} className="ml-2 rounded-sm bg-paper-2 px-1.5 py-0.5 font-mono text-xs">
                    {k}={v}
                  </span>
                ))}
                <Meta>
                  Made {when(s.created_at)} by {s.created_by}
                  {s.reason && ` · ${s.reason}`}
                </Meta>
              </Row>
            ))}
          </Rows>
          <ErrorText error={cancel.error} />
        </Card>
      )}
      <ErrorText error={subs.error} />

      <EventSubscriptions />

      <Modal open={adding} title="Subscribe an agent" onClose={() => setAdding(false)}>
        {agents.isPending && <Loading />}
        <ErrorText error={agents.error} />
        {agents.data?.length === 0 && (
          <p className="text-sm text-ink-2">
            <TextLink to="/agents">Add an agent</TextLink> first.
          </p>
        )}
        {!!agents.data?.length && (
          <form
            className="space-y-4"
            onSubmit={(e: FormEvent) => {
              e.preventDefault()
              create.mutate()
            }}
          >
            <div className="flex flex-col gap-4">
              <Select label="Agent" value={chosen} onChange={(e) => setAgent(e.target.value)}>
                {agents.data.map((a) => (
                  <option key={a.id}>{a.id}</option>
                ))}
              </Select>
              <Input label="Event type (or a pattern ending in .*)" placeholder="test.*" value={type} onChange={(e) => setType(e.target.value)} />
              <Input label="Filters (key=value, comma-separated)" placeholder="channel=C0123ABCD" value={filters} onChange={(e) => setFilters(e.target.value)} />
              <Input label="Why (optional)" value={reason} onChange={(e) => setReason(e.target.value)} />
            </div>
            {parsed.error ? (
              <p className="text-sm text-danger">{parsed.error.message}</p>
            ) : (
              Object.keys(parsed.filters).length > 0 && (
                <Hint>
                  Only events with{' '}
                  {Object.entries(parsed.filters)
                    .map(([k, v]) => `${k} = ${v}`)
                    .join(' and ')}
                </Hint>
              )
            )}
            <ErrorText error={create.error} />
            <Button type="submit" disabled={!type || !chosen || !!parsed.error || create.isPending}>
              Subscribe
            </Button>
          </form>
        )}
      </Modal>
      {subs.data?.length === 0 && <Empty title="No subscriptions yet">Use Subscribe agent to subscribe one to <code>test.*</code>, then send a test event.</Empty>}
    </div>
  )
}

// What each app may wake agents here for, by event and arguments: one row
// per subscription, revocable one at a time (delivery stops at once).
function EventSubscriptions() {
  const qc = useQueryClient()
  const list = useQuery({ queryKey: ['event-subscriptions'], queryFn: listEventSubscriptions })
  const revoke = useMutation({
    mutationFn: revokeEventSubscription,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['event-subscriptions'] }),
  })
  if (!list.data?.length) return <ErrorText error={list.error} />
  const where = (s: EventSubscription) =>
    s.destination === 'webhook'
      ? `a webhook at ${s.webhook_host ?? 'an MCP client'}`
      : s.destination === 'poll'
        ? `an MCP client that polls (${s.agent_id})`
        : s.destination === 'push'
          ? `an MCP client that holds a stream (${s.agent_id})`
          : `a conversation of ${s.agent_id}`
  return (
    <Card title="App events">
      <Rows>
        {list.data.map((s) => (
          <Row
            key={s.id}
            muted={!!s.ended_at}
            actions={
              !s.ended_at && (
                <Button
                  size="sm"
                  variant="danger"
                  onClick={() => {
                    if (window.confirm(`Stop ${s.source_name} waking ${s.agent_id} for ${s.event}?`)) revoke.mutate(s.id)
                  }}
                  disabled={revoke.isPending && revoke.variables === s.id}
                >
                  Revoke
                </Button>
              )
            }
          >
            <span className="font-medium">{s.source_name}</span> <span className="text-ink-3">→</span> <code>{s.event}</code>{' '}
            {s.ended_at && <Badge>{s.ended_reason ?? 'ended'}</Badge>}
            {Object.entries(s.arguments).map(([k, v]) => (
              <span key={k} className="ml-2 rounded-sm bg-paper-2 px-1.5 py-0.5 font-mono text-xs">
                {k}={typeof v === 'string' ? v : JSON.stringify(v)}
              </span>
            ))}
            <Meta>
              {s.reason && `${s.reason} · `}
              {s.description && `${s.description} · `}Goes to {where(s)} · since {when(s.created_at)}
            </Meta>
          </Row>
        ))}
      </Rows>
      <ErrorText error={revoke.error ?? list.error} />
    </Card>
  )
}
