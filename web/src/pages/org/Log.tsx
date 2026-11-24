import { useInfiniteQuery } from '@tanstack/react-query'
import { orgLog, roleLabel, type OrgLogEntry, type Role } from '../../api/orgs'
import { Button, Card, ErrorText, Loading, Meta, Row, Rows, when } from '../../components/ui'
import { actorLabel, OrgPage } from './common'

export default function OrgLog() {
  return (
    <OrgPage title="Log" intro="Everything that changed in this organization: who joined, left or was removed, roles, invites, its name.">
      {(org) => <Body key={org.id} id={org.id} />}
    </OrgPage>
  )
}

const role = (r: unknown) => (typeof r === 'string' && r in roleLabel ? roleLabel[r as Role].toLowerCase() : String(r ?? ''))

// describe says what an entry did, in a line.
function describe(e: OrgLogEntry): string {
  const d = e.detail
  switch (e.action) {
    case 'org.created':
      return d.backfilled ? 'Made for its owner when organizations arrived' : 'Made the organization'
    case 'org.renamed':
      return `Renamed it from ${d.old_name} to ${d.name}`
    case 'org.deleted':
      return 'Deleted the organization'
    case 'org.member_joined':
      return `Joined as ${role(d.role)}`
    case 'org.member_left':
      return 'Left'
    case 'org.member_removed':
      return `Removed a member (${role(d.role)})`
    case 'org.role_changed':
      return `Changed a member from ${role(d.old_role)} to ${role(d.role)}`
    case 'org.invite_sent':
      return `Invited ${d.email} as ${role(d.role)}`
    case 'org.invite_resent':
      return `Sent ${d.email}'s invite again`
    case 'org.invite_revoked':
      return d.expired ? `Replaced ${d.email}'s expired invite` : `Revoked ${d.email}'s invite`
    case 'org.invite_accepted':
      return `Accepted the invite for ${d.email}`
    default:
      return e.action
  }
}

function Body({ id }: { id: string }) {
  const log = useInfiniteQuery({
    queryKey: ['org-log', id],
    queryFn: ({ pageParam }) => orgLog(id, pageParam),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.length === 50 ? last[last.length - 1].id : undefined),
  })
  const entries = log.data?.pages.flat() ?? []
  return (
    <Card>
      {log.isPending && <Loading />}
      <Rows>
        {entries.map((e) => (
          <Row key={e.id}>
            {describe(e)}
            <Meta>
              {actorLabel(e.actor, e.actor_email)} · {when(e.at)}
            </Meta>
          </Row>
        ))}
      </Rows>
      {log.hasNextPage && (
        <div className="mt-4">
          <Button size="sm" variant="secondary" disabled={log.isFetchingNextPage} onClick={() => log.fetchNextPage()}>
            More
          </Button>
        </div>
      )}
      <ErrorText error={log.error} />
    </Card>
  )
}
