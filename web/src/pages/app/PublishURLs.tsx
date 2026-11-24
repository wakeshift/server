import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  createEventPublishURL,
  listPublishURLs,
  removePublishURLSecret,
  revokePublishURL,
  setPublishURLSecret,
  type OwnedApp,
  type PublishEvent,
  type PublishURL,
} from '../../api/developer'
import { Badge, Button, Card, Empty, ErrorText, Hint, Input, linkClass, Loading, Meta, Row, Rows, Secret, Select, when } from '../../components/ui'
import { useSite } from '../../lib/site'
import { ago, AppPage, count } from './common'

export default function AppPublishURLs() {
  return (
    <AppPage
      title="Publish URLs"
      intro="A publish URL emits one of your events: point a webhook at it (GitHub, Stripe, Svix, a CI system) and each POST wakes the conversations subscribed to that event with those arguments. Anyone with the URL can send that event, so it's shown once."
    >
      {(app) => <Body app={app} />}
    </AppPage>
  )
}

// label is how a publish URL is named on the page: its event, arguments and
// user. One made for a topic, before publish URLs moved onto events, no
// longer works.
function label(u: { event?: string; arguments?: Record<string, unknown>; user?: string; topic: string }): string {
  if (!u.event) return `${u.topic} (retired)`
  const args = u.arguments ? Object.entries(u.arguments).map(([k, v]) => `${k}=${typeof v === 'string' ? v : JSON.stringify(v)}`) : []
  return `${u.event}${args.length ? ` (${args.join(', ')})` : ''}${u.user ? ` → ${u.user}` : ''}`
}

// argValue turns what was typed into an argument's JSON value, as its
// schema types it.
function argValue(type: string, raw: string): unknown {
  switch (type) {
    case 'number':
    case 'integer':
      return Number(raw)
    case 'boolean':
      return raw === 'true'
    case 'string':
      return raw
  }
  try {
    return JSON.parse(raw)
  } catch {
    return raw
  }
}

