import { useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { createOwnedApp, registerApp, type NewKey, type OwnedApp } from '../api/developer'
import { appPath } from '../components/Layout'
import { Button, ButtonLink, Card, ErrorText, Hint, Input, Loading, PageHeader, Secret, Select } from '../components/ui'
import { chooseOrg, useCurrentOrg } from '../hooks/useOrg'
import { KeySDKNote } from '../components/KeySDKNote'
import { codeMatches, confirmError, Done, invalidLink, TypeKeyCode, useCLILink, useKeyCode } from '../components/CLIConfirm'
import { forgetLink, type CLILink } from '../lib/cliLink'

// /new-app is the form below, or, opened from @wakeshift/cli's link
// (#pk=…&id=…&name=…), the page that confirms the app the CLI proposed.
export default function NewApp() {
  const link = useCLILink()
  return link ? <ConfirmNewApp key={link.pk} link={link} /> : <RegisterApp />
}

// Registering an app: an ID, a name and (optionally) an MCP URL. The first
// key is shown once, here.
function RegisterApp() {
  const qc = useQueryClient()
  const [id, setID] = useState('')
  const [name, setName] = useState('')
  const [mcp, setMCP] = useState('')
  const [keyName, setKeyName] = useState('')
  const [made, setMade] = useState<(NewKey & { app: OwnedApp }) | null>(null)
  const org = useCurrentOrg().current
  const create = useMutation({
    mutationFn: () => createOwnedApp({ org_id: org?.id, id, name, mcp_url: mcp, key_name: keyName }),
    onSuccess: (m) => {
      setMade(m)
      qc.invalidateQueries({ queryKey: ['developer-apps'] })
    },
  })

  if (made) {
    return (
      <div className="space-y-7">
        <PageHeader context="New app" title={`${made.app.name} is registered`}>
          Here's its first key, named <b>{made.key_info.name}</b>. Make more, or revoke this one, in Keys.
        </PageHeader>
        <Card title="Your key" className="border-accent/40">
          <Secret id="app-key" value={made.key}>
            This is the only time it's shown. Put it in your app's environment as <code>WAKESHIFT_KEY</code>. Anyone with it can act as
            your app.
          </Secret>
          <KeySDKNote appKey={made.key} />
        </Card>
        <ButtonLink to={appPath(made.app.id)}>Open {made.app.name}</ButtonLink>
      </div>
    )
  }

  return (
    <div className="space-y-7">
      <PageHeader context="New app" title="Register an app">
        {org && !org.personal ? <>In {org.name}. </> : null}You get a key at once, ready for the SDK or the HTTP API.
      </PageHeader>
      <Card>
        <form
          className="grid max-w-xl gap-4"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            create.mutate()
          }}
        >
          <Input label="ID" placeholder="my-app" value={id} autoFocus onChange={(e) => setID(e.target.value)} />
          <Hint>Permanent and unique across Wakeshift: 2-40 lowercase letters, digits and dashes. Your events belong to it.</Hint>
          <Input label="Name" placeholder="My App" value={name} onChange={(e) => setName(e.target.value)} />
          <Input label="MCP URL (optional)" placeholder="https://my-app.example.com/mcp" value={mcp} onChange={(e) => setMCP(e.target.value)} />
          <Hint>Where your app's MCP server is, for your team's reference. Wakeshift doesn't use it.</Hint>
          <Input label="First key's name (optional)" placeholder="Default" value={keyName} maxLength={60} onChange={(e) => setKeyName(e.target.value)} />
          <Hint>Production, CI, Ada's laptop… You can rename it, and make more, under Keys.</Hint>
          <div>
            <Button type="submit" disabled={!org || !id || !name || create.isPending}>
              Register
            </Button>
          </div>
          <ErrorText error={create.error} />
        </form>
      </Card>
    </div>
  )
}

// ConfirmNewApp creates the app the CLI proposed, with the key it made on
// the developer's machine as its only key: the ID and name (editable), the
// organization (a choice if they're in several), and the key code, typed
// from the terminal. Nothing is sent until they press Create app.
function ConfirmNewApp({ link }: { link: CLILink }) {
  const qc = useQueryClient()
  const { orgs, current } = useCurrentOrg()
  const [orgID, setOrgID] = useState<string>()
  const org = orgs.data?.find((o) => o.id === orgID) ?? current
  const [id, setID] = useState(link.id)
  const [name, setName] = useState(link.name || link.id)
  const key = useKeyCode(link.pk)
  const [typed, setTyped] = useState('')
  const [made, setMade] = useState<OwnedApp | null>(null)
  const create = useMutation({
    mutationFn: () => registerApp({ org_id: org?.id, id, name, public_key: link.pk, key_code: typed.trim() }),
    onSuccess: (m) => {
      forgetLink()
      if (org) chooseOrg(org.id)
      qc.invalidateQueries({ queryKey: ['developer-apps'] })
      qc.invalidateQueries({ queryKey: ['orgs'] })
      setMade(m.app)
    },
  })

  if (made) {
    return (
      <div className="space-y-7">
        <PageHeader context="New app" title={`${made.name} is created`} />
        <Done title="Your terminal sees the key start working and carries on. The key stays on your machine; Wakeshift keeps only its public half.">
          <ButtonLink variant="secondary" to={appPath(made.id)}>
            Open {made.name}
          </ButtonLink>
        </Done>
      </div>
    )
  }

  return (
    <div className="space-y-7">
      <PageHeader context="New app" title="Create your app">
        Your terminal made a key for a new app. Type the key code it shows, then create the app with that key.
      </PageHeader>
      <Card>
        {key.invalid ? (
          invalidLink
        ) : (
          <form
            className="grid max-w-xl gap-4"
            onSubmit={(e: FormEvent) => {
              e.preventDefault()
              create.mutate()
            }}
          >
            <TypeKeyCode value={typed} onChange={setTyped} code={key.code} />
            {orgs.isPending && <Loading />}
            {orgs.data && orgs.data.length > 1 && (
              <Select label="Organization" value={org?.id ?? ''} onChange={(e) => setOrgID(e.target.value)}>
                {orgs.data.map((o) => (
                  <option key={o.id} value={o.id}>
                    {o.personal ? `${o.name} (personal)` : o.name}
                  </option>
                ))}
              </Select>
            )}
            <Input label="App ID" value={id} onChange={(e) => setID(e.target.value)} />
            <Hint>Permanent and unique across Wakeshift: 2-40 lowercase letters, digits and dashes. Your events belong to it.</Hint>
            <Input label="Name" value={name} onChange={(e) => setName(e.target.value)} />
            <Hint>What your app's users see. You can change it later in Settings.</Hint>
            <div>
              <Button type="submit" disabled={!org || !id || !name || !codeMatches(typed, key.code) || create.isPending}>
                Create app
              </Button>
            </div>
            {confirmError(create.error)}
          </form>
        )}
      </Card>
    </div>
  )
}
