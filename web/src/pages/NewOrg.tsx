import { useState, type FormEvent } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { createOrg } from '../api/orgs'
import { orgPath } from '../components/Layout'
import { Button, Card, ErrorText, Hint, Input, PageHeader } from '../components/ui'
import { chooseOrg } from '../hooks/useOrg'

// A new organization: a name; you're its owner.
export default function NewOrg() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const create = useMutation({
    mutationFn: () => createOrg(name),
    onSuccess: async (o) => {
      await qc.invalidateQueries({ queryKey: ['orgs'] })
      chooseOrg(o.id)
      navigate(orgPath(o.id, 'members'))
    },
  })
  return (
    <div className="space-y-7">
      <PageHeader kicker="New organization" title="Make an organization">
        An organization holds apps and the people who manage them with you. You're its owner; invite the others next.
      </PageHeader>
      <Card>
        <form
          className="grid max-w-xl gap-4"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            create.mutate()
          }}
        >
          <Input label="Name" placeholder="Acme" value={name} autoFocus onChange={(e) => setName(e.target.value)} />
          <Hint>Invite emails and the join page show it. You can change it later.</Hint>
          <div>
            <Button type="submit" disabled={!name.trim() || create.isPending}>
              Make it
            </Button>
          </div>
          <ErrorText error={create.error} />
        </form>
      </Card>
    </div>
  )
}
