import { useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { can, deleteOrg, renameOrg, roleLabel, type Org } from '../../api/orgs'
import { Button, Card, ErrorText, Facts, Hint, Input, plural, when } from '../../components/ui'
import { OrgPage } from './common'

export default function OrgSettings() {
  return <OrgPage title="Settings">{(org) => <Body key={org.id} org={org} />}</OrgPage>
}

function Body({ org }: { org: Org }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [name, setName] = useState(org.name)
  const [confirm, setConfirm] = useState('')
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['org', org.id] })
    qc.invalidateQueries({ queryKey: ['orgs'] })
  }
  const save = useMutation({ mutationFn: () => renameOrg(org.id, name), onSuccess: refresh })
  const remove = useMutation({
    mutationFn: () => deleteOrg(org.id),
    onSuccess: () => {
      qc.removeQueries({ queryKey: ['developer-apps'] })
      refresh()
      navigate('/')
    },
  })
  const editable = can.renameOrg(org.role)

  return (
    <>
      <Card title="Organization">
        <form
          className="grid max-w-xl gap-4"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            save.mutate()
          }}
        >
          <Input label="Name" value={name} disabled={!editable} onChange={(e) => setName(e.target.value)} />
          <Hint>Invite emails and the join page show it.</Hint>
          {editable ? (
            <div className="flex items-center gap-3">
              <Button type="submit" disabled={name === org.name || save.isPending}>
                Save
              </Button>
              {save.isSuccess && name === org.name && <span className="text-sm text-sage">Saved</span>}
            </div>
          ) : (
            <Hint>Admins and owners rename it.</Hint>
          )}
          <ErrorText error={save.error} />
        </form>
        <div className="mt-6 border-t border-rule pt-5">
          <Facts
            items={[
              ['Your role', roleLabel[org.role]],
              ['Members', org.members],
              ['Apps', org.apps],
              ['Made', when(org.created_at)],
              ['ID', <code className="text-xs">{org.id}</code>],
            ]}
          />
        </div>
      </Card>

      {can.deleteOrg(org.role) && (
        <section className="rounded-xl border border-danger/40 bg-card p-5 sm:p-6">
          <h2 className="font-serif text-xl text-danger">Danger zone</h2>
          {org.apps > 0 ? (
            <p className="mt-2 max-w-2xl text-sm leading-relaxed text-ink-2">
              {org.name} has {plural(org.apps, 'app')}. Delete {org.apps === 1 ? 'it' : 'them'} first: deleting an app deletes its users'
              data within 30 days.
            </p>
          ) : (
            <>
              <p className="mt-2 max-w-2xl text-sm leading-relaxed text-ink-2">
                Deleting {org.name} removes its members and revokes its invites. The logs of apps it deleted in the last 30 days can't be
                read any more; they're still deleted on schedule. This can't be undone.
              </p>
              <form
                className="mt-4 flex max-w-xl flex-col gap-3 sm:flex-row sm:items-end"
                onSubmit={(e: FormEvent) => {
                  e.preventDefault()
                  remove.mutate()
                }}
              >
                <div className="flex-1">
                  <Input label={`Type ${org.name} to confirm`} value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" />
                </div>
                <Button type="submit" variant="danger" className="border-danger/50" disabled={confirm !== org.name || remove.isPending}>
                  Delete organization
                </Button>
              </form>
            </>
          )}
          <ErrorText error={remove.error} />
        </section>
      )}
    </>
  )
}
