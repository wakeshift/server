import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getInvite } from '../../api/client'
import { can, createInvite, listInvites, resendInvite, revokeInvite, roleLabel, type Org, type Role, type SentInvite } from '../../api/orgs'
import type { ReactNode } from 'react'
import { Badge, Button, Card, ErrorText, Input, Meta, Notice, Row, Rows, Secret, Select, when } from '../../components/ui'

const roles: Role[] = ['developer', 'admin', 'owner']

// Invites: the Members tab's invite form and the invites still waiting, with
// the people already in the organization (children) between them. For
// someone who can't invite (a developer), just the children.
export function Invites({ org, children }: { org: Org; children: ReactNode }) {
  const qc = useQueryClient()
  const invites = useQuery({ queryKey: ['org-invites', org.id], queryFn: () => listInvites(org.id), enabled: can.invite(org.role) })
  const signUps = useQuery({ queryKey: ['invite'], queryFn: getInvite })
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>('developer')
  const [sent, setSent] = useState<SentInvite | null>(null)
  const refresh = () => qc.invalidateQueries({ queryKey: ['org-invites', org.id] })
  const onSent = (s: SentInvite) => {
    setSent(s)
    refresh()
  }
  const send = useMutation({
    mutationFn: () => createInvite(org.id, email, role),
    onSuccess: (s) => {
      setEmail('')
      onSent(s)
    },
  })
  const resend = useMutation({ mutationFn: (id: string) => resendInvite(org.id, id), onSuccess: onSent })
  const revoke = useMutation({ mutationFn: (id: string) => revokeInvite(org.id, id), onSuccess: refresh })

  if (!can.invite(org.role)) return <>{children}</>
  const pending = invites.data?.invites ?? []
  const left = signUps.data?.uses_left

  return (
    <>
      {sent && (
        <Card title={sent.emailed ? `Invite emailed to ${sent.invite.email}` : `Send ${sent.invite.email} this link`} className="border-accent/40">
          <Secret id="invite-url" value={sent.url} onDone={() => setSent(null)}>
            {sent.emailed
              ? "It's shown once, here, if you'd rather send it in a chat too. "
              : invites.data?.email
                ? "The email didn't go. Send them this link yourself; it's shown once. "
                : "Email isn't set up on this server yet. Send them this link yourself; it's shown once. "}
            It works only for {sent.invite.email}, until {when(sent.invite.expires_at)}.
          </Secret>
        </Card>
      )}

      <Card title="Invite someone">
        <form
          className="grid max-w-xl gap-4 sm:grid-cols-[1fr_11rem_auto] sm:items-end"
          onSubmit={(e: FormEvent) => {
            e.preventDefault()
            send.mutate()
          }}
        >
          <Input label="Email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="ada@example.com" />
          <Select label="Role" value={role} onChange={(e) => setRole(e.target.value as Role)}>
            {roles
              .filter((r) => can.grant(org.role, r))
              .map((r) => (
                <option key={r} value={r}>
                  {roleLabel[r]}
                </option>
              ))}
          </Select>
          <Button type="submit" disabled={!email || send.isPending}>
            Invite
          </Button>
        </form>
        <p className="mt-3 text-xs leading-relaxed text-ink-3">
          {org.role === 'admin' ? 'Admins invite developers. ' : ''}
          {left !== undefined &&
            (left > 0
              ? `Someone new to Wakeshift who signs up through an invite uses one of your sign-ups: you have ${left} left.`
              : 'You have no sign-ups left: invites work for people who already have a Wakeshift account.')}
        </p>
        <ErrorText error={send.error} />
      </Card>

      {children}

      {/* Only while some are waiting: it's where they're resent or revoked. */}
      {(pending.length > 0 || invites.error || resend.error || revoke.error) && (
        <Card title="Waiting invites">
          {pending.length > 0 && (
            <Rows>
              {pending.map((i) => (
                <Row
                  key={i.id}
                  muted={i.expired}
                  actions={
                    can.manage(org.role, i.role) && (
                      <>
                        <Button size="sm" variant="secondary" disabled={resend.isPending} onClick={() => resend.mutate(i.id)}>
                          Resend
                        </Button>
                        <Button size="sm" variant="danger" disabled={revoke.isPending} onClick={() => revoke.mutate(i.id)}>
                          Revoke
                        </Button>
                      </>
                    )
                  }
                >
                  {i.email} <Badge>{roleLabel[i.role]}</Badge>
                  <Meta>
                    {i.expired ? `Expired ${when(i.expires_at)}` : `Expires ${when(i.expires_at)}`} · invited by {i.invited_by}
                  </Meta>
                </Row>
              ))}
            </Rows>
          )}
          {invites.data && !invites.data.email && (
            <div className="mt-4">
              <Notice>Email isn't set up on this server yet: each invite shows its link to send yourself.</Notice>
            </div>
          )}
          <ErrorText error={invites.error ?? resend.error ?? revoke.error} />
        </Card>
      )}
    </>
  )
}
