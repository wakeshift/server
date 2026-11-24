import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { revokeKey, rotateKey, type NewKey } from '../../api/developer'
import { can, listMembers, memberMade, removeMember, roleLabel, setRole, type Member, type Org, type Role } from '../../api/orgs'
import { appPath } from '../../components/Layout'
import { Badge, Button, ButtonLink, Card, ErrorText, Loading, Meta, Notice, Row, Rows, Secret, Select, TextLink, when } from '../../components/ui'
import { useSession } from '../../hooks/useSession'
import { OrgPage } from './common'
import { Invites } from './Invites'

export default function OrgMembers() {
  return (
    <OrgPage
      title="Members"
      intro="Everyone here manages this organization's apps. Owners and admins manage people and invite them by email, with a role: the link works once, only for that address, for 7 days, and someone without a Wakeshift account can sign up through it. Developers do the work on apps, keys included."
    >
      {(org) => <Body key={org.id} org={org} />}
    </OrgPage>
  )
}

const roles: Role[] = ['owner', 'admin', 'developer']

function Body({ org }: { org: Org }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const me = useSession().session.data?.user?.id
  const members = useQuery({ queryKey: ['org-members', org.id], queryFn: () => listMembers(org.id) })
  const [removing, setRemoving] = useState<Member | null>(null)
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['org-members', org.id] })
    qc.invalidateQueries({ queryKey: ['org', org.id] })
    qc.invalidateQueries({ queryKey: ['orgs'] })
  }
  const change = useMutation({ mutationFn: (v: { user: string; role: Role }) => setRole(org.id, v.user, v.role), onSuccess: refresh })
  const leave = useMutation({
    mutationFn: () => removeMember(org.id, me!),
    onSuccess: () => {
      qc.removeQueries({ queryKey: ['developer-apps'] })
      refresh()
      navigate('/')
    },
  })

  return (
    <Invites org={org}>
      <Card title="People">
        {members.isPending && <Loading />}
        {members.data && (
          <Rows>
            {members.data.map((m) => {
              const self = m.user_id === me
              return (
                <Row
                  key={m.user_id}
                  actions={
                    <>
                      {can.changeRoles(org.role) ? (
                        <div className="w-36">
                          <Select
                            ariaLabel={`${m.email}'s role`}
                            value={m.role}
                            disabled={change.isPending}
                            onChange={(e) => change.mutate({ user: m.user_id, role: e.target.value as Role })}
                          >
                            {roles.map((r) => (
                              <option key={r} value={r}>
                                {roleLabel[r]}
                              </option>
                            ))}
                          </Select>
                        </div>
                      ) : (
                        <Badge tone={m.role === 'owner' ? 'accent' : 'neutral'}>{roleLabel[m.role]}</Badge>
                      )}
                      {self ? (
                        <Button
                          size="sm"
                          variant="danger"
                          disabled={leave.isPending}
                          onClick={() => window.confirm(`Leave ${org.name}? You lose access to its apps at once.`) && leave.mutate()}
                        >
                          Leave
                        </Button>
                      ) : (
                        can.manage(org.role, m.role) && (
                          <Button size="sm" variant="danger" onClick={() => setRemoving(m)}>
                            Remove
                          </Button>
                        )
                      )}
                    </>
                  }
                >
                  <span className="font-medium text-ink">{m.name || m.email}</span>
                  {self && <span className="ml-2 text-xs text-ink-3">you</span>}
                  <Meta>
                    {m.name && <>{m.email} · </>}joined {when(m.joined_at)}
                    {m.invited_by && <> · invited by {m.invited_by}</>}
                  </Meta>
                </Row>
              )
            })}
          </Rows>
        )}
        <ErrorText error={members.error ?? change.error ?? leave.error} />
      </Card>
      {removing && <Remove org={org} member={removing} onDone={() => setRemoving(null)} onRemoved={refresh} />}
    </Invites>
  )
}