function Body({ app }: { app: OwnedApp }) {
  const site = useSite()
  const qc = useQueryClient()
  const list = useQuery({ queryKey: ['developer-publish-urls', app.id], queryFn: () => listPublishURLs(app.id) })
  const events = list.data?.events ?? []
  const [eventName, setEventName] = useState('')
  const [args, setArgs] = useState<Record<string, string>>({})
  const [user, setUser] = useState('')
  const [secret, setSecret] = useState('')
  // The URL whose signing secret is being set, and what's typed.
  const [editing, setEditing] = useState<string | null>(null)
  const [newSecret, setNewSecret] = useState('')
  const [shown, setShown] = useState<{ label: string; url: string } | null>(null)
  const event: PublishEvent | undefined = events.find((e) => e.name === eventName) ?? events[0]
  const refresh = () => qc.invalidateQueries({ queryKey: ['developer-publish-urls', app.id] })
  const createEvent = useMutation({
    mutationFn: () => {
      const values: Record<string, unknown> = {}
      for (const a of event!.arguments) {
        const raw = (args[a.name] ?? '').trim()
        if (raw !== '') values[a.name] = argValue(a.type, raw)
      }
      return createEventPublishURL(app.id, event!.name, values, event!.personal ? user.trim() : undefined, secret.trim() || undefined).then((u) => ({
        url: u.url,
        label: label({ event: event!.name, arguments: values, user: event!.personal ? user.trim() : undefined, topic: '' }),
      }))
    },
    onSuccess: (u) => {
      setShown(u)
      setArgs({})
      setUser('')
      setSecret('')
      refresh()
    },
  })
  const revoke = useMutation({ mutationFn: (id: string) => revokePublishURL(app.id, id), onSuccess: refresh })
  const saveSecret = useMutation({
    mutationFn: (id: string) => setPublishURLSecret(app.id, id, newSecret.trim()),
    onSuccess: () => {
      setEditing(null)
      setNewSecret('')
      refresh()
    },
  })
  const removeSecret = useMutation({ mutationFn: (id: string) => removePublishURLSecret(app.id, id), onSuccess: refresh })
  const urls: PublishURL[] = list.data?.urls ?? []
  const active = urls.filter((u) => !u.revoked_at)
  const revoked = urls.filter((u) => u.revoked_at)
  const ready = !!event && event.arguments.every((a) => !a.required || (args[a.name] ?? '').trim() !== '') && (!event.personal || user.trim() !== '')

  return (
    <>
      {shown && (
        <Card title={`Publish URL for ${shown.label}`} className="border-accent/40">
          <Secret id="publish-url" value={shown.url} onDone={() => setShown(null)}>
            This is the only time it's shown. Each POST to it sends <code>{shown.label}</code>: a JSON object becomes the event's payload.
          </Secret>
          <pre className="mt-4 overflow-x-auto rounded-lg border border-rule bg-paper p-3 text-xs">
            <code>{`curl -H 'Content-Type: application/json' -d '{"branch":"main"}' '${shown.url}'`}</code>
          </pre>
        </Card>
      )}

      {!app.deleted_at && (
        <Card title="Make a publish URL">
          {list.isSuccess && events.length === 0 && <Empty>Your app's manifest offers no events yet. Define one, and you can make a publish URL for it here.</Empty>}
          {event && (
            <form
              className="flex flex-col gap-3"
              onSubmit={(e: FormEvent) => {
                e.preventDefault()
                createEvent.mutate()
              }}
            >
              <Select
                label="Event"
                value={event.name}
                onChange={(e) => {
                  setEventName(e.target.value)
                  setArgs({})
                }}
              >
                {events.map((e) => (
                  <option key={e.name} value={e.name}>
                    {e.name}
                    {e.personal ? ' (personal)' : ''}
                  </option>
                ))}
              </Select>
              {event.description && <Hint>{event.description}</Hint>}
              {event.arguments.map((a) => (
                <Input
                  key={a.name}
                  label={`${a.name}${a.required ? '' : ' (optional)'}`}
                  placeholder={a.type || 'JSON'}
                  value={args[a.name] ?? ''}
                  onChange={(e) => setArgs({ ...args, [a.name]: e.target.value })}
                />
              ))}
              {event.personal && (
                <Input label="User (your app's ID for the person it's for)" placeholder="user-123" value={user} onChange={(e) => setUser(e.target.value)} />
              )}
              <Input
                label="Signing secret (optional)"
                type="password"
                autoComplete="off"
                placeholder="whsec_…"
                value={secret}
                onChange={(e) => setSecret(e.target.value)}
              />
              <Hint>
                From the sender, such as a Svix endpoint's signing secret. With one set, only posts signed with it (Standard Webhooks) are
                accepted. You can add, change or remove it later.
              </Hint>
              <div>
                <Button type="submit" disabled={!ready || createEvent.isPending}>
                  Make URL
                </Button>
              </div>
            </form>
          )}
          <div className="mt-2">
            <Hint>
              Your app can make them too, with <code>event.publishUrl(args)</code>. See{' '}
              <a className={linkClass} href={`${site.docs}/publish-urls`}>
                publish URLs
              </a>{' '}
              in the docs.
            </Hint>
          </div>
          <ErrorText error={createEvent.error} />
        </Card>
      )}

      <Card title="In use">
        {list.isPending && <Loading />}
        {list.isSuccess && active.length === 0 && <Empty>No publish URLs. Make one above to wake agents from a webhook.</Empty>}
        {active.length > 0 && (
          <Rows>
            {active.map((u) => (
              <Row
                key={u.id}
                actions={
                  <>
                    <Button
                      size="sm"
                      variant="secondary"
                      onClick={() => {
                        setEditing(editing === u.id ? null : u.id)
                        setNewSecret('')
                      }}
                    >
                      {u.signing ? 'Rotate secret' : 'Set signing secret'}
                    </Button>
                    {u.signing && (
                      <Button
                        size="sm"
                        variant="secondary"
                        disabled={removeSecret.isPending && removeSecret.variables === u.id}
                        onClick={() =>
                          window.confirm(`Remove the signing secret for ${label(u)}? Unsigned posts are accepted from then on.`) && removeSecret.mutate(u.id)
                        }
                      >
                        Remove secret
                      </Button>
                    )}
                    <Button
                      size="sm"
                      variant="danger"
                      disabled={revoke.isPending && revoke.variables === u.id}
                      onClick={() => window.confirm(`Revoke this publish URL for ${label(u)}? Posts to it are refused from now on.`) && revoke.mutate(u.id)}
                    >
                      Revoke
                    </Button>
                  </>
                }
              >
                <code className="font-semibold">{label(u)}</code> {u.signing && <Badge tone="sage">signed</Badge>}
                <Meta>
                  Made {when(u.created_at)} · {u.last_used_at ? `last used ${ago(u.last_used_at)}` : 'never used'} ·{' '}
                  {count(u.publishes_24h, 'publish', 'publishes')} in 24 h
                </Meta>
                {u.signing && (
                  <Meta>
                    Signing secret <code>{u.signing.fingerprint}</code>, set {when(u.signing.set_at)}
                    {u.signing.previous_valid_until && <> · the previous one works until {when(u.signing.previous_valid_until)}</>}
                  </Meta>
                )}
                <Meta>
                  <code>{u.id}</code>
                </Meta>
                {editing === u.id && (
                  <form
                    className="mt-3 flex flex-col gap-2"
                    onSubmit={(e: FormEvent) => {
                      e.preventDefault()
                      saveSecret.mutate(u.id)
                    }}
                  >
                    <Input
                      label={u.signing ? 'New signing secret' : 'Signing secret'}
                      type="password"
                      autoComplete="off"
                      placeholder="whsec_…"
                      value={newSecret}
                      onChange={(e) => setNewSecret(e.target.value)}
                    />
                    {u.signing && <Hint>The current secret keeps working for 24 hours, so the sender can switch over.</Hint>}
                    <div>
                      <Button type="submit" size="sm" disabled={newSecret.trim() === '' || saveSecret.isPending}>
                        Save
                      </Button>
                    </div>
                    <ErrorText error={saveSecret.error} />
                  </form>
                )}
              </Row>
            ))}
          </Rows>
        )}
        <ErrorText error={list.error ?? revoke.error ?? removeSecret.error} />
      </Card>

      {revoked.length > 0 && (
        <Card title="Revoked">
          <Rows>
            {revoked.map((u) => (
              <Row key={u.id} muted>
                <code>{label(u)}</code> <Badge>revoked</Badge>
                <Meta>
                  Made {when(u.created_at)} · revoked {when(u.revoked_at!)}
                </Meta>
              </Row>
            ))}
          </Rows>
        </Card>
      )}
    </>
  )
}
