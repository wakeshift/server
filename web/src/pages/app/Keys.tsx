import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createKey, listKeys, registerKey, renameKey, revokeKey, rotateKey, type AppKey, type NewKey, type OwnedApp } from '../../api/developer'
import { Badge, Button, Card, Empty, ErrorText, Input, Loading, Meta, Notice, Row, Rows, Secret, when } from '../../components/ui'
import { KeySDKNote } from '../../components/KeySDKNote'
import { confirmError, Done, invalidLink, KeyCode, useCLILink, useKeyCode } from '../../components/CLIConfirm'
import { forgetLink, type CLILink } from '../../lib/cliLink'
import { ago, AppPage } from './common'

export default function AppKeys() {
  return (
    <AppPage
      title="Keys"
      intro={
        <>
          Your app signs its requests with a key; Wakeshift keeps only the public half, so a key is shown once, when it's made.
          Give each place that runs your app its own key, so you can revoke one without touching the others.
        </>
      }
    >
      {(app) => <Body app={app} />}
    </AppPage>
  )
}

function Body({ app }: { app: OwnedApp }) {
  const qc = useQueryClient()
  // Opened from `wakeshift-cli keys new`'s link (#pk=…).
  const link = useCLILink()
  const keys = useQuery({ queryKey: ['developer-keys', app.id], queryFn: () => listKeys(app.id) })
  const [name, setName] = useState('')
  const [shown, setShown] = useState<NewKey | null>(null)
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['developer-keys', app.id] })
    qc.invalidateQueries({ queryKey: ['developer-app', app.id] })
    qc.invalidateQueries({ queryKey: ['developer-apps'] })
  }
  const show = (k: NewKey) => {
    setShown(k)
    refresh()
  }
  const create = useMutation({ mutationFn: () => createKey(app.id, name), onSuccess: (k) => (setName(''), show(k)) })
  const rotate = useMutation({ mutationFn: (key: string) => rotateKey(app.id, key), onSuccess: show })
  const revoke = useMutation({ mutationFn: (key: string) => revokeKey(app.id, key), onSuccess: refresh })
  // Renaming, inline: the key being renamed and its new name.
  const [editing, setEditing] = useState<{ key: string; name: string } | null>(null)
  const rename = useMutation({
    mutationFn: (e: { key: string; name: string }) => renameKey(app.id, e.key, e.name),
    onSuccess: () => (setEditing(null), refresh()),
  })
  const active = keys.data?.keys.filter((k) => !k.revoked_at) ?? []
  const revoked = keys.data?.keys.filter((k) => k.revoked_at) ?? []
  const full = keys.data ? active.length >= keys.data.max : false

  return (
    <>
      {link && <ConfirmKey key={link.pk} app={app} link={link} onAdded={refresh} />}

      {shown && (
        <Card title={`New key: ${shown.key_info.name}`} className="border-accent/40">
          <Secret id="new-key" value={shown.key} onDone={() => setShown(null)}>
            This is the only time it's shown. Put it in your app's environment as <code>WAKESHIFT_KEY</code>. Anyone with it can act
            as your app.
          </Secret>
          <KeySDKNote appKey={shown.key} />
        </Card>
      )}

      {!app.deleted_at && (
        <Card title="Make a key">
          <form
            className="flex flex-col gap-3 sm:flex-row sm:items-end"
            onSubmit={(e: FormEvent) => {
              e.preventDefault()
              create.mutate()
            }}
          >
            <div className="flex-1">
              <Input label="Name" placeholder="Production, CI, Ada's laptop…" value={name} maxLength={60} onChange={(e) => setName(e.target.value)} />
            </div>
            <Button type="submit" disabled={create.isPending || full}>
              Make key
            </Button>
          </form>
          {full && <p className="mt-2 text-sm text-ink-3">An app can have {keys.data?.max} keys in use. Revoke one to make another.</p>}
          <ErrorText error={create.error} />
        </Card>
      )}

      <Card title="In use">
        {keys.isPending && <Loading />}
        {keys.isSuccess && active.length === 0 && (
          <Empty title="No keys in use">{app.deleted_at ? 'Deleting the app revoked its keys.' : 'Make one above. Your app stops working without one.'}</Empty>
        )}
        {active.length > 0 && (
          <Rows>
            {active.map((k) => (
              <Row
                key={k.key_id}
                actions={
                  editing?.key === k.key_id ? null : (
                  <>
                    <Button size="sm" variant="secondary" onClick={() => setEditing({ key: k.key_id, name: k.name })}>
                      Rename
                    </Button>
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={rotate.isPending}
                      onClick={() => window.confirm(`Replace "${k.name}" with a new key? This one stops working at once.`) && rotate.mutate(k.key_id)}
                    >
                      Rotate
                    </Button>
                    <Button
                      size="sm"
                      variant="danger"
                      disabled={revoke.isPending}
                      onClick={() =>
                        window.confirm(
                          active.length === 1
                            ? `Revoke "${k.name}"? It's your app's only key: your app stops working until you make another.`
                            : `Revoke "${k.name}"? It stops working at once.`,
                        ) && revoke.mutate(k.key_id)
                      }
                    >
                      Revoke
                    </Button>
                  </>
                  )
                }
              >
                {editing?.key === k.key_id ? (
                  <form
                    className="flex flex-col gap-2 sm:flex-row sm:items-end"
                    onSubmit={(e: FormEvent) => {
                      e.preventDefault()
                      rename.mutate(editing)
                    }}
                  >
                    <div className="flex-1">
                      <Input label={`Rename ${k.key_id}`} value={editing.name} maxLength={60} autoFocus onChange={(e) => setEditing({ key: k.key_id, name: e.target.value })} />
                    </div>
                    <Button type="submit" size="sm" disabled={rename.isPending}>
                      Save
                    </Button>
                    <Button type="button" size="sm" variant="secondary" onClick={() => setEditing(null)}>
                      Cancel
                    </Button>
                  </form>
                ) : (
                  <>
                    <span className="font-medium">{k.name}</span> <code className="ml-1 text-xs text-ink-3">{k.key_id}</code>
                  </>
                )}
                <Meta>
                  Made {when(k.created_at)} · {k.last_used_at ? `last used ${ago(k.last_used_at)}` : 'never used'}
                </Meta>
              </Row>
            ))}
          </Rows>
        )}
        <ErrorText error={keys.error ?? rotate.error ?? revoke.error ?? rename.error} />
      </Card>

      {revoked.length > 0 && (
        <Card title="Revoked">
          <Rows>
            {revoked.map((k) => (
              <Row key={k.key_id} muted>
                {k.name} <code className="ml-1 text-xs">{k.key_id}</code> <Badge>revoked</Badge>
                <Meta>
                  Made {when(k.created_at)} · revoked {when(k.revoked_at!)}
                  {k.last_used_at && ` · last used ${ago(k.last_used_at)}`}
                </Meta>
              </Row>
            ))}
          </Rows>
        </Card>
      )}
    </>
  )
}

