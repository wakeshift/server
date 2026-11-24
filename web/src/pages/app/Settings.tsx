import { useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { deleteOwnedApp, updateOwnedApp, type OwnedApp } from '../../api/developer'
import { PencilSimple } from '@phosphor-icons/react'
import { Button, Card, ErrorText, Facts, Hint, Input, plural } from '../../components/ui'
import { AppPage } from './common'
import { can, roleLabel } from '../../api/orgs'

export default function AppSettings() {
  return <AppPage title="Settings">{(app) => <Body key={app.id} app={app} />}</AppPage>
}

function Body({ app }: { app: OwnedApp }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [name, setName] = useState(app.name)
  const [mcp, setMCP] = useState(app.mcp_url)
  const [confirm, setConfirm] = useState('')
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['developer-app', app.id] })
    qc.invalidateQueries({ queryKey: ['developer-apps'] })
  }
  const [editing, setEditing] = useState(false)
  const save = useMutation({
    mutationFn: () => updateOwnedApp(app.id, { name, mcp_url: mcp }),
    onSuccess: () => {
      refresh()
      setEditing(false)
    },
  })
  const cancel = () => {
    setName(app.name)
    setMCP(app.mcp_url)
    save.reset()
    setEditing(false)
  }
  const remove = useMutation({
    mutationFn: () => deleteOwnedApp(app.id),
    onSuccess: () => {
      refresh()
      navigate('/')
    },
  })
  const changed = name !== app.name || mcp !== app.mcp_url
  const deleted = !!app.deleted_at
  // Settings and deleting are for admins and owners.
  const editable = can.updateApp(app.role) && !deleted

  return (
    <>
      <Card
        title="App"
        actions={
          editable &&
          !editing && (
            <button
              type="button"
              aria-label="Edit"
              title="Edit"
              className="-my-1 -mr-2 rounded-md p-2 text-ink-2 transition-colors hover:bg-paper-2 hover:text-ink focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ink"
              onClick={() => {
                save.reset()
                setEditing(true)
              }}
            >
              <PencilSimple size={18} aria-hidden />
            </button>
          )
        }
      >
        {editing ? (
          <form
            className="grid max-w-xl gap-4"
            onSubmit={(e: FormEvent) => {
              e.preventDefault()
              save.mutate()
            }}
          >
            <Input label="ID" value={app.id} disabled readOnly />
            <Hint>An app's ID is permanent: its keys and events carry it.</Hint>
            <Input label="Name" value={name} autoFocus onChange={(e) => setName(e.target.value)} />
            <Hint>Agents read it in what Wakeshift tells them ("Quizzer ended your subscription").</Hint>
            <Input label="MCP URL (optional)" value={mcp} placeholder="https://my-app.example.com/mcp" onChange={(e) => setMCP(e.target.value)} />
            <Hint>Where your app's MCP server is, for your team's reference. Wakeshift doesn't use it.</Hint>
            <div className="flex items-center gap-3">
              <Button type="submit" disabled={!changed || !name || save.isPending}>
                Save
              </Button>
              <Button type="button" variant="secondary" onClick={cancel}>
                Cancel
              </Button>
            </div>
            <ErrorText error={save.error} />
          </form>
        ) : (
          <div className="space-y-4">
            <Facts
              items={[
                ['ID', <code>{app.id}</code>],
                ['Name', app.name],
                ['MCP URL', app.mcp_url ? <span className="break-all">{app.mcp_url}</span> : <span className="text-ink-3">None</span>],
              ]}
            />
            {save.isSuccess && <p className="text-sm text-sage">Saved</p>}
            {!deleted && !editable && (
              <Hint>
                You're a {roleLabel[app.role].toLowerCase()} in this app's organization. Admins and owners change its settings and delete it.
              </Hint>
            )}
          </div>
        )}
      </Card>

      {(deleted || can.deleteApp(app.role)) && (
        <section className="rounded-xl border border-danger/40 bg-card p-5 sm:p-6">
          <h2 className="font-serif text-xl text-danger">Danger zone</h2>
          {deleted ? (
            <p className="mt-2 text-sm leading-relaxed text-ink-2">
              This app is deleted. Its users' data is deleted within 30 days, and its log 30 days after the app. Its ID can't be used again.
            </p>
          ) : (
            <>
              <p className="mt-2 max-w-2xl text-sm leading-relaxed text-ink-2">
                Deleting {app.name} stops {plural(app.keys, 'key')} at once, ends every subscription it made and closes its agents' streams.
                Its users' conversations stop waking.
              </p>
              <p className="mt-2 max-w-2xl text-sm leading-relaxed text-ink-2">
                <strong className="text-ink">This deletes your users' data within 30 days:</strong> the workspaces {app.name} keeps for its
                users, with their agents, conversations, subscriptions, events and deliveries, and the grants people gave it.
                This can't be undone. You can read its log for 30 days, then it's deleted too. Its ID can't be used again.
              </p>
              <form
                className="mt-4 flex max-w-xl flex-col gap-3 sm:flex-row sm:items-end"
                onSubmit={(e: FormEvent) => {
                  e.preventDefault()
                  remove.mutate()
                }}
              >
                <div className="flex-1">
                  <Input label={`Type ${app.id} to confirm`} value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" />
                </div>
                <Button type="submit" variant="danger" className="border-danger/50" disabled={confirm !== app.id || remove.isPending}>
                  Delete app
                </Button>
              </form>
              <ErrorText error={remove.error} />
            </>
          )}
        </section>
      )}
    </>
  )
}