// Remove: who's being removed, and what they made that still works. Keys
// belong to the app, not to the person, and Wakeshift can't know who copied
// a key that was shown once, so rotate any they could have seen.
function Remove({ org, member, onDone, onRemoved }: { org: Org; member: Member; onDone: () => void; onRemoved: () => void }) {
  const qc = useQueryClient()
  const made = useQuery({ queryKey: ['org-made', org.id, member.user_id], queryFn: () => memberMade(org.id, member.user_id) })
  const [rotated, setRotated] = useState<NewKey | null>(null)
  const refreshMade = () => qc.invalidateQueries({ queryKey: ['org-made', org.id, member.user_id] })
  const revoke = useMutation({ mutationFn: (k: { app: string; key: string }) => revokeKey(k.app, k.key), onSuccess: refreshMade })
  const rotate = useMutation({
    mutationFn: (k: { app: string; key: string }) => rotateKey(k.app, k.key),
    onSuccess: (n) => {
      setRotated(n)
      refreshMade()
    },
  })
  const remove = useMutation({
    mutationFn: () => removeMember(org.id, member.user_id),
    onSuccess: () => {
      onRemoved()
      onDone()
    },
  })
  const keys = made.data?.keys ?? []
  const urls = made.data?.publish_urls ?? []
  return (
    <Card title={`Remove ${member.name || member.email}`} className="border-danger/40">
      <p className="text-sm leading-relaxed text-ink-2">
        They lose access to {org.name}'s apps on their next request. Their own Wakeshift workspace isn't touched.
      </p>
      {made.isPending && <Loading />}
      {made.data && keys.length + urls.length === 0 && (
        <p className="mt-3 text-sm leading-relaxed text-ink-2">They made no keys or publish URLs that still work.</p>
      )}
      {keys.length + urls.length > 0 && (
        <div className="mt-4 space-y-3">
          <Notice tone="caution" title="What they made still works">
            Removing someone doesn't stop the keys and publish URLs they made: they belong to the app. Revoke or rotate these, and rotate
            any other key they could have seen.
          </Notice>
          <Rows>
            {keys.map((k) => (
              <Row
                key={`${k.app_id}/${k.key_id}`}
                actions={
                  <>
                    <Button size="sm" variant="secondary" disabled={rotate.isPending} onClick={() => rotate.mutate({ app: k.app_id, key: k.key_id })}>
                      Rotate
                    </Button>
                    <Button size="sm" variant="danger" disabled={revoke.isPending} onClick={() => revoke.mutate({ app: k.app_id, key: k.key_id })}>
                      Revoke
                    </Button>
                  </>
                }
              >
                Key <b>{k.name}</b> <code className="ml-1 text-xs">{k.key_id}</code>
                <Meta>
                  <TextLink to={appPath(k.app_id, 'keys')}>{k.app_id}</TextLink> · made {when(k.created_at)}
                </Meta>
              </Row>
            ))}
            {urls.map((u) => (
              <Row key={u.id} actions={<ButtonLink size="sm" variant="secondary" to={appPath(u.app_id, 'publish-urls')}>Revoke</ButtonLink>}>
                Publish URL for <code className="text-xs">{u.topic}</code>
                <Meta>
                  {u.app_id} · made {when(u.created_at)}
                </Meta>
              </Row>
            ))}
          </Rows>
        </div>
      )}
      {rotated && (
        <div className="mt-4">
          <Secret id="rotated-key" value={rotated.key} onDone={() => setRotated(null)}>
            The new key for <b>{rotated.key_info.name}</b>, shown once. The old one stopped working.
          </Secret>
        </div>
      )}
      <div className="mt-5 flex flex-wrap gap-2">
        <Button variant="danger" className="border-danger/50" disabled={remove.isPending} onClick={() => remove.mutate()}>
          Remove {member.name || member.email}
        </Button>
        <Button variant="secondary" onClick={onDone}>
          Cancel
        </Button>
      </div>
      <ErrorText error={made.error ?? revoke.error ?? rotate.error ?? remove.error} />
    </Card>
  )
}