// ConfirmKey adds the key `wakeshift-cli keys new` made on the developer's
// machine to the app, once the person has checked its code. The app's other
// keys keep working.
function ConfirmKey({ app, link, onAdded }: { app: OwnedApp; link: CLILink; onAdded: () => void }) {
  const key = useKeyCode(link.pk)
  const [name, setName] = useState('')
  const [added, setAdded] = useState<AppKey | null>(null)
  const add = useMutation({
    mutationFn: () => registerKey(app.id, { public_key: link.pk, name }),
    onSuccess: (k) => {
      forgetLink()
      setAdded(k.key_info)
      onAdded()
    },
  })
  if (added) {
    return (
      <Done title={`${added.name} (${added.key_id}) is one of ${app.name}'s keys. Your terminal sees it start working and carries on; your other keys keep working until you revoke them.`} />
    )
  }
  if (app.deleted_at) {
    return <Notice tone="caution" title="This app is deleted">It can't get new keys. Run the command again for another app.</Notice>
  }
  return (
    <Card title="Add the key from your terminal" className="border-accent/40">
      {key.invalid ? (
        invalidLink
      ) : (
        <form
          className="grid max-w-xl gap-4"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            add.mutate()
          }}
        >
          <p className="text-sm leading-relaxed text-ink-2">
            Your terminal made a new key for {app.name}. The key stays on your machine; Wakeshift keeps only its public half.
          </p>
          <KeyCode code={key.code} />
          <Input label="Name (optional)" placeholder="Ada's laptop, CI…" value={name} maxLength={60} onChange={(e) => setName(e.target.value)} />
          <div>
            <Button type="submit" disabled={!key.code || add.isPending}>
              Add key
            </Button>
          </div>
          {confirmError(add.error)}
        </form>
      )}
    </Card>
  )
}
